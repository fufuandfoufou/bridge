package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"bridge/adapters/claudecode"
	"bridge/adapters/codex"
	"bridge/adapters/external"
	"bridge/internal/config"
	"bridge/internal/daemon"
	"bridge/internal/filesync"
	"bridge/internal/fsutil"
	"bridge/internal/live"
	"bridge/internal/mcpserver"
	"bridge/internal/memory"
	"bridge/internal/model"
	"bridge/internal/project"
	"bridge/internal/store"
	kit "bridge/pkg/adapterkit"
)

func store0() store.SessionFilter { return store.SessionFilter{} }

func startDaemon(t *testing.T, e *env) func() {
	t.Helper()
	cmd := exec.Command(binPath, "daemon")
	cmd.Env = append(os.Environ(), "HOME="+e.home)
	must(t, cmd.Start())
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(fsutil.SocketPath()); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return func() { _ = cmd.Process.Signal(syscall.SIGTERM); _ = cmd.Wait() }
}

func (e *env) refOf(p string) (kit.Adapter, kit.SessionRef) {
	a := claudecode.New()
	refs, err := a.ListSessions(context.Background(), kit.Filter{})
	must(e.t, err)
	for _, r := range refs {
		if r.Path == p {
			return a, r
		}
	}
	e.t.Fatalf("ref %s not found", p)
	return nil, kit.SessionRef{}
}

func (e *env) ingestOnce(s *store.Store, p string) {
	e.t.Helper()
	a, r := e.refOf(p)
	_, err := (&daemon.Ingester{Store: s, Registry: e.reg(), Config: cfgFn}).IngestRef(context.Background(), a, r)
	must(e.t, err)
}

func stored(t *testing.T, s *store.Store) []model.Event {
	t.Helper()
	es, err := s.Events("claude-code:"+srcID, 0, 0)
	must(t, err)
	return es
}

func appendFile(t *testing.T, p, s string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	defer f.Close()
	_, err = f.WriteString(s)
	must(t, err)
}

func oneLine(cwd, text string) string {
	return claudeLines(cwd, srcID, msg{"user", text})
}

func TestING01_DuplicateNotificationsOnce(t *testing.T) {
	e := newEnv(t)
	p := e.writeClaude(e.proj, srcID, msg{"user", "one"})
	s := e.store()
	e.ingestOnce(s, p)
	appendFile(t, p, oneLine(e.proj, "two"))
	e.ingestOnce(s, p)
	e.ingestOnce(s, p)
	got := texts(stored(t, s), "")
	if strings.Join(got, "|") != "one|two" {
		t.Errorf("%q", got)
	}
	c, ok, err := s.GetCursor("claude-code", p)
	must(t, err)
	if st, _ := os.Stat(p); !ok || c.Offset != st.Size() {
		t.Errorf("cursor %d", c.Offset)
	}
}

func TestING02_PartialLineMultibyte(t *testing.T) {
	e := newEnv(t)
	p := e.writeClaude(e.proj, srcID, msg{"user", "one"})
	s := e.store()
	e.ingestOnce(s, p)
	line := oneLine(e.proj, "多字节字符测试🎉")
	idx := strings.Index(line, "字") + 1 // inside a multi-byte rune
	appendFile(t, p, line[:idx])
	e.ingestOnce(s, p)
	if n := len(stored(t, s)); n != 1 {
		t.Fatalf("partial line ingested: %d", n)
	}
	appendFile(t, p, line[idx:])
	e.ingestOnce(s, p)
	got := texts(stored(t, s), "")
	if len(got) != 2 || got[1] != "多字节字符测试🎉" {
		t.Errorf("%q", got)
	}
}

func TestING03_RotationAndTruncation(t *testing.T) {
	t.Run("replaced_file_new_content", func(t *testing.T) {
		e := newEnv(t)
		p := e.writeClaude(e.proj, srcID, msg{"user", "old1"}, msg{"user", "old2"})
		s := e.store()
		e.ingestOnce(s, p)
		must(t, os.Remove(p))
		time.Sleep(5 * time.Millisecond)
		write(t, p, claudeLines(e.proj, srcID, msg{"user", "new1"}, msg{"user", "new2"}, msg{"user", "new3"}))
		e.ingestOnce(s, p)
		joined := strings.Join(texts(stored(t, s), ""), "|")
		if !strings.Contains(joined, "new1") {
			t.Errorf("rewritten content skipped: %q", joined)
		}
	})
	t.Run("truncated_then_grown", func(t *testing.T) {
		e := newEnv(t)
		p := e.writeClaude(e.proj, srcID, msg{"user", "aaaaaaaa"}, msg{"user", "bbbbbbbb"})
		s := e.store()
		e.ingestOnce(s, p)
		must(t, os.WriteFile(p, []byte(claudeLines(e.proj, srcID, msg{"user", "cccc"})), 0o644))
		e.ingestOnce(s, p)
		if joined := strings.Join(texts(stored(t, s), ""), "|"); !strings.Contains(joined, "cccc") {
			t.Errorf("truncated content skipped: %q", joined)
		}
	})
}

func TestING04_CrashExactlyOnce(t *testing.T) {
	t.Run("rescan_without_cursor", func(t *testing.T) {
		e := newEnv(t)
		p := e.writeClaude(e.proj, srcID, msg{"user", "a"}, msg{"user", "b"})
		s := e.store()
		e.ingestOnce(s, p)
		s.Close()
		// A fresh process with no cursor row models a crash before the commit.
		e2 := e.store()
		before := len(stored(t, e2))
		_, err := e2.Ingest("claude-code:"+srcID, stored(t, e2), store.CursorState{Cursor: model.Cursor{Agent: "claude-code", NativeFile: "other"}})
		must(t, err)
		if n := len(stored(t, e2)); n != before {
			t.Errorf("duplicates: %d vs %d", n, before)
		}
	})
	t.Run("failed_commit_moves_nothing", func(t *testing.T) {
		e := newEnv(t)
		s := e.store()
		s.Close()
		_, err := s.Ingest("x", []model.Event{{Seq: 1, Kind: model.KindMeta}}, store.CursorState{})
		if err == nil {
			t.Error("commit on a closed db reported success")
		}
	})
}

func TestING05_MissedEventsAndUnreadable(t *testing.T) {
	e := newEnv(t)
	p := e.writeClaude(e.proj, srcID, msg{"user", "a"})
	bad := e.writeClaude(e.proj, "44444444-4444-4444-8444-444444444444", msg{"user", "b"})
	s := e.store()
	must(t, e.ingest(s))
	appendFile(t, p, oneLine(e.proj, "c")) // no notification delivered
	appendFile(t, bad, oneLine(e.proj, "d"))
	must(t, os.Chmod(bad, 0))
	defer os.Chmod(bad, 0o644)
	err := e.ingest(s)
	if err == nil {
		t.Error("unreadable file produced no warning")
	}
	if got := strings.Join(texts(stored(t, s), ""), "|"); got != "a|c" {
		t.Errorf("rescan missed events: %q", got)
	}
}

func (e *env) rulesFixture() {
	e.base()
	write(e.t, filepath.Join(e.proj, "AGENTS.md"), "# rules\n")
}

