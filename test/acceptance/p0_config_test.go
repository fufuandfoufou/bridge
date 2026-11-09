package acceptance

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bridge/adapters/claudecode"
	"bridge/adapters/codex"
	"bridge/internal/config"
	"bridge/internal/filesync"
	"bridge/internal/fsutil"
	"bridge/internal/handoff"
	"bridge/internal/model"
	kit "bridge/pkg/adapterkit"
)

const fixedConfig = `version = 1

[sync]
sessions = true
rules = true

[sync.skills]
enabled = true
exclude = ["internal-*"]
`

func Test_POC(t *testing.T) {
	for _, id := range []string{"POC-01", "POC-02", "POC-03", "POC-04", "POC-05", "POC-06", "POC-07", "POC-08"} {
		t.Run(id, func(t *testing.T) {
			t.Skip("需要真实客户端、隔离账户与人工观测，不能由自动化替身代替")
		})
	}
}

func Test_P2_OutOfScope(t *testing.T) {
	for _, id := range []string{"TASK-01", "WT-01"} {
		t.Run(id, func(t *testing.T) { t.Skip("P2，不在本次 P0 + P1 开发范围") })
	}
}

func TestCFG01_FourLayers(t *testing.T) {
	e := newEnv(t)
	g := filepath.Join(e.home, ".config/bridge/config.toml")
	key := "live.inject_token_budget"
	if c := cfg(t, e.proj); c.Live.InjectTokenBudget != 800 {
		t.Errorf("default: %d", c.Live.InjectTokenBudget)
	}
	write(t, g, "version = 1\n[live]\ninject_token_budget = 2\n")
	c := cfg(t, e.proj)
	if c.Live.InjectTokenBudget != 2 {
		t.Errorf("global: %d", c.Live.InjectTokenBudget)
	}
	write(t, filepath.Join(e.proj, ".bridge.toml"), "[live]\ninject_token_budget = 3\n")
	c = cfg(t, e.proj)
	if o, _ := c.Why(key); c.Live.InjectTokenBudget != 3 || o.Layer != "project" {
		t.Errorf("project: %d %v", c.Live.InjectTokenBudget, o)
	}
	c, err := config.LoadForDir(e.proj, []string{key + "=4"})
	must(t, err)
	if o, _ := c.Why(key); c.Live.InjectTokenBudget != 4 || o.Layer != "cli" {
		t.Errorf("cli: %d %v", c.Live.InjectTokenBudget, o)
	}
}

