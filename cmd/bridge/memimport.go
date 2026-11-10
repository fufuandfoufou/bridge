package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"bridge/internal/config"
	"bridge/internal/fsutil"
	"bridge/internal/memory"
	"bridge/internal/project"
	"bridge/internal/store"
)

// importScope is what the user chose to import.
type importScope struct {
	dir, since, agent string
	all               bool
}

func (s *importScope) flags(c *cobra.Command) {
	c.Flags().StringVar(&s.dir, "dir", "", "project directory (default: the current one)")
	c.Flags().BoolVar(&s.all, "all", false, "every project")
	c.Flags().StringVar(&s.since, "since", "90d", "how far back, such as 30d, 365d or all")
	c.Flags().StringVar(&s.agent, "agent", "", "only this agent's sessions")
}

func (s importScope) args() []string {
	out := []string{"--since", s.since}
	if s.all {
		out = append(out, "--all")
	} else if s.dir != "" {
		out = append(out, "--dir", s.dir)
	}
	if s.agent != "" {
		out = append(out, "--agent", s.agent)
	}
	return out
}

func parseSince(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "all" {
		return time.Time{}, nil
	}
	if d, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(d)
		if err != nil || n <= 0 {
			return time.Time{}, fmt.Errorf("--since %q：请写成 30d、365d 或 all", s)
		}
		return time.Now().UTC().AddDate(0, 0, -n), nil
	}
	return time.Time{}, fmt.Errorf("--since %q：请写成 30d、365d 或 all", s)
}

// planImport indexes sessions first, so history bridge hasn't read yet counts.
func planImport(ctx context.Context, st *store.Store, sc importScope) (memory.ImportPlan, *config.Config, error) {
	dir := sc.dir
	if dir == "" && !sc.all {
		wd, err := os.Getwd()
		if err != nil {
			return memory.ImportPlan{}, nil, err
		}
		dir = wd
	}
	cfg, err := cfgFor(dir)
	if err != nil {
		return memory.ImportPlan{}, nil, err
	}
	if !cfg.Sync.Memory {
		return memory.ImportPlan{}, cfg, errors.New("记忆同步已关闭（sync.memory），不能导入")
	}
	since, err := parseSince(sc.since)
	if err != nil {
		return memory.ImportPlan{}, cfg, err
	}
	if !fsutil.Paused() {
		if _, err := ingester(st, registry(cfg)).Sync(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "警告：部分会话采集失败：", err)
		}
	}
	f := memory.ImportFilter{Agent: sc.agent, Since: since}
	if !sc.all {
		f.ProjectID = project.Identify(config.ProjectRoot(dir)).ID
	}
	p, err := memory.PlanImport(st, cfgFor, f, cfg.Memory.Extractor)
	return p, cfg, err
}

func printImportPlan(p memory.ImportPlan) {
	fmt.Printf("将处理 %d 个会话，调用 %s %d 次，约 %d 个输入 token。\n", p.Sessions, p.Extractor, p.Calls, p.Tokens)
	for reason, n := range p.Skipped {
		fmt.Printf("  跳过 %d 个：%s\n", n, reason)
	}
	for i, it := range p.Items {
		if i == 10 {
			fmt.Printf("  …另有 %d 个\n", len(p.Items)-10)
			break
		}
		title := it.Title
		if title == "" {
			title = it.Session
		}
		fmt.Printf("  - %s · %s · %d token\n", title, it.Agent, it.Tokens)
	}
}

func memoryImportCmd() *cobra.Command {
	var sc importScope
	var dry, yes bool
	c := &cobra.Command{Use: "import", Short: "Extract memories from past sessions (sends conversation text to the extractor)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Cancelling from the app sends SIGTERM; progress must still be saved.
			ctx, stop := signalCtx()
			defer stop()
			if fsutil.Paused() {
				return errors.New("同步已暂停，恢复后再导入")
			}
			st, err := openStore()
			if err != nil {
				return err
			}
			defer st.Close()
			p, cfg, err := planImport(ctx, st, sc)
			if err != nil {
				return err
			}
			printImportPlan(p)
			if dry || p.Sessions == 0 {
				return nil
			}
			if !yes {
				fmt.Println("脱敏后的对话文本会发送给提取用的 agent，并消耗你的额度；工具输出不会发送。新记忆会先进入待审核。")
				if !confirm("继续？") {
					return errors.New("已取消")
				}
			}
			im := memory.Importer{Store: st, ConfigFor: cfgFor,
				NewExtractor: func(agent string) memory.Extractor { return memory.CLIExtractor{Agent: agent} },
				Progress: func(s memory.ImportStatus) {
					if s.State == memory.ImportRunning && s.Current != "" {
						fmt.Printf("[%d/%d] %s\n", s.Done+1, s.Total, s.Current)
					}
				}}
			res, err := im.Run(ctx, p)
			if _, rerr := memory.RenderBlocks(context.Background(), st, registry(cfg), cfg); rerr != nil && err == nil {
				err = rerr
			}
			fmt.Printf("完成 %d/%d 个会话，新增 %d 条记忆，已放入待审核（bridge memory review）。\n", res.Done, res.Total, res.Added)
			if res.State == memory.ImportCancelled {
				fmt.Println("已中断；再次运行会从中断处继续。")
			}
			return err
		}}
	sc.flags(c)
	c.Flags().BoolVar(&dry, "dry-run", false, "only show what would be sent")
	c.Flags().BoolVar(&yes, "yes", false, "skip the confirmation")
	return c
}

// startImport runs the import as its own process, so it outlives the app.
func startImport(sc importScope) error {
	if s, err := memory.LoadImportStatus(); err != nil {
		return err
	} else if s.State == memory.ImportRunning {
		return memory.ErrImportRunning
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := fsutil.EnsurePrivateDir(fsutil.DataDir()); err != nil {
		return err
	}
	logf, err := os.OpenFile(fsutil.DataDir()+"/memory-import.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(exe, append([]string{"memory", "import", "--yes"}, sc.args()...)...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = logf, logf, nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Report a start failure, such as a bad flag, instead of a silent exit.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for i := 0; i < 40; i++ {
		select {
		case err := <-done:
			if err != nil {
				b, _ := os.ReadFile(fsutil.DataDir() + "/memory-import.log")
				return fmt.Errorf("导入没有启动：%s", strings.TrimSpace(string(b)))
			}
			return nil
		case <-time.After(50 * time.Millisecond):
		}
		if s, _ := memory.LoadImportStatus(); s.State == memory.ImportRunning && s.PID == cmd.Process.Pid {
			return nil
		}
	}
	return nil
}

func cancelImport() error {
	s, err := memory.LoadImportStatus()
	if err != nil {
		return err
	}
	if s.State != memory.ImportRunning {
		return nil
	}
	return syscall.Kill(s.PID, syscall.SIGTERM)
}
