// Command bridge syncs sessions, memories, rules and skills across coding agents.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"bridge/adapters/claudecode"
	"bridge/adapters/codex"
	"bridge/adapters/dsh"
	"bridge/adapters/external"
	"bridge/adapters/pi"
	"bridge/internal/config"
	"bridge/internal/daemon"
	"bridge/internal/filesync"
	"bridge/internal/fsutil"
	"bridge/internal/handoff"
	"bridge/internal/mcpserver"
	"bridge/internal/memory"
	"bridge/internal/model"
	"bridge/internal/project"
	"bridge/internal/store"
	kit "bridge/pkg/adapterkit"
)

var setFlags []string

// stdin is shared so successive prompts don't lose buffered piped input.
var stdin = bufio.NewReader(os.Stdin)

func main() {
	root := &cobra.Command{Use: "bridge", Short: "Sync sessions, memory, rules and skills across coding agents", SilenceUsage: true}
	root.PersistentFlags().StringArrayVar(&setFlags, "set", nil, "override a config value, e.g. --set live.conflict_policy=block")
	root.AddCommand(initCmd(), daemonCmd(), planCmd(), syncCmd(), handoffCmd(), sessionsCmd(), statusCmd(), whyCmd(), memoryCmd(), taskCmd(),
		doctorCmd(), uninstallCmd(), hookCmd(), mcpCmd(), adapterCmd(), debugCmd(), configCmd(), apiCmd())
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func cfgFor(dir string) (*config.Config, error) { return config.LoadForDir(dir, setFlags) }

func cwdCfg() (*config.Config, string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, "", err
	}
	c, err := cfgFor(wd)
	return c, wd, err
}

func registry(cfg *config.Config) *kit.Registry {
	reg := kit.NewRegistry(claudecode.New(), codex.New(), pi.New(), dsh.New())
	for _, c := range external.Discover(cfg) {
		if c.Trusted {
			reg.Add(external.New(c.ID, c.Path))
		}
	}
	return reg
}

func openStore() (*store.Store, error) { return store.Open(fsutil.DBPath()) }

func signalCtx() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}

func self() string {
	if p, err := os.Executable(); err == nil {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return p
	}
	return "bridge"
}

func confirm(q string) bool {
	fmt.Printf("%s [y/N] ", q)
	line, _ := stdin.ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

func defaultConfig(ids []string) string {
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = strconv.Quote(id)
	}
	return "# bridge configuration. Schema: `bridge config schema`\nversion = 1\n\n[agents]\nenabled = [" + strings.Join(quoted, ", ") + "]\n"
}

const launchLabel = "com.bridge.daemon"

func plistPath() string {
	return filepath.Join(fsutil.Home(), "Library", "LaunchAgents", launchLabel+".plist")
}