func TestFILE01_CreateAndIdempotent(t *testing.T) {
	e := newEnv(t)
	e.rulesFixture()
	if r := e.run("", "sync"); r.err != nil {
		t.Fatal(r.out)
	}
	if read(t, filepath.Join(e.proj, "CLAUDE.md")) != "# rules\n" {
		t.Error("link wrong")
	}
	state := treeState(t, e.proj)
	r := e.run("", "sync")
	if !strings.Contains(r.out, "已同步 0 项") || treeState(t, e.proj) != state {
		t.Errorf("not idempotent: %s", r.out)
	}
}

func TestFILE02_ExistingRulesPreserved(t *testing.T) {
	e := newEnv(t)
	e.rulesFixture()
	write(t, filepath.Join(e.proj, "CLAUDE.md"), "my rules\n")
	before := treeState(t, e.proj)
	r := e.run("", "sync")
	if treeState(t, e.proj) != before {
		t.Error("project changed")
	}
	if !strings.Contains(r.out, "conflict") {
		t.Errorf("not listed: %s", r.out)
	}
}

func TestFILE03_FullTreeKeepsExistingSkills(t *testing.T) {
	e := newEnv(t)
	e.rulesFixture()
	write(t, filepath.Join(e.proj, ".agents/skills/foo/SKILL.md"), "s")
	write(t, filepath.Join(e.proj, ".agents/skills/foo/scripts/run.sh"), "x")
	write(t, filepath.Join(e.proj, ".claude/skills/mine/SKILL.md"), "keep")
	write(t, filepath.Join(e.proj, ".claude/skills/foo2/data"), "keep")
	if r := e.run("", "sync"); r.err != nil {
		t.Fatal(r.out)
	}
	if read(t, filepath.Join(e.proj, ".claude/skills/mine/SKILL.md")) != "keep" {
		t.Error("existing skill lost")
	}
	if read(t, filepath.Join(e.proj, ".claude/skills/foo/scripts/run.sh")) != "x" {
		t.Error("tree incomplete")
	}
	if st, _ := os.Lstat(filepath.Join(e.proj, ".claude/skills")); st.Mode()&os.ModeSymlink != 0 {
		t.Error("directory replaced by link")
	}
}

func TestFILE05_DangerousPaths(t *testing.T) {
	t.Run("source_loop", func(t *testing.T) {
		e := newEnv(t)
		e.base()
		must(t, os.Symlink("AGENTS.md", filepath.Join(e.proj, "AGENTS.md")))
		if r := e.run("", "sync"); strings.Contains(r.out, "CLAUDE.md") && strings.Contains(r.out, "link ") {
			t.Errorf("linked to a loop: %s", r.out)
		}
	})
	t.Run("target_link_escapes", func(t *testing.T) {
		e := newEnv(t)
		e.rulesFixture()
		out := filepath.Join(e.home, "outside.md")
		write(t, out, "precious")
		must(t, os.Symlink(out, filepath.Join(e.proj, "CLAUDE.md")))
		e.run("", "sync")
		if read(t, out) != "precious" {
			t.Error("write followed an escaping link")
		}
	})
	t.Run("skill_source_escapes_repo", func(t *testing.T) {
		e := newEnv(t)
		e.rulesFixture()
		ext := filepath.Join(e.home, "elsewhere/evil")
		write(t, filepath.Join(ext, "SKILL.md"), "x")
		must(t, os.MkdirAll(filepath.Join(e.proj, ".agents/skills"), 0o755))
		must(t, os.Symlink(ext, filepath.Join(e.proj, ".agents/skills/evil")))
		e.run("", "sync")
		if _, err := os.Lstat(filepath.Join(e.proj, ".claude/skills/evil")); err == nil {
			t.Error("synced a skill source escaping the repository")
		}
	})
	t.Run("read_only_target", func(t *testing.T) {
		e := newEnv(t)
		e.rulesFixture()
		must(t, os.Chmod(e.proj, 0o555))
		defer os.Chmod(e.proj, 0o755)
		if r := e.run("", "sync"); r.err == nil {
			t.Errorf("no error reported: %s", r.out)
		}
	})
}

func TestFILE06_UninstallKeepsUserChanges(t *testing.T) {
	e := newEnv(t)
	e.rulesFixture()
	write(t, filepath.Join(e.home, ".config/bridge/AGENTS.md"), "global")
	if r := e.run("", "sync"); r.err != nil {
		t.Fatal(r.out)
	}
	managed := filepath.Join(e.home, ".claude/CLAUDE.md")
	write(t, managed, read(t, managed)+"user edit\n")
	retarget := filepath.Join(e.proj, "CLAUDE.md")
	must(t, os.Remove(retarget))
	write(t, filepath.Join(e.proj, "MINE.md"), "m")
	must(t, os.Symlink("MINE.md", retarget))
	r := e.run("", "uninstall", "--yes")
	if !strings.Contains(read(t, managed), "user edit") {
		t.Error("user-edited generated file removed")
	}
	if !strings.Contains(r.out, "保留") {
		t.Errorf("conflict not reported: %s", r.out)
	}
	if l, err := os.Readlink(retarget); err != nil || l != "MINE.md" {
		t.Errorf("user's own symlink removed: %v %v", l, err)
	}
}

func TestFILE07_FailureLeavesOriginals(t *testing.T) {
	e := newEnv(t)
	e.rulesFixture()
	g := filepath.Join(e.home, ".config/bridge/AGENTS.md")
	write(t, g, "v1")
	must(t, nil)
	if r := e.run("", "sync"); r.err != nil {
		t.Fatal(r.out)
	}
	target := filepath.Join(e.home, ".claude/CLAUDE.md")
	before := read(t, target)
	write(t, g, "v2")
	dir := filepath.Dir(target)
	must(t, os.Chmod(dir, 0o555))
	r := e.run("", "sync")
	must(t, os.Chmod(dir, 0o755))
	if r.err == nil {
		t.Errorf("reported success: %s", r.out)
	}
	if read(t, target) != before {
		t.Error("original changed")
	}
	if tmp, _ := filepath.Glob(filepath.Join(dir, ".bridge-tmp-*")); len(tmp) > 0 {
		t.Errorf("leftovers: %v", tmp)
	}
	if r := e.run("", "sync"); r.err != nil || !strings.Contains(read(t, target), "v2") {
		t.Errorf("retry failed: %s", r.out)
	}
}

func liveSvc(s *store.Store) *live.Service {
	return &live.Service{Store: s, Config: cfgFn}
}

func TestLIVE01_OnlyUnread(t *testing.T) {
	e := newEnv(t)
	s := e.store()
	seedSession(t, s, "claude-code:A", "claude-code", e.proj, "t1", "t2", "t3", "t4", "t5", "t6")
	seedSession(t, s, "codex:B", "codex", e.proj, "B自己的内容")
	svc := liveSvc(s)
	_, err := svc.PromptSubmit("codex", "B", e.proj)
	must(t, err)
	svc.Ack("codex", "B")
	appendEvents(t, s, "claude-code:A", "claude-code", 7, "新指令七", "新回复八")
	out, err := svc.PromptSubmit("codex", "B", e.proj)
	must(t, err)
	if !strings.Contains(out, "新指令七") || !strings.Contains(out, "新回复八") || !strings.Contains(out, "claude-code") {
		t.Errorf("missing delta: %s", out)
	}
	if strings.Contains(out, "t6") || strings.Contains(out, "B自己") {
		t.Errorf("stale or own events: %s", out)
	}
}

