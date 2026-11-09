// Package claudecode is the built-in adapter for Claude Code. Sessions live
// at ~/.claude/projects/<encoded cwd>/<uuid>.jsonl.
package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"bridge/adapters/internal/common"
	"bridge/internal/fsutil"
	"bridge/internal/model"
	"bridge/internal/versions"
	kit "bridge/pkg/adapterkit"
)

// ID is the adapter identifier.
const ID = "claude-code"

// verifiedVersions are the release lines whose format has been checked.
var verifiedVersions = []string{"2.1"}

// Adapter implements adapterkit.Adapter for Claude Code.
type Adapter struct {
	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	ref   kit.SessionRef
	mtime time.Time
}

// New creates the adapter.
func New() *Adapter { return &Adapter{cache: map[string]cached{}} }

// ID implements Adapter.
func (a *Adapter) ID() string { return ID }

func root() string { return filepath.Join(fsutil.Home(), ".claude") }

// WatchRoots implements Watcher.
func (a *Adapter) WatchRoots() []string { return []string{filepath.Join(root(), "projects")} }

// Detect implements Adapter. The version comes from the newest session since
// desktop installs may have no claude binary on PATH.
func (a *Adapter) Detect(ctx context.Context) (kit.Install, error) {
	in := kit.Install{DataDir: root()}
	if _, err := os.Stat(root()); err != nil {
		return in, nil
	}
	in.Installed = true
	refs, err := a.ListSessions(ctx, kit.Filter{Limit: 5})
	if err != nil {
		return in, err
	}
	for _, r := range refs {
		if r.Version != "" {
			in.Version = kit.Version(r.Version)
			break
		}
	}
	return in, nil
}

var tools = []kit.ToolMapping{
	{Canonical: model.ToolReadFile, Native: "Read"},
	{Canonical: model.ToolWriteFile, Native: "Write"},
	{Canonical: model.ToolEditFile, Native: "Edit"},
	{Canonical: model.ToolShell, Native: "Bash"},
	{Canonical: model.ToolSearchText, Native: "Grep"},
	{Canonical: model.ToolSearchFiles, Native: "Glob"},
	{Canonical: model.ToolTodo, Native: "TodoWrite"},
	{Canonical: model.ToolWebSearch, Native: "WebSearch"},
	{Canonical: model.ToolWebFetch, Native: "WebFetch"},
	{Canonical: model.ToolSubagent, Native: "Task"},
	{Canonical: model.ToolMCPCall, Native: "mcp__"},
}

// Capabilities implements Adapter. Writing is only enabled for verified versions.
func (a *Adapter) Capabilities(v kit.Version) kit.Capabilities {
	return kit.Capabilities{
		SessionRead:  true,
		SessionWrite: common.VersionVerified(string(v), verifiedVersions) || versions.Allowed(ID, string(v)),
		LiveTail:     true,
		Hooks:        []kit.HookKind{kit.HookSessionStart, kit.HookPromptSubmit, kit.HookPreTool, kit.HookPostTool, kit.HookStop},
		MCP:          true,
		RulesFiles: []kit.PathSpec{
			{Scope: model.ScopeProject, Path: "CLAUDE.md", Format: kit.FormatSymlink},
			{Scope: model.ScopeGlobal, Path: "~/.claude/CLAUDE.md", Format: kit.FormatGenerate},
		},
		SkillsDirs: []kit.PathSpec{
			{Scope: model.ScopeProject, Path: ".claude/skills", Format: kit.FormatSymlink},
			{Scope: model.ScopeGlobal, Path: "~/.claude/skills", Format: kit.FormatSymlink},
		},
		ContextWindow: 200000,
		Tools:         tools,
	}
}

var encodeRe = regexp.MustCompile(`[^A-Za-z0-9]`)

// EncodeCWD converts a working directory to Claude Code's project folder name.
func EncodeCWD(cwd string) string { return encodeRe.ReplaceAllString(cwd, "-") }

type record struct {
	Type        string          `json:"type"`
	UUID        string          `json:"uuid"`
	SessionID   string          `json:"sessionId"`
	CWD         string          `json:"cwd"`
	Version     string          `json:"version"`
	Timestamp   string          `json:"timestamp"`
	IsSidechain bool            `json:"isSidechain"`
	IsMeta      bool            `json:"isMeta"`
	IsAPIError  bool            `json:"isApiErrorMessage"`
	CustomTitle string          `json:"customTitle"`
	Message     json.RawMessage `json:"message"`
}

