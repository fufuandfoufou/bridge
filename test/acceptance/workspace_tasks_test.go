package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"bridge/internal/mcpserver"
	"bridge/internal/model"
	"bridge/internal/project"
	"bridge/internal/store"
)

func fileEvent(t *testing.T, s *store.Store, id, agent string, seq int64, path string, at time.Time) {
	t.Helper()
	e := model.Event{SessionID: id, Seq: seq, Kind: model.KindFileChange, File: &model.FileChange{Path: path, Op: "modify"}, TS: at}
	_, err := s.Ingest(id, []model.Event{e}, store.CursorState{Cursor: model.Cursor{Agent: agent, NativeFile: id, NextSeq: seq}})
	must(t, err)
}

func TestLIVE06_Attribution(t *testing.T) {
	e := newEnv(t)
	s := e.store()
	seedSession(t, s, "claude-code:A", "claude-code", e.proj, "a1")
	seedSession(t, s, "codex:B", "codex", e.proj, "b1")
	svc := liveSvc(s)
	_, err := svc.PromptSubmit("codex", "B", e.proj)
	must(t, err)
	svc.Ack("codex", "B")

	root, err := filepath.EvalSymlinks(e.proj)
	must(t, err)
	now := time.Now().UTC()
	pid := projID(e.proj)
	// A's tool edited a.go by absolute path, B's own tool edited b.go by a
	// relative path; nobody's tool touched manual.txt.
	fileEvent(t, s, "claude-code:A", "claude-code", 2, filepath.Join(e.proj, "a.go"), now.Add(-time.Second))
	fileEvent(t, s, "codex:B", "codex", 2, "b.go", now.Add(-time.Second))
	for _, f := range []string{"a.go", "b.go", "manual.txt"} {
		must(t, s.AddWorkspaceChange(pid, filepath.Join(root, f), "modify", now))
	}
	out, err := svc.PromptSubmit("codex", "B", e.proj)
	must(t, err)
	if !strings.Contains(out, "a.go · 可能来自 claude-code（推测）") {
		t.Errorf("tool edit not attributed as a guess: %s", out)
	}
	if !strings.Contains(out, "manual.txt · 来源未知") {
		t.Errorf("manual edit not reported as unknown: %s", out)
	}
	if strings.Contains(out, "b.go") {
		t.Errorf("reader's own edit reported: %s", out)
	}
	// Not acknowledged: shown again. Acknowledged: gone.
	again, err := svc.PromptSubmit("codex", "B", e.proj)
	must(t, err)
	if !strings.Contains(again, "manual.txt") {
		t.Error("unacknowledged workspace changes lost")
	}
	svc.Ack("codex", "B")
	if after, err := svc.PromptSubmit("codex", "B", e.proj); err != nil || strings.Contains(after, "manual.txt") {
		t.Errorf("acknowledged changes repeated: %q %v", after, err)
	}

	// Two agents near the same change: no single source is claimed.
	seedSession(t, s, "codex:C", "codex", e.proj, "c1")
	fileEvent(t, s, "codex:C", "codex", 2, "shared.go", now)
	fileEvent(t, s, "claude-code:A", "claude-code", 3, "shared.go", now)
	must(t, s.AddWorkspaceChange(pid, filepath.Join(root, "shared.go"), "modify", now))
	out, err = svc.PromptSubmit("codex", "B", e.proj)
	must(t, err)
	if !strings.Contains(out, "shared.go · 来源不确定") {
		t.Errorf("ambiguous source asserted: %s", out)
	}
}