func TestLIVE02_UnconfirmedDigestRetried(t *testing.T) {
	e := newEnv(t)
	s := e.store()
	seedSession(t, s, "claude-code:A", "claude-code", e.proj, "待投递内容")
	svc := liveSvc(s)
	first, err := svc.PromptSubmit("codex", "B", e.proj)
	must(t, err)
	if first == "" {
		t.Fatal("no digest")
	}
	// The hook process dies before the agent receives the output: no ack sent.
	again, err := svc.PromptSubmit("codex", "B", e.proj)
	must(t, err)
	if !strings.Contains(again, "待投递内容") {
		t.Error("水位在投递确认前已前移，未送达的摘要丢失，也没有声明至多一次语义")
	}
	svc.Ack("codex", "B")
	if after, err := svc.PromptSubmit("codex", "B", e.proj); err != nil || after != "" {
		t.Errorf("确认后水位未前移: %q %v", after, err)
	}
}

func TestLIVE03_Budget(t *testing.T) {
	var groups []live.Group
	for i := 0; i < 40; i++ {
		ev := model.TextEvent(model.RoleUser, strings.Repeat("内容", 90), time.Unix(1, 0))
		groups = append(groups, live.Group{SessionID: fmt.Sprintf("s%d", i), Agent: "a", Events: []model.Event{ev}})
	}
	t.Run("within_budget_with_hint", func(t *testing.T) {
		out := live.Summarize(groups, 800)
		if n := model.EstimateTokens(out); n > 800 {
			t.Errorf("tokens=%d", n)
		}
		if !strings.Contains(out, live.Footer) {
			t.Error("no omission hint")
		}
	})
	t.Run("no_mcp_promise_when_unavailable", func(t *testing.T) {
		e := newEnv(t)
		s := e.store()
		for i := 0; i < 40; i++ {
			seedSession(t, s, fmt.Sprintf("claude-code:S%d", i), "claude-code", e.proj, strings.Repeat("内容", 90))
		}
		svc := liveSvc(s)
		svc.HasMCP = func(string) bool { return false }
		out, err := svc.PromptSubmit("codex", "B", e.proj)
		must(t, err)
		if out == "" || strings.Contains(out, "MCP") {
			t.Errorf("无 MCP 时仍有查询承诺: %s", out)
		}
	})
}

func TestLIVE04_DaemonDownReturnsFast(t *testing.T) {
	e := newEnv(t)
	var worst time.Duration
	for i := 0; i < 5; i++ {
		cmd := exec.Command(binPath, "hook", "prompt_submit", "--agent", "codex")
		cmd.Env = append(os.Environ(), "HOME="+e.home)
		cmd.Stdin = strings.NewReader(`{"session_id":"x","cwd":"/tmp"}`)
		start := time.Now()
		out, err := cmd.Output()
		d := time.Since(start)
		if d > worst {
			worst = d
		}
		if err != nil || len(out) != 0 {
			t.Errorf("err=%v out=%q", err, out)
		}
	}
	if worst > 50*time.Millisecond {
		t.Errorf("worst %v", worst)
	}
}

func TestLIVE05_MCPScope(t *testing.T) {
	e := newEnv(t)
	other := filepath.Join(e.home, "other")
	must(t, os.MkdirAll(filepath.Join(other, ".git"), 0o755))
	s := e.store()
	seedSession(t, s, "claude-code:A", "claude-code", e.proj, "e1", "e2", "e3")
	seedSession(t, s, "claude-code:O", "claude-code", other, "其他项目机密")
	h := &mcpserver.Handlers{Store: s, Config: cfg(t, e.proj), Project: project.Identify(e.proj)}
	t.Run("pagination", func(t *testing.T) {
		o, err := h.SessionEvents(mcpserver.SessionEventsIn{SessionID: "claude-code:A", After: 1, Limit: 1})
		must(t, err)
		if len(o.Events) != 1 || o.Events[0].Text != "e2" {
			t.Errorf("%+v", o.Events)
		}
	})
	t.Run("other_project_denied", func(t *testing.T) {
		if _, err := h.SessionEvents(mcpserver.SessionEventsIn{SessionID: "claude-code:O"}); err == nil {
			t.Error("cross-project read allowed")
		}
		l, _ := h.SessionList(mcpserver.SessionListIn{})
		for _, x := range l.Sessions {
			if x.ID == "claude-code:O" {
				t.Error("cross-project listing")
			}
		}
	})
	t.Run("time_range", func(t *testing.T) {
		t.Skip("用户决定：本次不实现按时间区间查询")
	})
}

type stubExtractor struct {
	reply  string
	err    error
	prompt string
	calls  int
}

func (x *stubExtractor) Complete(_ context.Context, p string) (string, error) {
	x.prompt, x.calls = p, x.calls+1
	return x.reply, x.err
}

func memSession(t *testing.T, e *env, s *store.Store) string {
	t.Helper()
	p := e.writeClaude(e.proj, srcID, msg{"user", "以后测试都用 pytest"}, msg{"assistant", []any{toolUse("t1", "Bash", map[string]any{"command": "TOOLINPUT"})}},
		msg{"user", []any{toolResult("t1", "TOOLOUTPUT", false)}}, msg{"assistant", []any{text("好的")}})
	e.ingestOnce(s, p)
	return "claude-code:" + srcID
}

const goodReply = `{"add":[{"scope":"global","category":"coding_style","content":"测试用 pytest","confidence":0.9,"source_events":[1]}],"update":[]}`

func TestMEM01_ExtractsOnlyConversation(t *testing.T) {
	e := newEnv(t)
	s := e.store()
	id := memSession(t, e, s)
	x := &stubExtractor{reply: goodReply}
	eng := memory.Engine{Store: s, Extractor: x, Config: cfg(t, e.proj)}
	n, err := eng.ExtractSession(context.Background(), id)
	must(t, err)
	if n != 1 {
		t.Fatalf("n=%d", n)
	}
	if strings.Contains(x.prompt, "TOOLOUTPUT") || strings.Contains(x.prompt, "TOOLINPUT") {
		t.Error("tool data sent to the model")
	}
	ms, _ := s.ListMemories(store.MemoryFilter{Status: model.MemoryActive})
	if len(ms) != 1 || ms[0].Scope != "global" || ms[0].Confidence != 0.9 || len(ms[0].SourceEvents) != 1 {
		t.Errorf("%+v", ms)
	}
}

