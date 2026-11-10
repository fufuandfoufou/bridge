package dsh

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bridge/internal/model"
	kit "bridge/pkg/adapterkit"
)

func nativeFixture(t *testing.T) (*Adapter, model.Session, []model.Event) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("DSH_HOME", filepath.Join(dir, "dsh"))
	t.Setenv("BRIDGE_DATA_DIR", "")
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "dsh"), []byte("#!/bin/sh\nprintf '0.2.2-rc.1\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	now := time.Now().UTC()
	call := &model.ToolCall{ID: "call-1", CanonicalName: model.ToolShell, Args: json.RawMessage(`{"command":"echo remembered-tool-value"}`)}
	events := []model.Event{model.TextEvent(model.RoleUser, "remembered-user-value", now),
		model.TextEvent(model.RoleAssistant, "checking", now),
		{Kind: model.KindToolCall, Role: model.RoleAssistant, Tool: call, TS: now},
		{Kind: model.KindToolResult, Role: model.RoleTool, Tool: &model.ToolCall{ID: call.ID, Result: "remembered-tool-value", Status: "ok"}, TS: now},
		model.TextEvent(model.RoleAssistant, "finished", now)}
	return New(), model.Session{CWD: filepath.Join(dir, "项目 🚀"), Title: "Migrated session"}, events
}

func TestNativeWriteReadAppend(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		name := "plain"
		if compressed {
			name = "compressed"
		}
		t.Run(name, func(t *testing.T) {
			a, session, events := nativeFixture(t)
			h, err := a.WriteSession(context.Background(), session, events)
			if err != nil {
				t.Fatal(err)
			}
			if !compressed {
				lines, err := decode(h.Path)
				if err != nil {
					t.Fatal(err)
				}
				var raw strings.Builder
				for _, line := range lines {
					raw.Write(line)
					raw.WriteByte('\n')
				}
				path := strings.TrimSuffix(h.Path, ".zstd")
				if err := os.WriteFile(path, []byte(raw.String()), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(h.Path); err != nil {
					t.Fatal(err)
				}
				h.Path = path
			}
			refs, err := a.ListSessions(context.Background(), kit.Filter{CWD: session.CWD})
			if err != nil || len(refs) != 1 || refs[0].NativeID != h.ID || refs[0].Title != session.Title {
				t.Fatalf("native session was not listed: %+v %v", refs, err)
			}
			got, cursor, err := a.ReadSession(context.Background(), refs[0], model.Cursor{})
			if err != nil || len(got) != len(events) {
				t.Fatalf("read back: %+v %v", got, err)
			}
			for i, event := range events {
				if got[i].Kind != event.Kind || got[i].Text() != event.Text() {
					t.Errorf("event %d changed: %+v", i, got[i])
				}
			}
			release, err := a.LockSession(refs[0])
			if err != nil {
				t.Fatal(err)
			}
			data, err := a.RenderAppend(context.Background(), refs[0], []model.Event{model.TextEvent(model.RoleUser, "new delta", time.Now())})
			if err != nil {
				release()
				t.Fatal(err)
			}
			f, err := os.OpenFile(h.Path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				release()
				t.Fatal(err)
			}
			_, err = f.Write(data)
			_ = f.Close()
			release()
			if err != nil {
				t.Fatal(err)
			}
			if err := a.ValidateAppend(refs[0]); err != nil {
				t.Fatal(err)
			}
			more, _, err := a.ReadSession(context.Background(), refs[0], cursor)
			if err != nil || len(more) != 1 || more[0].Text() != "new delta" {
				t.Fatalf("incremental append: %+v %v", more, err)
			}
			// Both a torn JSONL row and a torn compressed frame must stop writes.
			st, _ := os.Stat(h.Path)
			if err := os.Truncate(h.Path, st.Size()-1); err != nil {
				t.Fatal(err)
			}
			if err := a.ValidateAppend(refs[0]); err == nil {
				t.Error("incomplete tail allowed")
			}
		})
	}
}