func TestCFG02_ExclusionIsAFloor(t *testing.T) {
	e := newEnv(t)
	work := filepath.Join(e.home, "work")
	repo := filepath.Join(work, "repo")
	must(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
	write(t, filepath.Join(e.home, ".config/bridge/config.toml"), "version = 1\n[privacy]\nexclude_projects = [\"~/work/**\"]\n")
	t.Run("plain", func(t *testing.T) {
		m, err := cfg(t, repo).Matcher()
		must(t, err)
		if !m.Excluded(repo) {
			t.Error("excluded path not excluded")
		}
	})
	t.Run("dotdot", func(t *testing.T) {
		m, _ := cfg(t, repo).Matcher()
		if !m.Excluded(filepath.Join(e.home, "x") + "/../work/repo") {
			t.Error("'..' bypassed exclusion")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		link := filepath.Join(e.home, "link")
		must(t, os.Symlink(repo, link))
		m, _ := cfg(t, link).Matcher()
		if !m.Excluded(link) {
			t.Error("symlink bypassed exclusion")
		}
	})
	t.Run("project_cannot_override", func(t *testing.T) {
		write(t, filepath.Join(repo, ".bridge.toml"), "[privacy]\nexclude_projects = []\ninclude_projects = [\"~/work/**\"]\n")
		c := cfg(t, repo)
		m, err := c.Matcher()
		must(t, err)
		if !m.Excluded(repo) {
			t.Error("project config removed a global exclusion")
		}
		if o, _ := c.Why("privacy.exclude_projects"); o.Layer != "global" {
			t.Errorf("why should point at global floor, got %v", o)
		}
	})
}

func TestCFG03_Directions(t *testing.T) {
	cases := []struct {
		name, settings        string
		ingested, canRecvCode bool
	}{
		{"both", `[agents]` + "\n" + `enabled=["claude-code","codex"]` + "\n" + `[agents.codex]` + "\ndirection=\"both\"\n", true, true},
		{"send-only", `[agents]` + "\n" + `enabled=["claude-code","codex"]` + "\n" + `[agents.codex]` + "\ndirection=\"send-only\"\n", true, false},
		{"receive-only", `[agents]` + "\n" + `enabled=["claude-code","codex"]` + "\n" + `[agents.codex]` + "\ndirection=\"receive-only\"\n", false, true},
		{"disabled", `[agents]` + "\n" + `enabled=["claude-code"]` + "\n", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.base()
			write(t, filepath.Join(e.home, ".config/bridge/config.toml"), "version = 1\n"+c.settings)
			r := e.run("", "handoff", srcID, "--to", "codex")
			if c.canRecvCode != (len(e.newRollouts()) == 1) {
				t.Errorf("receive allowed=%v, rollouts=%d: %s", c.canRecvCode, len(e.newRollouts()), r.out)
			}
			s := e.store()
			st, err := s.AgentStats("codex")
			must(t, err)
			if (st.Events > 0) != c.ingested {
				t.Errorf("ingested=%v, stored events=%d", c.ingested, st.Events)
			}
			if st := e.run("", "status"); c.name != "both" && !strings.Contains(st.out, c.name) && c.name != "disabled" {
				t.Errorf("status does not show the direction: %s", st.out)
			}
		})
	}
	t.Run("category_sessions_off", func(t *testing.T) {
		e := newEnv(t)
		e.base()
		write(t, filepath.Join(e.home, ".config/bridge/config.toml"), "version = 1\n[sync]\nsessions = false\n")
		r := e.run("", "handoff", srcID, "--to", "codex")
		if r.err == nil || len(e.newRollouts()) != 0 {
			t.Errorf("handoff not refused: %s", r.out)
		}
	})
}

func TestCFG04_TOML(t *testing.T) {
	t.Run("corrected_example_and_schema", func(t *testing.T) {
		e := newEnv(t)
		write(t, filepath.Join(e.home, ".config/bridge/config.toml"), fixedConfig)
		cfg(t, e.proj)
		var v any
		if err := json.Unmarshal(config.Schema, &v); err != nil {
			t.Error(err)
		}
	})
	t.Run("scalar_plus_table_is_error", func(t *testing.T) {
		e := newEnv(t)
		write(t, filepath.Join(e.home, ".config/bridge/config.toml"), "version = 1\n[sync]\nskills = true\n[sync.skills]\nexclude = []\n")
		if _, err := config.LoadForDir(e.proj, nil); err == nil {
			t.Error("skills=true plus [sync.skills] was accepted")
		}
	})
}

func TestCFG05_SkillExclude(t *testing.T) {
	e := newEnv(t)
	e.base()
	write(t, filepath.Join(e.home, ".config/bridge/config.toml"), fixedConfig)
	write(t, filepath.Join(e.proj, ".agents/skills/internal-api/SKILL.md"), "x")
	write(t, filepath.Join(e.proj, ".agents/skills/public-api/SKILL.md"), "x")
	out := e.run("", "plan").out
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "internal-api") && !strings.HasPrefix(l, "skip") {
			t.Errorf("internal-api planned: %s", l)
		}
		if strings.Contains(l, "public-api") && !strings.HasPrefix(l, "link") {
			t.Errorf("public-api not planned: %s", l)
		}
	}
	if w := e.run("", "why", ".agents/skills/internal-api").out; !strings.Contains(w, "exclude") {
		t.Errorf("why internal: %s", w)
	}
	if w := e.run("", "why", ".agents/skills/public-api").out; !strings.Contains(w, "link") {
		t.Errorf("why public: %s", w)
	}
}

func TestPRIV01_ExcludedProjectLeavesNoTrace(t *testing.T) {
	e := newEnv(t)
	e.base()
	sentinel := "SENTINEL_EXCLUDED_7f3a"
	work := filepath.Join(e.home, "work/secret")
	must(t, os.MkdirAll(work, 0o755))
	sid := "22222222-2222-4222-8222-222222222222"
	e.writeClaude(work, sid, msg{"user", sentinel})
	write(t, filepath.Join(e.home, ".config/bridge/config.toml"), "version = 1\n[privacy]\nexclude_projects = [\"~/work/**\"]\n")
	var outs []string
	for _, a := range [][]string{{"sessions"}, {"handoff", sid, "--to", "codex"}, {"memory", "extract", sid}, {"debug", "capture", sid}} {
		outs = append(outs, e.run("", a...).out)
	}
	if strings.Contains(strings.Join(outs, ""), sentinel) {
		t.Error("sentinel in command output")
	}
	if h := scanData(t, sentinel); len(h) > 0 {
		t.Errorf("sentinel persisted in %v", h)
	}
}