func TestMEM02_FailureWritesNothing(t *testing.T) {
	for name, x := range map[string]*stubExtractor{
		"timeout": {err: context.DeadlineExceeded}, "malformed": {reply: "not json"}, "exit": {err: errors.New("exit 1")},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			s := e.store()
			id := memSession(t, e, s)
			eng := memory.Engine{Store: s, Extractor: x, Config: cfg(t, e.proj)}
			if _, err := eng.ExtractSession(context.Background(), id); err == nil {
				t.Error("no error")
			}
			ms, _ := s.ListMemories(store.MemoryFilter{})
			last, _ := s.ExtractedUpTo(id)
			if len(ms) != 0 || last != 0 {
				t.Errorf("memories=%d last=%d", len(ms), last)
			}
		})
	}
}

func TestMEM03_Supersede(t *testing.T) {
	e := newEnv(t)
	s := e.store()
	id := memSession(t, e, s)
	c := cfg(t, e.proj)
	old, err := memory.AddManual(s, "global", "", "coding_style", "测试用 unittest")
	must(t, err)
	unrelated, err := memory.AddManual(s, "global", "", "tools", "使用 uv 管理依赖")
	must(t, err)
	reply := fmt.Sprintf(`{"add":[],"update":[{"supersedes":%q,"scope":"global","category":"coding_style","content":"测试用 pytest","confidence":0.9,"source_events":[1]}]}`, old.ID)
	eng := memory.Engine{Store: s, Extractor: &stubExtractor{reply: reply}, Config: c}
	_, err = eng.ExtractSession(context.Background(), id)
	must(t, err)
	o, _ := s.GetMemory(old.ID)
	u, _ := s.GetMemory(unrelated.ID)
	if o.Status != model.MemorySuperseded || u.Status != model.MemoryActive {
		t.Errorf("old=%s unrelated=%s", o.Status, u.Status)
	}
	active, _ := s.ListMemories(store.MemoryFilter{Status: model.MemoryActive})
	if len(active) != 2 {
		t.Errorf("active=%d", len(active))
	}
}

func TestMEM04_PendingReview(t *testing.T) {
	e := newEnv(t)
	s := e.store()
	id := memSession(t, e, s)
	reply := `{"add":[{"scope":"project","category":"decision","content":"低置信度决定","confidence":0.3,"source_events":[1]}],"update":[]}`
	eng := memory.Engine{Store: s, Extractor: &stubExtractor{reply: reply}, Config: cfg(t, e.proj)}
	_, err := eng.ExtractSession(context.Background(), id)
	must(t, err)
	svc := liveSvc(s)
	out, _ := svc.SessionStart("codex", "N1", e.proj)
	if strings.Contains(out, "低置信度决定") {
		t.Error("pending memory injected")
	}
	ms, _ := s.ListMemories(store.MemoryFilter{Status: model.MemoryPending})
	if len(ms) != 1 {
		t.Fatalf("pending=%d", len(ms))
	}
	must(t, s.SetMemoryStatus(ms[0].ID, model.MemoryActive))
	if out, _ := svc.SessionStart("codex", "N2", e.proj); !strings.Contains(out, "低置信度决定") {
		t.Errorf("approved memory not injected: %s", out)
	}
}

func TestMEM05_InjectionScope(t *testing.T) {
	e := newEnv(t)
	other := filepath.Join(e.home, "other")
	must(t, os.MkdirAll(filepath.Join(other, ".git"), 0o755))
	s := e.store()
	_, err := memory.AddManual(s, "project", projID(e.proj), "decision", "本项目记忆")
	must(t, err)
	must(t, s.UpsertProject(project.Identify(other)))
	_, err = memory.AddManual(s, "project", projID(other), "decision", "别的项目记忆")
	must(t, err)
	out, err := liveSvc(s).SessionStart("codex", "N", e.proj)
	must(t, err)
	if !strings.Contains(out, "本项目记忆") || strings.Contains(out, "别的项目记忆") {
		t.Errorf("%s", out)
	}
}

func TestMEM06_Forget(t *testing.T) {
	e := newEnv(t)
	e.base()
	write(t, filepath.Join(e.home, ".claude/CLAUDE.md"), "hand\n")
	s := e.store()
	content := "FORGETSENTINEL 使用 tabs"
	m, err := memory.AddManual(s, "global", "", "coding_style", content)
	must(t, err)
	c := cfg(t, e.proj)
	_, err = memory.RenderBlocks(context.Background(), s, e.reg(), c)
	must(t, err)
	must(t, s.Forget(m.ID))
	_, err = memory.RenderBlocks(context.Background(), s, e.reg(), c)
	must(t, err)
	if strings.Contains(read(t, filepath.Join(e.home, ".claude/CLAUDE.md")), "FORGETSENTINEL") {
		t.Error("still in managed block")
	}
	if r, _ := s.SearchMemories("FORGETSENTINEL", "", 5); len(r) != 0 {
		t.Error("still searchable")
	}
	s2 := memory.Proposal{Scope: "global", Category: "coding_style", Content: content, Confidence: 0.9}
	eng := memory.Engine{Store: s, Config: c}
	if ok, err := eng.Apply(model.Session{ID: "x"}, s2); err != nil || ok {
		t.Errorf("re-created: %v %v", ok, err)
	}
	s.Close()
	if h := scanData(t, "FORGETSENTINEL"); len(h) > 0 {
		t.Errorf("forget 后原文仍在 %v 中（抑制信息不应暴露原文）", h)
	}
}

func TestMEM07_ManagedBlock(t *testing.T) {
	doc := "前面\n\n后面 手写\n"
	once := memory.UpsertBlock(doc, memory.BlockGlobal, []string{"- a"})
	twice := memory.UpsertBlock(once, memory.BlockGlobal, []string{"- b"})
	if strings.Count(twice, "bridge:begin") != 1 || strings.Contains(twice, "- a") || !strings.HasPrefix(twice, "前面\n\n后面 手写\n") {
		t.Errorf("%q", twice)
	}
	t.Run("corrupt_marker_stops", func(t *testing.T) {
		broken := "前面\n<!-- bridge:begin global-preferences x -->\n- a\n后面\n"
		got := memory.UpsertBlock(broken, memory.BlockGlobal, []string{"- b"})
		if strings.Count(got, "bridge:begin") > 1 || got != broken {
			t.Errorf("标记损坏时未停止并报告: %q", got)
		}
	})
}

func script(t *testing.T, e *env, body string) string {
	t.Helper()
	p := filepath.Join(e.home, "adapter.py")
	write(t, p, "#!/usr/bin/env python3\nimport sys,json,time\n"+body)
	must(t, os.Chmod(p, 0o755))
	return p
}

func TestEXT01_ProtocolNegotiation(t *testing.T) {
	const loop = `
for l in sys.stdin:
    r=json.loads(l)
    print(json.dumps({"jsonrpc":"2.0","id":r["id"],"result":{"protocol_version":"%s","capabilities":{"session_read":True,"context_window":5}}}),flush=True)
`
	e := newEnv(t)
	good := external.New("g", script(t, e, fmt.Sprintf(loop, kit.ProtocolVersion)))
	defer good.Close()
	if !good.Capabilities("1").SessionRead {
		t.Error("compatible adapter rejected")
	}
	bad := external.New("b", script(t, e, fmt.Sprintf(loop, "99")))
	defer bad.Close()
	if bad.Capabilities("1").SessionRead {
		t.Error("incompatible protocol accepted")
	}
}