type message struct {
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// ListSessions implements Adapter.
func (a *Adapter) ListSessions(ctx context.Context, f kit.Filter) ([]kit.SessionRef, error) {
	pattern := filepath.Join(root(), "projects", "*", "*.jsonl")
	files, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	var out []kit.SessionRef
	for _, p := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		st, err := os.Stat(p)
		if err != nil || st.Size() == 0 {
			continue
		}
		ref := a.head(p, st)
		if ref.CWD == "" {
			if !ref.Unreadable {
				continue // only metadata, no conversation
			}
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

// head extracts session metadata from the start of a file. Results are cached
// by mtime so the daemon can rescan cheaply.
func (a *Adapter) head(p string, st os.FileInfo) kit.SessionRef {
	a.mu.Lock()
	c, ok := a.cache[p]
	a.mu.Unlock()
	if ok && c.mtime.Equal(st.ModTime()) {
		return c.ref
	}
	ref := kit.SessionRef{Agent: ID, Path: p, NativeID: strings.TrimSuffix(filepath.Base(p), ".jsonl"),
		UpdatedAt: st.ModTime(), Size: st.Size()}
	file, err := os.Open(p)
	if err != nil { // not cached: permissions can change without touching mtime
		ref.Unreadable = true
		return ref
	}
	{
		defer file.Close()
		s := bufio.NewScanner(file)
		s.Buffer(make([]byte, 1<<20), 1<<26)
		for n := 0; s.Scan() && n < 400; n++ {
			var r record
			if json.Unmarshal(s.Bytes(), &r) != nil {
				continue
			}
			if r.CustomTitle != "" {
				ref.Title = r.CustomTitle
			}
			if r.Type != "user" && r.Type != "assistant" {
				continue
			}
			if ref.CWD == "" {
				ref.CWD, ref.Version = r.CWD, r.Version
				ref.StartedAt = common.ParseTime(r.Timestamp)
			}
			if ref.Version == "" {
				ref.Version = r.Version
			}
			if ref.Model == "" && r.Type == "assistant" {
				var m message
				if json.Unmarshal(r.Message, &m) == nil && m.Model != "<synthetic>" {
					ref.Model = m.Model
				}
			}
			if ref.CWD != "" && ref.Model != "" && ref.Title != "" {
				break
			}
		}
	}
	if c := latestCWD(p); c != "" {
		ref.CWD = c
	}
	a.mu.Lock()
	a.cache[p] = cached{ref: ref, mtime: st.ModTime()}
	a.mu.Unlock()
	return ref
}

// ReadSession implements Adapter. Unknown record types and malformed lines are
// skipped so a damaged or newer file never aborts reading.
func (a *Adapter) ReadSession(ctx context.Context, ref kit.SessionRef, from model.Cursor) ([]model.Event, model.Cursor, error) {
	from.Agent = ID
	var out []model.Event
	cur, err := common.ReadLines(ref.Path, from, func(line []byte, c *model.Cursor) {
		var r record
		if json.Unmarshal(line, &r) != nil {
			return
		}
		if r.Version != "" {
			c.FormatVersion = r.Version
		}
		for _, e := range ParseRecord(r, line) {
			e.Seq = c.NextSeq + 1
			c.NextSeq++
			out = append(out, e)
		}
	})
	return out, cur, err
}

// ParseRecord converts one native record to unified events (pure).
func ParseRecord(r record, line []byte) []model.Event {
	if (r.Type != "user" && r.Type != "assistant") || r.IsSidechain || r.IsMeta || r.IsAPIError || len(r.Message) == 0 {
		return nil
	}
	var m message
	if json.Unmarshal(r.Message, &m) != nil {
		return nil
	}
	ts := common.ParseTime(r.Timestamp)
	ref := sanitize(line)
	mk := func(e model.Event) model.Event { e.TS, e.NativeRef = ts, ref; return e }
	var str string
	if json.Unmarshal(m.Content, &str) == nil {
		if str == "" {
			return nil
		}
		return []model.Event{mk(model.TextEvent(m.Role, str, ts))}
	}
	var blocks []block
	if json.Unmarshal(m.Content, &blocks) != nil {
		return nil
	}
	var out []model.Event
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				out = append(out, mk(model.TextEvent(m.Role, b.Text, ts)))
			}
		case "image":
			out = append(out, mk(model.Event{Kind: model.KindMessage, Role: m.Role,
				Content: []model.ContentPart{{Type: "image_ref", Ref: "inline-image", Text: "[图片]"}}}))
		case "thinking", "redacted_thinking":
			out = append(out, model.Event{Kind: model.KindReasoning, Role: model.RoleAssistant, TS: ts})
		case "tool_use":
			t := ToCanonical(b.Name, b.Input)
			t.ID = b.ID
			out = append(out, mk(model.Event{Kind: model.KindToolCall, Role: model.RoleAssistant, Tool: t}))
			if fc := model.FileChangeFromTool(t); fc != nil {
				out = append(out, mk(model.Event{Kind: model.KindFileChange, Role: model.RoleAssistant, File: fc}))
			}
		case "tool_result":
			st := "ok"
			if b.IsError {
				st = "error"
			}
			out = append(out, mk(model.Event{Kind: model.KindToolResult, Role: model.RoleTool,
				Tool: &model.ToolCall{ID: b.ToolUseID, Result: resultText(b.Content), Status: st}}))
		}
	}
	return out
}