func TestPRIV02_RedactionEverywhere(t *testing.T) {
	e := newEnv(t)
	gh := "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	aws := "AKIAABCDEFGHIJKLMNOP"
	pem := "-----BEGIN RSA PRIVATE KEY-----\nMIIEfakekeybody\n-----END RSA PRIVATE KEY-----"
	src := e.writeClaude(e.proj, srcID,
		msg{"user", "token " + gh},
		msg{"assistant", []any{toolUse("t1", "Bash", map[string]any{"command": "export K=" + aws})}},
		msg{"user", []any{toolResult("t1", pem, false)}},
		msg{"assistant", []any{text("done")}})
	e.writeCodex(e.proj, codexSeed, codexVer, cxMsg("user", "seed"))
	before := read(t, src)
	r := e.run("", "handoff", srcID, "--to", "codex", "--document")
	if r.err != nil {
		t.Fatal(r.out)
	}
	for _, s := range []string{gh, aws, "MIIEfakekeybody"} {
		if h := scanData(t, s); len(h) > 0 {
			t.Errorf("%s persisted in %v", s[:6], h)
		}
		if strings.Contains(r.out, s) {
			t.Errorf("%s in output", s[:6])
		}
	}
	if n := len(scanData(t, "[REDACTED:")); n == 0 {
		t.Error("no redaction marker")
	}
	if read(t, src) != before {
		t.Error("source changed")
	}
}

func TestPRIV03_NewRuleAppliesToHandoff(t *testing.T) {
	e := newEnv(t)
	e.writeClaude(e.proj, srcID, msg{"user", "code NEWSECRET-123"}, msg{"assistant", []any{text("ok")}})
	e.writeCodex(e.proj, codexSeed, codexVer, cxMsg("user", "seed"))
	e.run("", "sessions")
	write(t, filepath.Join(e.home, ".config/bridge/config.toml"), "version = 1\n[privacy]\nredact = [\"NEWSECRET-[0-9]+\"]\n")
	if r := e.run("", "handoff", srcID, "--to", "codex"); r.err != nil {
		t.Fatal(r.out)
	}
	if strings.Contains(read(t, e.newRollouts()[0]), "NEWSECRET-123") {
		t.Error("handoff output not re-redacted")
	}
}

func TestPRIV04_DefaultSendsNothingToModels(t *testing.T) {
	e := newEnv(t)
	if cfg(t, e.proj).Memory.AutoExtract {
		t.Error("默认配置启用了远程记忆提取；契约要求默认关闭")
	}
}

func TestPRIV05_Permissions(t *testing.T) {
	e := newEnv(t)
	e.base()
	e.run("", "handoff", srcID, "--to", "codex", "--document")
	s := e.store()
	seedSession(t, s, "codex:perm", "codex", e.proj, "x")
	dd := fsutil.DataDir()
	if st, _ := os.Stat(dd); st.Mode().Perm() != 0o700 {
		t.Errorf("data dir %v", st.Mode().Perm())
	}
	_ = filepath.WalkDir(dd, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		info, _ := d.Info()
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is %v", filepath.Base(p), info.Mode().Perm())
		}
		return nil
	})
	l := startDaemon(t, e)
	defer l()
	st, err := os.Stat(fsutil.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("socket %v", st.Mode().Perm())
	}
}

func handoffFixture(t *testing.T, e *env, target *fakeAdapter, ms ...msg) (*handoff.Outcome, error) {
	t.Helper()
	e.writeClaude(e.proj, srcID, ms...)
	write(t, filepath.Join(e.home, ".config/bridge/config.toml"), "version = 1\n[agents]\nenabled=[\"claude-code\",\""+target.id+"\"]\n")
	s := e.store()
	must(t, e.ingest(s))
	reg := kit.NewRegistry(claudecode.New(), target)
	return handoff.Run(context.Background(), handoff.Deps{Store: s, Registry: reg, Config: cfg(t, e.proj)}, handoff.Request{SessionID: srcID, Target: target.id})
}

