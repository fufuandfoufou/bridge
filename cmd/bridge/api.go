package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"bridge/adapters/external"
	"bridge/internal/config"
	"bridge/internal/daemon"
	"bridge/internal/filesync"
	"bridge/internal/fsutil"
	"bridge/internal/handoff"
	"bridge/internal/live"
	"bridge/internal/memory"
	"bridge/internal/model"
	"bridge/internal/project"
	"bridge/internal/store"
	"bridge/internal/versions"
	kit "bridge/pkg/adapterkit"
)

// apiCmd is the machine-readable interface used by the desktop app. Every
// subcommand prints one JSON object: {"ok":true,"data":...} or
// {"ok":false,"error":"..."}.
func apiCmd() *cobra.Command {
	c := &cobra.Command{Use: "api", Short: "JSON interface for the desktop app", Hidden: true}
	var dir, adopt, to, agent, scope, category, key, value, prefer string
	var force, doc, again bool
	var limit int
	add := func(use string, n cobra.PositionalArgs, fn func(context.Context, []string) (any, error)) *cobra.Command {
		sub := &cobra.Command{Use: use, Args: n, RunE: func(cmd *cobra.Command, args []string) error {
			data, err := fn(cmd.Context(), args)
			out := map[string]any{"ok": err == nil, "data": data}
			if err != nil {
				out = map[string]any{"ok": false, "error": err.Error()}
			}
			e := json.NewEncoder(os.Stdout)
			e.SetEscapeHTML(false)
			return e.Encode(localizeAPI(out))
		}}
		c.AddCommand(sub)
		return sub
	}
	withStore := func(fn func(context.Context, *store.Store, *config.Config, []string) (any, error)) func(context.Context, []string) (any, error) {
		return func(ctx context.Context, args []string) (any, error) {
			cfg, err := cfgFor(dir)
			if err != nil {
				return nil, err
			}
			st, err := openStore()
			if err != nil {
				return nil, err
			}
			defer st.Close()
			return fn(ctx, st, cfg, args)
		}
	}
	dirFlag := func(s *cobra.Command) *cobra.Command {
		s.Flags().StringVar(&dir, "dir", "", "project directory")
		return s
	}
	refresh := func(ctx context.Context, st *store.Store, cfg *config.Config) error {
		_, err := memory.RenderBlocks(ctx, st, registry(cfg), cfg)
		return err
	}

	add("status", cobra.NoArgs, withStore(func(ctx context.Context, st *store.Store, cfg *config.Config, _ []string) (any, error) {
		agents := []map[string]any{}
		for _, a := range registry(cfg).All() {
			in, err := a.Detect(ctx)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", a.ID(), err)
			}
			caps := a.Capabilities(in.Version)
			eff := kit.Degrade(caps)
			s, err := st.AgentStats(a.ID())
			if err != nil {
				return nil, err
			}
			hooks := false
			if hc, ok := a.(kit.HookChecker); ok {
				hooks = hc.HooksInstalled()
			}
			trusted := true
			if tc, ok := a.(kit.HookTrustChecker); ok && hooks {
				trusted = tc.HooksTrusted()
			}
			last := ""
			if !s.LastSync.IsZero() {
				last = s.LastSync.Local().Format("2006-01-02 15:04")
			}
			versionState, versionError := "unverified", ""
			if r, ok := versions.Get(a.ID(), string(in.Version)); ok {
				versionState, versionError = r.State, r.Error
			} else if caps.SessionWrite {
				versionState = "builtin"
			}
			agents = append(agents, map[string]any{"id": a.ID(), "version_state": versionState, "version_error": versionError, "installed": in.Installed, "version": string(in.Version),
				"enabled": cfg.AgentEnabled(a.ID()), "direction": cfg.Direction(a.ID()), "writable": caps.SessionWrite, "hooks": hooks, "hooks_trusted": trusted,
				"handoff": eff.Handoff, "live": eff.Live, "memory": eff.Memory, "sessions": s.Sessions, "events": s.Events, "last": last})
		}
		return map[string]any{"daemon": daemonOnline(), "paused": fsutil.Paused(), "agents": agents, "warnings": cfg.Warnings}, nil
	}))

	s := add("sessions", cobra.NoArgs, withStore(func(ctx context.Context, st *store.Store, cfg *config.Config, _ []string) (any, error) {
		warning := ""
		if !fsutil.Paused() {
			if _, err := ingester(st, registry(cfg)).Sync(ctx); err != nil {
				warning = err.Error()
			}
		}
		list, err := st.ListSessions(store.SessionFilter{Agent: agent, Limit: limit})
		if err != nil {
			return nil, err
		}
		rows := []map[string]any{}
		for _, x := range list {
			if err := handoff.EnsureLink(st, x); err != nil {
				warning = err.Error()
			}
		}
		for _, x := range list {
			syncState, syncOther := "", ""
			if l, ok, err := handoff.LinkFor(st, x.ID); err == nil && ok {
				if s, err := handoff.LinkState(st, l); err == nil {
					syncState = string(s)
				}
				other := l.TargetID
				if other == x.ID {
					other = l.SourceID
				}
				syncOther, _, _ = strings.Cut(other, ":")
			}
			if x.Title == "" {
				if x.Title, err = st.FirstUserText(x.ID); err != nil {
					return nil, err
				}
			}
			rows = append(rows, map[string]any{"id": x.ID, "agent": x.Agent, "title": x.Title, "cwd": x.CWD, "model": x.Model,
				"last": x.EndedAt.Local().Format("2006-01-02 15:04"), "parent": x.Parent, "sync_state": syncState, "sync_other": syncOther})
		}
		return map[string]any{"sessions": rows, "warning": warning}, nil
	}))
	s.Flags().StringVar(&agent, "agent", "", "filter by agent")
	s.Flags().IntVar(&limit, "limit", 500, "maximum results")

	h := add("handoff <session>", cobra.ExactArgs(1), withStore(func(ctx context.Context, st *store.Store, cfg *config.Config, a []string) (any, error) {
		if to == "" {
			return nil, errors.New("缺少目标 agent")
		}
		if !again && !doc {
			src, err := st.FindSession(a[0])
			if err != nil {
				return nil, fmt.Errorf("session %q: %w", a[0], err)
			}
			prior, found, err := st.RecentChild(src.ID, to, time.Now().Add(-24*time.Hour))
			if err != nil {
				return nil, err
			}
			if found {
				command := ""
				switch to {
				case "codex":
					command = "codex resume " + prior.NativeID
				case "claude-code":
					command = "claude --resume " + prior.NativeID
				}
				return map[string]any{"mode": "duplicate", "message": "24 小时内已经迁移过这个会话（" + prior.EndedAt.Local().Format("15:04") + "）。",
					"resume_command": command, "document_path": "", "tokens": 0, "reasoning": 0, "degraded": 0, "summarized": 0, "truncated": 0}, nil
			}
		}
		out, err := runHandoff(ctx, st, registry(cfg), a[0], to, doc)
		if err != nil {
			return nil, err
		}
		r := out.Result
		return map[string]any{"mode": out.Mode, "message": out.Message, "resume_command": out.Handle.ResumeCommand, "document_path": out.DocumentPath,
			"tokens": r.EstimatedTokens, "reasoning": r.ReasoningOmit, "degraded": r.Degraded, "summarized": r.SummarizedTurns,
			"truncated": r.TruncatedTools + r.TruncatedMessages}, nil
	}))
	h.Flags().StringVar(&to, "to", "", "target agent")
	h.Flags().BoolVar(&doc, "document", false, "write a handoff document")
	h.Flags().BoolVar(&again, "again", false, "migrate even if a recent copy exists")

	ss := add("sync-session <session>", cobra.ExactArgs(1), withStore(func(ctx context.Context, st *store.Store, cfg *config.Config, a []string) (any, error) {
		reg := registry(cfg)
		ing := ingester(st, reg)
		if _, err := ing.Sync(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "警告：", err)
		}
		return handoff.SyncConversation(ctx, handoff.SyncDeps{Deps: handoff.Deps{Store: st, Registry: reg, Config: cfg},
			Ingest: func(ctx context.Context, ad kit.Adapter) error { _, err := ing.SyncAdapter(ctx, ad); return err }}, a[0], prefer)
	}))
	ss.Flags().StringVar(&prefer, "prefer", "", "agent to take when both sides changed")

	planFn := func(ctx context.Context) ([]filesync.Action, error) {
		cfg, err := cfgFor(dir)
		if err != nil {
			return nil, err
		}
		return planFor(ctx, cfg, dir, registry(cfg))
	}
	dirFlag(add("plan", cobra.NoArgs, func(ctx context.Context, _ []string) (any, error) {
		acts, err := planFn(ctx)
		if acts == nil {
			acts = []filesync.Action{}
		}
		return map[string]any{"actions": acts}, err
	}))
	sy := dirFlag(add("sync", cobra.NoArgs, func(ctx context.Context, _ []string) (any, error) {
		if force && adopt != "" {
			return nil, errors.New("--adopt and --force are mutually exclusive")
		}
		acts, err := planFn(ctx)
		if err != nil {
			return nil, err
		}
		r, err := filesync.Apply(acts, filesync.Options{Force: force, Adopt: adopt})
		if err != nil {
			return nil, err
		}
		return map[string]any{"applied": orEmpty(r.Applied), "blocked": orEmpty(r.Blocked)}, nil
	}))
	sy.Flags().BoolVar(&force, "force", false, "overwrite drifted replicas")
	sy.Flags().StringVar(&adopt, "adopt", "", "merge a replica back")

	add("memories", cobra.NoArgs, withStore(func(_ context.Context, st *store.Store, _ *config.Config, _ []string) (any, error) {
		ms, err := st.ListMemories(store.MemoryFilter{})
		if ms == nil {
			ms = []model.Memory{}
		}
		return map[string]any{"memories": ms}, err
	}))
	ma := dirFlag(add("memory-add <content>", cobra.ExactArgs(1), withStore(func(ctx context.Context, st *store.Store, cfg *config.Config, a []string) (any, error) {
		p := project.Identify(dir)
		if scope == model.ScopeProject {
			if dir == "" {
				return nil, errors.New("项目记忆需要先选择项目目录")
			}
			if err := st.UpsertProject(p); err != nil {
				return nil, err
			}
		}
		m, err := memory.AddManual(st, scope, p.ID, category, a[0])
		if err != nil {
			return nil, err
		}
		return m, refresh(ctx, st, cfg)
	})))
	ma.Flags().StringVar(&scope, "scope", model.ScopeGlobal, "global or project")
	ma.Flags().StringVar(&category, "category", "manual", "category")
	add("memory-edit <id> <content>", cobra.ExactArgs(2), withStore(func(ctx context.Context, st *store.Store, cfg *config.Config, a []string) (any, error) {
		if strings.TrimSpace(a[1]) == "" {
			return nil, errors.New("记忆内容不能为空")
		}
		m, err := st.GetMemory(a[0])
		if err != nil {
			return nil, err
		}
		if m.Status != model.MemoryActive && m.Status != model.MemoryPending {
			return nil, errors.New("只能编辑生效中或待审核的记忆")
		}
		redactor, err := cfg.Redactor()
		if err != nil {
			return nil, err
		}
		if err := st.UpdateMemoryContent(a[0], redactor.String(a[1])); err != nil {
			return nil, err
		}
		return nil, refresh(ctx, st, cfg)
	}))
	add("memory-approve <id>", cobra.ExactArgs(1), withStore(func(ctx context.Context, st *store.Store, cfg *config.Config, a []string) (any, error) {
		if err := st.SetMemoryStatus(a[0], model.MemoryActive); err != nil {
			return nil, err
		}
		return nil, refresh(ctx, st, cfg)
	}))
	add("memory-forget <id>", cobra.ExactArgs(1), withStore(func(ctx context.Context, st *store.Store, cfg *config.Config, a []string) (any, error) {
		if err := st.Forget(a[0]); err != nil {
			return nil, err
		}
		return nil, refresh(ctx, st, cfg)
	}))

	add("doctor", cobra.NoArgs, func(ctx context.Context, _ []string) (any, error) {
		cfg, err := cfgFor(dir)
		if err != nil {
			return nil, err
		}
		checks := []map[string]any{}
		report := func(ok bool, msg string) { checks = append(checks, map[string]any{"ok": ok, "name": msg}) }
		st, err := openStore()
		report(err == nil, "数据库可打开")
		if err == nil {
			st.Close()
			fi, e := os.Stat(fsutil.DBPath())
			report(e == nil && fi.Mode().Perm() == 0o600, "数据库权限为 0600")
		}
		report(daemonOnline(), "守护进程在线")
		for _, a := range registry(cfg).All() {
			in, err := a.Detect(ctx)
			if err != nil || !in.Installed {
				continue
			}
			report(a.Capabilities(in.Version).SessionWrite, fmt.Sprintf("%s %s 已验证写入", a.ID(), orDash(string(in.Version))))
			if hc, ok := a.(kit.HookChecker); ok {
				report(hc.HooksInstalled(), a.ID()+" hooks 已安装")
			}
			if tc, ok := a.(kit.HookTrustChecker); ok {
				report(tc.HooksTrusted(), a.ID()+" hooks 已被信任")
			}
		}
		for _, x := range external.Discover(cfg) {
			report(x.Trusted, "外部适配器 "+x.ID+" 已信任")
		}
		return map[string]any{"checks": checks}, nil
	})

	versionOf := func(ctx context.Context, cfg *config.Config, args []string) (kit.Adapter, string, error) {
		a, ok := registry(cfg).Get(args[0])
		if !ok {
			return nil, "", fmt.Errorf("unknown agent %q", args[0])
		}
		if len(args) > 1 {
			return a, args[1], nil
		}
		in, err := a.Detect(ctx)
		if err != nil {
			return nil, "", err
		}
		if in.Version == "" {
			return nil, "", errors.New("无法确定该 agent 的版本")
		}
		return a, string(in.Version), nil
	}
	withCfg := func(fn func(context.Context, *config.Config, []string) (any, error)) func(context.Context, []string) (any, error) {
		return func(ctx context.Context, args []string) (any, error) {
			cfg, err := cfgFor("")
			if err != nil {
				return nil, err
			}
			return fn(ctx, cfg, args)
		}
	}
	add("verify-version <agent> [version]", cobra.RangeArgs(1, 2), withCfg(func(ctx context.Context, cfg *config.Config, args []string) (any, error) {
		a, v, err := versionOf(ctx, cfg, args)
		if err != nil {
			return nil, err
		}
		r, err := versions.Probe(ctx, a, v)
		return map[string]any{"version": v, "state": r.State, "error": r.Error}, err
	}))
	add("approve-version <agent> [version]", cobra.RangeArgs(1, 2), withCfg(func(ctx context.Context, cfg *config.Config, args []string) (any, error) {
		a, v, err := versionOf(ctx, cfg, args)
		if err != nil {
			return nil, err
		}
		return nil, versions.Set(a.ID(), v, versions.Record{State: versions.Approved, CheckedAt: time.Now().UTC()})
	}))
	add("revoke-version <agent> [version]", cobra.RangeArgs(1, 2), withCfg(func(ctx context.Context, cfg *config.Config, args []string) (any, error) {
		a, v, err := versionOf(ctx, cfg, args)
		if err != nil {
			return nil, err
		}
		return nil, versions.Clear(a.ID(), v)
	}))

	add("pause", cobra.NoArgs, func(context.Context, []string) (any, error) { return nil, fsutil.SetPaused(true) })
	add("claims", cobra.NoArgs, func(context.Context, []string) (any, error) {
		response, err := daemon.Ask(daemon.Request{Method: "claims"}, 100*time.Millisecond, 500*time.Millisecond)
		if err != nil {
			return map[string]any{"claims": []live.Claim{}}, nil // claims live in the daemon
		}
		if response.Error != "" {
			return nil, errors.New(response.Error)
		}
		var claims []live.Claim
		if err := json.Unmarshal([]byte(response.Output), &claims); err != nil {
			return nil, err
		}
		if claims == nil {
			claims = []live.Claim{}
		}
		return map[string]any{"claims": claims}, nil
	})
	type taskRow struct {
		store.Task
		Project string `json:"project"`
	}
	tl := dirFlag(add("tasks", cobra.NoArgs, withStore(func(_ context.Context, st *store.Store, _ *config.Config, _ []string) (any, error) {
		pid := ""
		if dir != "" {
			pid = project.Identify(dir).ID
		}
		tasks, err := st.ListTasks(pid, force)
		if err != nil {
			return nil, err
		}
		projects, err := st.ListProjects()
		if err != nil {
			return nil, err
		}
		paths := map[string]string{}
		for _, p := range projects {
			paths[p.ID] = p.Path
		}
		rows := []taskRow{}
		for _, t := range tasks {
			rows = append(rows, taskRow{Task: t, Project: paths[t.ProjectID]})
		}
		return map[string]any{"tasks": rows}, nil
	})))
	tl.Flags().BoolVar(&force, "all", false, "include finished tasks")
	ta := dirFlag(add("task-add <title>", cobra.ExactArgs(1), withStore(func(_ context.Context, st *store.Store, _ *config.Config, a []string) (any, error) {
		if dir == "" {
			return nil, errors.New("任务需要先选择项目目录")
		}
		p := project.Identify(dir)
		if err := st.UpsertProject(p); err != nil {
			return nil, err
		}
		return st.AddTask(p.ID, a[0], value)
	})))
	ta.Flags().StringVar(&value, "detail", "", "details")
	add("task-reopen <id>", cobra.ExactArgs(1), withStore(func(_ context.Context, st *store.Store, _ *config.Config, a []string) (any, error) {
		return st.ReopenTask(a[0])
	}))
	add("task-delete <id>", cobra.ExactArgs(1), withStore(func(_ context.Context, st *store.Store, _ *config.Config, a []string) (any, error) {
		return nil, st.DeleteTask(a[0])
	}))
	add("resume", cobra.NoArgs, func(context.Context, []string) (any, error) { return nil, fsutil.SetPaused(false) })

	add("ensure-daemon", cobra.NoArgs, func(context.Context, []string) (any, error) {
		return map[string]any{"online": ensureDaemon()}, nil
	})

	add("config", cobra.NoArgs, func(context.Context, []string) (any, error) {
		cfg, err := cfgFor("")
		if err != nil {
			return nil, err
		}
		dirs := map[string]string{}
		for id := range cfg.Agents {
			dirs[id] = cfg.Direction(id)
		}
		return map[string]any{"sessions": cfg.Sync.Sessions, "memory": cfg.Sync.Memory, "global_memory": cfg.Sync.GlobalMemory, "project_memory": cfg.Sync.ProjectMemory, "rules": cfg.Sync.Rules, "skills": cfg.Sync.Skills.Enabled,
			"auto_extract": cfg.Memory.AutoExtract, "extractor": cfg.Memory.Extractor, "inject_on_prompt": cfg.Live.InjectOnPrompt,
			"inject_token_budget": cfg.Live.InjectTokenBudget, "conflict_policy": cfg.Live.ConflictPolicy, "keep_recent_turns": cfg.Handoff.KeepRecentTurns,
			"exclude_projects": orEmptyS(cfg.Privacy.ExcludeProjects), "include_projects": orEmptyS(cfg.Privacy.IncludeProjects), "exclude_files": orEmptyS(cfg.Sync.ExcludeFiles), "exclude_skills": orEmptyS(cfg.Sync.Skills.Exclude), "redact": orEmptyS(cfg.Privacy.Redact), "enabled": orEmptyS(cfg.Enabled), "directions": dirs}, nil
	})
	cs := add("config-set", cobra.NoArgs, func(context.Context, []string) (any, error) { return nil, setConfig(key, value) })
	cs.Flags().StringVar(&key, "key", "", "dotted key")
	cs.Flags().StringVar(&value, "value", "", "TOML literal")
	return c
}