// sanitize returns the raw record without reasoning blocks, which must never
// be stored. Lines without them are kept byte for byte.
func sanitize(line []byte) json.RawMessage {
	if !bytes.Contains(line, []byte("thinking")) {
		return common.Compact(line)
	}
	var top, msg map[string]json.RawMessage
	var content []json.RawMessage
	if json.Unmarshal(line, &top) != nil || json.Unmarshal(top["message"], &msg) != nil || json.Unmarshal(msg["content"], &content) != nil {
		return nil // a reasoning-bearing line of unknown shape is not retained
	}
	kept := make([]json.RawMessage, 0, len(content))
	for _, c := range content {
		var b block
		if json.Unmarshal(c, &b) == nil && (b.Type == "thinking" || b.Type == "redacted_thinking") {
			continue
		}
		kept = append(kept, c)
	}
	msg["content"] = common.MustJSON(kept)
	top["message"] = common.MustJSON(msg)
	return common.MustJSON(top)
}

func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []block
	if json.Unmarshal(raw, &parts) != nil {
		return string(raw)
	}
	var b []string
	for _, p := range parts {
		switch p.Type {
		case "text":
			b = append(b, p.Text)
		case "image":
			b = append(b, "[图片]")
		}
	}
	return strings.Join(b, "\n")
}

type todoItem struct {
	Content    string `json:"content"`
	Status     string `json:"status"`
	ActiveForm string `json:"activeForm,omitempty"`
}

// ToCanonical maps a native tool call to the unified vocabulary (pure).
func ToCanonical(name string, input json.RawMessage) *model.ToolCall {
	t := &model.ToolCall{NativeName: name, CanonicalName: model.ToolOpaque, Args: input}
	if o, ok := common.TakeOrigin(input); ok {
		t.CanonicalName, t.Args = o.Canonical, o.Args
		return t
	}
	var in map[string]any
	_ = json.Unmarshal(input, &in)
	str := func(k string) string { s, _ := in[k].(string); return s }
	set := func(c string, a map[string]any) {
		t.CanonicalName, t.Args = c, common.MustJSON(dropEmpty(a))
	}
	switch name {
	case "Read":
		set(model.ToolReadFile, map[string]any{"path": str("file_path"), "offset": in["offset"], "limit": in["limit"]})
	case "Write":
		set(model.ToolWriteFile, map[string]any{"path": str("file_path"), "content": str("content")})
	case "Edit":
		set(model.ToolEditFile, map[string]any{"path": str("file_path"), "old": str("old_string"), "new": str("new_string"), "replace_all": in["replace_all"]})
	case "MultiEdit":
		set(model.ToolEditFile, map[string]any{"path": str("file_path"), "edits": in["edits"]})
	case "Bash":
		set(model.ToolShell, map[string]any{"command": str("command"), "description": str("description")})
	case "Grep":
		set(model.ToolSearchText, map[string]any{"pattern": str("pattern"), "path": str("path"), "glob": str("glob")})
	case "Glob":
		set(model.ToolSearchFiles, map[string]any{"pattern": str("pattern"), "path": str("path")})
	case "TodoWrite":
		set(model.ToolTodo, map[string]any{"items": in["todos"]})
	case "WebSearch":
		set(model.ToolWebSearch, map[string]any{"query": str("query")})
	case "WebFetch":
		set(model.ToolWebFetch, map[string]any{"url": str("url"), "prompt": str("prompt")})
	case "Task", "Agent":
		set(model.ToolSubagent, map[string]any{"description": str("description"), "prompt": str("prompt")})
	default:
		if strings.HasPrefix(name, "mcp__") {
			parts := strings.SplitN(name, "__", 3)
			if len(parts) == 3 {
				t.CanonicalName = model.ToolMCPCall
				t.Args = common.MustJSON(map[string]any{"server": parts[1], "tool": parts[2], "args": json.RawMessage(orEmptyObject(input))})
			}
		}
	}
	return t
}