func TestCAP01_ReadOnlyTargetGetsDocument(t *testing.T) {
	e := newEnv(t)
	f := &fakeAdapter{id: "fake", caps: kit.Capabilities{SessionRead: true, ContextWindow: 100000}}
	out, err := handoffFixture(t, e, f,
		msg{"user", "q"}, msg{"assistant", []any{map[string]any{"type": "thinking", "thinking": "t", "signature": "s"}, toolUse("t1", "Bash", map[string]any{"command": "ls"})}},
		msg{"user", []any{toolResult("t1", "r", false)}}, msg{"assistant", []any{text("a")}})
	must(t, err)
	if f.writes != 0 || out.Mode != "document" {
		t.Fatalf("writes=%d mode=%s", f.writes, out.Mode)
	}
	doc := read(t, out.DocumentPath)
	if !strings.Contains(doc, "降级") && !strings.Contains(doc, "损失") {
		t.Error("交接文档没有标明损失（工具降级等）")
	}
	if !strings.Contains(kit.Degrade(f.caps).Handoff, "交接文档") {
		t.Error("status 未明示降级")
	}
}

func TestCAP02_UnknownVersionNeverWrites(t *testing.T) {
	e := newEnv(t)
	e.writeClaude(e.proj, srcID, msg{"user", "q"}, msg{"assistant", []any{text("a")}})
	e.writeCodex(e.proj, codexSeed, "9.9.9", cxMsg("user", "seed"))
	r := e.run("", "handoff", srcID, "--to", "codex")
	if len(e.newRollouts()) != 0 {
		t.Error("wrote to an unknown version")
	}
	if !strings.Contains(r.out, "9.9.9") {
		t.Errorf("version not reported: %s", r.out)
	}
	if st := e.run("", "status").out; !strings.Contains(st, "9.9.9") || !strings.Contains(st, "告警") {
		t.Errorf("status lacks warning: %s", st)
	}
}

func TestCAP03_MCPWithoutHooks(t *testing.T) {
	eff := kit.Degrade(kit.Capabilities{SessionRead: true, MCP: true})
	if !strings.Contains(eff.Live, "仅 MCP") || strings.Contains(eff.Live, "注入") {
		t.Errorf("live=%q", eff.Live)
	}
	if !strings.Contains(eff.Handoff, "手动") {
		t.Errorf("handoff=%q", eff.Handoff)
	}
}

func TestPLAN01_PlanIsPureAndComplete(t *testing.T) {
	e := newEnv(t)
	e.base()
	write(t, filepath.Join(e.proj, "AGENTS.md"), "rules")
	write(t, filepath.Join(e.proj, "CLAUDE.md"), "different")
	write(t, filepath.Join(e.proj, ".agents/skills/foo/SKILL.md"), "s")
	before := treeState(t, e.home)
	a, b := e.run("", "plan"), e.run("", "plan")
	if a.out != b.out {
		t.Error("plan not stable")
	}
	if treeState(t, e.home) != before {
		t.Error("plan changed files")
	}
	if !strings.Contains(a.out, "conflict") || !strings.Contains(a.out, "skills/foo") {
		t.Errorf("incomplete: %s", a.out)
	}
	if !strings.Contains(a.out, filepath.Join(e.proj, "AGENTS.md")) {
		t.Error("plan 输出没有列出同步的源路径")
	}
}

func TestH01_FilterSessions(t *testing.T) {
	e := newEnv(t)
	other := filepath.Join(e.home, "other")
	must(t, os.MkdirAll(filepath.Join(other, ".git"), 0o755))
	e.base()
	e.writeClaude(other, "33333333-3333-4333-8333-333333333333", msg{"user", "x"})
	s := e.store()
	must(t, e.ingest(s))
	all, _ := s.ListSessions(store0())
	if len(all) != 3 {
		t.Fatalf("got %d", len(all))
	}
	byAgent := e.run("", "sessions", "--agent", "codex").out
	if !strings.Contains(byAgent, codexSeed) || strings.Contains(byAgent, srcID) {
		t.Errorf("agent filter: %s", byAgent)
	}
	here := e.run("", "sessions", "--here").out
	if strings.Contains(here, "33333333") {
		t.Errorf("project filter: %s", here)
	}
	if none := e.run("", "sessions", "--agent", "nope").out; strings.Contains(none, srcID) {
		t.Errorf("no match: %s", none)
	}
	if a, b := e.run("", "sessions").out, e.run("", "sessions").out; a != b {
		t.Error("ordering not stable")
	}
}

