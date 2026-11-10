// Package dsh adapts DeepSeek Harness (github.com/deepseek-ai/deepseek-harness).
// Its sessions are versioned event logs, Zstandard-compressed by default,
// under $DSH_HOME/sessions/--<cwd>--/<session>/session.vN.jsonl[.zstd].
// Native writes are gated on the installed version and real-client probes.
package dsh

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"bridge/adapters/internal/common"
	"bridge/internal/fsutil"
	"bridge/internal/model"
	"bridge/internal/versions"
	kit "bridge/pkg/adapterkit"
)

// ID is the adapter identifier.
const ID = "dsh"

// formatVersion is the session log generation this reader understands.
const formatVersion = 4

// Adapter implements adapterkit.Adapter for DeepSeek Harness.
type Adapter struct {
	mu      sync.Mutex
	cache   map[string]cached
	version struct {
		bin   string
		mtime time.Time
		v     string
	}
}

type cached struct {
	ref   kit.SessionRef
	mtime time.Time
	size  int64
}

// New creates the adapter.
func New() *Adapter { return &Adapter{cache: map[string]cached{}} }

// ID implements Adapter.
func (a *Adapter) ID() string { return ID }

// Home is the harness data root.
func Home() string {
	if h := strings.TrimSpace(os.Getenv("DSH_HOME")); h != "" {
		return h
	}
	return filepath.Join(fsutil.Home(), ".dsh")
}

func sessionsDir() string { return filepath.Join(Home(), "sessions") }

// WatchRoots implements Watcher.
func (a *Adapter) WatchRoots() []string { return []string{sessionsDir()} }

func binary() string {
	if p, err := exec.LookPath("dsh"); err == nil {
		return p
	}
	for _, p := range []string{"/opt/homebrew/bin/dsh", "/usr/local/bin/dsh"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// Detect implements Adapter.
func (a *Adapter) Detect(ctx context.Context) (kit.Install, error) {
	in := kit.Install{DataDir: Home()}
	bin := binary()
	if _, err := os.Stat(Home()); err != nil && bin == "" {
		return in, nil
	}
	in.Installed = true
	st, err := os.Stat(bin)
	if bin == "" || err != nil {
		return in, nil
	}
	a.mu.Lock()
	if a.version.bin == bin && a.version.mtime.Equal(st.ModTime()) {
		in.Version = kit.Version(a.version.v)
		a.mu.Unlock()
		return in, nil
	}
	a.mu.Unlock()
	v := common.CachedVersion(bin, func() (string, error) {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, bin, "--version").Output()
		return strings.TrimSpace(string(out)), err
	})
	a.mu.Lock()
	a.version.bin, a.version.mtime, a.version.v = bin, st.ModTime(), v
	a.mu.Unlock()
	in.Version = kit.Version(v)
	return in, nil
}

var tools = []kit.ToolMapping{
	{Canonical: model.ToolReadFile, Native: "read"},
	{Canonical: model.ToolWriteFile, Native: "write"},
	{Canonical: model.ToolEditFile, Native: "edit"},
	{Canonical: model.ToolShell, Native: "bash"},
	{Canonical: model.ToolSearchText, Native: "grep"},
	{Canonical: model.ToolSearchFiles, Native: "glob"},
	{Canonical: model.ToolTodo, Native: "todo_write"},
	{Canonical: model.ToolWebFetch, Native: "web_fetch"},
	{Canonical: model.ToolWebSearch, Native: "web_search"},
	{Canonical: model.ToolSubagent, Native: "subagent"},
}

// Capabilities implements Adapter.
func (a *Adapter) Capabilities(v kit.Version) kit.Capabilities {
	return kit.Capabilities{
		SessionRead:  true,
		SessionWrite: string(v) == "0.2.2-rc.1" || versions.Allowed(ID, string(v)),
		LiveTail:     true,
		Hooks:        pluginHooks(v),
		MCP:          true,
		RulesFiles: []kit.PathSpec{
			{Scope: model.ScopeProject, Path: "AGENTS.md", Format: kit.FormatSource},
			{Scope: model.ScopeGlobal, Path: "~/.dsh/AGENTS.md", Format: kit.FormatGenerate},
		},
		SkillsDirs: []kit.PathSpec{
			{Scope: model.ScopeProject, Path: ".agents/skills", Format: kit.FormatSource},
			{Scope: model.ScopeGlobal, Path: "~/.agents/skills", Format: kit.FormatSource, Collect: "~/.dsh/skills"},
		},
		ContextWindow: 1000000,
		Tools:         tools,
	}
}

var logRe = regexp.MustCompile(`^session(?:\.v(\d+))?\.jsonl(\.zstd)?$`)

// current returns a session directory's newest log generation.
func current(dir string) (string, int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", -1
	}
	best, ver := "", -1
	for _, e := range entries {
		m := logRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		v := 0
		if m[1] != "" {
			v, _ = strconv.Atoi(m[1])
		}
		if v > ver || (v == ver && m[2] == "") {
			best, ver = filepath.Join(dir, e.Name()), v
		}
	}
	return best, ver
}

