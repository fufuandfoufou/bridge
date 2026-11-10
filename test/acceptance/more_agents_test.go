package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"bridge/adapters/dsh"
	"bridge/adapters/pi"
	"bridge/internal/fsutil"
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
	must(t, a.InstallHooks(context.Background(), kit.HookConfig{Command: "/x/bridge", MCPCommand: []string{"/x/bridge", "mcp"}}))
	if !a.HooksInstalled() || !strings.Contains(read(t, filepath.Join(e.home, ".pi/agent/mcp.json")), "/x/bridge") ||
		!strings.Contains(read(t, filepath.Join(e.home, ".pi/agent/extensions/bridge.js")), `"/x/bridge"`) {
		t.Error("bridge's extension and MCP server not both registered for pi")
	}
	must(t, a.UninstallHooks(context.Background()))
	if a.HooksInstalled() {
		t.Error("uninstall left bridge registered")
	}
}

func deepSeekEnv(t *testing.T) (*env, string) {
	t.Helper()
	e := newEnv(t)
	src := e.base()
	binDir := filepath.Join(e.home, "bin")
	write(t, filepath.Join(binDir, "dsh"), "#!/bin/sh\nprintf '0.2.2-rc.1\\n'\n")
	must(t, os.Chmod(filepath.Join(binDir, "dsh"), 0o700))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return e, src
}

func TestE5_DeepSeekHarnessNativeMigration(t *testing.T) {
	e, src := deepSeekEnv(t)
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
	// Into dsh: the app API writes a native session and a usable resume command.
	r = e.run("", "api", "handoff", srcID, "--to", "dsh")
	if !strings.Contains(r.out, `"mode":"native"`) || !strings.Contains(r.out, "dsh headless --session-id") {
		t.Fatalf("claude -> dsh: %s", r.out)
	}
	if s := e.run("", "api", "status").out; !strings.Contains(s, `"id":"dsh"`) || strings.Contains(s, `"native_write":false`) {
		t.Errorf("status: %s", s)
	}
	r = e.run("", "api", "handoff", srcID, "--to", "dsh")
	if !strings.Contains(r.out, `"mode":"duplicate"`) || !strings.Contains(r.out, "dsh headless --session-id") {
		t.Fatalf("duplicate migration: %s", r.out)
	}
	a := dsh.New()
	refs, err := a.ListSessions(context.Background(), kit.Filter{CWD: e.proj})
	must(t, err)
	var ref kit.SessionRef
	for _, candidate := range refs {
		if candidate.NativeID != "session-dsh1" {
			ref = candidate
		}
	}
	if ref.Path == "" || !strings.HasSuffix(ref.Path, ".zstd") {
		t.Fatalf("native compressed target not indexed: %+v", refs)
	}
	// Later source work updates the same compressed session, once.
	appendFile(t, src, claudeLines(e.proj, srcID, msg{"user", "new Claude work"}))
	if out := syncAPI(e); !strings.Contains(out, `"state":"synced"`) {
		t.Fatal(out)
	}
	events, _, err := a.ReadSession(context.Background(), ref, model.Cursor{})
	must(t, err)
	if got := strings.Join(texts(events, ""), "|"); !strings.Contains(got, "q1") || !strings.Contains(got, "new Claude work") {
		t.Errorf("native update: %s", got)
	}
	if out := syncAPI(e); !strings.Contains(out, `"state":"current"`) {
		t.Fatal(out)
	}
	// New work on the dsh side returns to the original Claude session.
	delta, err := a.RenderAppend(context.Background(), ref, []model.Event{model.TextEvent(model.RoleUser, "new DeepSeek work", time.Now())})
	must(t, err)
	appendFile(t, ref.Path, string(delta))
	if out := syncAPI(e); !strings.Contains(out, `"state":"synced"`) {
		t.Fatal(out)
	}
	if got := strings.Join(texts(readClaude(t, src), ""), "|"); !strings.Contains(got, "new DeepSeek work") {
		t.Errorf("reverse update: %s", got)
	}
	// A real native write lease blocks Bridge until the harness releases it.
	appendFile(t, src, claudeLines(e.proj, srcID, msg{"user", "work while locked"}))
	release, err := a.LockSession(ref)
	must(t, err)
	out := syncAPI(e)
	release()
	if !strings.Contains(out, `"state":"blocked"`) {
		t.Fatalf("active target: %s", out)
	}
	if out := syncAPI(e); !strings.Contains(out, `"state":"synced"`) {
		t.Fatal(out)
	}
}

