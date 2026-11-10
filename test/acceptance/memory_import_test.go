package acceptance

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bridge/internal/memory"
	"bridge/internal/model"
	"bridge/internal/store"
)

// fakeExtractor puts a `claude` on PATH that logs each prompt and answers
// with one project memory, so no real model is ever called.
func fakeExtractor(t *testing.T, e *env, sleep string) string {
	t.Helper()
	bin := filepath.Join(e.home, "fakebin")
	log := filepath.Join(e.home, "extract.log")
	write(t, filepath.Join(bin, "claude"), `#!/bin/sh
if [ "$1" = "--version" ]; then echo "2.1.309 (Claude Code)"; exit 0; fi
cat >> "`+log+`"
echo "=====" >> "`+log+`"
sleep `+sleep+`
echo '{"add":[{"scope":"project","category":"decision","content":"测试统一用 pytest","confidence":0.95,"source_events":[1]}],"update":[]}'
`)
	must(t, os.Chmod(filepath.Join(bin, "claude"), 0o755))
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	return log
}

func importStatus(t *testing.T, e *env) memory.ImportStatus {
	t.Helper()
	r := e.run("", "api", "memory-import-status")
	var env struct {
		Data memory.ImportStatus `json:"data"`
	}
	must(t, json.Unmarshal([]byte(strings.TrimSpace(r.out)), &env))
	return env.Data
}

func TestMEM08_HistoryImport(t *testing.T) {
	e := newEnv(t)
	log := fakeExtractor(t, e, "0")
	e.writeClaude(e.proj, srcID, msg{"user", "以后测试都用 pytest"}, msg{"assistant", []any{toolUse("t1", "Bash", map[string]any{"command": "TOOLINPUT"})}},
		msg{"user", []any{toolResult("t1", "TOOLOUTPUT", false)}}, msg{"assistant", []any{text("好的，记住了")}})
	other := filepath.Join(e.home, "secret")
	must(t, os.MkdirAll(filepath.Join(other, ".git"), 0o755))
	e.writeClaude(other, "22222222-2222-4222-8222-222222222222", msg{"user", "私密项目的内容"}, msg{"assistant", []any{text("收到")}})
	write(t, filepath.Join(e.home, ".config/bridge/config.toml"), "version = 1\n[privacy]\nexclude_projects = [\""+other+"\"]\n")

	// The estimate sends nothing.
	r := e.run("", "memory", "import", "--all", "--dry-run")
	// The excluded project is never even collected.
	if r.err != nil || !strings.Contains(r.out, "将处理 1 个会话") {
		t.Fatalf("plan: %s", r.out)
	}
	if _, err := os.Stat(log); err == nil {
		t.Fatal("dry run called the extractor")
	}

	// A migrated copy is not extracted again.
	s := e.store()
	now := time.Now().UTC()
	must(t, s.UpsertSession(model.Session{ID: "codex:copy", ProjectID: projID(e.proj), Agent: "codex", NativeID: "copy", CWD: e.proj, StartedAt: now, EndedAt: now, Parent: "claude-code:" + srcID}))
	appendEvents(t, s, "codex:copy", "codex", 1, "以后测试都用 pytest", "好的")

	r = e.run("", "memory", "import", "--yes")
	if r.err != nil || !strings.Contains(r.out, "新增 1 条记忆") {
		t.Fatalf("import: %s", r.out)
	}
	sent := read(t, log)
	if strings.Count(sent, "=====") != 1 || strings.Contains(sent, "TOOLOUTPUT") || strings.Contains(sent, "私密项目") {
		t.Errorf("sent: %s", sent)
	}
	ms, err := s.ListMemories(store.MemoryFilter{})
	must(t, err)
	if len(ms) != 1 || ms[0].Status != model.MemoryPending {
		t.Errorf("imported memories must wait for review: %+v", ms)
	}
	if st := importStatus(t, e); st.State != memory.ImportDone || st.Done != 1 || st.Added != 1 {
		t.Errorf("status: %+v", st)
	}

	// Nothing is sent twice.
	r = e.run("", "memory", "import", "--yes")
	if !strings.Contains(r.out, "将处理 0 个会话") || strings.Count(read(t, log), "=====") != 1 {
		t.Errorf("repeat: %s", r.out)
	}
}

func TestMEM09_LongSessionInParts(t *testing.T) {
	e := newEnv(t)
	s := e.store()
	long := strings.Repeat("很长的讨论内容", 2500) // ~17k tokens, clipped per message
	var texts []string
	for i := 0; i < 12; i++ {
		texts = append(texts, long)
	}
	seedSession(t, s, "claude-code:long", "claude-code", e.proj, texts...)
	x := &stubExtractor{reply: `{"add":[],"update":[]}`}
	eng := memory.Engine{Store: s, Extractor: x, Config: cfg(t, e.proj)}
	_, err := eng.ExtractSession(context.Background(), "claude-code:long")
	must(t, err)
	if x.calls < 2 {
		t.Fatalf("long session sent in %d call(s)", x.calls)
	}
	if n := model.EstimateTokens(x.prompt); n > memory.ChunkTokens+2000 {
		t.Errorf("prompt of %d tokens", n)
	}
	if !strings.Contains(x.prompt, "已截断") {
		t.Error("oversized message not clipped")
	}
	if up, _ := s.ExtractedUpTo("claude-code:long"); up != 12 {
		t.Errorf("progress %d", up)
	}

	// A failure keeps the parts already done.
	seedSession(t, s, "claude-code:long2", "claude-code", e.proj, texts...)
	fail := &failAfter{ok: 1}
	eng.Extractor = fail
	if _, err := eng.ExtractSession(context.Background(), "claude-code:long2"); err == nil {
		t.Fatal("expected failure")
	}
	if up, _ := s.ExtractedUpTo("claude-code:long2"); up == 0 || up >= 12 {
		t.Errorf("partial progress %d", up)
	}
}

type failAfter struct{ ok, calls int }

func (f *failAfter) Complete(context.Context, string) (string, error) {
	f.calls++
	if f.calls > f.ok {
		return "", os.ErrDeadlineExceeded
	}
	return `{"add":[],"update":[]}`, nil
}

func TestMEM10_BackgroundImportAndCancel(t *testing.T) {
	e := newEnv(t)
	fakeExtractor(t, e, "3")
	for i, id := range []string{"33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444"} {
		e.writeClaude(e.proj, id, msg{"user", "第 " + string(rune('1'+i)) + " 个会话"}, msg{"assistant", []any{text("好")}})
	}
	r := e.run("", "api", "memory-import-plan", "--dir", e.proj)
	if !strings.Contains(r.out, `"sessions":2`) {
		t.Fatalf("plan: %s", r.out)
	}
	r = e.run("", "api", "memory-import-start", "--dir", e.proj)
	if !strings.Contains(r.out, `"ok":true`) {
		t.Fatalf("start: %s", r.out)
	}
	if st := importStatus(t, e); st.State != memory.ImportRunning || st.Total != 2 {
		t.Fatalf("not running: %+v", st)
	}
	if r := e.run("", "api", "memory-import-start", "--dir", e.proj); !strings.Contains(r.out, "正在进行") {
		t.Errorf("second import allowed: %s", r.out)
	}
	must(t, e.run("", "api", "memory-import-cancel").err)
	var st memory.ImportStatus
	for i := 0; i < 100; i++ {
		if st = importStatus(t, e); st.State != memory.ImportRunning {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if st.State != memory.ImportCancelled || st.Done == 2 {
		t.Errorf("cancel: %+v", st)
	}
}