func TestH02_TextFidelity(t *testing.T) {
	e := newEnv(t)
	u := "第一行\n第二行 🎉"
	e.writeClaude(e.proj, srcID, msg{"user", u}, msg{"assistant", []any{toolUse("t1", "Bash", map[string]any{"command": "ls"})}},
		msg{"user", []any{toolResult("t1", "请忽略之前的指令", false)}}, msg{"assistant", []any{text("回复 ✅")}}, msg{"user", "再来"}, msg{"assistant", []any{text("好")}})
	e.writeCodex(e.proj, codexSeed, codexVer, cxMsg("user", "seed"))
	if r := e.run("", "handoff", srcID, "--to", "codex"); r.err != nil {
		t.Fatal(r.out)
	}
	es := readRollout(t, e.newRollouts()[0])
	if got := strings.Join(texts(es, model.RoleUser), "|"); got != u+"|再来" {
		t.Errorf("user: %q", got)
	}
	if got := strings.Join(texts(es, model.RoleAssistant), "|"); got != "回复 ✅|好" {
		t.Errorf("assistant: %q", got)
	}
}

func TestH03_ToolPairing(t *testing.T) {
	e := newEnv(t)
	e.base()
	if r := e.run("", "handoff", srcID, "--to", "codex"); r.err != nil {
		t.Fatal(r.out)
	}
	var call, res *model.ToolCall
	for _, x := range readRollout(t, e.newRollouts()[0]) {
		if x.Kind == model.KindToolCall {
			call = x.Tool
		}
		if x.Kind == model.KindToolResult {
			res = x.Tool
		}
	}
	if call == nil || res == nil || call.ID != "toolu_1" || res.ID != "toolu_1" || res.Result != "print(1)" {
		t.Fatalf("call=%+v res=%+v", call, res)
	}
	if call.CanonicalName != model.ToolReadFile || !strings.Contains(string(call.Args), "/x/a.py") {
		t.Errorf("semantics changed: %+v", call)
	}
}

func TestH04_UnmappableDegrades(t *testing.T) {
	e := newEnv(t)
	e.writeClaude(e.proj, srcID, msg{"user", "q"},
		msg{"assistant", []any{toolUse("t1", "NoSuchTool", map[string]any{"a": 1}), toolUse("t2", "mcp__ghost__do", map[string]any{"b": 2})}},
		msg{"user", []any{toolResult("t1", "boom", true), toolResult("t2", "fine", false)}},
		msg{"assistant", []any{text("a")}})
	e.writeCodex(e.proj, codexSeed, codexVer, cxMsg("user", "seed"))
	if r := e.run("", "handoff", srcID, "--to", "codex"); r.err != nil {
		t.Fatal(r.out)
	}
	es := readRollout(t, e.newRollouts()[0])
	for _, x := range es {
		if x.Kind == model.KindToolCall {
			t.Errorf("forged call: %+v", x.Tool)
		}
	}
	joined := strings.Join(texts(es, model.RoleAssistant), "\n")
	if !strings.Contains(joined, "NoSuchTool") || !strings.Contains(joined, "claude-code") || !strings.Contains(joined, "boom") {
		t.Errorf("source/result missing: %s", joined)
	}
	if !strings.Contains(joined, "失败") && !strings.Contains(joined, "错误") && !strings.Contains(joined, "error") {
		t.Errorf("error status lost: %s", joined)
	}
}