func TestEXT02_MisbehavingAdapters(t *testing.T) {
	run := func(name, body string, wantErr bool) {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			c := external.New("x", script(t, e, body))
			defer c.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if _, err := c.ListSessions(ctx, kit.Filter{}); (err != nil) != wantErr {
				t.Errorf("err=%v", err)
			}
		})
	}
	run("timeout", "time.sleep(60)\n", true)
	run("exit", "sys.exit(3)\n", true)
	run("malformed", "for l in sys.stdin:\n    print('garbage',flush=True)\n", true)
	run("oversized", `
for l in sys.stdin:
    r=json.loads(l)
    print(json.dumps({"jsonrpc":"2.0","id":r["id"],"result":{"sessions":[],"pad":"x"*(64*1024*1024)}}),flush=True)
`, true)
}

func TestEXT03_Testkit(t *testing.T) {
	e := newEnv(t)
	e.base()
	failed := func(cs []kit.Check) (out []string) {
		for _, c := range cs {
			if !c.Passed {
				out = append(out, c.Name+": "+c.Detail)
			}
		}
		return
	}
	for _, a := range e.reg().All() {
		if f := failed(kit.Conformance(context.Background(), a, kit.ConformanceOptions{Write: true, CWD: e.proj})); len(f) > 0 {
			t.Errorf("%s: %v", a.ID(), f)
		}
	}
	liar := &fakeAdapter{id: "liar", caps: kit.Capabilities{LiveTail: true}}
	if len(failed(kit.Conformance(context.Background(), liar, kit.ConformanceOptions{}))) == 0 {
		t.Error("inconsistent capabilities passed conformance")
	}
}

func TestDRIFT01_DriftAndAdopt(t *testing.T) {
	e := newEnv(t)
	e.base()
	g := filepath.Join(e.home, ".config/bridge/AGENTS.md")
	target := filepath.Join(e.home, ".claude/CLAUDE.md")
	write(t, g, "base\n")
	must(t, e.run("", "sync").err)
	write(t, target, read(t, target)+"replica edit\n")
	write(t, g, "base\nsource edit\n")
	r := e.run("", "sync")
	if !strings.Contains(r.out, "drift") || !strings.Contains(read(t, target), "replica edit") {
		t.Errorf("drift: %s", r.out)
	}
	r2 := e.run("", "sync", "--adopt", "claude-code")
	merged := read(t, g)
	if !strings.Contains(merged, "source edit") || !strings.Contains(read(t, target), "replica edit") {
		t.Errorf("adopt 冲突时未保留双方，源: %q", merged)
	}
	if !strings.Contains(r2.out, "手工合并") {
		t.Errorf("未请求用户处理: %s", r2.out)
	}
}

var (
	_ = filesync.Noop
	_ = config.GlobalPath
)

func TestCodexHookTrustDetection(t *testing.T) {
	e := newEnv(t)
	a := codex.New()
	path := filepath.Join(e.home, ".codex/hooks.json")
	bin := filepath.Join(e.home, "bin", "codex")
	t.Setenv("PATH", filepath.Dir(bin)+":"+os.Getenv("PATH"))
	makeResponse := func(status string, enabled bool, count int) {
		var hooks []map[string]any
		for _, event := range []string{"sessionStart", "userPromptSubmit", "preToolUse", "postToolUse", "stop"}[:count] {
			hooks = append(hooks, map[string]any{"eventName": event, "sourcePath": path, "command": "/x/bridge hook stop --agent codex", "enabled": enabled, "trustStatus": status})
		}
		response, _ := json.Marshal(map[string]any{"id": 2, "result": map[string]any{"data": []any{map[string]any{"hooks": hooks}}}})
		write(t, bin, "#!/bin/sh\ncat <<'EOF'\n"+string(response)+"\nEOF\n")
		must(t, os.Chmod(bin, 0o755))
	}
	for _, tc := range []struct {
		status  string
		enabled bool
		count   int
		want    bool
	}{
		{"trusted", true, 5, true}, {"modified", true, 5, false}, {"untrusted", true, 5, false}, {"trusted", false, 5, false}, {"trusted", true, 4, false},
	} {
		makeResponse(tc.status, tc.enabled, tc.count)
		if got := a.HooksTrusted(); got != tc.want {
			t.Errorf("%+v: got %v", tc, got)
		}
	}
}

func TestVersionGate(t *testing.T) {
	setup := func(t *testing.T) *env {
		e := newEnv(t)
		e.writeClaude(e.proj, srcID, msg{"user", "q"}, msg{"assistant", []any{text("a")}})
		e.writeCodex(e.proj, codexSeed, "9.9.9", cxMsg("user", "seed"))
		return e
	}
	api := func(t *testing.T, e *env, args ...string) string {
		t.Helper()
		r := e.run("", append([]string{"api"}, args...)...)
		must(t, r.err)
		return r.out
	}
	t.Run("approve_enables_only_that_version_and_revoke_restores", func(t *testing.T) {
		e := setup(t)
		api(t, e, "approve-version", "codex")
		if r := e.run("", "handoff", srcID, "--to", "codex"); r.err != nil || len(e.newRollouts()) != 1 {
			t.Fatalf("approved write failed: %s", r.out)
		}
		api(t, e, "revoke-version", "codex")
		e.run("", "handoff", srcID, "--to", "codex")
		if n := len(e.newRollouts()); n != 1 {
			t.Errorf("wrote after revoke: %d", n)
		}
	})
	t.Run("failed_probe_keeps_writing_off", func(t *testing.T) {
		e := setup(t)
		if out := api(t, e, "verify-version", "codex"); !strings.Contains(out, `"state":"failed"`) {
			t.Fatalf("no client of version 9.9.9 exists, yet: %s", out)
		}
		e.run("", "handoff", srcID, "--to", "codex")
		if n := len(e.newRollouts()); n != 0 {
			t.Errorf("wrote to a failed version: %d", n)
		}
	})
	t.Run("approval_does_not_carry_to_other_versions", func(t *testing.T) {
		e := setup(t)
		api(t, e, "approve-version", "codex", "9.9.8")
		e.run("", "handoff", srcID, "--to", "codex")
		if n := len(e.newRollouts()); n != 0 {
			t.Errorf("approval leaked across versions: %d", n)
		}
	})
}

func TestPauseStopsInjection(t *testing.T) {
	e := newEnv(t)
	s := e.store()
	seedSession(t, s, "claude-code:A", "claude-code", e.proj, "暂停测试内容")
	stop := startDaemon(t, e)
	defer stop()
	hook := func() string {
		payload, _ := json.Marshal(map[string]string{"session_id": "B", "cwd": e.proj})
		return e.run(string(payload), "hook", "prompt_submit", "--agent", "codex").out
	}
	must(t, e.run("", "api", "pause").err)
	if st := e.run("", "api", "status").out; !strings.Contains(st, `"paused":true`) {
		t.Errorf("status: %s", st)
	}
	if out := hook(); out != "" {
		t.Errorf("injected while paused: %s", out)
	}
	must(t, e.run("", "api", "resume").err)
	if out := hook(); !strings.Contains(out, "暂停测试内容") {
		t.Errorf("not injected after resume: %q", out)
	}
}