func installLaunchd() error {
	logPath := filepath.Join(fsutil.DataDir(), "daemon.log")
	if err := fsutil.EnsurePrivateDir(fsutil.DataDir()); err != nil {
		return err
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>%s</string><string>daemon</string></array>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><true/>
<key>StandardOutPath</key><string>%s</string>
<key>StandardErrorPath</key><string>%s</string>
</dict></plist>
`, launchLabel, self(), logPath, logPath)
	if err := fsutil.SafeWrite(plistPath(), []byte(plist), 0o644); err != nil {
		return err
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	_ = exec.Command("launchctl", "bootout", domain+"/"+launchLabel).Run()
	return exec.Command("launchctl", "bootstrap", domain, plistPath()).Run()
}

// ensureDaemon restarts the service of a user who has connected, for example
// after an app update. Without the plist the user never opted in, so nothing
// is started.
func ensureDaemon() bool {
	if daemonOnline() {
		return true
	}
	if _, err := os.Stat(plistPath()); err != nil {
		return false
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	_ = exec.Command("launchctl", "bootstrap", domain, plistPath()).Run()
	_ = exec.Command("launchctl", "kickstart", domain+"/"+launchLabel).Run()
	for i := 0; i < 20 && !daemonOnline(); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	return daemonOnline()
}

func removeLaunchd() error {
	// launchd is per user, not per HOME: without our plist here, a service of
	// that name belongs to some other installation and must be left alone.
	if _, err := os.Stat(plistPath()); os.IsNotExist(err) {
		return nil
	}
	_ = exec.Command("launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid())+"/"+launchLabel).Run()
	err := os.Remove(plistPath())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func initCmd() *cobra.Command {
	var yes, noLaunchd bool
	c := &cobra.Command{Use: "init", Short: "Detect agents, install hooks and MCP, write the config", RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		probe, err := config.Load(config.Options{GlobalPath: filepath.Join(fsutil.ConfigDir(), "config.toml"), Set: setFlags})
		if err != nil {
			return err
		}
		reg := registry(probe)
		var found []kit.Adapter
		for _, a := range reg.All() {
			in, err := a.Detect(ctx)
			if err != nil {
				fmt.Printf("  %s：探测失败：%v\n", a.ID(), err)
				continue
			}
			if !in.Installed {
				fmt.Printf("  %s：未安装\n", a.ID())
				continue
			}
			state := "只读（此版本未验证写入）"
			if a.Capabilities(in.Version).SessionWrite {
				state = "可读写"
			}
			fmt.Printf("  %s：已安装，版本 %s，%s\n", a.ID(), orDash(string(in.Version)), state)
			found = append(found, a)
		}
		if len(found) == 0 {
			return errors.New("没有探测到受支持的 agent")
		}
		if !yes && !confirm("将为以上 agent 安装 hooks 与 MCP（修改前自动备份）并注册后台服务，继续？") {
			return errors.New("已取消")
		}
		if _, err := os.Stat(config.GlobalPath()); os.IsNotExist(err) {
			ids := make([]string, len(found))
			for i, a := range found {
				ids[i] = a.ID()
			}
			if err := fsutil.AtomicWrite(config.GlobalPath(), []byte(defaultConfig(ids)), 0o644); err != nil {
				return err
			}
			fmt.Println("已写入配置：", config.GlobalPath())
		}
		bin := self()
		for _, a := range found {
			if err := a.InstallHooks(ctx, kit.HookConfig{Command: bin, MCPCommand: []string{bin, "mcp"}}); errors.Is(err, kit.ErrNotSupported) {
				fmt.Printf("%s 没有可安装的 hooks，只同步会话和文件\n", a.ID())
				continue
			} else if err != nil {
				return fmt.Errorf("%s: %w", a.ID(), err)
			}
			fmt.Printf("已接入 %s\n", a.ID())
		}
		if !noLaunchd && runtime.GOOS == "darwin" {
			if err := installLaunchd(); err != nil {
				return fmt.Errorf("注册后台服务失败：%w", err)
			}
			fmt.Println("后台服务已启动")
		}
		return nil
	}}
	c.Flags().BoolVar(&yes, "yes", false, "skip the confirmation")
	c.Flags().BoolVar(&noLaunchd, "no-launchd", false, "do not register the background service")
	return c
}

func daemonCmd() *cobra.Command {
	return &cobra.Command{Use: "daemon", Short: "Run the background process", RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, stop := signalCtx()
		defer stop()
		st, err := openStore()
		if err != nil {
			return err
		}
		defer st.Close()
		cfg, err := cfgFor("")
		if err != nil {
			return err
		}
		d := &daemon.Daemon{Store: st, Registry: registry(cfg), Config: cfgFor, Logger: log.New(os.Stderr, "bridge: ", log.LstdFlags)}
		return d.Run(ctx)
	}}
}

func installedAgents(ctx context.Context, reg *kit.Registry) ([]filesync.Agent, error) {
	return daemon.DetectAgents(ctx, reg)
}

func planFor(ctx context.Context, cfg *config.Config, wd string, reg *kit.Registry) ([]filesync.Action, error) {
	agents, err := installedAgents(ctx, reg)
	if err != nil {
		return nil, err
	}
	root := config.ProjectRoot(wd)
	if m, err := cfg.Matcher(); err != nil {
		return nil, err
	} else if m.Excluded(root) {
		root = ""
	}
	return filesync.Plan(filesync.Input{ProjectRoot: root, Agents: agents, Config: cfg})
}

// syncFiles applies a plan. Adopting changes the sources, which can give
// other agents something new to receive, so a second pass delivers it.
func syncFiles(plan func() ([]filesync.Action, error), o filesync.Options) (filesync.Report, error) {
	acts, err := plan()
	if err != nil {
		return filesync.Report{}, err
	}
	r, err := filesync.Apply(acts, o)
	if err != nil || o.Adopt == "" {
		return r, err
	}
	if acts, err = plan(); err != nil {
		return r, err
	}
	again, err := filesync.Apply(acts, filesync.Options{})
	r.Applied = append(r.Applied, again.Applied...)
	// The first pass explains what adopting could not resolve; the second
	// adds anything still waiting that the first did not cover.
	seen := map[string]bool{}
	key := func(a filesync.Action) string {
		return a.Category + "\x00" + a.Agent + "\x00" + a.Target + "\x00" + a.Name
	}
	for _, a := range r.Blocked {
		seen[key(a)] = true
	}
	for _, a := range again.Blocked {
		if !seen[key(a)] {
			r.Blocked = append(r.Blocked, a)
		}
	}
	return r, err
}

func printActions(acts []filesync.Action) {
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "动作\t类别\tagent\t目标\t原因")
	for _, a := range acts {
		target := a.Target
		if a.Name != "" {
			target += " · " + a.Name
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", a.Kind, a.Category, a.Agent, target, a.Reason)
	}
	w.Flush()
}

func planCmd() *cobra.Command {
	return &cobra.Command{Use: "plan", Short: "Preview what would be synced; changes nothing", RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, wd, err := cwdCfg()
		if err != nil {
			return err
		}
		for _, w := range cfg.Warnings {
			fmt.Fprintln(os.Stderr, "警告：", w)
		}
		acts, err := planFor(cmd.Context(), cfg, wd, registry(cfg))
		if err != nil {
			return err
		}
		if len(acts) == 0 {
			fmt.Println("没有需要同步的内容。")
			return nil
		}
		printActions(acts)
		return nil
	}}
}

func syncCmd() *cobra.Command {
	var adopt string
	var force bool
	c := &cobra.Command{Use: "sync", Short: "Run file sync now", RunE: func(cmd *cobra.Command, _ []string) error {
		if adopt != "" && force {
			return errors.New("--adopt and --force are mutually exclusive")
		}
		cfg, wd, err := cwdCfg()
		if err != nil {
			return err
		}
		r, err := syncFiles(func() ([]filesync.Action, error) { return planFor(cmd.Context(), cfg, wd, registry(cfg)) }, filesync.Options{Force: force, Adopt: adopt})
		if err != nil {
			return err
		}
		fmt.Printf("已同步 %d 项\n", len(r.Applied))
		if len(r.Applied) > 0 {
			printActions(r.Applied)
		}
		if len(r.Blocked) > 0 {
			fmt.Printf("\n有 %d 项未处理：\n", len(r.Blocked))
			printActions(r.Blocked)
		}
		return nil
	}}
	c.Flags().StringVar(&adopt, "adopt", "", "merge this agent's replica (rule file or MCP servers) back into the source")
	c.Flags().BoolVar(&force, "force", false, "overwrite drifted replicas from the source")
	return c
}

func ingester(st *store.Store, reg *kit.Registry) *daemon.Ingester {
	return &daemon.Ingester{Store: st, Registry: reg, Config: cfgFor}
}

func handoffCmd() *cobra.Command {
	var to string
	var doc bool
	c := &cobra.Command{Use: "handoff [session]", Short: "Migrate a session to another agent", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		cfg, wd, err := cwdCfg()
		if err != nil {
			return err
		}
		st, err := openStore()
		if err != nil {
			return err
		}
		defer st.Close()
		reg := registry(cfg)
		if _, err := ingester(st, reg).Sync(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "警告：部分会话采集失败：", err)
		}
		id := ""
		if len(args) == 1 {
			id = args[0]
		} else {
			if id, err = pickSession(st, wd); err != nil {
				return err
			}
		}
		if to == "" {
			fmt.Print("迁移到哪个 agent？")
			line, _ := stdin.ReadString('\n')
			to = strings.TrimSpace(line)
			if to == "" {
				return errors.New("没有指定目标 agent，请使用 --to")
			}
		}
		out, err := runHandoff(ctx, st, reg, id, to, doc)
		if err != nil {
			return err
		}
		r := out.Result
		fmt.Printf("迁移完成（%s）：约 %d tokens，省略推理 %d 段，降级工具 %d 个，压缩 %d 轮，截断结果 %d 个，截断消息 %d 条\n",
			out.Mode, r.EstimatedTokens, r.ReasoningOmit, r.Degraded, r.SummarizedTurns, r.TruncatedTools, r.TruncatedMessages)
		fmt.Println(out.Message)
		return nil
	}}
	c.Flags().StringVar(&to, "to", "", "target agent")
	c.Flags().BoolVar(&doc, "document", false, "write a handoff document instead of a native session")
	return c
}

// runHandoff migrates a session and marks the new copy as already extracted,
// so its history is not sent for memory extraction a second time.
func runHandoff(ctx context.Context, st *store.Store, reg *kit.Registry, id, to string, doc bool) (*handoff.Outcome, error) {
	src, err := st.FindSession(id)
	if err != nil {
		return nil, fmt.Errorf("session %q: %w", id, err)
	}
	sc, err := cfgFor(src.CWD)
	if err != nil {
		return nil, err
	}
	out, err := handoff.Run(ctx, handoff.Deps{Store: st, Registry: reg, Config: sc}, handoff.Request{SessionID: id, Target: to, Force: doc})
	if err != nil {
		return nil, err
	}
	if out.Mode == "native" {
		ingested := false
		if t, ok := reg.Get(to); ok {
			if _, err := ingester(st, reg).SyncAdapter(ctx, t); err != nil {
				fmt.Fprintln(os.Stderr, "警告：", err)
			} else {
				ingested = true
			}
		}
		if max, err := st.MaxSeq(out.TargetSession); err == nil {
			if err := errors.Join(st.FinishExtraction(out.TargetSession, max), st.SetBaseline(out.TargetSession, max)); err != nil {
				fmt.Fprintln(os.Stderr, "警告：", err)
			}
			if ingested && out.SourceSeq > 0 && max > 0 {
				if err := st.CreateConversationLink(src.ID, out.TargetSession, out.SourceSeq, max); err != nil {
					fmt.Fprintln(os.Stderr, "警告：未能记录会话同步边界：", err)
				}
			}
		}
	}
	return out, nil
}

func pickSession(st *store.Store, wd string) (string, error) {
	list, err := st.ListSessions(store.SessionFilter{ProjectID: project.Identify(wd).ID, Limit: 20})
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		if list, err = st.ListSessions(store.SessionFilter{Limit: 20}); err != nil {
			return "", err
		}
	}
	if len(list) == 0 {
		return "", errors.New("没有可迁移的会话")
	}
	for i, s := range list {
		fmt.Printf("%2d. %-12s %s  %s  %s\n", i+1, s.Agent, s.EndedAt.Local().Format("01-02 15:04"), s.CWD, s.Title)
	}
	fmt.Print("选择序号：")
	line, _ := stdin.ReadString('\n')
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > len(list) {
		return "", errors.New("序号无效")
	}
	return list[n-1].ID, nil
}

func sessionsCmd() *cobra.Command {
	var agent, proj string
	var since time.Duration
	var limit int
	var here bool
	c := &cobra.Command{Use: "sessions", Short: "List indexed sessions", RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, wd, err := cwdCfg()
		if err != nil {
			return err
		}
		st, err := openStore()
		if err != nil {
			return err
		}
		defer st.Close()
		if _, err := ingester(st, registry(cfg)).Sync(cmd.Context()); err != nil {
			fmt.Fprintln(os.Stderr, "警告：部分会话采集失败：", err)
		}
		f := store.SessionFilter{Agent: agent, Limit: limit}
		if here {
			f.ProjectID = project.Identify(wd).ID
		} else if proj != "" {
			f.ProjectID = project.Identify(fsutil.Expand(proj)).ID
		}
		if since > 0 {
			f.Since = time.Now().Add(-since)
		}
		list, err := st.ListSessions(f)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "ID\t最近活动\t目录\t标题")
		for _, s := range list {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.ID, s.EndedAt.Local().Format("2006-01-02 15:04"), s.CWD, s.Title)
		}
		return w.Flush()
	}}
	c.Flags().StringVar(&agent, "agent", "", "filter by agent")
	c.Flags().StringVar(&proj, "project", "", "filter by project path")
	c.Flags().BoolVar(&here, "here", false, "only the current project")
	c.Flags().DurationVar(&since, "since", 0, "only sessions active within this duration")
	c.Flags().IntVar(&limit, "limit", 30, "maximum results")
	return c
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func daemonOnline() bool {
	r, err := daemon.Ask(daemon.Request{Method: "ping"}, 100*time.Millisecond, 300*time.Millisecond)
	return err == nil && r.Output == "pong"
}

func statusCmd() *cobra.Command {
	return &cobra.Command{Use: "status", Short: "Show agents, effective channels and last sync", RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		cfg, _, err := cwdCfg()
		if err != nil {
			return err
		}
		st, err := openStore()
		if err != nil {
			return err
		}
		defer st.Close()
		fmt.Printf("守护进程：%v\n\n", map[bool]string{true: "运行中", false: "未运行"}[daemonOnline()])
		for _, a := range registry(cfg).All() {
			in, err := a.Detect(ctx)
			if err != nil {
				fmt.Printf("%s：探测失败：%v\n\n", a.ID(), err)
				continue
			}
			if !in.Installed {
				fmt.Printf("%s：未安装\n\n", a.ID())
				continue
			}
			caps := a.Capabilities(in.Version)
			eff := kit.Degrade(caps)
			s, err := st.AgentStats(a.ID())
			if err != nil {
				return err
			}
			fmt.Printf("%s（版本 %s，启用 %v，方向 %s）\n", a.ID(), orDash(string(in.Version)), cfg.AgentEnabled(a.ID()), cfg.Direction(a.ID()))
			if !caps.SessionWrite {
				fmt.Println("  告警：此版本未验证写入，已切换为只读")
			}
			if tc, ok := a.(kit.HookTrustChecker); ok {
				if hc, ok := a.(kit.HookChecker); ok && hc.HooksInstalled() && !tc.HooksTrusted() {
					fmt.Println("  告警：无法确认 hooks 信任状态；请在该 agent 中审核")
				}
			}
			fmt.Printf("  会话迁移：%s\n  实时协作：%s\n  记忆注入：%s\n", eff.Handoff, eff.Live, eff.Memory)
			last := "-"
			if !s.LastSync.IsZero() {
				last = s.LastSync.Local().Format("2006-01-02 15:04:05")
			}
			fmt.Printf("  已索引：%d 个会话，%d 条事件；最近活动 %s\n\n", s.Sessions, s.Events, last)
		}
		return nil
	}}
}

func keysFor(a filesync.Action) []string {
	k := []string{"agents.enabled", "agents." + a.Agent + ".direction"}
	if a.Category == "rule" {
		return append(k, "sync.rules", "sync.rules_source", "sync.exclude_files")
	}
	return append(k, "sync.skills.enabled", "sync.skills.exclude")
}

func whyCmd() *cobra.Command {
	return &cobra.Command{Use: "why <path|memory-id>", Short: "Explain why something was synced or skipped", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cfg, wd, err := cwdCfg()
		if err != nil {
			return err
		}
		st, err := openStore()
		if err != nil {
			return err
		}
		defer st.Close()
		if m, err := st.GetMemory(args[0]); err == nil {
			fmt.Printf("记忆 %s\n状态：%s\n范围：%s / %s\n置信度：%.2f（阈值 %.2f）\n内容：%s\n来源事件：%s\n", m.ID, m.Status, m.Scope, m.Category, m.Confidence, cfg.Memory.MinConfidence, m.Content, strings.Join(m.SourceEvents, ", "))
			if m.Supersedes != "" {
				fmt.Println("取代：", m.Supersedes)
			}
			return nil
		}
		target, err := filepath.Abs(fsutil.Expand(args[0]))
		if err != nil {
			return err
		}
		acts, err := planFor(cmd.Context(), cfg, wd, registry(cfg))
		if err != nil {
			return err
		}
		found := false
		for _, a := range acts {
			if filepath.Clean(a.Target) != target && filepath.Clean(a.Source) != target {
				continue
			}
			found = true
			fmt.Printf("%s（%s，agent %s）\n  来源：%s\n  结果：%s\n  原因：%s\n  相关配置：\n", a.Target, a.Category, a.Agent, a.Source, a.Kind, a.Reason)
			for _, k := range keysFor(a) {
				if o, ok := cfg.Why(k); ok {
					fmt.Printf("    %s ← %s\n", k, o)
				}
			}
		}
		if m, err := cfg.Matcher(); err == nil && m.Excluded(target) {
			found = true
			o, _ := cfg.Why("privacy.exclude_projects")
			fmt.Printf("%s 被隐私规则排除，不采集。\n  privacy.exclude_projects ← %s\n", target, o)
		}
		if !found {
			fmt.Printf("%s 不在同步范围内（不是任何规则或 skill 的源或副本）。\n", target)
		}
		return nil
	}}
}

func memoryCmd() *cobra.Command {
	c := &cobra.Command{Use: "memory", Short: "Manage shared memories"}
	withStore := func(f func(context.Context, *store.Store, *config.Config, string, []string) error) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			cfg, wd, err := cwdCfg()
			if err != nil {
				return err
			}
			st, err := openStore()
			if err != nil {
				return err
			}
			defer st.Close()
			return f(cmd.Context(), st, cfg, wd, args)
		}
	}
	refresh := func(ctx context.Context, st *store.Store, cfg *config.Config) error {
		_, err := memory.RenderBlocks(ctx, st, registry(cfg), cfg)
		return err
	}
	show := func(ms []model.Memory) {
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "ID\t状态\t范围\t类别\t置信度\t内容")
		for _, m := range ms {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%.2f\t%s\n", m.ID, m.Status, m.Scope, m.Category, m.Confidence, m.Content)
		}
		w.Flush()
	}
	var all bool
	list := &cobra.Command{Use: "list", Short: "List memories", RunE: withStore(func(_ context.Context, st *store.Store, _ *config.Config, _ string, _ []string) error {
		f := store.MemoryFilter{Status: model.MemoryActive}
		if all {
			f.Status = ""
		}
		ms, err := st.ListMemories(f)
		if err != nil {
			return err
		}
		show(ms)
		return nil
	})}
	list.Flags().BoolVar(&all, "all", false, "include pending, superseded and forgotten")
	var approve, reject []string
	review := &cobra.Command{Use: "review", Short: "Review memories below the confidence threshold", RunE: withStore(func(ctx context.Context, st *store.Store, cfg *config.Config, _ string, _ []string) error {
		for _, id := range approve {
			if err := st.SetMemoryStatus(id, model.MemoryActive); err != nil {
				return err
			}
		}
		for _, id := range reject {
			if err := st.Forget(id); err != nil {
				return err
			}
		}
		if len(approve)+len(reject) > 0 {
			return refresh(ctx, st, cfg)
		}
		ms, err := st.ListMemories(store.MemoryFilter{Status: model.MemoryPending})
		if err != nil {
			return err
		}
		if len(ms) == 0 {
			fmt.Println("待审区为空。")
			return nil
		}
		changed := false
		for _, m := range ms {
			fmt.Printf("\n[%s] (%s/%s，置信度 %.2f)\n%s\n通过(a) / 拒绝(r) / 跳过(其他)：", m.ID, m.Scope, m.Category, m.Confidence, m.Content)
			line, err := stdin.ReadString('\n')
			if err != nil && line == "" {
				break
			}
			switch strings.TrimSpace(line) {
			case "a":
				if err := st.SetMemoryStatus(m.ID, model.MemoryActive); err != nil {
					return err
				}
				changed = true
			case "r":
				if err := st.Forget(m.ID); err != nil {
					return err
				}
			}
		}
		if changed {
			return refresh(ctx, st, cfg)
		}
		return nil
	})}
	review.Flags().StringArrayVar(&approve, "approve", nil, "approve a memory without prompting")
	review.Flags().StringArrayVar(&reject, "reject", nil, "reject a memory without prompting")
	var scope, category string
	add := &cobra.Command{Use: "add <content>", Short: "Add a memory by hand", Args: cobra.MinimumNArgs(1), RunE: withStore(func(ctx context.Context, st *store.Store, cfg *config.Config, wd string, args []string) error {
		p := project.Identify(wd)
		if scope == model.ScopeProject {
			if err := st.UpsertProject(p); err != nil {
				return err
			}
		}
		m, err := memory.AddManual(st, scope, p.ID, category, strings.Join(args, " "))
		if err != nil {
			return err
		}
		fmt.Println("已添加：", m.ID)
		return refresh(ctx, st, cfg)
	})}
	add.Flags().StringVar(&scope, "scope", model.ScopeProject, "global or project")
	add.Flags().StringVar(&category, "category", "manual", "category")
	edit := &cobra.Command{Use: "edit <id> <content>", Short: "Edit a memory", Args: cobra.MinimumNArgs(2), RunE: withStore(func(ctx context.Context, st *store.Store, cfg *config.Config, _ string, args []string) error {
		if err := st.UpdateMemoryContent(args[0], strings.Join(args[1:], " ")); err != nil {
			return err
		}
		return refresh(ctx, st, cfg)
	})}
	forget := &cobra.Command{Use: "forget <id>", Short: "Delete a memory and block re-extraction", Args: cobra.ExactArgs(1), RunE: withStore(func(ctx context.Context, st *store.Store, cfg *config.Config, _ string, args []string) error {
		if err := st.Forget(args[0]); err != nil {
			return err
		}
		return refresh(ctx, st, cfg)
	})}
	extract := &cobra.Command{Use: "extract <session>", Short: "Run memory extraction on one session now", Args: cobra.ExactArgs(1), RunE: withStore(func(ctx context.Context, st *store.Store, cfg *config.Config, _ string, args []string) error {
		reg := registry(cfg)
		if _, err := ingester(st, reg).Sync(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "警告：部分会话采集失败：", err)
		}
		s, err := st.FindSession(args[0])
		if err != nil {
			return err
		}
		e := memory.Engine{Store: st, Extractor: memory.CLIExtractor{Agent: cfg.Memory.Extractor}, Config: cfg}
		n, err := e.ExtractSession(ctx, s.ID)
		if err != nil {
			return err
		}
		fmt.Printf("新增 %d 条记忆\n", n)
		return refresh(ctx, st, cfg)
	})}
	c.AddCommand(list, review, add, edit, forget, extract, memoryImportCmd())
	return c
}

func doctorCmd() *cobra.Command {
	return &cobra.Command{Use: "doctor", Short: "Check versions, hooks and permissions", RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		cfg, _, err := cwdCfg()
		if err != nil {
			return fmt.Errorf("配置无效：%w", err)
		}
		fails := 0
		report := func(ok bool, msg string) {
			mark := "ok  "
			if !ok {
				mark = "FAIL"
				fails++
			}
			fmt.Printf("[%s] %s\n", mark, msg)
		}
		for _, w := range cfg.Warnings {
			report(false, w)
		}
		st, err := openStore()
		report(err == nil, "数据库可打开："+fsutil.DBPath())
		if err == nil {
			st.Close()
			fi, e := os.Stat(fsutil.DBPath())
			report(e == nil && fi.Mode().Perm() == 0o600, "数据库权限为 0600")
			di, e := os.Stat(fsutil.DataDir())
			report(e == nil && di.Mode().Perm() == 0o700, "数据目录权限为 0700")
		}
		report(daemonOnline(), "守护进程在线")
		for _, a := range registry(cfg).All() {
			in, err := a.Detect(ctx)
			if err != nil {
				report(false, a.ID()+" 探测失败："+err.Error())
				continue
			}
			if !in.Installed {
				fmt.Printf("[skip] %s 未安装\n", a.ID())
				continue
			}
			report(a.Capabilities(in.Version).SessionWrite, fmt.Sprintf("%s 版本 %s 已验证写入", a.ID(), orDash(string(in.Version))))
			if hc, ok := a.(kit.HookChecker); ok {
				report(hc.HooksInstalled(), a.ID()+" hooks 已安装")
			}
			if tc, ok := a.(kit.HookTrustChecker); ok {
				report(tc.HooksTrusted(), a.ID()+" hooks 已被信任（否则不会运行，请在该 agent 中审核）")
			}
		}
		for _, c := range external.Discover(cfg) {
			report(c.Trusted, "外部适配器 "+c.ID+" 已信任："+c.Path)
		}
		if fails > 0 {
			return fmt.Errorf("%d 项检查未通过", fails)
		}
		return nil
	}}
}

func uninstallCmd() *cobra.Command {
	var yes, purge bool
	c := &cobra.Command{Use: "uninstall", Short: "Remove hooks and links and restore changes", RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		if !yes && !confirm("将卸载 hooks、软链接和托管区块，继续？") {
			return errors.New("已取消")
		}
		cfg, err := cfgFor("")
		if err != nil {
			return err
		}
		if err := removeLaunchd(); err != nil {
			return err
		}
		var ruleFiles []string
		var errs []error
		for _, a := range registry(cfg).All() {
			in, err := a.Detect(ctx)
			if err != nil || !in.Installed {
				continue
			}
			if err := a.UninstallHooks(ctx); err != nil && !errors.Is(err, kit.ErrNotSupported) {
				errs = append(errs, fmt.Errorf("%s: %w", a.ID(), err))
			}
			for _, s := range a.Capabilities(in.Version).RulesFiles {
				if s.Scope == model.ScopeGlobal {
					ruleFiles = append(ruleFiles, fsutil.Expand(s.Path))
				}
			}
		}
		if err := filesync.StripManagedBlocks(ruleFiles); err != nil {
			errs = append(errs, err)
		}
		// MCP entries go first: their removal needs the agents' config readers.
		if agents, err := installedAgents(ctx, registry(cfg)); err != nil {
			errs = append(errs, err)
		} else if notes, err := filesync.UninstallMCP(agents); err != nil {
			errs = append(errs, err)
		} else {
			for _, n := range notes {
				fmt.Println(n)
			}
		}
		notes, err := filesync.Uninstall()
		if err != nil {
			errs = append(errs, err)
		}
		for _, n := range notes {
			fmt.Println(n)
		}
		if err := errors.Join(errs...); err != nil {
			return err
		}
		if purge {
			if err := os.RemoveAll(fsutil.DataDir()); err != nil {
				return err
			}
			fmt.Println("已删除数据目录。")
		}
		fmt.Println("卸载完成。")
		return nil
	}}
	c.Flags().BoolVar(&yes, "yes", false, "skip the confirmation")
	c.Flags().BoolVar(&purge, "purge", false, "also delete bridge's database and backups")
	return c
}

// hookCmd never fails or blocks the agent: every problem yields empty output
// and exit status 0.
func hookCmd() *cobra.Command {
	var agent string
	c := &cobra.Command{Use: "hook <event>", Short: "Agent hook entry point", Args: cobra.ExactArgs(1), Hidden: true, RunE: func(cmd *cobra.Command, args []string) error {
		if os.Getenv("BRIDGE_INTERNAL") != "" {
			return nil
		}
		var in struct {
			SessionID string          `json:"session_id"`
			CWD       string          `json:"cwd"`
			ToolName  string          `json:"tool_name"`
			ToolInput json.RawMessage `json:"tool_input"`
		}
		if b, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20)); err == nil {
			_ = json.Unmarshal(b, &in)
		}
		if in.SessionID == "" {
			return nil
		}
		event := args[0]
		r, err := daemon.Ask(daemon.Request{Method: "hook", Event: event, Agent: agent, Session: in.SessionID, CWD: in.CWD, ToolName: in.ToolName, ToolInput: in.ToolInput}, 50*time.Millisecond, 280*time.Millisecond)
		if err != nil || r.Error != "" {
			return nil
		}
		native := map[string]string{"session_start": "SessionStart", "prompt_submit": "UserPromptSubmit", "pre_tool": "PreToolUse"}[event]
		if native == "" {
			return nil
		}
		if r.Deny {
			return json.NewEncoder(os.Stdout).Encode(map[string]any{"hookSpecificOutput": map[string]string{
				"hookEventName": native, "permissionDecision": "deny", "permissionDecisionReason": r.Output}})
		}
		if r.Output != "" {
			if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"hookSpecificOutput": map[string]string{
				"hookEventName": native, "additionalContext": r.Output}}); err != nil {
				return nil // undelivered: no ack, so it is offered again next time
			}
		}
		if event != "pre_tool" {
			_, _ = daemon.Ask(daemon.Request{Method: "ack", Agent: agent, Session: in.SessionID}, 50*time.Millisecond, 100*time.Millisecond)
		}
		return nil
	}}
	c.Flags().StringVar(&agent, "agent", "", "agent id")
	return c
}

func mcpCmd() *cobra.Command {
	return &cobra.Command{Use: "mcp", Short: "Run the stdio MCP server", Hidden: true, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, wd, err := cwdCfg()
		if err != nil {
			return err
		}
		st, err := openStore()
		if err != nil {
			return err
		}
		defer st.Close()
		ctx, stop := signalCtx()
		defer stop()
		return mcpserver.Serve(ctx, st, cfg, wd)
	}}
}

func adapterCmd() *cobra.Command {
	c := &cobra.Command{Use: "adapter", Short: "Manage adapters"}
	c.AddCommand(&cobra.Command{Use: "list", Short: "List adapters", RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, _, err := cwdCfg()
		if err != nil {
			return err
		}
		for _, a := range registry(cfg).All() {
			fmt.Println(a.ID())
		}
		for _, x := range external.Discover(cfg) {
			if !x.Trusted {
				fmt.Printf("%s（待信任，%s）\n", x.ID, x.Path)
			}
		}
		return nil
	}})
	c.AddCommand(&cobra.Command{Use: "trust <id>", Short: "Approve an external adapter", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cfg, _, err := cwdCfg()
		if err != nil {
			return err
		}
		for _, x := range external.Discover(cfg) {
			if x.ID == args[0] {
				if err := external.Trust(x.Path); err != nil {
					return err
				}
				fmt.Println("已信任：", x.Path)
				return nil
			}
		}
		return fmt.Errorf("adapter %q not found", args[0])
	}})
	var write bool
	test := &cobra.Command{Use: "test <id>", Short: "Run the conformance suite", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cfg, wd, err := cwdCfg()
		if err != nil {
			return err
		}
		a, ok := registry(cfg).Get(args[0])
		if !ok {
			return fmt.Errorf("adapter %q not found or not trusted", args[0])
		}
		failed := 0
		for _, ch := range kit.Conformance(cmd.Context(), a, kit.ConformanceOptions{Write: write, CWD: wd}) {
			if ch.Passed {
				fmt.Println("PASS", ch.Name)
			} else {
				failed++
				fmt.Println("FAIL", ch.Name, "-", ch.Detail)
			}
		}
		if x, ok := a.(*external.Client); ok {
			x.Close()
		}
		if failed > 0 {
			return fmt.Errorf("%d checks failed", failed)
		}
		return nil
	}}
	test.Flags().BoolVar(&write, "write", false, "also write a test session into the agent's real data directory, then remove it")
	c.AddCommand(test)
	return c
}

func debugCmd() *cobra.Command {
	c := &cobra.Command{Use: "debug", Short: "Debugging helpers"}
	var out string
	capture := &cobra.Command{Use: "capture <session>", Short: "Export a redacted session as a test sample", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cfg, _, err := cwdCfg()
		if err != nil {
			return err
		}
		st, err := openStore()
		if err != nil {
			return err
		}
		defer st.Close()
		if _, err := ingester(st, registry(cfg)).Sync(cmd.Context()); err != nil {
			fmt.Fprintln(os.Stderr, "警告：部分会话采集失败：", err)
		}
		s, err := st.FindSession(args[0])
		if err != nil {
			return err
		}
		es, err := st.Events(s.ID, 0, 0)
		if err != nil {
			return err
		}
		var b strings.Builder
		for _, e := range es {
			if len(e.NativeRef) > 0 {
				b.Write(e.NativeRef)
				b.WriteByte('\n')
			}
		}
		if out == "" {
			fmt.Print(dedupeLines(b.String()))
			return nil
		}
		return fsutil.AtomicWrite(out, []byte(dedupeLines(b.String())), 0o600)
	}}
	capture.Flags().StringVarP(&out, "output", "o", "", "output file")
	c.AddCommand(capture)
	return c
}

// dedupeLines collapses adjacent identical lines: one native record may
// produce several events that share it.
func dedupeLines(s string) string {
	var b strings.Builder
	prev := ""
	for _, l := range strings.SplitAfter(s, "\n") {
		if l != prev {
			b.WriteString(l)
		}
		prev = l
	}
	return b.String()
}

func configCmd() *cobra.Command {
	c := &cobra.Command{Use: "config", Short: "Configuration helpers"}
	c.AddCommand(&cobra.Command{Use: "schema", Short: "Print the JSON Schema", Run: func(*cobra.Command, []string) { os.Stdout.Write(config.Schema) }})
	return c
}
