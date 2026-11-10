package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"bridge/internal/config"
	"bridge/internal/filesync"
	"bridge/internal/model"
	"bridge/internal/project"
	"bridge/internal/store"
	"bridge/internal/worktree"
)

// Activity is the latest session in a worktree.
type Activity struct {
	Agent       string    `json:"agent"`
	Session     string    `json:"session"`
	Title       string    `json:"title"`
	Last        time.Time `json:"last"`
	Instruction string    `json:"instruction"`
}

// worktreeRow is a worktree with what the agents last did in it.
type worktreeRow struct {
	worktree.Worktree
	Activity *Activity `json:"activity,omitempty"`
}

func mainCheckout(dir string) (string, error) {
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		dir = wd
	}
	root := config.ProjectRoot(dir)
	if _, err := os.Stat(root + "/.git"); err != nil {
		return "", fmt.Errorf("%s 不是 git 仓库", root)
	}
	return project.MainCheckout(root), nil
}

func listWorktrees(ctx context.Context, dir string) (string, []worktreeRow, error) {
	main, err := mainCheckout(dir)
	if err != nil {
		return "", nil, err
	}
	list, err := worktree.List(ctx, main)
	if err != nil {
		return main, nil, err
	}
	rows := make([]worktreeRow, len(list))
	for i, w := range list {
		rows[i].Worktree = w
	}
	st, err := openStore()
	if err != nil {
		return main, rows, err
	}
	defer st.Close()
	sessions, err := st.ListSessions(store.SessionFilter{ProjectID: project.Identify(main).ID, Limit: 200})
	if err != nil {
		return main, rows, err
	}
	for i := range rows {
		for _, s := range sessions { // most recent first
			if s.CWD == "" || !sameDir(config.ProjectRoot(s.CWD), rows[i].Path) {
				continue
			}
			rows[i].Activity = &Activity{Agent: s.Agent, Session: s.ID, Title: s.Title, Last: s.EndedAt, Instruction: lastInstruction(st, s.ID)}
			break
		}
	}
	return main, rows, nil
}

func lastInstruction(st *store.Store, id string) string {
	max, err := st.MaxSeq(id)
	if err != nil {
		return ""
	}
	es, err := st.Events(id, max-60, 0)
	if err != nil {
		return ""
	}
	for i := len(es) - 1; i >= 0; i-- {
		if es[i].Kind == model.KindMessage && es[i].Role == model.RoleUser {
			r := []rune(es[i].Text())
			if len(r) > 120 {
				r = append(r[:120], '…')
			}
			return string(r)
		}
	}
	return ""
}

func sameDir(a, b string) bool {
	if a == b {
		return true
	}
	ra, e1 := filepath.EvalSymlinks(a)
	rb, e2 := filepath.EvalSymlinks(b)
	return e1 == nil && e2 == nil && ra == rb
}

// addWorktree creates the worktree and gives it the project's rule files.
func addWorktree(ctx context.Context, dir, agent, base, path string) (worktree.Worktree, error) {
	main, err := mainCheckout(dir)
	if err != nil {
		return worktree.Worktree{}, err
	}
	cfg, err := cfgFor(main)
	if err != nil {
		return worktree.Worktree{}, err
	}
	known := false
	for _, a := range registry(cfg).All() {
		known = known || a.ID() == agent
	}
	if !known {
		return worktree.Worktree{}, fmt.Errorf("未知的 agent：%s", agent)
	}
	w, err := worktree.Add(ctx, main, agent, base, path, "")
	if err != nil {
		return w, err
	}
	// Generated rule copies are git-ignored, so a fresh checkout lacks them.
	if wcfg, err := cfgFor(w.Path); err == nil {
		if acts, err := planFor(ctx, wcfg, w.Path, registry(wcfg)); err == nil {
			r, _ := filesync.Apply(acts, filesync.Options{})
			var rels []string
			for _, a := range r.Applied {
				if a.Scope != "project" || a.Category == "mcp" {
					continue
				}
				if rel, err := filepath.Rel(w.Path, a.Target); err == nil && !strings.HasPrefix(rel, "..") {
					rels = append(rels, rel)
				}
			}
			if err := worktree.Exclude(ctx, w.Path, rels); err != nil {
				return w, err
			}
		}
	}
	return w, nil
}

func startHint(agent, path string) string {
	bin := map[string]string{"claude-code": "claude", "codex": "codex"}[agent]
	if bin == "" {
		return "cd " + path
	}
	return "cd " + path + " && " + bin
}

func worktreeCmd() *cobra.Command {
	c := &cobra.Command{Use: "worktree", Short: "Give each agent its own git worktree"}
	var base, path string
	var force bool
	add := &cobra.Command{Use: "add <agent>", Short: "Create a worktree and branch for an agent", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			w, err := addWorktree(cmd.Context(), "", args[0], base, path)
			if err != nil {
				return err
			}
			fmt.Printf("已创建 %s（分支 %s）。在这里启动 %s：\n  %s\n", w.Path, w.Branch, args[0], startHint(args[0], w.Path))
			return nil
		}}
	add.Flags().StringVar(&base, "base", "", "start the branch from this commit (default: HEAD)")
	add.Flags().StringVar(&path, "path", "", "directory (default: next to the repository)")
	list := &cobra.Command{Use: "list", Aliases: []string{"status"}, Short: "Show every worktree and the agents' progress", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			main, rows, err := listWorktrees(cmd.Context(), "")
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "分支\t目录\t领先/落后\t未提交\t最近活动")
			for _, r := range rows {
				act := "-"
				if r.Activity != nil {
					act = r.Activity.Agent + " " + r.Activity.Last.Local().Format("01-02 15:04")
					if r.Activity.Instruction != "" {
						act += "：" + r.Activity.Instruction
					}
				}
				state := fmt.Sprintf("+%d/-%d", r.Ahead, r.Behind)
				if r.Main {
					state = "主目录"
				}
				if r.Missing {
					state = "目录已不存在"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n", r.Branch, r.Path, state, r.Changed, act)
			}
			w.Flush()
			for _, r := range rows {
				if !r.Main && r.Ahead > 0 && rows[0].Branch != "" {
					fmt.Printf("\n%s 有 %d 个提交可以合并：git -C %s merge %s\n", r.Branch, r.Ahead, main, r.Branch)
				}
			}
			return nil
		}}
	remove := &cobra.Command{Use: "remove <agent|branch|path>", Short: "Delete a worktree; its branch and commits stay", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			main, err := mainCheckout("")
			if err != nil {
				return err
			}
			w, err := worktree.Remove(cmd.Context(), main, args[0], force)
			if err != nil {
				return err
			}
			fmt.Printf("已删除 %s；分支 %s 保留。\n", w.Path, w.Branch)
			return nil
		}}
	remove.Flags().BoolVar(&force, "force", false, "also discard uncommitted changes")
	c.AddCommand(add, list, remove)
	return c
}

var errNoDir = errors.New("请先选择项目目录")
