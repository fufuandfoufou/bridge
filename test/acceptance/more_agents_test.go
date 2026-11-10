package acceptance

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"bridge/adapters/pi"
	"bridge/internal/model"
	kit "bridge/pkg/adapterkit"
)

func TestE5_PiAdapter(t *testing.T) {
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skip("pi is not installed")
	}
	e := newEnv(t)
	e.base()
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(e.home, ".pi/agent"))
	a := pi.New()
	for _, c := range kit.Conformance(context.Background(), a, kit.ConformanceOptions{Write: true, CWD: e.proj}) {
		if !c.Passed {
			t.Errorf("conformance %s: %s", c.Name, c.Detail)
		}
	}

	// A Claude session migrates natively and pi sees its history.
	r := e.run("", "handoff", srcID, "--to", "pi")
	if r.err != nil || !strings.Contains(r.out, "pi --session") {
		t.Fatalf("handoff: %s", r.out)
	}
	refs, err := a.ListSessions(context.Background(), kit.Filter{CWD: e.proj})
	if err != nil || len(refs) != 1 {
		t.Fatalf("pi sessions: %v %v", refs, err)
	}
	got, _, err := a.ReadSession(context.Background(), refs[0], model.Cursor{})
	must(t, err)
	joined := strings.Join(texts(got, ""), "|")
	for _, want := range []string{"q1", "a1", "q3", "a3"} {
		if !strings.Contains(joined, want) {
			t.Errorf("migrated session lacks %q: %s", want, joined)
		}
	}

	// Status lists pi, and connecting registers bridge's MCP server for it.
	if s := e.run("", "api", "status").out; !strings.Contains(s, `"id":"pi"`) {
		t.Errorf("status: %s", s)
	}
	must(t, a.InstallHooks(context.Background(), kit.HookConfig{MCPCommand: []string{"/x/bridge", "mcp"}}))
	if !a.HooksInstalled() || !strings.Contains(read(t, filepath.Join(e.home, ".pi/agent/mcp.json")), "/x/bridge") {
		t.Error("bridge MCP not registered for pi")
	}
	must(t, a.UninstallHooks(context.Background()))
	if a.HooksInstalled() {
		t.Error("uninstall left bridge registered")
	}
}

func TestE5_DeepSeekHarnessReadOnly(t *testing.T) {
	e := newEnv(t)
	e.base()
	dir := filepath.Join(e.home, ".dsh/sessions", "--x--", "session-dsh1")
	must(t, os.MkdirAll(dir, 0o755))
	log := `{"type":"session","version":4,"id":"session-dsh1","createdAt":1794304764000,"cwd":"` + e.proj + `","delegationDepth":0}
{"type":"user/message","seq":1,"time":1794304764700,"data":{"content":[{"type":"text","text":"在 dsh 里讨论的方案"}],"source":{"kind":"user"},"role":"user","id":"u1"}}
{"type":"assistant/message","seq":2,"time":1794304764800,"data":{"message":{"role":"assistant","content":[{"type":"text","text":"dsh 给出的结论"}],"source":{"kind":"model"},"id":"a1"}}}
`
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf)
	must(t, err)
	_, _ = enc.Write([]byte(log))
	must(t, enc.Close())
	write(t, filepath.Join(dir, "session.v4.jsonl.zstd"), buf.String())

	r := e.run("", "api", "sessions")
	if !strings.Contains(r.out, `"agent":"dsh"`) || !strings.Contains(r.out, "在 dsh 里讨论的方案") {
		t.Fatalf("sessions: %s", r.out)
	}
	// Out of dsh: a native Claude Code session.
	r = e.run("", "api", "handoff", "dsh:session-dsh1", "--to", "claude-code")
	if !strings.Contains(r.out, `"mode":"native"`) {
		t.Fatalf("dsh -> claude: %s", r.out)
	}
	// Into dsh: a handoff document, never a native write.
	r = e.run("", "api", "handoff", srcID, "--to", "dsh")
	if !strings.Contains(r.out, `"mode":"document"`) && !strings.Contains(r.out, "交接文档") {
		t.Fatalf("claude -> dsh: %s", r.out)
	}
	if s := e.run("", "api", "status").out; !strings.Contains(s, `"id":"dsh"`) || !strings.Contains(s, `"native_write":false`) {
		t.Errorf("status: %s", s)
	}
}
