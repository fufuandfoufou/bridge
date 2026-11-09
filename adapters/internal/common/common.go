// Package common holds helpers shared by the built-in adapters: incremental
// JSONL reading, version gating, the origin marker for synthesized tool calls,
// and idempotent edits of hook configuration.
package common

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"bridge/internal/fsutil"
	"bridge/internal/model"
)

// ReadLines reads complete JSONL lines after the cursor. A trailing partial
// line (a write in progress) is left for the next read. If the file was
// replaced or truncated, reading restarts at the beginning with NextSeq reset;
// the store ignores events it already holds.
func ReadLines(path string, from model.Cursor, fn func(line []byte, c *model.Cursor)) (model.Cursor, error) {
	cur := from
	cur.NativeFile = path
	f, err := os.Open(path)
	if err != nil {
		return cur, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return cur, err
	}
	ino := fsutil.Inode(st)
	if (cur.Inode != 0 && cur.Inode != ino) || cur.Offset > st.Size() {
		cur.Offset, cur.NextSeq = 0, 0
	}
	cur.Inode = ino
	if _, err := f.Seek(cur.Offset, io.SeekStart); err != nil {
		return cur, err
	}
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if err == io.EOF {
			return cur, nil // an unterminated tail is not consumed
		}
		if err != nil {
			return cur, err
		}
		cur.Offset += int64(len(line))
		if t := bytes.TrimSpace(line); len(t) > 0 {
			fn(t, &cur)
		}
	}
}

// VersionVerified reports whether v falls in one of the "major.minor" lines
// that have been tested for writing.
func VersionVerified(v string, verified []string) bool {
	for _, p := range verified {
		if v == p || strings.HasPrefix(v, p+".") {
			return true
		}
	}
	return false
}

// ParseTime parses an RFC 3339 timestamp, returning zero on failure.
func ParseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// Compact returns a copy of a raw line suitable for NativeRef.
func Compact(b []byte) json.RawMessage { return append(json.RawMessage{}, b...) }

// RecoveryGrace is how long after an interrupted write's start a target file
// may still have been touched by that write. A later change means someone
// else has used it, so recovery must not remove it.
const RecoveryGrace = 2 * time.Minute

// OriginKey marks tool arguments synthesized during conversion, so a round
// trip can restore the original tool.
const OriginKey = "_bridge_origin"

// Origin records the tool a synthesized call stands in for.
type Origin struct {
	Canonical string          `json:"canonical"`
	Args      json.RawMessage `json:"args,omitempty"`
}

// WithOrigin adds the origin marker to an argument object.
func WithOrigin(args map[string]any, canonical string, original json.RawMessage) map[string]any {
	args[OriginKey] = Origin{Canonical: canonical, Args: original}
	return args
}

// TakeOrigin extracts the origin marker from raw arguments, if present.
func TakeOrigin(raw []byte) (Origin, bool) {
	var w map[string]json.RawMessage
	if json.Unmarshal(raw, &w) != nil || w[OriginKey] == nil {
		return Origin{}, false
	}
	var o Origin
	if json.Unmarshal(w[OriginKey], &o) != nil || o.Canonical == "" {
		return Origin{}, false
	}
	return o, true
}

// MustJSON marshals v without HTML escaping.
func MustJSON(v any) json.RawMessage {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	_ = e.Encode(v)
	return json.RawMessage(bytes.TrimRight(b.Bytes(), "\n"))
}

// ToolResultRole is the sentinel error text for a call that never returned.
const InterruptedResult = "(tool call was interrupted; no result recorded)"

// HookMarker identifies bridge's entries in an agent's hook configuration.
const HookMarker = " hook "

// HookSpec maps a native hook event to the bridge hook name.
type HookSpec struct {
	Native string
	Bridge string
}

// IsBridgeCommand reports whether a hook command belongs to bridge.
func IsBridgeCommand(cmd string) bool {
	return strings.Contains(cmd, HookMarker) && strings.Contains(cmd, "--agent ") && strings.Contains(cmd, "bridge")
}

