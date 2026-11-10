// Package pi adapts the pi coding agent (github.com/earendil-works/pi).
// Sessions are JSONL trees under ~/.pi/agent/sessions/--<cwd>--/; pi has no
// shell hooks, so live progress reaches it through MCP queries and the
// managed block in its global AGENTS.md.
package pi

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"bridge/adapters/internal/common"
	"bridge/internal/fsutil"
	"bridge/internal/model"
	"bridge/internal/versions"
	kit "bridge/pkg/adapterkit"
)

// ID is the adapter identifier.
const ID = "pi"

// verifiedVersions are the release lines whose written sessions pi has been
// seen to load (see ProbeVersion).
var verifiedVersions = []string{"1.5"}

// Adapter implements adapterkit.Adapter for pi.
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
}

// New creates the adapter.
func New() *Adapter { return &Adapter{cache: map[string]cached{}} }

// ID implements Adapter.
func (a *Adapter) ID() string { return ID }

// Root is pi's agent directory.
func Root() string {
	if d := os.Getenv("PI_CODING_AGENT_DIR"); d != "" {
		return d
	}
	return filepath.Join(fsutil.Home(), ".pi", "agent")
}

func sessionsDir() string { return filepath.Join(Root(), "sessions") }

// WatchRoots implements Watcher.
func (a *Adapter) WatchRoots() []string { return []string{sessionsDir()} }

func binary() string {
	if p, err := exec.LookPath("pi"); err == nil {
		return p
	}
	for _, p := range []string{"/opt/homebrew/bin/pi", "/usr/local/bin/pi", filepath.Join(fsutil.Home(), ".local/bin/pi")} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// Detect implements Adapter. The version comes from the CLI, cached until the
// binary changes; pi's session files don't record it.
func (a *Adapter) Detect(ctx context.Context) (kit.Install, error) {
	in := kit.Install{DataDir: Root()}
	bin := binary()
	if _, err := os.Stat(Root()); err != nil && bin == "" {
		return in, nil
	}
	in.Installed = true
	if bin == "" {
		return in, nil
	}
	st, err := os.Stat(bin)
	if err != nil {
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
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, bin, "--version").Output()
		v := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "pi "))
		if f := strings.Fields(v); len(f) > 0 {
			v = f[0]
		}
		return v, err
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
	{Canonical: model.ToolSearchText, Native: "bash"},
	{Canonical: model.ToolSearchFiles, Native: "bash"},
	{Canonical: model.ToolMCPCall, Native: "mcp__"},
}

// Capabilities implements Adapter.
func (a *Adapter) Capabilities(v kit.Version) kit.Capabilities {
	return kit.Capabilities{
		SessionRead:  true,
		SessionWrite: common.VersionVerified(string(v), verifiedVersions) || versions.Allowed(ID, string(v)),
		LiveTail:     true,
		MCP:          true,
		RulesFiles: []kit.PathSpec{
			{Scope: model.ScopeProject, Path: "AGENTS.md", Format: kit.FormatSource},
			{Scope: model.ScopeGlobal, Path: "~/.pi/agent/AGENTS.md", Format: kit.FormatGenerate},
		},
		SkillsDirs: []kit.PathSpec{
			{Scope: model.ScopeProject, Path: ".agents/skills", Format: kit.FormatSource},
			{Scope: model.ScopeGlobal, Path: "~/.pi/agent/skills", Format: kit.FormatSymlink},
		},
		ContextWindow: 128000,
		Tools:         tools,
	}
}

// entry is one line of a session file.
type entry struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	ParentID  *string         `json:"parentId"`
	Timestamp string          `json:"timestamp"`
	CWD       string          `json:"cwd"`
	Version   int             `json:"version"`
	Provider  string          `json:"provider"`
	ModelID   string          `json:"modelId"`
	Name      string          `json:"name"`
	Message   json.RawMessage `json:"message"`
}