func orEmptyObject(b json.RawMessage) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage(`{}`)
	}
	return b
}

func dropEmpty(m map[string]any) map[string]any {
	for k, v := range m {
		if v == nil || v == "" {
			delete(m, k)
		}
	}
	return m
}

// FromCanonical maps a unified tool call to a native name and input (pure).
// ok is false when no native equivalent exists.
func FromCanonical(t *model.ToolCall) (string, json.RawMessage, bool) {
	var a map[string]any
	_ = json.Unmarshal(t.Args, &a)
	if a == nil {
		a = map[string]any{}
	}
	str := func(k string) string { s, _ := a[k].(string); return s }
	out := func(n string, m map[string]any) (string, json.RawMessage, bool) {
		return n, common.MustJSON(dropEmpty(m)), true
	}
	switch t.CanonicalName {
	case model.ToolReadFile:
		return out("Read", map[string]any{"file_path": str("path"), "offset": a["offset"], "limit": a["limit"]})
	case model.ToolWriteFile:
		return out("Write", map[string]any{"file_path": str("path"), "content": str("content")})
	case model.ToolEditFile:
		if a["edits"] != nil {
			return out("MultiEdit", map[string]any{"file_path": str("path"), "edits": a["edits"]})
		}
		if str("patch") != "" { // a patch cannot be expressed as old/new strings
			return "", nil, false
		}
		return out("Edit", map[string]any{"file_path": str("path"), "old_string": str("old"), "new_string": str("new"), "replace_all": a["replace_all"]})
	case model.ToolShell:
		return out("Bash", map[string]any{"command": str("command"), "description": str("description")})
	case model.ToolSearchText:
		return out("Grep", map[string]any{"pattern": str("pattern"), "path": str("path"), "glob": str("glob")})
	case model.ToolSearchFiles:
		return out("Glob", map[string]any{"pattern": str("pattern"), "path": str("path")})
	case model.ToolTodo:
		return out("TodoWrite", map[string]any{"todos": normalizeTodos(a["items"])})
	case model.ToolWebSearch:
		return out("WebSearch", map[string]any{"query": str("query")})
	case model.ToolWebFetch:
		return out("WebFetch", map[string]any{"url": str("url"), "prompt": str("prompt")})
	case model.ToolSubagent:
		return out("Task", map[string]any{"description": str("description"), "prompt": str("prompt"), "subagent_type": "general-purpose"})
	case model.ToolMCPCall:
		if str("server") == "" || str("tool") == "" {
			return "", nil, false
		}
		args, _ := json.Marshal(a["args"])
		return "mcp__" + str("server") + "__" + str("tool"), args, true
	}
	return "", nil, false
}

func normalizeTodos(v any) []todoItem {
	list, _ := v.([]any)
	out := make([]todoItem, 0, len(list))
	for _, it := range list {
		m, _ := it.(map[string]any)
		c, _ := m["content"].(string)
		if c == "" {
			c, _ = m["step"].(string)
		}
		s, _ := m["status"].(string)
		if s == "" {
			s = "pending"
		}
		af, _ := m["activeForm"].(string)
		if af == "" {
			af = c
		}
		out = append(out, todoItem{Content: c, Status: s, ActiveForm: af})
	}
	return out
}

// CanRender implements adapterkit.ToolRenderer.
func (a *Adapter) CanRender(t *model.ToolCall) bool {
	_, _, ok := FromCanonical(t)
	return ok
}