// decode returns the log's lines. A compressed log is a series of
// independent frames; a torn final frame (a write in progress) contributes
// only the lines decoded before the tear.
func decode(path string) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".zstd") {
		d, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return nil, err
		}
		defer d.Close()
		r = d
	}
	var lines [][]byte
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		l, err := br.ReadBytes('\n')
		if len(l) > 0 && l[len(l)-1] == '\n' {
			if t := bytes.TrimSpace(l); len(t) > 0 {
				lines = append(lines, append([]byte(nil), t...))
			}
		}
		if err == io.EOF {
			return lines, nil
		}
		if err != nil {
			if len(lines) > 0 && !errors.Is(err, os.ErrPermission) {
				return lines, nil // torn tail
			}
			return lines, err
		}
	}
}

type event struct {
	Type    string          `json:"type"`
	Seq     int64           `json:"seq"`
	Time    int64           `json:"time"`
	ID      string          `json:"id"`
	CWD     string          `json:"cwd"`
	Version int             `json:"version"`
	Created int64           `json:"createdAt"`
	Depth   int             `json:"delegationDepth"`
	Data    json.RawMessage `json:"data"`
}

type message struct {
	Role    string `json:"role"`
	Content []struct {
		Type      string `json:"type"`
		Text      string `json:"text"`
		ID        string `json:"id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"content"`
	Source struct {
		Kind   string `json:"kind"`
		CallID string `json:"callId"`
	} `json:"source"`
	ToolCallID string `json:"toolCallId"`
	IsError    bool   `json:"isError"`
}

// ListSessions implements Adapter. Sessions of delegated subagents are not
// listed: they are part of their parent's work.
func (a *Adapter) ListSessions(ctx context.Context, f kit.Filter) ([]kit.SessionRef, error) {
	dirs, err := filepath.Glob(filepath.Join(sessionsDir(), "*", "session-*"))
	if err != nil {
		return nil, err
	}
	var out []kit.SessionRef
	for _, dir := range dirs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p, ver := current(dir)
		if p == "" {
			continue
		}
		st, err := os.Stat(p)
		if err != nil || st.Size() == 0 {
			continue
		}
		ref, ok := a.head(p, ver, st)
		if !ok {
			continue
		}
		if f.CWD != "" && filepath.Clean(ref.CWD) != filepath.Clean(f.CWD) {
			continue
		}
		if !f.Since.IsZero() && ref.UpdatedAt.Before(f.Since) {
			continue
		}
		out = append(out, ref)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

func (a *Adapter) head(p string, ver int, st os.FileInfo) (kit.SessionRef, bool) {
	a.mu.Lock()
	c, hit := a.cache[p]
	a.mu.Unlock()
	if hit && c.mtime.Equal(st.ModTime()) && c.size == st.Size() {
		return c.ref, c.ref.NativeID != ""
	}
	ref := kit.SessionRef{Agent: ID, Path: p, UpdatedAt: st.ModTime(), Size: st.Size(), Version: strconv.Itoa(ver)}
	if ver != formatVersion {
		// An older generation dsh hasn't migrated yet, or a newer one this
		// reader doesn't know: listed only once dsh writes the current one.
		return ref, false
	}
	lines, err := decode(p)
	if err != nil && len(lines) == 0 {
		ref.Unreadable, ref.NativeID = true, filepath.Base(filepath.Dir(p))
		return ref, true
	}
	subagent := false
	first := ""
	for _, l := range lines {
		var e event
		if json.Unmarshal(l, &e) != nil {
			continue
		}
		switch e.Type {
		case "session":
			ref.NativeID, ref.CWD = e.ID, e.CWD
			ref.StartedAt = time.UnixMilli(e.Created)
			subagent = e.Depth > 0
		case "session/title":
			var d struct {
				Title string `json:"title"`
			}
			if json.Unmarshal(e.Data, &d) == nil && d.Title != "" {
				ref.Title = d.Title
			}
		case "request/context":
			var d struct {
				Provider string `json:"provider"`
				Model    string `json:"model"`
			}
			if json.Unmarshal(e.Data, &d) == nil && d.Model != "" {
				ref.Model = d.Provider + "/" + d.Model
			}
		case "user/message":
			if first == "" {
				var m message
				if json.Unmarshal(e.Data, &m) == nil && m.Source.Kind == "user" {
					for _, b := range m.Content {
						if b.Type == "text" && b.Text != "" {
							first = b.Text
							break
						}
					}
				}
			}
		}
	}
	if ref.Title == "" {
		ref.Title = first
	}
	if subagent {
		ref.NativeID = ""
	}
	a.mu.Lock()
	a.cache[p] = cached{ref: ref, mtime: st.ModTime(), size: st.Size()}
	a.mu.Unlock()
	return ref, ref.NativeID != ""
}

// ReadSession implements Adapter. A compressed log can't be resumed at a
// byte offset, so the cursor's Offset holds the next dsh event seq to read.
func (a *Adapter) ReadSession(ctx context.Context, ref kit.SessionRef, from model.Cursor) ([]model.Event, model.Cursor, error) {
	cur := from
	cur.Agent, cur.NativeFile = ID, ref.Path
	st, err := os.Stat(ref.Path)
	if err != nil {
		return nil, cur, err
	}
	if ino := fsutil.Inode(st); cur.Inode != 0 && cur.Inode != ino {
		cur.Offset, cur.NextSeq = 0, 0
	}
	cur.Inode = fsutil.Inode(st)
	lines, err := decode(ref.Path)
	if err != nil && len(lines) == 0 {
		return nil, cur, err
	}
	var out []model.Event
	for _, l := range lines {
		var e event
		if json.Unmarshal(l, &e) != nil || e.Type == "session" || e.Seq < cur.Offset {
			continue
		}
		for _, ev := range parse(e, l) {
			ev.Seq = cur.NextSeq + 1
			cur.NextSeq++
			out = append(out, ev)
		}
		cur.Offset = e.Seq + 1
	}
	return out, cur, nil
}

func parse(e event, raw []byte) []model.Event {
	ts := time.UnixMilli(e.Time).UTC()
	messagePath := []string{"data", "message"}
	if e.Type == "user/message" {
		messagePath = messagePath[:1]
	}
	ref := common.WithoutReasoning(raw, messagePath...)
	if e.Type == "assistant/message" && len(ref) > 0 {
		// Provider streams and replay state duplicate private reasoning and signatures.
		var top, data, message, source map[string]json.RawMessage
		_ = json.Unmarshal(ref, &top)
		_ = json.Unmarshal(top["data"], &data)
		_ = json.Unmarshal(data["message"], &message)
		_ = json.Unmarshal(message["source"], &source)
		delete(data, "stream")
		if source != nil {
			delete(source, "replayState")
			message["source"] = common.MustJSON(source)
		}
		data["message"] = common.MustJSON(message)
		top["data"] = common.MustJSON(data)
		ref = common.MustJSON(top)
	}
	mk := func(ev model.Event) model.Event { ev.TS, ev.NativeRef = ts, ref; return ev }
	switch e.Type {
	case "user/message":
		var m message
		if json.Unmarshal(e.Data, &m) != nil || m.Source.Kind != "user" {
			return nil // runtime context and other harness injections
		}
		var out []model.Event
		for _, b := range m.Content {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				out = append(out, mk(model.TextEvent(model.RoleUser, b.Text, ts)))
			}
		}
		return out
	case "assistant/message":
		var d struct {
			Message message `json:"message"`
		}
		if json.Unmarshal(e.Data, &d) != nil {
			return nil
		}
		var out []model.Event
		for _, b := range d.Message.Content {
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) != "" {
					out = append(out, mk(model.TextEvent(model.RoleAssistant, b.Text, ts)))
				}
			case "reasoning", "thinking", "redacted_thinking":
				out = append(out, model.Event{Kind: model.KindReasoning, Role: model.RoleAssistant, TS: ts})
			case "tool-call":
				t := ToCanonical(b.Name, json.RawMessage(b.Arguments))
				t.ID = b.ID
				out = append(out, mk(model.Event{Kind: model.KindToolCall, Role: model.RoleAssistant, Tool: t}))
				if fc := model.FileChangeFromTool(t); fc != nil {
					out = append(out, mk(model.Event{Kind: model.KindFileChange, Role: model.RoleAssistant, File: fc}))
				}
			}
		}
		return out
	case "tool/result":
		var d struct {
			Message message `json:"message"`
		}
		if json.Unmarshal(e.Data, &d) != nil {
			return nil
		}
		var text []string
		for _, b := range d.Message.Content {
			if b.Type == "text" {
				text = append(text, b.Text)
			}
		}
		status := "ok"
		if d.Message.IsError {
			status = "error"
		}
		id := d.Message.ToolCallID
		if id == "" {
			id = d.Message.Source.CallID
		}
		return []model.Event{mk(model.Event{Kind: model.KindToolResult, Role: model.RoleTool,
			Tool: &model.ToolCall{ID: id, Result: strings.Join(text, "\n"), Status: status}})}
	}
	return nil
}

