// Package acceptance turns the review document's test cases (审查与TDD测试用例.md)
// into black-box tests. Every test runs under an isolated temporary HOME and
// never touches real agent data. Test names carry the case ID.
package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bridge/adapters/claudecode"
	"bridge/adapters/codex"
	"bridge/internal/config"
	"bridge/internal/daemon"
	"bridge/internal/fsutil"
	"bridge/internal/model"
	"bridge/internal/project"
	"bridge/internal/store"
	kit "bridge/pkg/adapterkit"
)

var binPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "bridge-bin")
	if err != nil {
		panic(err)
	}
	binPath = filepath.Join(dir, "bridge")
	if out, err := exec.Command("go", "build", "-o", binPath, "bridge/cmd/bridge").CombinedOutput(); err != nil {
		panic(string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type env struct {
	t    *testing.T
	home string
	proj string
}

const (
	claudeVer = "2.1.309"
	codexVer  = "0.144.1"
	srcID     = "11111111-1111-4111-8111-111111111111"
	codexSeed = "019a0000-0000-7000-8000-00000000seed"
)

func newEnv(t *testing.T) *env {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("BRIDGE_DATA_DIR", "")
	e := &env{t: t, home: home, proj: filepath.Join(home, "proj")}
	for _, d := range []string{".claude/projects", ".codex/sessions/2026/11/09", ".config/bridge", "proj/.git"} {
		must(t, os.MkdirAll(filepath.Join(home, d), 0o755))
	}
	return e
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, p, s string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(p), 0o755))
	must(t, os.WriteFile(p, []byte(s), 0o644))
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	must(t, err)
	return string(b)
}

type msg struct {
	role    string
	content any
}

func text(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

func toolUse(id, name string, in map[string]any) map[string]any {
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": in}
}

func toolResult(id, res string, isErr bool) map[string]any {
	return map[string]any{"type": "tool_result", "tool_use_id": id, "content": res, "is_error": isErr}
}

func claudeLines(cwd, sid string, ms ...msg) string {
	var b strings.Builder
	parent := any(nil)
	for i, m := range ms {
		typ := "user"
		mm := map[string]any{"role": m.role, "content": m.content}
		if m.role == "assistant" {
			typ, mm["model"] = "assistant", "claude-x"
		}
		u := fmt.Sprintf("u%d-%s", i, sid[:4])
		j, _ := json.Marshal(map[string]any{"type": typ, "uuid": u, "parentUuid": parent, "sessionId": sid, "cwd": cwd,
			"version": claudeVer, "timestamp": fmt.Sprintf("2026-11-09T10:%02d:00.000Z", i), "isSidechain": false, "message": mm})
		b.Write(j)
		b.WriteByte('\n')
		parent = u
	}
	return b.String()
}

func (e *env) claudePath(cwd, sid string) string {
	return filepath.Join(e.home, ".claude/projects", claudecode.EncodeCWD(cwd), sid+".jsonl")
}

func (e *env) writeClaude(cwd, sid string, ms ...msg) string {
	p := e.claudePath(cwd, sid)
	write(e.t, p, claudeLines(cwd, sid, ms...))
	return p
}

func codexLine(typ string, p map[string]any) string {
	j, _ := json.Marshal(map[string]any{"timestamp": "2026-11-09T10:00:00.000Z", "type": typ, "payload": p})
	return string(j) + "\n"
}

func (e *env) writeCodex(cwd, id, ver string, items ...map[string]any) string {
	var b strings.Builder
	b.WriteString(codexLine("session_meta", map[string]any{"id": id, "timestamp": "2026-11-09T10:00:00.000Z", "cwd": cwd, "cli_version": ver, "source": "cli"}))
	b.WriteString(codexLine("turn_context", map[string]any{"model": "gpt-x", "cwd": cwd}))
	for _, it := range items {
		b.WriteString(codexLine("response_item", it))
	}
	p := filepath.Join(e.home, ".codex/sessions/2026/11/09", "rollout-2026-11-09T10-00-00-"+id+".jsonl")
	write(e.t, p, b.String())
	return p
}

func cxMsg(role, s string) map[string]any {
	kind := "input_text"
	if role == "assistant" {
		kind = "output_text"
	}
	return map[string]any{"type": "message", "role": role, "content": []map[string]string{{"type": kind, "text": s}}}
}

// base writes the standard fixture: a 3-turn Claude source (one turn with a
// tool call) plus a seed Codex session so the target's version is detectable.
func (e *env) base() string {
	p := e.writeClaude(e.proj, srcID,
		msg{"user", "q1"}, msg{"assistant", []any{text("a1")}},
		msg{"user", "q2"}, msg{"assistant", []any{toolUse("toolu_1", "Read", map[string]any{"file_path": "/x/a.py"})}},
		msg{"user", []any{toolResult("toolu_1", "print(1)", false)}}, msg{"assistant", []any{text("a2")}},
		msg{"user", "q3"}, msg{"assistant", []any{text("a3")}})
	e.writeCodex(e.proj, codexSeed, codexVer, cxMsg("user", "seed"))
	return p
}

type result struct {
	out string
	err error
}

func (e *env) run(stdin string, args ...string) result {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath, args...)
	cmd.Dir = e.proj
	cmd.Env = append(os.Environ(), "HOME="+e.home)
	cmd.Stdin = strings.NewReader(stdin)
	b, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		e.t.Fatalf("command hung: bridge %v", args)
	}
	return result{string(b), err}
}