var toolIDRe = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// WriteSession implements Adapter.
func (a *Adapter) WriteSession(ctx context.Context, s model.Session, events []model.Event) (kit.NativeHandle, error) {
	in, err := a.Detect(ctx)
	if err != nil {
		return kit.NativeHandle{}, err
	}
	if !a.Capabilities(in.Version).SessionWrite {
		return kit.NativeHandle{}, fmt.Errorf("%s %q: %w", ID, in.Version, kit.ErrUnsupportedVersion)
	}
	if s.CWD == "" {
		return kit.NativeHandle{}, fmt.Errorf("session has no working directory")
	}
	id := s.NativeID
	if id == "" {
		id = newV4()
	}
	data, err := Render(s, id, string(in.Version), events)
	if err != nil {
		return kit.NativeHandle{}, err
	}
	path := sessionPath(s.CWD, id)
	if _, err := os.Lstat(path); err == nil {
		return kit.NativeHandle{}, fmt.Errorf("%s: %w", id, kit.ErrConflict)
	}
	if err := fsutil.AtomicWrite(path, data, 0o600); err != nil {
		return kit.NativeHandle{}, err
	}
	fsutil.Failpoint("claude.after_write")
	return kit.NativeHandle{ID: id, Path: path,
		ResumeCommand: "cd " + common.ShellQuote(s.CWD) + " && claude --resume " + id}, nil
}

func sessionPath(cwd, id string) string {
	return filepath.Join(root(), "projects", EncodeCWD(cwd), id+".jsonl")
}

// NewSessionID implements adapterkit.SessionIDMinter.
func (a *Adapter) NewSessionID() string { return newV4() }

// RollbackSession implements adapterkit.SessionRecoverer.
func (a *Adapter) RollbackSession(ctx context.Context, s model.Session, started time.Time) error {
	p := sessionPath(s.CWD, s.NativeID)
	st, err := os.Stat(p)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.ModTime().After(started.Add(common.RecoveryGrace)) {
		return fmt.Errorf("%s: %w", p, kit.ErrOwnership)
	}
	return os.Remove(p)
}

func newV4() string {
	u := []byte(fsutil.NewUUID())
	// Claude Code session ids are conventional v4 UUIDs.
	u[14] = '4'
	return string(u)
}

// RemoveSession implements SessionRemover.
func (a *Adapter) RemoveSession(ctx context.Context, h kit.NativeHandle) error {
	return os.Remove(h.Path)
}

// Render produces the JSONL for a session (pure apart from generated UUIDs).
func Render(s model.Session, id, version string, events []model.Event) ([]byte, error) {
	return render(s, id, version, nil, events)
}