// ToCanonical maps a dsh tool call to the unified vocabulary (pure). dsh's
// file tools share Claude Code's argument names.
func ToCanonical(name string, args json.RawMessage) *model.ToolCall {
	t := &model.ToolCall{NativeName: name, CanonicalName: model.ToolOpaque, Args: args}
	var a map[string]any
	_ = json.Unmarshal(args, &a)
	str := func(k string) string { s, _ := a[k].(string); return s }
	set := func(c string, m map[string]any) {
		for k, v := range m {
			// Empty file contents and edit replacements are meaningful values.
			if v == nil || (v == "" && k != "content" && k != "old" && k != "new") {
				delete(m, k)
			}
		}
		t.CanonicalName, t.Args = c, common.MustJSON(m)
	}
	switch name {
	case "read":
		set(model.ToolReadFile, map[string]any{"path": str("file_path"), "offset": a["offset"], "limit": a["limit"]})
	case "write":
		set(model.ToolWriteFile, map[string]any{"path": str("file_path"), "content": a["content"]})
	case "edit":
		set(model.ToolEditFile, map[string]any{"path": str("file_path"), "old": a["old_string"], "new": a["new_string"], "replace_all": a["replace_all"]})
	case "bash":
		set(model.ToolShell, map[string]any{"command": str("command"), "description": str("description")})
	case "grep":
		set(model.ToolSearchText, map[string]any{"pattern": str("pattern"), "path": str("path"), "glob": str("include")})
	case "glob":
		set(model.ToolSearchFiles, map[string]any{"pattern": str("pattern"), "path": str("path")})
	case "todo_write":
		set(model.ToolTodo, map[string]any{"items": a["todos"]})
	case "web_fetch":
		set(model.ToolWebFetch, map[string]any{"url": str("url")})
	case "web_search":
		q := str("query")
		if qs, ok := a["queries"].([]any); ok && len(qs) > 0 {
			q, _ = qs[0].(string)
		}
		set(model.ToolWebSearch, map[string]any{"query": q})
	case "subagent", "subagent_fork":
		set(model.ToolSubagent, map[string]any{"description": str("description"), "prompt": str("prompt")})
	}
	return t
}