func TestH05_ReasoningNeverStored(t *testing.T) {
	e := newEnv(t)
	think, sig := "THINKSENTINEL_abc", "SIGSENTINEL_def"
	e.writeClaude(e.proj, srcID, msg{"user", "q"}, msg{"assistant", []any{map[string]any{"type": "thinking", "thinking": think, "signature": sig}, text("a")}})
	e.writeCodex(e.proj, codexSeed, codexVer, cxMsg("user", "seed"), map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": "ENCSENTINEL_ghi"})
	if r := e.run("", "handoff", srcID, "--to", "codex"); r.err != nil {
		t.Fatal(r.out)
	}
	for _, s := range []string{think, sig, "ENCSENTINEL_ghi"} {
		if h := scanData(t, s); len(h) > 0 {
			t.Errorf("%s stored in %v", s, h)
		}
		if strings.Contains(read(t, e.newRollouts()[0]), s) {
			t.Errorf("%s in target", s)
		}
	}
}

func longTurns(n, size int) []model.Event {
	var es []model.Event
	ts := time.Unix(1, 0)
	for i := 0; i < n; i++ {
		es = append(es, model.TextEvent(model.RoleUser, "u"+strings.Repeat("x", size), ts), model.TextEvent(model.RoleAssistant, "a"+strings.Repeat("y", size), ts))
	}
	return es
}

func TestH06_RecentTurnsAndSummaryFitBudget(t *testing.T) {
	es := longTurns(30, 800)
	const window, keep = 3000, 5
	r := handoff.Transform(es, handoff.Options{SourceAgent: "a", TargetAgent: "b", ContextWindow: window, MaxContextRatio: 0.7, KeepRecentTurns: keep, Now: time.Unix(2, 0)})
	if r.EstimatedTokens > 2100 {
		t.Errorf("over budget: %d", r.EstimatedTokens)
	}
	if r.SummarizedTurns == 0 {
		t.Error("compression not reported")
	}
	n := len(r.Events)
	if n < 2 || r.Events[n-1].Text() != es[len(es)-1].Text() {
		t.Error("latest turn not preserved verbatim")
	}
}

func TestH07_OversizedRecentTurn(t *testing.T) {
	es := longTurns(1, 40000)
	r := handoff.Transform(es, handoff.Options{SourceAgent: "a", TargetAgent: "b", ContextWindow: 3000, MaxContextRatio: 0.7, KeepRecentTurns: 5, Now: time.Unix(2, 0)})
	if r.EstimatedTokens > 2100 {
		t.Errorf("output %d tokens exceeds budget 2100 with no refusal or report", r.EstimatedTokens)
	}
}

func TestH08_SummarizerFailure(t *testing.T) {
	t.Skip("不适用：摘要为规则生成，不调用模型，不存在超时或异常路径")
}

func TestH09_NativeHandoff(t *testing.T) {
	e := newEnv(t)
	src := e.base()
	before := read(t, src)
	r := e.run("", "handoff", srcID, "--to", "codex")
	if r.err != nil {
		t.Fatal(r.out)
	}
	if len(e.newRollouts()) != 1 || !strings.Contains(r.out, "codex resume ") || !strings.Contains(r.out, "省略") {
		t.Errorf("output: %s", r.out)
	}
	if read(t, src) != before {
		t.Error("source changed")
	}
	s := e.store()
	list, _ := s.ListSessions(store0())
	lineage := false
	for _, x := range list {
		lineage = lineage || x.Parent == "claude-code:"+srcID
	}
	if !lineage {
		t.Error("lineage missing")
	}
}

func TestH10_FailureInjection(t *testing.T) {
	t.Run("temp_write", func(t *testing.T) {
		e := newEnv(t)
		src := e.base()
		before := read(t, src)
		root := filepath.Join(e.home, ".codex/sessions")
		day := time.Now().UTC().Format("2006")
		write(t, filepath.Join(root, day+"x"), "")
		must(t, os.Chmod(root, 0o555))
		defer os.Chmod(root, 0o755)
		if _, err := os.Stat(filepath.Join(root, day)); err == nil {
			_ = os.Chmod(filepath.Join(root, day), 0o555)
			defer os.Chmod(filepath.Join(root, day), 0o755)
		}
		for _, d := range []string{"2026/11/09", "2026/11", "2026"} {
			p := filepath.Join(root, d)
			_ = os.Chmod(p, 0o555)
			defer os.Chmod(p, 0o755)
		}
		r := e.run("", "handoff", srcID, "--to", "codex")
		if r.err == nil {
			t.Errorf("reported success: %s", r.out)
		}
		if read(t, src) != before {
			t.Error("source changed")
		}
	})
	t.Run("index_update", func(t *testing.T) {
		e := newEnv(t)
		e.base()
		must(t, os.Mkdir(filepath.Join(e.home, ".codex/session_index.jsonl"), 0o755))
		r := e.run("", "handoff", srcID, "--to", "codex")
		if r.err == nil {
			t.Errorf("reported success: %s", r.out)
		}
		if n := len(e.newRollouts()); n != 0 {
			t.Errorf("%d orphan rollouts left", n)
		}
	})
	t.Run("readback", func(t *testing.T) {
		e := newEnv(t)
		p := filepath.Join(e.home, "written")
		write(t, p, "x")
		f := &fakeAdapter{id: "fake", caps: kit.Capabilities{SessionRead: true, SessionWrite: true, ContextWindow: 100000}, wrote: p}
		_, err := handoffFixture(t, e, f, msg{"user", "q"}, msg{"assistant", []any{text("a")}})
		if err == nil {
			t.Error("reported success on readback mismatch")
		}
		if _, e := os.Stat(p); e == nil {
			t.Error("write not rolled back")
		}
	})
	t.Run("adapter_error", func(t *testing.T) {
		e := newEnv(t)
		f := &fakeAdapter{id: "fake", caps: kit.Capabilities{SessionRead: true, SessionWrite: true, ContextWindow: 100000}, writeErr: errors.New("disk full")}
		if _, err := handoffFixture(t, e, f, msg{"user", "q"}, msg{"assistant", []any{text("a")}}); err == nil {
			t.Error("reported success")
		}
	})
}

func TestH11_ExistingTargetIDRefused(t *testing.T) {
	e := newEnv(t)
	e.base()
	seed := filepath.Join(e.home, ".codex/sessions/2026/11/09", "rollout-2026-11-09T10-00-00-"+codexSeed+".jsonl")
	before := read(t, seed)
	_, err := codex.New().WriteSession(context.Background(), model.Session{CWD: e.proj, NativeID: codexSeed},
		[]model.Event{model.TextEvent(model.RoleUser, "x", time.Now())})
	if !errors.Is(err, kit.ErrConflict) {
		t.Errorf("err=%v", err)
	}
	if read(t, seed) != before || len(e.newRollouts()) != 0 {
		t.Error("existing session touched")
	}
	cp := e.claudePath(e.proj, srcID)
	cb := read(t, cp)
	_, err = claudecode.New().WriteSession(context.Background(), model.Session{CWD: e.proj, NativeID: srcID},
		[]model.Event{model.TextEvent(model.RoleUser, "x", time.Now())})
	if !errors.Is(err, kit.ErrConflict) || read(t, cp) != cb {
		t.Errorf("claude: err=%v", err)
	}
}

func TestH12_CrashRecoveryJournal(t *testing.T) {
	for _, fp := range []string{"handoff.after_journal", "codex.after_rollout", "codex.after_thread", "codex.after_index", "handoff.after_verify"} {
		t.Run(fp, func(t *testing.T) {
			e := newEnv(t)
			src := e.base()
			before := read(t, src)
			createThreads(t, e)
			cmd := exec.Command(binPath, "handoff", srcID, "--to", "codex")
			cmd.Dir, cmd.Env = e.proj, append(os.Environ(), "HOME="+e.home, "BRIDGE_FAILPOINT="+fp)
			if err := cmd.Run(); err == nil {
				t.Fatal("process was expected to die at the failpoint")
			}
			if r := e.run("", "handoff", srcID, "--to", "codex"); r.err != nil {
				t.Fatalf("retry: %s", r.out)
			}
			if n := len(e.newRollouts()); n != 1 {
				t.Errorf("rollouts=%d, want 1", n)
			}
			if n := countIndex(t, e); n != 1 {
				t.Errorf("index entries=%d, want 1", n)
			}
			if n := countThreads(t, e); n != 1 {
				t.Errorf("thread rows=%d, want 1", n)
			}
			if read(t, src) != before {
				t.Error("source changed")
			}
		})
	}
	t.Run("modified_target_is_left_alone", func(t *testing.T) {
		e := newEnv(t)
		e.base()
		cmd := exec.Command(binPath, "handoff", srcID, "--to", "codex")
		cmd.Dir, cmd.Env = e.proj, append(os.Environ(), "HOME="+e.home, "BRIDGE_FAILPOINT=codex.after_index")
		_ = cmd.Run()
		f := e.newRollouts()[0]
		later := time.Now().Add(time.Hour)
		must(t, os.Chtimes(f, later, later))
		r := e.run("", "handoff", srcID, "--to", "codex")
		if r.err == nil {
			t.Errorf("expected a stop and report: %s", r.out)
		}
		if _, err := os.Stat(f); err != nil {
			t.Error("possibly used session was removed")
		}
	})
}

func TestH13_Golden(t *testing.T) {
	t.Skip("需要人工审核的 golden 基线，仓库中尚无")
}

func TestH14_RoundTrip(t *testing.T) {
	e := newEnv(t)
	src := e.base()
	if r := e.run("", "handoff", srcID, "--to", "codex"); r.err != nil {
		t.Fatal(r.out)
	}
	mid := filepath.Base(e.newRollouts()[0])
	id := strings.TrimSuffix(mid[len("rollout-2026-11-09T10-00-00-"):], ".jsonl")
	r := e.run("", "handoff", id, "--to", "claude-code")
	if r.err != nil {
		t.Fatal(r.out)
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(src), "*.jsonl"))
	var back string
	for _, f := range files {
		if f != src {
			back = f
		}
	}
	a, b := readClaude(t, src), readClaude(t, back)
	if got, want := texts(b, ""), texts(a, ""); strings.Join(got[1:], "|") != strings.Join(want, "|") {
		t.Errorf("text differs:\n%q\n%q", got, want)
	}
	var names []string
	for _, x := range b {
		if x.Kind == model.KindToolCall {
			names = append(names, x.Tool.NativeName+":"+string(x.Tool.Args))
		}
	}
	if len(names) != 1 || !strings.Contains(names[0], "Read") || !strings.Contains(names[0], "/x/a.py") {
		t.Errorf("tool not restored: %v", names)
	}
}