type message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	IsError    bool            `json:"isError"`
	API        string          `json:"api"`
	Provider   string          `json:"provider"`
	Model      string          `json:"model"`
	Command    string          `json:"command"`
	Output     string          `json:"output"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func blocks(raw json.RawMessage) []block {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []block{{Type: "text", Text: s}}
	}
	var out []block
	_ = json.Unmarshal(raw, &out)
	return out
}

// ListSessions implements Adapter.
func (a *Adapter) ListSessions(ctx context.Context, f kit.Filter) ([]kit.SessionRef, error) {
	files, err := filepath.Glob(filepath.Join(sessionsDir(), "*", "*.jsonl"))
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
		ref, ok := a.head(p, st)
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

func (a *Adapter) head(p string, st os.FileInfo) (kit.SessionRef, bool) {
	a.mu.Lock()
	c, hit := a.cache[p]
	a.mu.Unlock()
	if hit && c.mtime.Equal(st.ModTime()) {
		return c.ref, c.ref.NativeID != ""
	}
	ref := kit.SessionRef{Agent: ID, Path: p, UpdatedAt: st.ModTime(), Size: st.Size()}
	f, err := os.Open(p)
	if err != nil {
		ref.Unreadable, ref.NativeID = true, filepath.Base(p)
		return ref, true
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1<<20), 1<<26)
	first := ""
	for n := 0; s.Scan(); n++ {
		var e entry
		if json.Unmarshal(s.Bytes(), &e) != nil {
			continue
		}
		switch e.Type {
		case "session":
			ref.NativeID, ref.CWD, ref.StartedAt = e.ID, e.CWD, common.ParseTime(e.Timestamp)
		case "model_change":
			ref.Model = e.Provider + "/" + e.ModelID
		case "session_info":
			if e.Name != "" {
				ref.Title = e.Name
			}
		case "message":
			if first == "" {
				var m message
				if json.Unmarshal(e.Message, &m) == nil && m.Role == "user" {
					for _, b := range blocks(m.Content) {
						if b.Type == "text" && b.Text != "" {
							first = b.Text
							break
						}
					}
				}
			}
		}
		if n > 400 && ref.NativeID != "" && first != "" {
			break // titles and models are near the top; don't read huge files whole
		}
	}
	if ref.Title == "" {
		ref.Title = clip(first, 80)
	}
	a.mu.Lock()
	a.cache[p] = cached{ref: ref, mtime: st.ModTime()}
	a.mu.Unlock()
	return ref, ref.NativeID != ""
}

func clip(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) > n {
		return string(r[:n])
	}
	return string(r)
}

// ReadSession implements Adapter. Entries are read in file order; pi's
// branches (/tree) appear in the order they were written.
func (a *Adapter) ReadSession(ctx context.Context, ref kit.SessionRef, from model.Cursor) ([]model.Event, model.Cursor, error) {
	from.Agent = ID
	var out []model.Event
	cur, err := common.ReadLines(ref.Path, from, func(raw []byte, c *model.Cursor) {
		var e entry
		if json.Unmarshal(raw, &e) != nil || e.Type != "message" {
			return
		}
		for _, ev := range parseMessage(e, raw) {
			ev.Seq = c.NextSeq + 1
			c.NextSeq++
			out = append(out, ev)
		}
	})
	return out, cur, err
}

func parseMessage(e entry, raw []byte) []model.Event {
	var m message
	if json.Unmarshal(e.Message, &m) != nil {
		return nil
	}
	ts := common.ParseTime(e.Timestamp)
	ref := common.WithoutReasoning(raw, "message")
	mk := func(ev model.Event) model.Event { ev.TS, ev.NativeRef = ts, ref; return ev }
	var out []model.Event
	switch m.Role {
	case "user", "assistant":
		for _, b := range blocks(m.Content) {
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) != "" {
					out = append(out, mk(model.TextEvent(m.Role, b.Text, ts)))
				}
			case "image":
				out = append(out, mk(model.Event{Kind: model.KindMessage, Role: m.Role,
					Content: []model.ContentPart{{Type: "image_ref", Ref: "inline-image", Text: "[图片]"}}}))
			case "thinking", "redacted_thinking", "reasoning":
				out = append(out, model.Event{Kind: model.KindReasoning, Role: model.RoleAssistant, TS: ts})
			case "toolCall":
				t := ToCanonical(b.Name, b.Arguments)
				t.ID = b.ID
				out = append(out, mk(model.Event{Kind: model.KindToolCall, Role: model.RoleAssistant, Tool: t}))
				if fc := model.FileChangeFromTool(t); fc != nil {
					out = append(out, mk(model.Event{Kind: model.KindFileChange, Role: model.RoleAssistant, File: fc}))
				}
			}
		}
	case "toolResult":
		var text []string
		for _, b := range blocks(m.Content) {
			if b.Type == "text" {
				text = append(text, b.Text)
			}
		}
		status := "ok"
		if m.IsError {
			status = "error"
		}
		out = append(out, mk(model.Event{Kind: model.KindToolResult, Role: model.RoleTool,
			Tool: &model.ToolCall{ID: m.ToolCallID, NativeName: m.ToolName, Result: strings.Join(text, "\n"), Status: status}}))
	case "bashExecution": // a command the user ran directly with !
		t := &model.ToolCall{ID: e.ID, NativeName: "bash", CanonicalName: model.ToolShell, Args: common.MustJSON(map[string]any{"command": m.Command})}
		out = append(out, mk(model.Event{Kind: model.KindToolCall, Role: model.RoleUser, Tool: t}),
			mk(model.Event{Kind: model.KindToolResult, Role: model.RoleTool, Tool: &model.ToolCall{ID: e.ID, Result: m.Output, Status: "ok"}}))
	}
	return out
}

// ToCanonical maps a pi tool call to the unified vocabulary (pure).
func ToCanonical(name string, args json.RawMessage) *model.ToolCall {
	t := &model.ToolCall{NativeName: name, CanonicalName: model.ToolOpaque, Args: args}
	if o, ok := common.TakeOrigin(args); ok {
		t.CanonicalName, t.Args = o.Canonical, o.Args
		return t
	}
	var a map[string]any
	_ = json.Unmarshal(args, &a)
	str := func(k string) string { s, _ := a[k].(string); return s }
	switch name {
	case "read":
		t.CanonicalName, t.Args = model.ToolReadFile, common.MustJSON(dropEmpty(map[string]any{"path": str("path"), "offset": a["offset"], "limit": a["limit"]}))
	case "write":
		t.CanonicalName, t.Args = model.ToolWriteFile, common.MustJSON(map[string]any{"path": str("path"), "content": str("content")})
	case "edit":
		list, _ := a["edits"].([]any)
		if len(list) == 1 {
			e, _ := list[0].(map[string]any)
			t.CanonicalName, t.Args = model.ToolEditFile, common.MustJSON(map[string]any{"path": str("path"), "old": e["oldText"], "new": e["newText"]})
		} else {
			edits := make([]map[string]any, 0, len(list))
			for _, x := range list {
				e, _ := x.(map[string]any)
				edits = append(edits, map[string]any{"old_string": e["oldText"], "new_string": e["newText"]})
			}
			t.CanonicalName, t.Args = model.ToolEditFile, common.MustJSON(map[string]any{"path": str("path"), "edits": edits})
		}
	case "bash":
		t.CanonicalName, t.Args = model.ToolShell, common.MustJSON(map[string]any{"command": str("command")})
	default:
		if strings.HasPrefix(name, "mcp__") {
			if parts := strings.SplitN(name, "__", 3); len(parts) == 3 {
				t.CanonicalName = model.ToolMCPCall
				raw := args
				if len(raw) == 0 {
					raw = json.RawMessage(`{}`)
				}
				t.Args = common.MustJSON(map[string]any{"server": parts[1], "tool": parts[2], "args": raw})
			}
		}
	}
	return t
}

func dropEmpty(m map[string]any) map[string]any {
	for k, v := range m {
		if v == nil || v == "" {
			delete(m, k)
		}
	}
	return m
}

// FromCanonical maps a unified tool call to a pi tool (pure). ok is false
// when pi has no equivalent, and the conversion keeps it as text.
func FromCanonical(t *model.ToolCall) (string, json.RawMessage, bool) {
	var a map[string]any
	_ = json.Unmarshal(t.Args, &a)
	if a == nil {
		a = map[string]any{}
	}
	str := func(k string) string { s, _ := a[k].(string); return s }
	switch t.CanonicalName {
	case model.ToolReadFile:
		return "read", common.MustJSON(dropEmpty(map[string]any{"path": str("path"), "offset": a["offset"], "limit": a["limit"]})), true
	case model.ToolWriteFile:
		return "write", common.MustJSON(map[string]any{"path": str("path"), "content": str("content")}), true
	case model.ToolEditFile:
		var edits []map[string]any
		if list, ok := a["edits"].([]any); ok {
			for _, x := range list {
				e, _ := x.(map[string]any)
				edits = append(edits, map[string]any{"oldText": e["old_string"], "newText": e["new_string"]})
			}
		} else if str("patch") == "" && a["replace_all"] != true {
			edits = []map[string]any{{"oldText": str("old"), "newText": str("new")}}
		}
		if len(edits) == 0 {
			return "", nil, false
		}
		return "edit", common.MustJSON(map[string]any{"path": str("path"), "edits": edits}), true
	case model.ToolShell:
		return "bash", common.MustJSON(map[string]any{"command": str("command")}), true
	case model.ToolMCPCall:
		if str("server") == "" || str("tool") == "" {
			return "", nil, false
		}
		args, _ := json.Marshal(a["args"])
		return "mcp__" + str("server") + "__" + str("tool"), args, true
	}
	return "", nil, false
}

// CanRender implements adapterkit.ToolRenderer.
func (a *Adapter) CanRender(t *model.ToolCall) bool {
	_, _, ok := FromCanonical(t)
	return ok
}

// NewSessionID implements adapterkit.SessionIDMinter.
func (a *Adapter) NewSessionID() string { return fsutil.NewUUID() }

func entryID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// encodeCWD is pi's directory naming: no leading separator, separators and
// colons replaced by dashes, wrapped in "--".
func encodeCWD(cwd string) string {
	s := strings.TrimLeft(cwd, `/\`)
	s = strings.NewReplacer("/", "-", `\`, "-", ":", "-").Replace(s)
	return "--" + s + "--"
}

// lastModel is the provider and model of the newest session, so a migrated
// session opens with a model the user has set up.
func (a *Adapter) lastModel(ctx context.Context) (string, string) {
	refs, _ := a.ListSessions(ctx, kit.Filter{Limit: 10})
	for _, r := range refs {
		if p, m, ok := strings.Cut(r.Model, "/"); ok && p != "" && m != "" {
			return p, m
		}
	}
	return "", ""
}

// writer renders events as session entries chained by parentId.
type writer struct {
	buf      bytes.Buffer
	enc      *json.Encoder
	parent   string
	now      time.Time
	provider string
	model    string
	api      string
}

func newWriter(parent string, now time.Time, provider, mdl string) *writer {
	w := &writer{parent: parent, now: now, provider: provider, model: mdl, api: "openai-completions"}
	if w.provider == "" {
		w.provider, w.model = "bridge", "imported"
	}
	w.enc = json.NewEncoder(&w.buf)
	w.enc.SetEscapeHTML(false)
	return w
}

func (w *writer) ts(t time.Time) time.Time {
	if t.IsZero() || t.After(w.now) {
		return w.now
	}
	return t
}

func (w *writer) put(t time.Time, fields map[string]any) error {
	id := entryID()
	fields["id"] = id
	if w.parent == "" {
		fields["parentId"] = nil
	} else {
		fields["parentId"] = w.parent
	}
	fields["timestamp"] = w.ts(t).UTC().Format("2006-01-02T15:04:05.000Z")
	w.parent = id
	return w.enc.Encode(fields)
}

func (w *writer) message(t time.Time, m map[string]any) error {
	m["timestamp"] = w.ts(t).UnixMilli()
	return w.put(t, map[string]any{"type": "message", "message": m})
}

func zeroUsage() map[string]any {
	return map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "totalTokens": 0,
		"cost": map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "total": 0}}
}

func (w *writer) assistant(t time.Time, content []map[string]any, stop string) error {
	return w.message(t, map[string]any{"role": "assistant", "content": content, "api": w.api, "provider": w.provider,
		"model": w.model, "usage": zeroUsage(), "stopReason": stop})
}

func (w *writer) events(events []model.Event) error {
	for _, e := range events {
		var err error
		switch e.Kind {
		case model.KindMessage:
			text := e.Text()
			switch e.Role {
			case model.RoleAssistant:
				err = w.assistant(e.TS, []map[string]any{{"type": "text", "text": text}}, "stop")
			case model.RoleSystem:
				err = w.put(e.TS, map[string]any{"type": "custom_message", "customType": "bridge", "content": text, "display": true})
			default:
				err = w.message(e.TS, map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": text}}})
			}
		case model.KindToolCall:
			name, args, ok := FromCanonical(e.Tool)
			if !ok {
				return fmt.Errorf("tool %q reached writer without a mapping", e.Tool.CanonicalName)
			}
			var obj any
			_ = json.Unmarshal(args, &obj)
			err = w.assistant(e.TS, []map[string]any{{"type": "toolCall", "id": e.Tool.ID, "name": name, "arguments": obj}}, "toolUse")
		case model.KindToolResult:
			name := e.Tool.NativeName
			if name == "" {
				name = "bash"
			}
			err = w.message(e.TS, map[string]any{"role": "toolResult", "toolCallId": e.Tool.ID, "toolName": name,
				"content": []map[string]any{{"type": "text", "text": e.Tool.Result}}, "isError": e.Tool.Status == "error"})
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Render produces a whole session file (pure apart from random entry IDs).
func Render(s model.Session, id string, now time.Time, provider, mdl string, events []model.Event) ([]byte, error) {
	w := newWriter("", now, provider, mdl)
	if err := w.enc.Encode(map[string]any{"type": "session", "version": 3, "id": id,
		"timestamp": now.UTC().Format("2006-01-02T15:04:05.000Z"), "cwd": s.CWD}); err != nil {
		return nil, err
	}
	if provider != "" {
		if err := w.put(now, map[string]any{"type": "model_change", "provider": provider, "modelId": mdl}); err != nil {
			return nil, err
		}
	}
	if s.Title != "" {
		if err := w.put(now, map[string]any{"type": "session_info", "name": clip(s.Title, 80)}); err != nil {
			return nil, err
		}
	}
	if err := w.events(events); err != nil {
		return nil, err
	}
	return w.buf.Bytes(), nil
}

func sessionFiles(id string) ([]string, error) {
	return filepath.Glob(filepath.Join(sessionsDir(), "*", "*_"+id+".jsonl"))
}

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
		id = a.NewSessionID()
	}
	if existing, _ := sessionFiles(id); len(existing) > 0 {
		return kit.NativeHandle{}, fmt.Errorf("%s: %w", id, kit.ErrConflict)
	}
	now := time.Now().UTC()
	provider, mdl := a.lastModel(ctx)
	data, err := Render(s, id, now, provider, mdl, events)
	if err != nil {
		return kit.NativeHandle{}, err
	}
	stamp := fmt.Sprintf("%s-%03dZ", now.Format("2006-01-02T15-04-05"), now.Nanosecond()/int(time.Millisecond))
	path := filepath.Join(sessionsDir(), encodeCWD(s.CWD), stamp+"_"+id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return kit.NativeHandle{}, err
	}
	if err := fsutil.AtomicWrite(path, data, 0o600); err != nil {
		return kit.NativeHandle{}, err
	}
	return kit.NativeHandle{ID: id, Path: path, ResumeCommand: "cd " + common.ShellQuote(s.CWD) + " && pi --session " + id}, nil
}

// RemoveSession implements SessionRemover.
func (a *Adapter) RemoveSession(ctx context.Context, h kit.NativeHandle) error {
	return os.Remove(h.Path)
}

// RollbackSession implements adapterkit.SessionRecoverer.
func (a *Adapter) RollbackSession(ctx context.Context, s model.Session, started time.Time) error {
	files, err := sessionFiles(s.NativeID)
	if err != nil {
		return err
	}
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			return err
		}
		if st.ModTime().After(started.Add(common.RecoveryGrace)) {
			return fmt.Errorf("%s: %w", f, kit.ErrOwnership)
		}
	}
	for _, f := range files {
		if err := os.Remove(f); err != nil {
			return err
		}
	}
	return nil
}

// activeGrace is how recently a session file must have changed to count as
// in use; pi keeps no registry of running sessions.
const activeGrace = 60 * time.Second

// SessionActive implements adapterkit.SessionAppender.
func (a *Adapter) SessionActive(ref kit.SessionRef) bool {
	st, err := os.Stat(ref.Path)
	return err == nil && time.Since(st.ModTime()) < activeGrace
}

// leaf is the newest entry, which new entries continue from.
func leaf(path string) (string, string, string) {
	lines := common.TailLines(path, 1<<20)
	var id, provider, mdl string
	for i := len(lines) - 1; i >= 0; i-- {
		var e entry
		if json.Unmarshal(lines[i], &e) != nil {
			continue
		}
		if id == "" && e.ID != "" && e.Type != "session" {
			id = e.ID
		}
		if provider == "" && e.Type == "message" {
			var m message
			if json.Unmarshal(e.Message, &m) == nil && m.Role == "assistant" && m.Provider != "" {
				provider, mdl = m.Provider, m.Model
			}
		}
		if provider == "" && e.Type == "model_change" {
			provider, mdl = e.Provider, e.ModelID
		}
		if id != "" && provider != "" {
			break
		}
	}
	return id, provider, mdl
}

// RenderAppend implements adapterkit.SessionAppender: new entries continue
// the tree from its newest entry.
func (a *Adapter) RenderAppend(ctx context.Context, ref kit.SessionRef, events []model.Event) ([]byte, error) {
	parent, provider, mdl := leaf(ref.Path)
	w := newWriter(parent, time.Now().UTC(), provider, mdl)
	if err := w.events(events); err != nil {
		return nil, err
	}
	return w.buf.Bytes(), nil
}

// AfterAppend implements adapterkit.SessionAppender; pi has no index.
func (a *Adapter) AfterAppend(ctx context.Context, ref kit.SessionRef) error { return nil }