func TestSessionUsesLatestWorkingDirectory(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
		e := newEnv(t)
		first := filepath.Join(e.home, "first")
		must(t, os.MkdirAll(first, 0o755))
		p := e.writeClaude(first, srcID, msg{"user", "q"}, msg{"assistant", []any{text("a")}})
		appendFile(t, p, claudeLines(e.proj, srcID, msg{"user", "moved"}))
		refs, err := claudecode.New().ListSessions(context.Background(), kit.Filter{})
		must(t, err)
		if len(refs) != 1 || refs[0].CWD != e.proj {
			t.Errorf("%+v", refs)
		}
	})
	t.Run("codex", func(t *testing.T) {
		e := newEnv(t)
		first := filepath.Join(e.home, "first")
		p := e.writeCodex(first, codexSeed, codexVer, cxMsg("user", "q"))
		appendFile(t, p, codexLine("turn_context", map[string]any{"model": "gpt-x", "cwd": e.proj}))
		refs, err := codex.New().ListSessions(context.Background(), kit.Filter{})
		must(t, err)
		if len(refs) != 1 || refs[0].CWD != e.proj {
			t.Errorf("%+v", refs)
		}
	})
	t.Run("already_indexed_session_is_corrected", func(t *testing.T) {
		e := newEnv(t)
		first := filepath.Join(e.home, "first")
		must(t, os.MkdirAll(first, 0o755))
		e.writeClaude(e.proj, srcID, msg{"user", "q"})
		s := e.store()
		must(t, e.ingest(s))
		x, err := s.GetSession("claude-code:" + srcID)
		must(t, err)
		x.CWD = first
		must(t, s.UpsertSession(x))
		must(t, e.ingest(s))
		if x, _ = s.GetSession("claude-code:" + srcID); x.CWD != e.proj {
			t.Errorf("cwd=%s", x.CWD)
		}
	})
}

func TestAPIWarnsOnRepeatedMigration(t *testing.T) {
	e := newEnv(t)
	e.base()
	e.run("", "sessions")
	r := e.run("", "api", "handoff", srcID, "--to", "codex")
	if !strings.Contains(r.out, `"mode":"native"`) {
		t.Fatal(r.out)
	}
	r = e.run("", "api", "handoff", srcID, "--to", "codex")
	if !strings.Contains(r.out, `"mode":"duplicate"`) || len(e.newRollouts()) != 1 {
		t.Errorf("not detected: %s", r.out)
	}
	r = e.run("", "api", "handoff", srcID, "--to", "codex", "--again")
	if !strings.Contains(r.out, `"mode":"native"`) || len(e.newRollouts()) != 2 {
		t.Errorf("--again: %s", r.out)
	}
}

func TestMigratedHistoryIsNotProgress(t *testing.T) {
	e := newEnv(t)
	e.base()
	e.run("", "sessions")
	r := e.run("", "api", "handoff", srcID, "--to", "codex")
	if !strings.Contains(r.out, `"mode":"native"`) {
		t.Fatal(r.out)
	}
	s := e.store()
	svc := liveSvc(s)
	out, err := svc.PromptSubmit("claude-code", "reader", e.proj)
	must(t, err)
	if strings.Contains(out, "q1") || strings.Contains(out, "a3") {
		t.Errorf("copied history injected as progress: %s", out)
	}
	list, _ := s.ListSessions(store.SessionFilter{Agent: "codex"})
	var copyID string
	for _, x := range list {
		if x.Parent != "" {
			copyID = x.ID
		}
	}
	if copyID == "" {
		t.Fatal("copy not indexed")
	}
	max, _ := s.MaxSeq(copyID)
	appendEvents(t, s, copyID, "codex", max+1, "迁移后的真实新进展")
	if out, err = svc.PromptSubmit("claude-code", "reader", e.proj); err != nil || !strings.Contains(out, "迁移后的真实新进展") {
		t.Errorf("real progress lost: %q %v", out, err)
	}
}

func TestPausedSessionListDoesNotIngest(t *testing.T) {
	e := newEnv(t)
	e.writeClaude(e.proj, srcID, msg{"user", "pause marker"})
	must(t, e.run("", "api", "pause").err)
	r := e.run("", "api", "sessions")
	if strings.Contains(r.out, srcID) {
		t.Fatalf("session collected while paused: %s", r.out)
	}
	must(t, e.run("", "api", "resume").err)
	r = e.run("", "api", "sessions")
	if !strings.Contains(r.out, srcID) {
		t.Fatalf("session missing after resume: %s", r.out)
	}
}

func TestDuplicateResumeCommandMatchesTarget(t *testing.T) {
	e := newEnv(t)
	e.base()
	e.run("", "api", "sessions")
	r := e.run("", "api", "handoff", codexSeed, "--to", "claude-code")
	if !strings.Contains(r.out, `"mode":"native"`) {
		t.Fatal(r.out)
	}
	r = e.run("", "api", "handoff", codexSeed, "--to", "claude-code")
	if !strings.Contains(r.out, `"mode":"duplicate"`) || !strings.Contains(r.out, "claude --resume ") || strings.Contains(r.out, "codex resume ") {
		t.Fatal(r.out)
	}
}

func TestDesktopMemoryEditAndSettings(t *testing.T) {
	e := newEnv(t)
	call := func(args ...string) map[string]any {
		t.Helper()
		r := e.run("", append([]string{"api"}, args...)...)
		must(t, r.err)
		var v map[string]any
		must(t, json.Unmarshal([]byte(r.out), &v))
		return v
	}
	add := call("memory-add", "original", "--scope", "global")
	id := add["data"].(map[string]any)["id"].(string)
	if out := call("memory-edit", id, "prefer testing with secret-value"); out["ok"] != true {
		t.Fatal(out)
	}
	if out := call("memory-edit", id, " "); out["ok"] != false {
		t.Fatal("empty edit accepted", out)
	}
	for key, value := range map[string]string{
		"sync.global_memory": "false", "sync.project_memory": "false",
		"privacy.include_projects": `["~/work/**"]`, "sync.exclude_files": `["**/*.pem"]`,
		"sync.skills.exclude": `["secret-*"]`, "privacy.redact": `["secret-[a-z]+"]`,
	} {
		if out := call("config-set", "--key", key, "--value", value); out["ok"] != true {
			t.Fatalf("%s: %v", key, out)
		}
	}
	if out := call("memory-edit", id, "prefer secret-value"); out["ok"] != true {
		t.Fatal(out)
	}
	mem := call("memories")
	ms := mem["data"].(map[string]any)["memories"].([]any)
	content := ms[0].(map[string]any)["content"].(string)
	if strings.Contains(content, "secret-value") || !strings.Contains(content, "[REDACTED:custom]") {
		t.Fatalf("edit not redacted: %q", content)
	}
	out := call("config")
	cfg := out["data"].(map[string]any)
	if cfg["global_memory"] != false || cfg["project_memory"] != false {
		t.Fatal(cfg)
	}
	for _, key := range []string{"include_projects", "exclude_files", "exclude_skills", "redact"} {
		if len(cfg[key].([]any)) != 1 {
			t.Fatalf("%s: %v", key, cfg[key])
		}
	}
	if out := call("config-set", "--key", "privacy.redact", "--value", `["["]`); out["ok"] != false {
		t.Fatal("invalid regex accepted", out)
	}
	if out := call("config"); len(out["data"].(map[string]any)["redact"].([]any)) != 1 {
		t.Fatal("invalid setting changed config", out)
	}
}