func TestDeepSeekCrashRecovery(t *testing.T) {
	for _, failpoint := range []string{"dsh.after_write", "sync.after_append"} {
		t.Run(failpoint, func(t *testing.T) {
			e, src := deepSeekEnv(t)
			e.run("", "api", "sessions")
			args := []string{"api", "handoff", srcID, "--to", "dsh"}
			if failpoint == "sync.after_append" {
				if r := e.run("", args...); !strings.Contains(r.out, `"mode":"native"`) {
					t.Fatal(r.out)
				}
				appendFile(t, src, claudeLines(e.proj, srcID, msg{"user", "work survives crash"}))
				args = []string{"api", "sync-session", srcID}
			}
			cmd := exec.Command(binPath, args...)
			cmd.Dir = e.proj
			cmd.Env = append(os.Environ(), "BRIDGE_FAILPOINT="+failpoint)
			if out, err := cmd.CombinedOutput(); err == nil {
				t.Fatalf("failpoint was not reached: %s", out)
			}
			r := e.run("", args...)
			if !strings.Contains(r.out, `"ok":true`) {
				t.Fatal(r.out)
			}
			refs, err := dsh.New().ListSessions(context.Background(), kit.Filter{CWD: e.proj})
			must(t, err)
			if len(refs) != 1 {
				t.Fatalf("recovery left duplicate sessions: %+v", refs)
			}
			events, _, err := dsh.New().ReadSession(context.Background(), refs[0], model.Cursor{})
			must(t, err)
			if failpoint == "sync.after_append" && strings.Count(strings.Join(texts(events, ""), "|"), "work survives crash") != 1 {
				t.Error("recovery lost or repeated the delta")
			}
		})
	}
	t.Run("unknown_version_document", func(t *testing.T) {
		e, _ := deepSeekEnv(t)
		e.run("", "api", "sessions")
		write(t, filepath.Join(e.home, "bin/dsh"), "#!/bin/sh\nprintf '0.2.2-rc.2\\n'\n")
		r := e.run("", "api", "handoff", srcID, "--to", "dsh")
		if !strings.Contains(r.out, `"mode":"document"`) {
			t.Fatalf("unverified version wrote natively: %s", r.out)
		}
	})
}

func TestDeepSeekInterruptedAppendOwnership(t *testing.T) {
	for _, otherWriter := range []bool{false, true} {
		name := "owned_partial_tail"
		if otherWriter {
			name = "native_work_after_crash"
		}
		t.Run(name, func(t *testing.T) {
			e, src := deepSeekEnv(t)
			e.run("", "api", "sessions")
			if r := e.run("", "api", "handoff", srcID, "--to", "dsh"); !strings.Contains(r.out, `"mode":"native"`) {
				t.Fatal(r.out)
			}
			var payload strings.Builder
			payload.WriteString("source delta marker ")
			for i := 0; i < 4096; i++ {
				payload.WriteString(strconv.Itoa(i))
				payload.WriteByte(',')
			}
			appendFile(t, src, claudeLines(e.proj, srcID, msg{"user", payload.String()}))
			cmd := exec.Command(binPath, "api", "sync-session", srcID)
			cmd.Dir, cmd.Env = e.proj, append(os.Environ(), "BRIDGE_FAILPOINT=sync.after_append")
			if out, err := cmd.CombinedOutput(); err == nil {
				t.Fatalf("failpoint was not reached: %s", out)
			}
			var journal struct {
				Path       string `json:"path"`
				To         string `json:"to"`
				Before     int64  `json:"before"`
				AppendData []byte `json:"append_data"`
			}
			must(t, json.Unmarshal([]byte(read(t, filepath.Join(fsutil.DataDir(), "sync-journal.json"))), &journal))
			if len(journal.AppendData) == 0 {
				t.Fatal("journal has no append fingerprint")
			}
			cut := journal.Before + int64(len(journal.AppendData)/2)
			if otherWriter {
				cut = journal.Before
			}
			must(t, os.Truncate(journal.Path, cut))
			a := dsh.New()
			ref := kit.SessionRef{NativeID: strings.TrimPrefix(journal.To, "dsh:"), Path: journal.Path}
			if otherWriter {
				data, err := a.RenderAppend(context.Background(), ref, []model.Event{model.TextEvent(model.RoleUser, "native work to preserve", time.Now())})
				must(t, err)
				if len(data) >= len(journal.AppendData) {
					t.Fatal("fixture does not reproduce a shorter foreign append")
				}
				appendFile(t, ref.Path, string(data))
			}
			before := read(t, ref.Path)
			out := syncAPI(e)
			if otherWriter {
				if !strings.Contains(out, `"ok":false`) || read(t, ref.Path) != before {
					t.Fatalf("foreign history was not preserved: %s", out)
				}
				return
			}
			if !strings.Contains(out, `"state":"synced"`) {
				t.Fatal(out)
			}
			events, _, err := a.ReadSession(context.Background(), ref, model.Cursor{})
			must(t, err)
			if strings.Count(strings.Join(texts(events, ""), "|"), "source delta marker") != 1 {
				t.Error("partial recovery lost or repeated the delta")
			}
		})
	}
}