func TestNativeWriteLockAndOwnership(t *testing.T) {
	a, session, events := nativeFixture(t)
	h, err := a.WriteSession(context.Background(), session, events)
	if err != nil {
		t.Fatal(err)
	}
	ref := kit.SessionRef{NativeID: h.ID, Path: h.Path}
	release, err := a.LockSession(ref)
	if err != nil {
		t.Fatal(err)
	}
	if !a.SessionActive(ref) {
		t.Error("native write lock was ignored")
	}
	if err := a.RemoveSession(context.Background(), h); !errors.Is(err, kit.ErrOwnership) {
		t.Errorf("active session removal: %v", err)
	}
	release()
	if a.SessionActive(ref) {
		t.Error("released lease stayed active")
	}
	session.NativeID = h.ID
	if _, err := a.WriteSession(context.Background(), session, events); !errors.Is(err, kit.ErrConflict) {
		t.Errorf("duplicate write: %v", err)
	}
	data, err := a.RenderAppend(context.Background(), ref, []model.Event{model.TextEvent(model.RoleUser, "new work", time.Now())})
	if err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(h.Path, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.Write(data)
	_ = f.Close()
	if err := a.RollbackSession(context.Background(), session, time.Now()); !errors.Is(err, kit.ErrOwnership) {
		t.Errorf("continued session rollback: %v", err)
	}
	if _, err := os.Stat(h.Path); err != nil {
		t.Error("continued history was removed")
	}
}

func TestNativeVersionAndPaths(t *testing.T) {
	a, session, events := nativeFixture(t)
	if a.Capabilities("0.2.2-rc.2").SessionWrite || a.Capabilities("").SessionWrite {
		t.Error("unverified version allowed")
	}
	if got := kit.Degrade(a.Capabilities("0.2.2-rc.1")).Handoff; got != "原生写回" {
		t.Errorf("native migration was incorrectly tied to shell hooks: %s", got)
	}
	session.NativeID = "../../escape"
	if _, err := a.WriteSession(context.Background(), session, events); err == nil {
		t.Error("unsafe id accepted")
	}
	for _, c := range []struct{ cwd, want string }{
		{"/w", "--w--"}, {"/项目/🚀", "--~9879~76EE-~D83D~DE80--"}, {"/a b/", "--a~0020b---"}, {"/", "--root--"},
	} {
		if got := projectKey(c.cwd); got != c.want {
			t.Errorf("projectKey(%q) = %q, want %q", c.cwd, got, c.want)
		}
	}
}

func TestProbeNativeResume(t *testing.T) {
	bin, err := exec.LookPath("dsh")
	if err != nil {
		t.Skip("dsh is not installed")
	}
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	if err := New().ProbeVersion(context.Background(), strings.TrimSpace(string(out))); err != nil {
		t.Fatal(err)
	}
}

func TestProbeNativeToolHistory(t *testing.T) {
	bin, err := exec.LookPath("dsh")
	if err != nil {
		t.Skip("dsh is not installed")
	}
	now := time.Now().UTC()
	marker := "bridge-tool-history-sentinel"
	events := []model.Event{model.TextEvent(model.RoleUser, "run a lookup", now),
		{Kind: model.KindToolCall, Role: model.RoleAssistant, Tool: &model.ToolCall{ID: "lookup", CanonicalName: model.ToolShell, Args: json.RawMessage(`{"command":"lookup"}`)}, TS: now},
		{Kind: model.KindToolResult, Role: model.RoleTool, Tool: &model.ToolCall{ID: "lookup", Result: marker, Status: "ok"}, TS: now},
		model.TextEvent(model.RoleAssistant, "lookup complete", now)}
	if err := probe(context.Background(), bin, events, marker); err != nil {
		t.Fatal(err)
	}
}

func TestNativeToolMappings(t *testing.T) {
	for _, tc := range []struct{ name, args string }{
		{"read", `{"file_path":"a.go","offset":3,"limit":20}`},
		{"write", `{"file_path":"a.go","content":"new"}`},
		{"edit", `{"file_path":"a.go","old_string":"old","new_string":"","replace_all":true}`},
		{"bash", `{"command":"ls","description":"list"}`},
		{"grep", `{"pattern":"x","path":"src","include":"*.go"}`},
		{"glob", `{"pattern":"*.go","path":"src"}`},
		{"todo_write", `{"todos":[{"content":"task","status":"pending"}]}`},
		{"web_fetch", `{"url":"https://example.com"}`},
		{"web_search", `{"query":"test"}`},
		{"subagent", `{"description":"investigate","prompt":"look at the code"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			canonical := ToCanonical(tc.name, json.RawMessage(tc.args))
			name, args, ok := FromCanonical(canonical)
			if !ok || name != tc.name {
				t.Fatalf("unmapped tool: %s", tc.name)
			}
			roundtrip := ToCanonical(name, args)
			if roundtrip.CanonicalName != canonical.CanonicalName || string(roundtrip.Args) != string(canonical.Args) {
				t.Errorf("mapping changed: %s -> %s", canonical.Args, roundtrip.Args)
			}
		})
	}
}