func TestFileClaimsHookFlow(t *testing.T) {
	e := newEnv(t)
	stop := startDaemon(t, e)
	defer stop()
	invoke := func(agent, session, event, tool string, input any) result {
		body, err := json.Marshal(map[string]any{"session_id": session, "cwd": e.proj, "tool_name": tool, "tool_input": input})
		must(t, err)
		return e.run(string(body), "hook", event, "--agent", agent)
	}
	target := filepath.Join(e.proj, "file.go")
	a := invoke("claude-code", "one", "pre_tool", "Edit", map[string]any{"file_path": target})
	if a.out != "" {
		t.Fatal(a.out)
	}
	b := invoke("codex", "two", "pre_tool", "apply_patch", map[string]any{"command": "*** Begin Patch\n*** Update File: file.go\n*** End Patch"})
	if !strings.Contains(b.out, "PreToolUse") || !strings.Contains(b.out, "claude-code") || !strings.Contains(b.out, "file.go") {
		t.Fatal("missing conflict warning", b.out)
	}
	claims := e.run("", "api", "claims")
	if !strings.Contains(claims.out, "file.go") || !strings.Contains(claims.out, "one") {
		t.Fatal(claims.out)
	}
	invoke("claude-code", "one", "stop", "", nil)
	b = invoke("codex", "two", "pre_tool", "apply_patch", map[string]any{"command": "*** Update File: file.go"})
	if b.out != "" {
		t.Fatal("claim not released", b.out)
	}
}

func TestHandoffRecordsExactConversationBoundary(t *testing.T) {
	e := newEnv(t)
	e.base()
	e.run("", "api", "sessions")
	r := e.run("", "api", "handoff", srcID, "--to", "codex")
	if !strings.Contains(r.out, `"mode":"native"`) {
		t.Fatal(r.out)
	}
	st := e.store()
	links, err := st.ConversationLinksFor("claude-code:" + srcID)
	must(t, err)
	if len(links) != 1 {
		t.Fatalf("links: %+v", links)
	}
	srcMax, err := st.MaxSeq(links[0].SourceID)
	must(t, err)
	dstMax, err := st.MaxSeq(links[0].TargetID)
	must(t, err)
	if links[0].SourceSeq != srcMax || links[0].TargetSeq != dstMax || srcMax == 0 || dstMax == 0 {
		t.Fatalf("link %+v, maxima %d %d", links[0], srcMax, dstMax)
	}
	if err := st.CreateConversationLink(links[0].SourceID, links[0].TargetID, srcMax, dstMax); err == nil {
		t.Fatal("duplicate link overwrote baseline")
	}
}

func linkedPair(t *testing.T) (*env, string, string) {
	t.Helper()
	e := newEnv(t)
	src := e.base()
	e.run("", "sessions")
	if r := e.run("", "api", "handoff", srcID, "--to", "codex"); !strings.Contains(r.out, `"mode":"native"`) {
		t.Fatal(r.out)
	}
	return e, src, e.newRollouts()[0]
}

func age(t *testing.T, p string) {
	t.Helper()
	old := time.Now().Add(-10 * time.Minute)
	must(t, os.Chtimes(p, old, old))
}

func addCodexTurn(t *testing.T, p, msg string) {
	t.Helper()
	appendFile(t, p, codexLine("response_item", cxMsg("user", msg))+codexLine("response_item", cxMsg("assistant", msg+" 回复")))
}

func syncAPI(e *env) string { return e.run("", "api", "sync-session", srcID).out }

func TestSessionSyncBothDirections(t *testing.T) {
	t.Run("copy_ahead_updates_source_in_place", func(t *testing.T) {
		e, src, cp := linkedPair(t)
		addCodexTurn(t, cp, "在 Codex 里的新工作")
		out := syncAPI(e)
		if !strings.Contains(out, `"state":"synced"`) {
			t.Fatal(out)
		}
		got := strings.Join(texts(readClaude(t, src), ""), "|")
		if !strings.Contains(got, "在 Codex 里的新工作") || !strings.Contains(got, "q1") {
			t.Errorf("source: %s", got)
		}
		if out := syncAPI(e); !strings.Contains(out, `"state":"current"`) {
			t.Errorf("second pass should be a no-op: %s", out)
		}
		n := 0
		for _, x := range texts(readClaude(t, src), model.RoleUser) {
			if x == "在 Codex 里的新工作" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("appended %d times", n)
		}
	})
	t.Run("source_ahead_updates_copy", func(t *testing.T) {
		e, src, cp := linkedPair(t)
		appendFile(t, src, claudeLines(e.proj, srcID, msg{"user", "Claude 的后续"}))
		age(t, cp)
		if out := syncAPI(e); !strings.Contains(out, `"state":"synced"`) {
			t.Fatal(out)
		}
		if got := strings.Join(texts(readRollout(t, cp), ""), "|"); !strings.Contains(got, "Claude 的后续") {
			t.Errorf("copy: %s", got)
		}
	})
	t.Run("diverged_latest_wins_and_nothing_is_lost", func(t *testing.T) {
		e, src, cp := linkedPair(t)
		appendFile(t, src, claudeLines(e.proj, srcID, msg{"user", "旧侧内容"}))
		old := time.Now().Add(-time.Hour)
		must(t, os.Chtimes(src, old, old))
		addCodexTurn(t, cp, "新侧内容")
		if out := syncAPI(e); !strings.Contains(out, "已以 codex 为准") {
			t.Fatal(out)
		}
		got := strings.Join(texts(readClaude(t, src), ""), "|")
		if !strings.Contains(got, "旧侧内容") || !strings.Contains(got, "新侧内容") {
			t.Errorf("source: %s", got)
		}
	})
	t.Run("diverged_user_can_pick_the_older_side", func(t *testing.T) {
		e, src, cp := linkedPair(t)
		addCodexTurn(t, cp, "Codex 较早的工作")
		old := time.Now().Add(-time.Hour)
		must(t, os.Chtimes(cp, old, old))
		appendFile(t, src, claudeLines(e.proj, srcID, msg{"user", "Claude 较新的工作"}))
		out := e.run("", "api", "sync-session", srcID, "--prefer", "codex").out
		if !strings.Contains(out, "以 codex 为准") {
			t.Fatal(out)
		}
		if got := strings.Join(texts(readClaude(t, src), ""), "|"); !strings.Contains(got, "Codex 较早的工作") {
			t.Errorf("source: %s", got)
		}
	})
	t.Run("running_target_is_refused", func(t *testing.T) {
		e, src, cp := linkedPair(t)
		addCodexTurn(t, cp, "不该写入")
		write(t, filepath.Join(e.home, ".claude/sessions/1.json"), fmt.Sprintf(`{"pid":%d,"sessionId":%q}`, os.Getpid(), srcID))
		before := read(t, src)
		if out := syncAPI(e); !strings.Contains(out, "正在使用") {
			t.Fatal(out)
		}
		if read(t, src) != before {
			t.Error("wrote to a running session")
		}
	})
	t.Run("crash_after_append_does_not_duplicate", func(t *testing.T) {
		e, src, cp := linkedPair(t)
		addCodexTurn(t, cp, "崩溃测试")
		cmd := exec.Command(binPath, "api", "sync-session", srcID)
		cmd.Dir, cmd.Env = e.proj, append(os.Environ(), "HOME="+e.home, "BRIDGE_FAILPOINT=sync.after_append")
		if err := cmd.Run(); err == nil {
			t.Fatal("expected the process to die")
		}
		if out := syncAPI(e); !strings.Contains(out, `"state":"current"`) {
			t.Fatalf("a complete append should be rolled forward: %s", out)
		}
		n := 0
		for _, x := range texts(readClaude(t, src), model.RoleUser) {
			if x == "崩溃测试" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("appended %d times", n)
		}
	})
}

