package dsh

import (
	"bridge/internal/fakellm"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"bridge/internal/model"
	kit "bridge/pkg/adapterkit"
)

// A v4 log in the shape the real harness writes (see the package comment),
// stored as independent zstd frames the way it appends batches.
var frames = []string{
	`{"type":"session","version":4,"id":"session-1","createdAt":1794304764000,"cwd":"/w","isSeeded":false,"delegationDepth":0}
{"type":"user/message","seq":8,"time":1794304764700,"data":{"content":[{"type":"text","text":"run a probe command"}],"source":{"kind":"user"},"role":"user","id":"u1"},"surfaceOp":"append"}
{"type":"user/message","seq":9,"time":1794304764700,"data":{"content":[{"type":"text","text":"Current runtime context."}],"source":{"kind":"runtime-context"},"role":"user","id":"u2"},"surfaceOp":"append"}
{"type":"request/context","seq":11,"time":1794304764700,"data":{"provider":"deepseek-official","model":"deepseek-flash"}}
{"type":"session/title","seq":12,"time":1794304764701,"data":{"title":"Probe run"}}
`,
	`{"type":"assistant/message","seq":13,"time":1794304764802,"data":{"turn":1,"step":1,"message":{"role":"assistant","content":[{"type":"tool-call","id":"call_1","name":"edit","arguments":"{\"file_path\":\"/w/a.go\",\"old_string\":\"x\",\"new_string\":\"y\"}"}],"source":{"kind":"model"},"id":"a1"}}}
{"type":"tool/call","seq":14,"time":1794304764802,"data":{"callId":"call_1","name":"edit","arguments":"{}"}}
{"type":"tool/result","seq":17,"time":1794304764821,"data":{"message":{"role":"tool","source":{"kind":"tool","callId":"call_1"},"toolCallId":"call_1","content":[{"type":"text","text":"edited"}],"isError":false,"id":"t1"}},"sourceEventSeqs":[14],"surfaceOp":"append"}
`,
	`{"type":"assistant/message","seq":20,"time":1794304764836,"data":{"turn":1,"step":2,"message":{"role":"assistant","content":[{"type":"text","text":"done"}],"source":{"kind":"model"},"id":"a2"}}}
`,
}

func compress(t *testing.T, parts ...string) []byte {
	var out bytes.Buffer
	for _, p := range parts {
		enc, err := zstd.NewWriter(&out)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = enc.Write([]byte(p))
		_ = enc.Close()
	}
	return out.Bytes()
}

func TestReadCompressedLogIncrementally(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	dir := filepath.Join(home, "sessions", "--w--", "session-1")
	_ = os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, "session.v4.jsonl.zstd")
	// An older generation next to it is ignored in favour of v4.
	_ = os.WriteFile(filepath.Join(dir, "session.v3.jsonl.zstd"), compress(t, "{}\n"), 0o600)
	_ = os.WriteFile(p, compress(t, frames[0], frames[1]), 0o600)

	a := New()
	refs, err := a.ListSessions(context.Background(), kit.Filter{})
	if err != nil || len(refs) != 1 {
		t.Fatalf("list: %+v %v", refs, err)
	}
	r := refs[0]
	if r.NativeID != "session-1" || r.CWD != "/w" || r.Title != "Probe run" || r.Model != "deepseek-official/deepseek-flash" || r.Path != p {
		t.Errorf("ref: %+v", r)
	}
	got, cur, err := a.ReadSession(context.Background(), r, model.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range got {
		kinds = append(kinds, string(e.Kind)+":"+e.Text())
	}
	if strings.Join(kinds, "|") != "message:run a probe command|tool_call:|file_change:|tool_result:" {
		t.Errorf("first read: %v", kinds)
	}
	if got[1].Tool.CanonicalName != model.ToolEditFile || got[2].File.Path != "/w/a.go" || got[3].Tool.Result != "edited" {
		t.Errorf("tool events: %+v %+v %+v", got[1].Tool, got[2].File, got[3].Tool)
	}

	// The harness appends a frame; only the new events come back.
	_ = os.WriteFile(p, compress(t, frames[0], frames[1], frames[2]), 0o600)
	more, cur2, err := a.ReadSession(context.Background(), r, cur)
	if err != nil {
		t.Fatal(err)
	}
	if len(more) != 1 || more[0].Text() != "done" || more[0].Seq != cur.NextSeq+1 || cur2.NextSeq != cur.NextSeq+1 {
		t.Errorf("incremental: %+v", more)
	}

	// A torn final frame yields what decoded before the tear.
	full := compress(t, frames[0], frames[1], frames[2])
	_ = os.WriteFile(p, full[:len(full)-5], 0o600)
	torn, _, err := a.ReadSession(context.Background(), r, model.Cursor{})
	if err != nil || len(torn) < 4 {
		t.Errorf("torn: %d events, %v", len(torn), err)
	}
}