func render(s model.Session, id, version string, parent any, events []model.Event) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if raw := allNative(events); raw != nil {
		return renderRaw(raw, id)
	}
	emit := func(typ string, msg map[string]any, ts time.Time) error {
		if ts.IsZero() {
			ts = time.Now().UTC()
		}
		u := newV4()
		err := enc.Encode(map[string]any{
			"parentUuid": parent, "isSidechain": false, "userType": "external", "cwd": s.CWD,
			"sessionId": id, "version": version, "gitBranch": "", "type": typ, "message": msg,
			"uuid": u, "timestamp": ts.UTC().Format("2006-01-02T15:04:05.000Z"),
		})
		parent = u
		return err
	}
	var asst, results []any
	var asstTS, resTS time.Time
	flushAsst := func() error {
		if len(asst) == 0 {
			return nil
		}
		err := emit("assistant", map[string]any{
			"id": "msg_" + strings.ReplaceAll(newV4(), "-", ""), "type": "message", "role": "assistant",
			"model": orDefault(s.Model, "claude-sonnet-5-5"), "content": asst, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]int{"input_tokens": 0, "output_tokens": 0},
		}, asstTS)
		asst = nil
		return err
	}
	flushRes := func() error {
		if len(results) == 0 {
			return nil
		}
		err := emit("user", map[string]any{"role": "user", "content": results}, resTS)
		results = nil
		return err
	}
	for _, e := range events {
		var err error
		switch e.Kind {
		case model.KindMessage:
			if e.Role == model.RoleAssistant {
				if err = flushRes(); err == nil {
					asst, asstTS = append(asst, map[string]any{"type": "text", "text": e.Text()}), e.TS
				}
			} else {
				if err = flushRes(); err == nil {
					if err = flushAsst(); err == nil {
						err = emit("user", map[string]any{"role": "user", "content": e.Text()}, e.TS)
					}
				}
			}
		case model.KindToolCall:
			name, input, ok := FromCanonical(e.Tool)
			if !ok {
				return nil, fmt.Errorf("tool %q reached writer without a mapping", e.Tool.CanonicalName)
			}
			if err = flushRes(); err == nil {
				asst, asstTS = append(asst, map[string]any{"type": "tool_use", "id": toolIDRe.ReplaceAllString(e.Tool.ID, "_"), "name": name, "input": json.RawMessage(input)}), e.TS
			}
		case model.KindToolResult:
			if err = flushAsst(); err == nil {
				results, resTS = append(results, map[string]any{"type": "tool_result", "tool_use_id": toolIDRe.ReplaceAllString(e.Tool.ID, "_"),
					"content": e.Tool.Result, "is_error": e.Tool.Status == "error"}), e.TS
			}
		}
		if err != nil {
			return nil, err
		}
	}
	if err := flushRes(); err != nil {
		return nil, err
	}
	if err := flushAsst(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// allNative returns de-duplicated raw lines when every event carries one, the
// same-agent lossless path.
func allNative(events []model.Event) []json.RawMessage {
	if len(events) == 0 {
		return nil
	}
	var out []json.RawMessage
	for _, e := range events {
		if len(e.NativeRef) == 0 {
			return nil
		}
		if len(out) == 0 || !bytes.Equal(out[len(out)-1], e.NativeRef) {
			out = append(out, e.NativeRef)
		}
	}
	return out
}

func renderRaw(lines []json.RawMessage, id string) ([]byte, error) {
	var buf bytes.Buffer
	for _, l := range lines {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(l, &m); err != nil {
			return nil, err
		}
		m["sessionId"], _ = json.Marshal(id)
		b, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

var hookSpecs = []common.HookSpec{
	{Native: "SessionStart", Bridge: "session_start"},
	{Native: "UserPromptSubmit", Bridge: "prompt_submit"},
	{Native: "PreToolUse", Bridge: "pre_tool"},
	{Native: "PostToolUse", Bridge: "post_tool"},
	{Native: "Stop", Bridge: "stop"},
}

func settingsPath() string { return filepath.Join(root(), "settings.json") }
func globalJSON() string   { return filepath.Join(fsutil.Home(), ".claude.json") }

// InstallHooks implements Adapter; it is idempotent.
func (a *Adapter) InstallHooks(ctx context.Context, cfg kit.HookConfig) error {
	if err := common.EditHooksFile(settingsPath(), cfg.Command, ID, hookSpecs); err != nil {
		return err
	}
	return editMCP(cfg.MCPCommand)
}

// UninstallHooks implements Adapter.
func (a *Adapter) UninstallHooks(ctx context.Context) error {
	if err := common.EditHooksFile(settingsPath(), "", ID, nil); err != nil {
		return err
	}
	return editMCP(nil)
}

func editMCP(argv []string) error {
	path := globalJSON()
	orig, err := os.ReadFile(path)
	if os.IsNotExist(err) && len(argv) == 0 {
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	doc := map[string]any{}
	if len(bytes.TrimSpace(orig)) > 0 {
		if err := json.Unmarshal(orig, &doc); err != nil {
			return err
		}
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	if len(argv) == 0 {
		if servers == nil || servers["bridge"] == nil {
			return nil
		}
		delete(servers, "bridge")
	} else {
		if servers == nil {
			servers = map[string]any{}
			doc["mcpServers"] = servers
		}
		servers["bridge"] = map[string]any{"type": "stdio", "command": argv[0], "args": argv[1:]}
	}
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	e.SetIndent("", "  ")
	if err := e.Encode(doc); err != nil {
		return err
	}
	return fsutil.SafeWrite(path, b.Bytes(), 0o600)
}

// HooksInstalled implements adapterkit.HookChecker.
func (a *Adapter) HooksInstalled() bool { return common.HooksInstalled(settingsPath()) }

// ProbeVersion implements adapterkit.VersionProber. The real client resumes a
// session under a throwaway HOME with its API pointed at a closed local port,
// so nothing leaves the machine and no credentials are used. The client
// recording a new turn chained to the written history proves it loaded it.
func (a *Adapter) ProbeVersion(ctx context.Context, version string) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	bundled, _ := filepath.Glob(filepath.Join(fsutil.Home(), "Library/Application Support/Claude/claude-code/*/*/claude.app/Contents/MacOS/claude"))
	bin, err := common.FindBinary(ctx, version, append([]string{"claude", filepath.Join(fsutil.Home(), ".local/bin/claude"), "/opt/homebrew/bin/claude"}, bundled...))
	if err != nil {
		return err
	}
	home, err := os.MkdirTemp("", "bridge-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(home)
	if home, err = filepath.EvalSymlinks(home); err != nil {
		return err
	}
	now := time.Now().UTC()
	id := newV4()
	data, err := Render(model.Session{CWD: home, Model: "probe"}, id, version, []model.Event{
		model.TextEvent(model.RoleUser, "bridge-probe-question", now), model.TextEvent(model.RoleAssistant, "bridge-probe-answer", now)})
	if err != nil {
		return err
	}
	var last struct {
		UUID string `json:"uuid"`
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if err := json.Unmarshal(lines[len(lines)-1], &last); err != nil {
		return err
	}
	p := filepath.Join(home, ".claude", "projects", EncodeCWD(home), id+".jsonl")
	if err := fsutil.AtomicWrite(p, data, 0o600); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, bin, "-p", "ok", "--resume", id, "--output-format", "json", "--tools", "")
	cmd.Dir = home
	cmd.Env = common.ProbeEnv("HOME="+home, "ANTHROPIC_API_KEY=bridge-probe", "ANTHROPIC_BASE_URL=http://127.0.0.1:9", "CLAUDE_CODE_MAX_RETRIES=0")
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	defer func() { _ = cmd.Process.Kill(); <-exited }()
	chained := func() bool {
		b, err := os.ReadFile(p)
		if err != nil {
			return false
		}
		// The client may insert system records, so a new user turn only has
		// to descend from the written history.
		descends := map[string]bool{last.UUID: true}
		for _, l := range bytes.Split(b, []byte("\n")) {
			var r struct {
				Type       string `json:"type"`
				UUID       string `json:"uuid"`
				ParentUUID string `json:"parentUuid"`
			}
			if json.Unmarshal(l, &r) != nil || r.UUID == "" || !descends[r.ParentUUID] {
				continue
			}
			if r.Type == "user" {
				return true
			}
			descends[r.UUID] = true
		}
		return false
	}
	for {
		if chained() {
			return nil
		}
		select {
		case <-exited:
			if chained() {
				return nil
			}
			return errors.New("客户端未能继续探测会话")
		case <-ctx.Done():
			return errors.New("客户端未能在限定时间内继续探测会话")
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// latestCWD returns the working directory of the newest conversation record;
// a session can move between directories while it runs.
func latestCWD(p string) string {
	lines := common.TailLines(p, 1<<20)
	for i := len(lines) - 1; i >= 0; i-- {
		var r struct {
			Type string `json:"type"`
			CWD  string `json:"cwd"`
		}
		if json.Unmarshal(lines[i], &r) == nil && r.CWD != "" && (r.Type == "user" || r.Type == "assistant") {
			return r.CWD
		}
	}
	return ""
}

// SessionActive implements adapterkit.SessionAppender using the registry of
// running sessions the client keeps at ~/.claude/sessions/<pid>.json.
func (a *Adapter) SessionActive(ref kit.SessionRef) bool {
	files, _ := filepath.Glob(filepath.Join(root(), "sessions", "*.json"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var s struct {
			PID       int    `json:"pid"`
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(b, &s) != nil || s.SessionID != ref.NativeID || s.PID <= 0 {
			continue
		}
		if err := syscall.Kill(s.PID, 0); err == nil || errors.Is(err, syscall.EPERM) {
			return true
		}
	}
	return false
}

// RenderAppend implements adapterkit.SessionAppender, chaining the new records
// to the newest record of the existing conversation.
func (a *Adapter) RenderAppend(ctx context.Context, ref kit.SessionRef, events []model.Event) ([]byte, error) {
	lines := common.TailLines(ref.Path, 4<<20)
	leaf := ""
	for i := len(lines) - 1; i >= 0 && leaf == ""; i-- {
		var r struct {
			UUID        string `json:"uuid"`
			IsSidechain bool   `json:"isSidechain"`
		}
		if json.Unmarshal(lines[i], &r) == nil && !r.IsSidechain {
			leaf = r.UUID
		}
	}
	if leaf == "" {
		return nil, errors.New("no conversation record to continue from")
	}
	s := model.Session{CWD: ref.CWD, Model: ref.Model}
	return render(s, ref.NativeID, ref.Version, leaf, events)
}

// AfterAppend implements adapterkit.SessionAppender; Claude Code has no index.
func (a *Adapter) AfterAppend(ctx context.Context, ref kit.SessionRef) error { return nil }