func orEmpty(a []filesync.Action) []filesync.Action {
	if a == nil {
		return []filesync.Action{}
	}
	return a
}

func orEmptyS(a []string) []string {
	if a == nil {
		return []string{}
	}
	return a
}

var settableKey = regexp.MustCompile(`^(sync\.(sessions|memory|global_memory|project_memory|rules|exclude_files)|sync\.skills\.(enabled|exclude)|memory\.(auto_extract|extractor)|live\.(inject_on_prompt|inject_token_budget|conflict_policy)|handoff\.keep_recent_turns|privacy\.(exclude_projects|include_projects|redact)|agents\.enabled|agents\.[a-z0-9-]+\.direction)$`)

// setConfig edits one value in the global file, keeping comments and layout.
// The result must load as a valid configuration before it replaces the file.
func setConfig(key, value string) error {
	if !settableKey.MatchString(key) {
		return fmt.Errorf("不支持修改配置项 %q", key)
	}
	path := config.GlobalPath()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		b, err = []byte("version = 1\n"), nil
	}
	if err != nil {
		return err
	}
	next := setTOMLValue(string(b), key, value)
	tmp, err := os.CreateTemp(filepath.Dir(fsutil.DBPath()), "config-*.toml")
	if err != nil {
		if e := fsutil.EnsurePrivateDir(filepath.Dir(fsutil.DBPath())); e != nil {
			return e
		}
		if tmp, err = os.CreateTemp(filepath.Dir(fsutil.DBPath()), "config-*.toml"); err != nil {
			return err
		}
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(next); err != nil {
		return err
	}
	tmp.Close()
	if _, err := config.Load(config.Options{GlobalPath: tmp.Name()}); err != nil {
		return err
	}
	return fsutil.SafeWrite(path, []byte(next), 0o644)
}

// setTOMLValue replaces or inserts one key in its section.
func setTOMLValue(text, key, value string) string {
	i := strings.LastIndex(key, ".")
	section, name := key[:i], key[i+1:]
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	header := "[" + section + "]"
	start := -1
	for n, l := range lines {
		if strings.TrimSpace(strings.SplitN(l, "#", 2)[0]) == header {
			start = n
			break
		}
	}
	entry := name + " = " + value
	if start < 0 {
		return strings.Join(lines, "\n") + "\n\n" + header + "\n" + entry + "\n"
	}
	keyRe := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(name) + `\s*=`)
	for n := start + 1; n < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[n]), "["); n++ {
		if keyRe.MatchString(lines[n]) {
			lines[n] = entry
			return strings.Join(lines, "\n") + "\n"
		}
	}
	out := append(append(append([]string{}, lines[:start+1]...), entry), lines[start+1:]...)
	return strings.Join(out, "\n") + "\n"
}