func cfg(t *testing.T, dir string) *config.Config {
	t.Helper()
	c, err := config.LoadForDir(dir, nil)
	must(t, err)
	return c
}

func (e *env) reg() *kit.Registry { return kit.NewRegistry(claudecode.New(), codex.New()) }

func (e *env) store() *store.Store {
	e.t.Helper()
	s, err := store.Open(fsutil.DBPath())
	must(e.t, err)
	e.t.Cleanup(func() { s.Close() })
	return s
}

func cfgFn(cwd string) (*config.Config, error) { return config.LoadForDir(cwd, nil) }

func (e *env) ingest(s *store.Store) error {
	_, err := (&daemon.Ingester{Store: s, Registry: e.reg(), Config: cfgFn}).Sync(context.Background())
	return err
}

func readRollout(t *testing.T, p string) []model.Event {
	t.Helper()
	es, _, err := codex.New().ReadSession(context.Background(), kit.SessionRef{Path: p}, model.Cursor{})
	must(t, err)
	return es
}

func readClaude(t *testing.T, p string) []model.Event {
	t.Helper()
	es, _, err := claudecode.New().ReadSession(context.Background(), kit.SessionRef{Path: p}, model.Cursor{})
	must(t, err)
	return es
}

// newRollouts lists rollout files other than the seed.
func (e *env) newRollouts() []string {
	fs, _ := filepath.Glob(filepath.Join(e.home, ".codex/sessions/*/*/*/rollout-*.jsonl"))
	var out []string
	for _, f := range fs {
		if !strings.Contains(f, "seed") {
			out = append(out, f)
		}
	}
	return out
}

func texts(es []model.Event, role string) []string {
	var out []string
	for _, e := range es {
		if e.Kind == model.KindMessage && (role == "" || e.Role == role) {
			out = append(out, e.Text())
		}
	}
	return out
}

// scanData reports every file under the data directory holding the needle,
// including the SQLite WAL and any freed pages.
func scanData(t *testing.T, needle string) []string {
	t.Helper()
	var hits []string
	_ = filepath.WalkDir(fsutil.DataDir(), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, []byte(needle)) {
			hits = append(hits, filepath.Base(p))
		}
		return nil
	})
	return hits
}

// treeState fingerprints a tree: path, type, link target and content hash.
func treeState(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		info, _ := d.Info()
		fmt.Fprintf(&b, "%s %v ", p, info.Mode())
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			l, _ := os.Readlink(p)
			b.WriteString(l)
		case info.Mode().IsRegular():
			h, _ := fsutil.HashFile(p)
			b.WriteString(h)
		}
		b.WriteByte('\n')
		return nil
	})
	return b.String()
}

func projID(cwd string) string { return project.Identify(cwd).ID }

// seedSession stores a session with n user/assistant events directly.
func seedSession(t *testing.T, s *store.Store, id, agent, cwd string, texts ...string) {
	t.Helper()
	p := project.Identify(cwd)
	must(t, s.UpsertProject(p))
	now := time.Now().UTC()
	must(t, s.UpsertSession(model.Session{ID: id, ProjectID: p.ID, Agent: agent, NativeID: strings.SplitN(id, ":", 2)[1], CWD: cwd, StartedAt: now, EndedAt: now}))
	appendEvents(t, s, id, agent, 1, texts...)
}

func appendEvents(t *testing.T, s *store.Store, id, agent string, from int64, texts ...string) {
	t.Helper()
	var es []model.Event
	seq := from
	for i, x := range texts {
		role := model.RoleUser
		if i%2 == 1 {
			role = model.RoleAssistant
		}
		e := model.TextEvent(role, x, time.Now().UTC())
		e.SessionID, e.Seq = id, seq
		es = append(es, e)
		seq++
	}
	_, err := s.Ingest(id, es, store.CursorState{Cursor: model.Cursor{Agent: agent, NativeFile: id, NextSeq: seq - 1}})
	must(t, err)
}

// fakeAdapter is the injectable target of the document's unified fixture.
type fakeAdapter struct {
	id       string
	caps     kit.Capabilities
	writes   int
	writeErr error
	readBack []model.Event
	wrote    string
}

func (f *fakeAdapter) ID() string { return f.id }
func (f *fakeAdapter) Detect(context.Context) (kit.Install, error) {
	return kit.Install{Installed: true, Version: "1.0"}, nil
}
func (f *fakeAdapter) Capabilities(kit.Version) kit.Capabilities { return f.caps }
func (f *fakeAdapter) ListSessions(context.Context, kit.Filter) ([]kit.SessionRef, error) {
	return nil, nil
}
func (f *fakeAdapter) ReadSession(context.Context, kit.SessionRef, model.Cursor) ([]model.Event, model.Cursor, error) {
	return f.readBack, model.Cursor{}, nil
}
func (f *fakeAdapter) WriteSession(_ context.Context, _ model.Session, es []model.Event) (kit.NativeHandle, error) {
	f.writes++
	if f.writeErr != nil {
		return kit.NativeHandle{}, f.writeErr
	}
	return kit.NativeHandle{ID: "new", Path: f.wrote}, nil
}
func (f *fakeAdapter) InstallHooks(context.Context, kit.HookConfig) error { return nil }
func (f *fakeAdapter) UninstallHooks(context.Context) error               { return nil }

// missing marks a capability the contract requires that does not exist yet.
func missing(t *testing.T, what string) {
	t.Helper()
	t.Errorf("未实现: %s", what)
}