func preTool(e *env, agent, session, path string) string {
	in, _ := json.Marshal(map[string]any{"session_id": session, "cwd": e.proj, "tool_name": "Write",
		"tool_input": map[string]string{"file_path": path, "content": "x"}})
	return e.run(string(in), "hook", "pre_tool", "--agent", agent).out
}

func TestLOCK_FileClaims(t *testing.T) {
	file := func(e *env, n string) string { return filepath.Join(e.proj, n) }
	t.Run("LOCK-01_warn_allows_and_block_denies", func(t *testing.T) {
		e := newEnv(t)
		write(t, filepath.Join(e.proj, ".bridge.toml"), "[live]\nconflict_policy = \"warn\"\n")
		stop := startDaemon(t, e)
		defer stop()
		if out := preTool(e, "claude-code", "A", file(e, "a.go")); out != "" {
			t.Fatalf("first claim: %s", out)
		}
		out := preTool(e, "codex", "B", file(e, "a.go"))
		if !strings.Contains(out, "additionalContext") || strings.Contains(out, "permissionDecision") {
			t.Errorf("warn: %s", out)
		}
		write(t, filepath.Join(e.proj, ".bridge.toml"), "[live]\nconflict_policy = \"block\"\n")
		out = preTool(e, "codex", "B", file(e, "a.go"))
		if !strings.Contains(out, `"permissionDecision":"deny"`) {
			t.Errorf("block: %s", out)
		}
		if out := preTool(e, "claude-code", "A", file(e, "a.go")); strings.Contains(out, "deny") {
			t.Errorf("owner denied: %s", out)
		}
	})
	t.Run("LOCK-01_concurrent_claims_have_one_winner", func(t *testing.T) {
		var c live.Claims
		var mu sync.Mutex
		var wg sync.WaitGroup
		winners := 0
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if len(c.TryAcquire("p", "a", fmt.Sprint(i), []string{"/f"}, time.Now())) == 0 {
					mu.Lock()
					winners++
					mu.Unlock()
				}
			}(i)
		}
		wg.Wait()
		if winners != 1 {
			t.Errorf("winners=%d", winners)
		}
	})
	t.Run("LOCK-02_expiry_and_stale_owner", func(t *testing.T) {
		var c live.Claims
		start := time.Now()
		c.TryAcquire("p", "a", "A", []string{"/f"}, start)
		later := start.Add(live.ClaimTTL + time.Second)
		if got := c.TryAcquire("p", "b", "B", []string{"/f"}, later); len(got) != 0 {
			t.Fatalf("expired lease still held: %v", got)
		}
		c.Release("a", "A") // the expired owner must not free the new lease
		if got := c.TryAcquire("p", "c", "C", []string{"/f"}, later); len(got) != 1 || got[0].Session != "B" {
			t.Errorf("stale owner released it: %v", got)
		}
	})
	t.Run("denied_edit_claims_nothing", func(t *testing.T) {
		var c live.Claims
		now := time.Now()
		c.TryAcquire("p", "a", "A", []string{"/taken"}, now)
		if got := c.TryAcquire("p", "b", "B", []string{"/free", "/taken"}, now); len(got) != 1 {
			t.Fatalf("%v", got)
		}
		if got := c.TryAcquire("p", "c", "C", []string{"/free"}, now); len(got) != 0 {
			t.Errorf("denied edit left a claim: %v", got)
		}
	})
	t.Run("survives_daemon_restart", func(t *testing.T) {
		e := newEnv(t)
		write(t, filepath.Join(e.proj, ".bridge.toml"), "[live]\nconflict_policy = \"block\"\n")
		stop := startDaemon(t, e)
		preTool(e, "claude-code", "A", file(e, "a.go"))
		stop()
		stop = startDaemon(t, e)
		defer stop()
		if out := preTool(e, "codex", "B", file(e, "a.go")); !strings.Contains(out, "deny") {
			t.Errorf("claim lost across restart: %q", out)
		}
	})
	t.Run("LOCK-03_shell_is_not_claimed", func(t *testing.T) {
		in, _ := json.Marshal(map[string]string{"command": "echo x > a.go"})
		if got := live.EditPaths("Bash", in, "/p", "/p"); len(got) != 0 {
			t.Errorf("%v", got)
		}
	})
	t.Run("daemon_down_fails_open", func(t *testing.T) {
		e := newEnv(t)
		if out := preTool(e, "codex", "B", file(e, "a.go")); out != "" {
			t.Errorf("%s", out)
		}
	})
}

// Codex desktop threads are paginated: appended turns use Codex's own turn
// structure, and reading them back yields each message once.
func TestSessionSyncIntoPaginatedCodexThread(t *testing.T) {
	e, src, cp := linkedPair(t)
	lines := strings.SplitAfter(read(t, cp), "\n")
	var meta map[string]any
	must(t, json.Unmarshal([]byte(lines[0]), &meta))
	meta["payload"].(map[string]any)["history_mode"] = "paginated"
	b, _ := json.Marshal(meta)
	lines[0] = string(b) + "\n"
	write(t, cp, strings.Join(lines, ""))
	age(t, cp)
	appendFile(t, src, claudeLines(e.proj, srcID, msg{"user", "分页后续问题"}, msg{"assistant", []any{text("分页后续回答")}}))
	if out := syncAPI(e); !strings.Contains(out, `"state":"synced"`) {
		t.Fatal(out)
	}
	tail := read(t, cp)[len(strings.Join(lines, "")):]
	for _, want := range []string{`"task_started"`, `"UserMessage"`, `"AgentMessage"`, `"task_complete"`, `"ordinal"`} {
		if !strings.Contains(tail, want) {
			t.Errorf("appended turn lacks %s:\n%s", want, tail)
		}
	}
	if strings.Contains(tail, `"user_message"`) {
		t.Error("legacy events written into a paginated thread")
	}
	n := 0
	for _, x := range texts(readRollout(t, cp), "") {
		if x == "分页后续问题" || x == "分页后续回答" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("read back %d of the 2 appended messages", n)
	}
	if out := syncAPI(e); !strings.Contains(out, `"state":"current"`) {
		t.Errorf("second pass: %s", out)
	}
}