func TestWorkspaceWatcherRecordsManualEdits(t *testing.T) {
	e := newEnv(t)
	must(t, os.MkdirAll(filepath.Join(e.proj, ".git"), 0o755))
	must(t, os.MkdirAll(filepath.Join(e.proj, "node_modules"), 0o755))
	must(t, os.MkdirAll(filepath.Join(e.proj, "src"), 0o755))
	s := e.store()
	seedSession(t, s, "claude-code:A", "claude-code", e.proj, "a1")
	stop := startDaemon(t, e)
	defer stop()
	time.Sleep(300 * time.Millisecond)
	write(t, filepath.Join(e.proj, "src", "main.go"), "package main\n")
	write(t, filepath.Join(e.proj, "node_modules", "x.js"), "x")
	write(t, filepath.Join(e.proj, ".git", "index"), "x")
	must(t, os.MkdirAll(filepath.Join(e.proj, "fresh"), 0o755))
	time.Sleep(200 * time.Millisecond)
	write(t, filepath.Join(e.proj, "fresh", "new.txt"), "n")
	var got []store.WorkspaceChange
	for i := 0; i < 50; i++ {
		var err error
		got, err = s.WorkspaceChanges(projID(e.proj), 0, time.Time{}, 100)
		must(t, err)
		if len(got) >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	paths := []string{}
	for _, c := range got {
		paths = append(paths, c.Path)
	}
	joined := strings.Join(paths, "\n")
	if !strings.Contains(joined, filepath.Join("src", "main.go")) || !strings.Contains(joined, filepath.Join("fresh", "new.txt")) {
		t.Errorf("edits not recorded: %v", paths)
	}
	if strings.Contains(joined, "node_modules") || strings.Contains(joined, ".git") {
		t.Errorf("ignored directories recorded: %v", paths)
	}
}

func TestTASK01_SharedBoard(t *testing.T) {
	e := newEnv(t)
	s := e.store()
	p := project.Identify(e.proj)
	must(t, s.UpsertProject(p))
	h := &mcpserver.Handlers{Store: s, Config: cfg(t, e.proj), Project: p}
	added, err := h.TaskAdd(mcpserver.TaskAddIn{Title: "写迁移脚本", Detail: "含回滚"})
	must(t, err)
	id := added.Task.ID

	// Concurrent claims: exactly one wins, the loser learns who holds it.
	var wg sync.WaitGroup
	results := make([]mcpserver.TaskOut, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := h.TaskClaim(mcpserver.TaskRef{ID: store.ShortTaskID(id)}, "agent#"+string(rune('a'+i)))
			if err != nil {
				t.Error(err)
			}
			results[i] = r
		}(i)
	}
	wg.Wait()
	winners, owner := 0, ""
	for _, r := range results {
		if r.OK {
			winners++
			owner = r.Task.Owner
		}
	}
	if winners != 1 {
		t.Fatalf("%d claimants won", winners)
	}
	for _, r := range results {
		if !r.OK && (r.Task.Owner != owner || !strings.Contains(r.Message, owner)) {
			t.Errorf("loser not told the holder: %+v", r)
		}
	}
	// Only the holder may update or finish it.
	if r, err := h.TaskDone(mcpserver.TaskDoneIn{ID: id}, "someone-else"); err != nil || r.OK {
		t.Errorf("non-holder finished the task: %+v %v", r, err)
	}
	if r, err := h.TaskUpdate(mcpserver.TaskUpdateIn{ID: id, Note: "进行到一半"}, owner); err != nil || !r.OK || r.Task.Note != "进行到一半" {
		t.Errorf("holder update: %+v %v", r, err)
	}
	// The board shows up when a session starts in the project.
	seedSession(t, s, "codex:B", "codex", e.proj, "b1")
	start, err := liveSvc(s).SessionStart("codex", "B", e.proj)
	must(t, err)
	if !strings.Contains(start, "写迁移脚本") || !strings.Contains(start, owner) {
		t.Errorf("task board not injected: %s", start)
	}
	if r, err := h.TaskDone(mcpserver.TaskDoneIn{ID: id, Note: "完成"}, owner); err != nil || !r.OK || r.Task.Status != store.TaskDone {
		t.Errorf("holder done: %+v %v", r, err)
	}
	if r, _ := h.TaskClaim(mcpserver.TaskRef{ID: id}, "late"); r.OK {
		t.Error("finished task claimed again")
	}
	// Tasks of another project are invisible.
	other := project.Identify(t.TempDir())
	must(t, s.UpsertProject(other))
	foreign, err := s.AddTask(other.ID, "别的项目", "")
	must(t, err)
	if _, err := h.TaskClaim(mcpserver.TaskRef{ID: foreign.ID}, "x"); err == nil {
		t.Error("claimed a task of another project")
	}
	if l, _ := h.TaskList(mcpserver.TaskListIn{}); len(l.Tasks) != 0 {
		t.Errorf("done or foreign tasks listed: %+v", l.Tasks)
	}

	// Short IDs stay distinct for tasks created in the same instant.
	x, err := s.AddTask(p.ID, "x", "")
	must(t, err)
	y, err := s.AddTask(p.ID, "y", "")
	must(t, err)
	if got, err := s.GetTask(store.ShortTaskID(y.ID)); err != nil || got.ID != y.ID || store.ShortTaskID(x.ID) == store.ShortTaskID(y.ID) {
		t.Errorf("short ids: %v %v", got.ID, err)
	}

	// The desktop API: add, list, reopen, delete.
	r := e.run("", "api", "task-add", "UI 任务", "--dir", e.proj)
	if !strings.Contains(r.out, `"ok":true`) {
		t.Fatal(r.out)
	}
	r = e.run("", "api", "tasks")
	if !strings.Contains(r.out, "UI 任务") || !strings.Contains(r.out, "别的项目") {
		t.Fatal(r.out)
	}
}