func TestSubagentSessionsAreNotListed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	dir := filepath.Join(home, "sessions", "--w--", "session-2")
	_ = os.MkdirAll(dir, 0o755)
	header := `{"type":"session","version":4,"id":"session-2","createdAt":1,"cwd":"/w","delegationDepth":1}` + "\n"
	_ = os.WriteFile(filepath.Join(dir, "session.v4.jsonl"), []byte(header), 0o600)
	if refs, _ := New().ListSessions(context.Background(), kit.Filter{}); len(refs) != 0 {
		t.Errorf("subagent listed: %+v", refs)
	}
}

// TestRealHarness has the installed dsh run one task in a throwaway home,
// with uploads and telemetry off, its model pointed at a local stand-in and
// every other request sent to a dead proxy, then reads the session back.
func TestRealHarness(t *testing.T) {
	bin, err := exec.LookPath("dsh")
	if err != nil {
		t.Skip("dsh is not installed")
	}
	llm, err := fakellm.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer llm.Close()
	tmp, _ := filepath.EvalSymlinks(t.TempDir())
	home, work := filepath.Join(tmp, "dsh"), filepath.Join(tmp, "work")
	_ = os.MkdirAll(home, 0o755)
	_ = os.MkdirAll(work, 0o755)
	patch := `- id: session-log-deepseek
  disabled: true
- id: session-telemetry-otel
  disabled: true
- id: otel
  disabled: true
- id: web-search-deepseek
  disabled: true
- id: web-deepseek-api
  disabled: true
- id: llm-deepseek-account
  disabled: true
- id: plugin-package-inventory-deepseek
  disabled: true
- id: session-title-llm
  disabled: true
- id: llm-pi-ai
  config:
    providers:
      bridgeprobe:
        apiKeyEnv: BRIDGE_PROBE_KEY
        api: openai-completions
        baseURL: ` + llm.URL + `
        models:
          - id: probe
            contextWindow: 128000
- id: agent-default-model
  config:
    provider: bridgeprobe
    model: probe
`
	_ = os.WriteFile(filepath.Join(home, "cordis.patch.yml"), []byte(patch), 0o600)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "headless", "bridge real harness check")
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "HOME="+tmp, "DSH_HOME="+home, "DSH_AGENTS_HOME="+filepath.Join(tmp, "agents"), "BRIDGE_PROBE_KEY=x",
		"DSH_TELEMETRY_MODE=OFF", "HTTPS_PROXY=http://127.0.0.1:1", "HTTP_PROXY=http://127.0.0.1:1", "NO_PROXY=127.0.0.1,localhost")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("dsh headless: %v\n%s", err, out)
	}
	t.Setenv("DSH_HOME", home)
	refs, err := New().ListSessions(context.Background(), kit.Filter{CWD: work})
	if err != nil || len(refs) != 1 {
		t.Fatalf("list: %+v %v", refs, err)
	}
	got, _, err := New().ReadSession(context.Background(), refs[0], model.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, e := range got {
		texts = append(texts, e.Role+":"+e.Text())
	}
	if j := strings.Join(texts, "|"); j != "user:bridge real harness check|assistant:probe ok" {
		t.Errorf("read back %q", j)
	}
}