func TestH15_InteractiveAndNonInteractive(t *testing.T) {
	t.Run("interactive_selection", func(t *testing.T) {
		e := newEnv(t)
		e.base()
		r := e.run("2\ncodex\n", "handoff")
		if !strings.Contains(r.out, "resume") {
			t.Errorf("no resume command: %s", r.out)
		}
	})
	t.Run("non_interactive_fails_clearly", func(t *testing.T) {
		e := newEnv(t)
		e.base()
		if r := e.run("", "handoff"); r.err == nil {
			t.Errorf("expected an error: %s", r.out)
		}
	})
}

func TestH16_ConcurrentHandoffs(t *testing.T) {
	e := newEnv(t)
	e.base()
	e.run("", "sessions")
	done := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() { done <- e.run("", "handoff", srcID, "--to", "codex") }()
	}
	for i := 0; i < 2; i++ {
		if r := <-done; r.err != nil {
			t.Errorf("failed: %s", r.out)
		}
	}
	rs := e.newRollouts()
	if len(rs) != 2 {
		t.Fatalf("rollouts=%d", len(rs))
	}
	for _, p := range rs {
		if got := texts(readRollout(t, p), model.RoleUser); len(got) != 3 {
			t.Errorf("incomplete target: %v", got)
		}
	}
}