// EditHooksFile installs (or, with specs empty, removes) bridge's hooks in a
// Claude-style hooks JSON document, leaving every other entry alone. It is
// idempotent, backs up the file first, and does nothing when unchanged.
func EditHooksFile(path, command, agent string, specs []HookSpec) error {
	doc := map[string]any{}
	orig, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if exists && len(bytes.TrimSpace(orig)) > 0 {
		if err := json.Unmarshal(orig, &doc); err != nil {
			return err
		}
	}
	hooks, _ := doc["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	for ev, v := range hooks {
		groups, _ := v.([]any)
		var kept []any
		for _, g := range groups {
			gm, _ := g.(map[string]any)
			list, _ := gm["hooks"].([]any)
			var keep []any
			for _, h := range list {
				hm, _ := h.(map[string]any)
				if c, _ := hm["command"].(string); !IsBridgeCommand(c) {
					keep = append(keep, h)
				}
			}
			if len(keep) > 0 {
				gm["hooks"] = keep
				kept = append(kept, gm)
			} else if len(list) == 0 {
				kept = append(kept, g)
			}
		}
		if len(kept) == 0 {
			delete(hooks, ev)
		} else {
			hooks[ev] = kept
		}
	}
	for _, s := range specs {
		entry := map[string]any{"hooks": []any{map[string]any{
			"type": "command", "timeout": 2,
			"command": ShellQuote(command) + " hook " + s.Bridge + " --agent " + agent,
		}}}
		list, _ := hooks[s.Native].([]any)
		hooks[s.Native] = append(list, entry)
	}
	if len(hooks) == 0 {
		delete(doc, "hooks")
	} else {
		doc["hooks"] = hooks
	}
	if len(doc) == 0 { // nothing but our hooks was in the file
		if !exists {
			return nil
		}
		if _, err := fsutil.Backup(path); err != nil {
			return err
		}
		return os.Remove(path)
	}
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	e.SetIndent("", "  ")
	if err := e.Encode(doc); err != nil {
		return err
	}
	if exists && semanticallyEqual(orig, b.Bytes()) {
		return nil
	}
	return fsutil.SafeWrite(path, b.Bytes(), 0o644)
}

func semanticallyEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	p, _ := json.Marshal(x)
	q, _ := json.Marshal(y)
	return bytes.Equal(p, q)
}

// ShellQuote quotes a word for sh when it holds anything unusual.
func ShellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// HooksInstalled reports whether a hooks JSON file holds bridge's entries.
func HooksInstalled(path string) bool {
	b, err := os.ReadFile(path)
	return err == nil && bytes.Contains(b, []byte(HookMarker+"session_start"))
}

// FindBinary returns the first candidate whose --version output names the
// exact version.
func FindBinary(ctx context.Context, version string, candidates []string) (string, error) {
	for _, c := range candidates {
		if _, err := os.Stat(c); err != nil {
			if p, e := exec.LookPath(c); e == nil {
				c = p
			} else {
				continue
			}
		}
		cmd := exec.CommandContext(ctx, c, "--version")
		cmd.Env = append(os.Environ(), "PATH=/opt/homebrew/bin:/usr/local/bin:"+os.Getenv("PATH"))
		out, err := cmd.Output()
		if err == nil && strings.Contains(string(out), version) {
			return c, nil
		}
	}
	return "", fmt.Errorf("本机没有找到版本 %s 的客户端", version)
}

// ProbeEnv is the environment for a probe subprocess: no credentials from
// this process, and the given overrides.
func ProbeEnv(overrides ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "ANTHROPIC_") || strings.HasPrefix(kv, "CLAUDE") || strings.HasPrefix(kv, "OPENAI_") || strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "CODEX_HOME=") || strings.HasPrefix(kv, "PATH=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:"+os.Getenv("PATH"))
	return append(env, overrides...)
}

// TailLines returns the complete lines in the last n bytes of a file.
func TailLines(path string, n int64) [][]byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	start := st.Size() - n
	if start < 0 {
		start = 0
	}
	buf := make([]byte, st.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		return nil
	}
	lines := bytes.Split(buf, []byte("\n"))
	if start > 0 && len(lines) > 0 {
		lines = lines[1:] // the first line was cut by the offset
	}
	return lines
}
