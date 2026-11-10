package pi

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bridge/internal/model"
	kit "bridge/pkg/adapterkit"
)

func TestToolMappingRoundTrip(t *testing.T) {
	cases := []struct{ name, args string }{
		{"read", `{"path":"a.go","offset":3}`},
		{"write", `{"path":"a.go","content":"x"}`},
		{"edit", `{"path":"a.go","edits":[{"oldText":"a","newText":"b"}]}`},
		{"edit", `{"path":"a.go","edits":[{"oldText":"a","newText":"b"},{"oldText":"c","newText":"d"}]}`},
		{"bash", `{"command":"ls"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			canon := ToCanonical(c.name, json.RawMessage(c.args))
			if canon.CanonicalName == model.ToolOpaque {
				t.Fatalf("%s not mapped", c.name)
			}
			name, args, ok := FromCanonical(canon)
			if !ok || name != c.name {
				t.Fatalf("back: %s %v", name, ok)
			}
			var a, b any
			_ = json.Unmarshal([]byte(c.args), &a)
			_ = json.Unmarshal(args, &b)
			ja, _ := json.Marshal(a)
			jb, _ := json.Marshal(b)
			if string(ja) != string(jb) {
				t.Errorf("round trip: %s -> %s", ja, jb)
			}
		})
	}
}

func TestReadWriteAppend(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", dir)
	now := time.Now().UTC()
	call := &model.ToolCall{ID: "c1", CanonicalName: model.ToolShell, Args: json.RawMessage(`{"command":"ls"}`)}
	events := []model.Event{
		model.TextEvent(model.RoleUser, "hello", now),
		{Kind: model.KindToolCall, Role: model.RoleAssistant, Tool: call, TS: now},
		{Kind: model.KindToolResult, Role: model.RoleTool, Tool: &model.ToolCall{ID: "c1", Result: "a.go", Status: "ok"}, TS: now},
		model.TextEvent(model.RoleAssistant, "done", now),
	}
	data, err := Render(model.Session{CWD: "/w", Title: "t"}, "id1", now, "", "", events)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "sessions", encodeCWD("/w"), "x_id1.jsonl")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, data, 0o600)
	a := New()
	refs, err := a.ListSessions(context.Background(), kit.Filter{})
	if err != nil || len(refs) != 1 || refs[0].NativeID != "id1" || refs[0].CWD != "/w" || refs[0].Title != "t" {
		t.Fatalf("list: %+v %v", refs, err)
	}
	got, _, err := a.ReadSession(context.Background(), refs[0], model.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range got {
		kinds = append(kinds, string(e.Kind)+":"+e.Text())
	}
	if strings.Join(kinds, "|") != "message:hello|tool_call:|tool_result:|message:done" {
		t.Errorf("read: %v", kinds)
	}
	more, err := a.RenderAppend(context.Background(), refs[0], []model.Event{model.TextEvent(model.RoleUser, "next", now)})
	if err != nil {
		t.Fatal(err)
	}
	var e entry
	_ = json.Unmarshal(more, &e)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var last entry
	_ = json.Unmarshal([]byte(lines[len(lines)-1]), &last)
	if e.ParentID == nil || *e.ParentID != last.ID {
		t.Errorf("append does not continue the tree: %s", more)
	}
}

// TestProbeRealClient needs the pi CLI; it runs it against a local endpoint.
func TestProbeRealClient(t *testing.T) {
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skip("pi is not installed")
	}
	out, err := exec.Command("pi", "--version").Output()
	if err != nil {
		t.Skip(err)
	}
	v := strings.TrimSpace(string(out))
	if err := New().ProbeVersion(context.Background(), v); err != nil {
		t.Fatalf("pi %s did not load a session bridge wrote: %v", v, err)
	}
}

// TestProbeToolHistory checks that pi accepts written tool calls and results.
func TestProbeToolHistory(t *testing.T) {
	bin, err := exec.LookPath("pi")
	if err != nil {
		t.Skip("pi is not installed")
	}
	now := time.Now().UTC()
	marker := "bridge-tool-" + entryID()
	call := &model.ToolCall{ID: "call_1", CanonicalName: model.ToolShell, Args: json.RawMessage(`{"command":"echo ` + marker + `"}`)}
	edit := &model.ToolCall{ID: "call_2", CanonicalName: model.ToolEditFile, Args: json.RawMessage(`{"path":"a.go","old":"x","new":"y"}`)}
	events := []model.Event{
		model.TextEvent(model.RoleUser, "run it", now),
		{Kind: model.KindToolCall, Role: model.RoleAssistant, Tool: call, TS: now},
		{Kind: model.KindToolResult, Role: model.RoleTool, Tool: &model.ToolCall{ID: "call_1", NativeName: "bash", Result: marker, Status: "ok"}, TS: now},
		{Kind: model.KindToolCall, Role: model.RoleAssistant, Tool: edit, TS: now},
		{Kind: model.KindToolResult, Role: model.RoleTool, Tool: &model.ToolCall{ID: "call_2", NativeName: "edit", Result: "edited", Status: "ok"}, TS: now},
		model.TextEvent(model.RoleAssistant, "done", now),
	}
	if err := probe(context.Background(), bin, marker, events); err != nil {
		t.Fatal(err)
	}
}