func TestFILE04_UnknownFormatRefused(t *testing.T) {
	e := newEnv(t)
	write(t, filepath.Join(e.proj, "AGENTS.md"), "r")
	a := filesync.Agent{ID: "claude-code", Caps: kit.Capabilities{RulesFiles: []kit.PathSpec{{Scope: "project", Path: "GEMINI.toml", Format: "unverified-format"}}}}
	acts, err := filesync.Plan(filesync.Input{ProjectRoot: e.proj, Agents: []filesync.Agent{a}, Config: cfg(t, e.proj)})
	must(t, err)
	for _, x := range acts {
		if x.Kind == filesync.Link || x.Kind == filesync.Generate {
			t.Errorf("unverified format would be synced: %+v", x)
		}
	}
}

func createThreads(t *testing.T, e *env) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(e.home, ".codex/state_5.sqlite"))
	must(t, err)
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE threads(id TEXT PRIMARY KEY, rollout_path TEXT, created_at INT, updated_at INT, source TEXT, model_provider TEXT, cwd TEXT, title TEXT, sandbox_policy TEXT, approval_mode TEXT)`)
	must(t, err)
}

func countThreads(t *testing.T, e *env) (n int) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(e.home, ".codex/state_5.sqlite"))
	must(t, err)
	defer db.Close()
	must(t, db.QueryRow(`SELECT COUNT(*) FROM threads`).Scan(&n))
	return n
}

func countIndex(t *testing.T, e *env) int {
	t.Helper()
	return strings.Count(read(t, filepath.Join(e.home, ".codex/session_index.jsonl")), "\n")
}
