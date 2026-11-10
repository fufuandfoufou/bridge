// Package golden checks conversions against reviewed expected output, as the
// technical design asks: sample native session → unified events → each other
// agent's native format, plus the handoff document. Run with -update to
// rewrite the baselines, then review the diff before committing it.
package golden

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
	"time"

	"bridge/adapters/claudecode"
	"bridge/adapters/codex"
	"bridge/adapters/dsh"
	"bridge/adapters/pi"
	"bridge/internal/handoff"
	"bridge/internal/model"
	"bridge/internal/privacy"
	kit "bridge/pkg/adapterkit"
)

var update = flag.Bool("update", false, "rewrite the golden files instead of comparing")

// Fixed inputs, so the output only changes when conversion does.
var (
	now      = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	versions = map[string]string{"claude-code": "2.1.317", "codex": "0.144.1", "pi": "1.5.1", "dsh": "0.2.2-rc.1"}
)

func adapter(id string) kit.Adapter {
	switch id {
	case "claude-code":
		return claudecode.New()
	case "codex":
		return codex.New()
	case "pi":
		return pi.New()
	}
	return dsh.New()
}

// render writes events in the target's native format.
func render(target string, s model.Session, events []model.Event) ([]byte, error) {
	switch target {
	case "claude-code":
		return claudecode.Render(s, s.NativeID, versions[target], events)
	case "codex":
		b, _, err := codex.Render(s, s.NativeID, versions[target], now, events)
		return b, err
	case "pi":
		return pi.Render(s, s.NativeID, now, "", "", events)
	}
	return dsh.Render(s, s.NativeID, now, events)
}

var (
	uuidRe  = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	msgRe   = regexp.MustCompile(`msg_[0-9a-f]{32}`)
	entryRe = regexp.MustCompile(`"(id|parentId)":"([0-9a-f]{8})"`)
)

// normalize replaces generated identifiers with placeholders numbered by first
// appearance, so references between them stay checkable.
func normalize(b []byte) []byte {
	seen := map[string]string{}
	name := func(kind, v string) string {
		if p, ok := seen[v]; ok {
			return p
		}
		p := fmt.Sprintf("<%s-%d>", kind, len(seen)+1)
		seen[v] = p
		return p
	}
	b = uuidRe.ReplaceAllFunc(b, func(m []byte) []byte { return []byte(name("uuid", string(m))) })
	b = msgRe.ReplaceAllFunc(b, func(m []byte) []byte { return []byte(name("msg", string(m))) })
	return entryRe.ReplaceAllFunc(b, func(m []byte) []byte {
		g := entryRe.FindSubmatch(m)
		return []byte(fmt.Sprintf(`"%s":"%s"`, g[1], name("entry", string(g[2]))))
	})
}

// eventsJSON is the unified model without native references, which only
// repeat the input.
func eventsJSON(es []model.Event) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	for _, e := range es {
		e.NativeRef = nil
		_ = enc.Encode(e)
	}
	return b.Bytes()
}

func check(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./test/golden -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from its golden file; if the change is intended, run go test ./test/golden -update and review the diff\n%s",
			path, firstDiff(want, got))
	}
}

func firstDiff(want, got []byte) string {
	w, g := bytes.Split(want, []byte("\n")), bytes.Split(got, []byte("\n"))
	for i := 0; i < len(w) || i < len(g); i++ {
		var a, b []byte
		if i < len(w) {
			a = w[i]
		}
		if i < len(g) {
			b = g[i]
		}
		if !bytes.Equal(a, b) {
			return fmt.Sprintf("line %d\n  want: %s\n  got:  %s", i+1, a, b)
		}
	}
	return ""
}

func TestGolden(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // capabilities read approvals from the data dir
	red, err := privacy.NewRedactor(nil)
	if err != nil {
		t.Fatal(err)
	}
	cases, err := filepath.Glob(filepath.Join("testdata", "*", "*", "input.jsonl"))
	if err != nil || len(cases) == 0 {
		t.Fatalf("no golden cases: %v", err)
	}
	sort.Strings(cases)
	for _, in := range cases {
		dir := filepath.Dir(in)
		source := filepath.Base(filepath.Dir(dir))
		t.Run(source+"/"+filepath.Base(dir), func(t *testing.T) {
			events, _, err := adapter(source).ReadSession(context.Background(), kit.SessionRef{Path: in}, model.Cursor{})
			if err != nil {
				t.Fatal(err)
			}
			check(t, filepath.Join(dir, "events.jsonl"), normalize(eventsJSON(events)))
			src := model.Session{ID: source + ":golden", Agent: source, CWD: "/work/demo", Title: "golden " + source}
			for _, target := range []string{"claude-code", "codex", "pi", "dsh"} {
				if target == source {
					continue
				}
				a := adapter(target)
				caps := a.Capabilities(kit.Version(versions[target]))
				canRender := func(c *model.ToolCall) bool {
					if !caps.SupportsTool(c.CanonicalName) {
						return false
					}
					if r, ok := a.(kit.ToolRenderer); ok {
						return r.CanRender(c)
					}
					return true
				}
				res := handoff.Transform(events, handoff.Options{SourceAgent: source, TargetAgent: target,
					ContextWindow: caps.ContextWindow, MaxContextRatio: 0.7, KeepRecentTurns: 20,
					CanRender: canRender, Redactor: red, Now: now})
				dst := model.Session{NativeID: "golden-target", Agent: target, CWD: "/work/demo", Title: src.Title}
				out, err := render(target, dst, res.Events)
				if err != nil {
					t.Fatalf("to %s: %v", target, err)
				}
				check(t, filepath.Join(dir, "to-"+target+".jsonl"), normalize(out))
				if target == "codex" || (source == "codex" && target == "claude-code") {
					check(t, filepath.Join(dir, "document.md"), normalize([]byte(handoff.Document(src, res))))
				}
			}
		})
	}
}
