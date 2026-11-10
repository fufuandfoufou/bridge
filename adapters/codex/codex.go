// Package codex is the built-in adapter for the Codex CLI and desktop app.
// Sessions live at ~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl and
// are indexed by session_index.jsonl and the state_N.sqlite threads table.
package codex

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // SQLite driver

	"bridge/adapters/internal/common"
	"bridge/internal/fsutil"
	"bridge/internal/model"
	"bridge/internal/versions"
	kit "bridge/pkg/adapterkit"
)

// ID is the adapter identifier.
const ID = "codex"

// verifiedVersions are the release lines whose format has been checked.
var verifiedVersions = []string{"0.144", "0.166"}

// Adapter implements adapterkit.Adapter for Codex.
type Adapter struct {
	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	ref   kit.SessionRef
	mtime time.Time
	skip  bool
}

// New creates the adapter.
func New() *Adapter { return &Adapter{cache: map[string]cached{}} }

// ID implements Adapter.
func (a *Adapter) ID() string { return ID }

func root() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	return filepath.Join(fsutil.Home(), ".codex")
}

// WatchRoots implements Watcher.
func (a *Adapter) WatchRoots() []string { return []string{filepath.Join(root(), "sessions")} }

// Detect implements Adapter.
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
	{Canonical: model.ToolReadFile, Native: "shell"},
	{Canonical: model.ToolWriteFile, Native: "apply_patch"},
	{Canonical: model.ToolEditFile, Native: "apply_patch"},
	{Canonical: model.ToolShell, Native: "shell"},
	{Canonical: model.ToolSearchText, Native: "shell"},
	{Canonical: model.ToolSearchFiles, Native: "shell"},
	{Canonical: model.ToolTodo, Native: "update_plan"},
	{Canonical: model.ToolWebSearch, Native: "web_search"},
	{Canonical: model.ToolMCPCall, Native: "mcp__"},
}

// Capabilities implements Adapter.
func (a *Adapter) Capabilities(v kit.Version) kit.Capabilities {
	return kit.Capabilities{
		SessionRead:  true,
		SessionWrite: common.VersionVerified(string(v), verifiedVersions) || versions.Allowed(ID, string(v)),
		LiveTail:     true,
		Hooks:        []kit.HookKind{kit.HookSessionStart, kit.HookPromptSubmit, kit.HookPreTool, kit.HookPostTool, kit.HookStop},
		MCP:          true,
		RulesFiles: []kit.PathSpec{
			{Scope: model.ScopeProject, Path: "AGENTS.md", Format: kit.FormatSource},
			{Scope: model.ScopeGlobal, Path: "~/.codex/AGENTS.md", Format: kit.FormatGenerate},
		},
		SkillsDirs: []kit.PathSpec{
			{Scope: model.ScopeProject, Path: ".agents/skills", Format: kit.FormatSource},
			{Scope: model.ScopeGlobal, Path: "~/.agents/skills", Format: kit.FormatSource, Collect: "~/.codex/skills"},
		},
		ContextWindow: 258400,
		Tools:         tools,
	}
}

type line struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type sessionMeta struct {
	ID         string          `json:"id"`
	Timestamp  string          `json:"timestamp"`
	CWD        string          `json:"cwd"`
	CLIVersion string          `json:"cli_version"`
	Source     json.RawMessage `json:"source"`
}

// ListSessions implements Adapter.
func (a *Adapter) ListSessions(ctx context.Context, f kit.Filter) ([]kit.SessionRef, error) {
	files, err := filepath.Glob(filepath.Join(root(), "sessions", "*", "*", "*", "rollout-*.jsonl"))
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

// head reads metadata; ok is false for subagent threads, which are not
// user-facing sessions, and for files without a session_meta header.
func (a *Adapter) head(p string, st os.FileInfo) (kit.SessionRef, bool) {
	a.mu.Lock()
	c, hit := a.cache[p]
	a.mu.Unlock()
	if hit && c.mtime.Equal(st.ModTime()) {
		return c.ref, !c.skip
	}
	ref := kit.SessionRef{Agent: ID, Path: p, UpdatedAt: st.ModTime(), Size: st.Size()}
	skip := true
	file, err := os.Open(p)
	if err != nil { // not cached: permissions can change without touching mtime
		ref.Unreadable = true
		ref.NativeID = filepath.Base(p)
		return ref, true
	}
	{
		defer file.Close()
		s := bufio.NewScanner(file)
		s.Buffer(make([]byte, 1<<20), 1<<26)
		for n := 0; s.Scan() && n < 200; n++ {
			var l line
			if json.Unmarshal(s.Bytes(), &l) != nil {
				continue
			}
			switch l.Type {
			case "session_meta":
				var m sessionMeta
				if json.Unmarshal(l.Payload, &m) == nil {
					ref.NativeID, ref.CWD, ref.Version = m.ID, m.CWD, m.CLIVersion
					ref.StartedAt = common.ParseTime(m.Timestamp)
					skip = m.ID == "" || bytes.HasPrefix(bytes.TrimSpace(m.Source), []byte("{"))
				}
			case "turn_context":
				var t struct {
					Model string `json:"model"`
				}
				if json.Unmarshal(l.Payload, &t) == nil && ref.Model == "" {
					ref.Model = t.Model
				}
			}
			if ref.NativeID != "" && ref.Model != "" {
				break
			}
		}
	}
	if c := latestCWD(p); c != "" {
		ref.CWD = c
	}
	a.mu.Lock()
	a.cache[p] = cached{ref: ref, mtime: st.ModTime(), skip: skip}
	a.mu.Unlock()
	return ref, !skip
}

// ReadSession implements Adapter. Malformed and unknown lines are skipped.
func (a *Adapter) ReadSession(ctx context.Context, ref kit.SessionRef, from model.Cursor) ([]model.Event, model.Cursor, error) {
	from.Agent = ID
	var out []model.Event
	cur, err := common.ReadLines(ref.Path, from, func(raw []byte, c *model.Cursor) {
		var l line
		if json.Unmarshal(raw, &l) != nil {
			return
		}
		if l.Type == "session_meta" {
			var m sessionMeta
			if json.Unmarshal(l.Payload, &m) == nil {
				c.FormatVersion = m.CLIVersion
			}
			return
		}
		if l.Type != "response_item" {
			return
		}
		for _, e := range ParseItem(l.Payload, common.ParseTime(l.Timestamp), raw) {
			e.Seq = c.NextSeq + 1
			c.NextSeq++
			out = append(out, e)
		}
	})
	return out, cur, err
}

type item struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Input     string          `json:"input"`
	CallID    string          `json:"call_id"`
	ID        string          `json:"id"`
	Output    json.RawMessage `json:"output"`
	Action    json.RawMessage `json:"action"`
}

// injectedPrefixes identify context Codex adds to user turns, not user text.
var injectedPrefixes = []string{"<environment_context>", "<user_instructions>", "# AGENTS.md instructions", "<turn_aborted>"}

// ParseItem converts one response_item payload to unified events (pure).
func ParseItem(payload json.RawMessage, ts time.Time, raw []byte) []model.Event {
	var it item
	if json.Unmarshal(payload, &it) != nil {
		return nil
	}
	ref := sanitize(raw)
	mk := func(e model.Event) model.Event { e.TS, e.NativeRef = ts, ref; return e }
	switch it.Type {
	case "message":
		if it.Role != "user" && it.Role != "assistant" {
			return nil
		}
		var parts []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ImageURL string `json:"image_url"`
		}
		_ = json.Unmarshal(it.Content, &parts)
		var out []model.Event
		for _, p := range parts {
			switch p.Type {
			case "input_text", "output_text", "text":
				if p.Text == "" || isInjected(p.Text) {
					continue
				}
				out = append(out, mk(model.TextEvent(it.Role, p.Text, ts)))
			case "input_image":
				out = append(out, mk(model.Event{Kind: model.KindMessage, Role: it.Role,
					Content: []model.ContentPart{{Type: "image_ref", Ref: "inline-image", Text: "[图片]"}}}))
			}
		}
		return out
	case "reasoning":
		return []model.Event{{Kind: model.KindReasoning, Role: model.RoleAssistant, TS: ts}}
	case "function_call":
		return callEvents(mk, ToCanonical(it.Name, []byte(it.Arguments)), it.CallID)
	case "custom_tool_call":
		args := common.MustJSON(map[string]any{"input": it.Input})
		return callEvents(mk, ToCanonical(it.Name, args), it.CallID)
	case "web_search_call":
		var act struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(it.Action, &act)
		t := &model.ToolCall{ID: it.ID, NativeName: "web_search", CanonicalName: model.ToolWebSearch,
			Args: common.MustJSON(map[string]any{"query": act.Query})}
		return []model.Event{
			mk(model.Event{Kind: model.KindToolCall, Role: model.RoleAssistant, Tool: t}),
			mk(model.Event{Kind: model.KindToolResult, Role: model.RoleTool, Tool: &model.ToolCall{ID: it.ID, Status: "ok"}}),
		}
	case "function_call_output", "custom_tool_call_output":
		text, st := outputText(it.Output)
		return []model.Event{mk(model.Event{Kind: model.KindToolResult, Role: model.RoleTool,
			Tool: &model.ToolCall{ID: it.CallID, Result: text, Status: st}})}
	}
	return nil
}

// sanitize drops encrypted reasoning, which any response item may carry.
func sanitize(raw []byte) json.RawMessage {
	if !bytes.Contains(raw, []byte("encrypted_content")) {
		return common.Compact(raw)
	}
	var top, payload map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil || json.Unmarshal(top["payload"], &payload) != nil {
		return nil
	}
	delete(payload, "encrypted_content")
	top["payload"] = common.MustJSON(payload)
	return common.MustJSON(top)
}

func callEvents(mk func(model.Event) model.Event, t *model.ToolCall, id string) []model.Event {
	t.ID = id
	out := []model.Event{mk(model.Event{Kind: model.KindToolCall, Role: model.RoleAssistant, Tool: t})}
	if fc := model.FileChangeFromTool(t); fc != nil {
		out = append(out, mk(model.Event{Kind: model.KindFileChange, Role: model.RoleAssistant, File: fc}))
	}
	return out
}

func isInjected(s string) bool {
	for _, p := range injectedPrefixes {
		if strings.HasPrefix(strings.TrimSpace(s), p) {
			return true
		}
	}
	return false
}

func outputText(raw json.RawMessage) (string, string) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, "ok"
	}
	var obj struct {
		Content string `json:"content"`
		Success *bool  `json:"success"`
	}
	if json.Unmarshal(raw, &obj) == nil && (obj.Content != "" || obj.Success != nil) {
		st := "ok"
		if obj.Success != nil && !*obj.Success {
			st = "error"
		}
		return obj.Content, st
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b []string
		for _, p := range parts {
			if p.Text != "" {
				b = append(b, p.Text)
			}
		}
		return strings.Join(b, "\n"), "ok"
	}
	return string(raw), "ok"
}

// ToCanonical maps a native tool call to the unified vocabulary (pure).
// The origin marker from a synthesized call wins over the surface name.
func ToCanonical(name string, args []byte) *model.ToolCall {
	t := &model.ToolCall{NativeName: name, CanonicalName: model.ToolOpaque, Args: args}
	if o, ok := common.TakeOrigin(args); ok {
		t.CanonicalName, t.Args = o.Canonical, o.Args
		return t
	}
	var a map[string]any
	_ = json.Unmarshal(args, &a)
	switch name {
	case "shell", "shell_command", "container.exec", "local_shell", "exec_command":
		t.CanonicalName = model.ToolShell
		t.Args = common.MustJSON(map[string]any{"command": commandString(a)})
	case "apply_patch":
		input, _ := a["input"].(string)
		if input == "" {
			input, _ = a["patch"].(string)
		}
		c, ar := ParsePatch(input)
		t.CanonicalName, t.Args = c, ar
	case "update_plan":
		list, _ := a["plan"].([]any)
		items := make([]map[string]any, 0, len(list))
		for _, it := range list {
			m, _ := it.(map[string]any)
			items = append(items, map[string]any{"content": m["step"], "status": m["status"]})
		}
		t.CanonicalName = model.ToolTodo
		t.Args = common.MustJSON(map[string]any{"items": items})
	case "web_search":
		t.CanonicalName = model.ToolWebSearch
	default:
		if strings.HasPrefix(name, "mcp__") {
			if parts := strings.SplitN(name, "__", 3); len(parts) == 3 {
				t.CanonicalName = model.ToolMCPCall
				t.Args = common.MustJSON(map[string]any{"server": parts[1], "tool": parts[2], "args": json.RawMessage(nonEmpty(args))})
			}
		}
	}
	return t
}

func nonEmpty(b []byte) []byte {
	if len(bytes.TrimSpace(b)) == 0 {
		return []byte(`{}`)
	}
	return b
}

func commandString(a map[string]any) string {
	if s, ok := a["cmd"].(string); ok {
		return s
	}
	switch c := a["command"].(type) {
	case string:
		return c
	case []any:
		var w []string
		for _, x := range c {
			s, _ := x.(string)
			w = append(w, s)
		}
		if len(w) == 3 && (w[0] == "bash" || w[0] == "zsh" || w[0] == "sh") && (w[1] == "-lc" || w[1] == "-c") {
			return w[2]
		}
		return strings.Join(w, " ")
	}
	return ""
}

// ParsePatch reverse-parses an apply_patch envelope. A patch adding one file
// becomes write_file, one hunk updating one file becomes edit_file with
// old/new strings, and anything larger stays edit_file with the raw patch.
func ParsePatch(patch string) (string, json.RawMessage) {
	var files []string
	var kind string
	var hunks int
	var old, neu, added []string
	for _, l := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(l, "*** Add File: "):
			files, kind = append(files, strings.TrimPrefix(l, "*** Add File: ")), "add"
		case strings.HasPrefix(l, "*** Update File: "):
			files, kind = append(files, strings.TrimPrefix(l, "*** Update File: ")), "update"
		case strings.HasPrefix(l, "*** Delete File: "):
			files, kind = append(files, strings.TrimPrefix(l, "*** Delete File: ")), "delete"
		case strings.HasPrefix(l, "***"):
		case strings.HasPrefix(l, "@@"):
			hunks++
		case kind == "add" && strings.HasPrefix(l, "+"):
			added = append(added, l[1:])
		case kind == "update" && strings.HasPrefix(l, "-"):
			old = append(old, l[1:])
		case kind == "update" && strings.HasPrefix(l, "+"):
			neu = append(neu, l[1:])
		case kind == "update" && strings.HasPrefix(l, " "):
			old, neu = append(old, l[1:]), append(neu, l[1:])
		}
	}
	if len(files) == 1 && kind == "add" {
		return model.ToolWriteFile, common.MustJSON(map[string]any{"path": files[0], "content": strings.Join(added, "\n")})
	}
	if len(files) == 1 && kind == "update" && hunks <= 1 {
		return model.ToolEditFile, common.MustJSON(map[string]any{"path": files[0], "old": strings.Join(old, "\n"), "new": strings.Join(neu, "\n")})
	}
	m := map[string]any{"patch": patch}
	if len(files) > 0 {
		m["path"] = files[0]
	}
	return model.ToolEditFile, common.MustJSON(m)
}

func patchLines(prefix, s string) string {
	return prefix + strings.ReplaceAll(s, "\n", "\n"+prefix)
}

// FromCanonical returns a native function name and argument JSON (pure).
// Calls without a dedicated Codex tool are synthesized and carry the marker.
func FromCanonical(t *model.ToolCall) (string, json.RawMessage, bool) {
	var a map[string]any
	_ = json.Unmarshal(t.Args, &a)
	if a == nil {
		a = map[string]any{}
	}
	str := func(k string) string { s, _ := a[k].(string); return s }
	marked := func(n string, m map[string]any) (string, json.RawMessage, bool) {
		return n, common.MustJSON(common.WithOrigin(m, t.CanonicalName, t.Args)), true
	}
	sh := func(cmd string) map[string]any { return map[string]any{"command": []string{"bash", "-lc", cmd}} }
	q := common.ShellQuote
	switch t.CanonicalName {
	case model.ToolShell:
		return "shell", common.MustJSON(sh(str("command"))), true
	case model.ToolReadFile:
		cmd := "cat " + q(str("path"))
		if off, ok := a["offset"].(float64); ok {
			end := "$"
			if l, ok := a["limit"].(float64); ok {
				end = strconv.Itoa(int(off + l - 1))
			}
			cmd = fmt.Sprintf("sed -n '%d,%sp' %s", int(off), end, q(str("path")))
		}
		return marked("shell", sh(cmd))
	case model.ToolSearchText:
		cmd := "rg -n " + q(str("pattern"))
		if g := str("glob"); g != "" {
			cmd += " -g " + q(g)
		}
		if p := str("path"); p != "" {
			cmd += " " + q(p)
		}
		return marked("shell", sh(cmd))
	case model.ToolSearchFiles:
		cmd := "rg --files -g " + q(str("pattern"))
		if p := str("path"); p != "" {
			cmd += " " + q(p)
		}
		return marked("shell", sh(cmd))
	case model.ToolWriteFile:
		patch := "*** Begin Patch\n*** Add File: " + str("path") + "\n" + patchLines("+", str("content")) + "\n*** End Patch"
		return marked("apply_patch", map[string]any{"input": patch})
	case model.ToolEditFile:
		if p := str("patch"); p != "" {
			return "apply_patch", common.MustJSON(map[string]any{"input": p}), true
		}
		if a["edits"] != nil {
			return "", nil, false // MultiEdit has no single-hunk rendering
		}
		patch := "*** Begin Patch\n*** Update File: " + str("path") + "\n@@\n" + patchLines("-", str("old")) + "\n" + patchLines("+", str("new")) + "\n*** End Patch"
		return marked("apply_patch", map[string]any{"input": patch})
	case model.ToolTodo:
		list, _ := a["items"].([]any)
		plan := make([]map[string]any, 0, len(list))
		for _, it := range list {
			m, _ := it.(map[string]any)
			plan = append(plan, map[string]any{"step": m["content"], "status": m["status"]})
		}
		return marked("update_plan", map[string]any{"plan": plan})
	case model.ToolWebSearch:
		return "web_search", common.MustJSON(map[string]any{"query": str("query")}), true
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

// WriteSession implements Adapter: the rollout file first, then both indexes.
// Any index failure removes what was already written.
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
	if s.Model == "" {
		s.Model = a.recentModel(ctx)
	}
	id := s.NativeID
	if id == "" {
		id = fsutil.NewUUID()
	}
	if existing, _ := rolloutFiles(id); len(existing) > 0 {
		return kit.NativeHandle{}, fmt.Errorf("%s: %w", id, kit.ErrConflict)
	}
	now := time.Now().UTC()
	data, first, err := Render(s, id, string(in.Version), now, events)
	if err != nil {
		return kit.NativeHandle{}, err
	}
	dir := filepath.Join(root(), "sessions", now.Format("2006"), now.Format("01"), now.Format("02"))
	path := filepath.Join(dir, "rollout-"+now.Format("2006-01-02T15-04-05")+"-"+id+".jsonl")
	if err := fsutil.AtomicWrite(path, data, 0o600); err != nil {
		return kit.NativeHandle{}, err
	}
	fsutil.Failpoint("codex.after_rollout")
	h := kit.NativeHandle{ID: id, Path: path, ResumeCommand: "cd " + common.ShellQuote(s.CWD) + " && codex resume " + id}
	title := s.Title
	if title == "" {
		title = first
	}
	title = clip(title, 80)
	if err := insertThread(ctx, id, path, s, string(in.Version), title, first, now); err != nil {
		_ = os.Remove(path)
		return kit.NativeHandle{}, fmt.Errorf("update codex thread index: %w", err)
	}
	fsutil.Failpoint("codex.after_thread")
	if err := appendIndex(id, title, now); err != nil {
		_ = deleteThread(ctx, id)
		_ = os.Remove(path)
		return kit.NativeHandle{}, fmt.Errorf("update codex session index: %w", err)
	}
	fsutil.Failpoint("codex.after_index")
	return h, nil
}

func rolloutFiles(id string) ([]string, error) {
	return filepath.Glob(filepath.Join(root(), "sessions", "*", "*", "*", "rollout-*-"+id+".jsonl"))
}

// RollbackSession implements adapterkit.SessionRecoverer.
func (a *Adapter) RollbackSession(ctx context.Context, s model.Session, started time.Time) error {
	files, err := rolloutFiles(s.NativeID)
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
	if err := deleteThread(ctx, s.NativeID); err != nil {
		return err
	}
	if err := removeIndex(s.NativeID); err != nil {
		return err
	}
	for _, f := range files {
		if err := os.Remove(f); err != nil {
			return err
		}
	}
	return nil
}

func (a *Adapter) recentModel(ctx context.Context) string {
	refs, _ := a.ListSessions(ctx, kit.Filter{Limit: 5})
	for _, r := range refs {
		if r.Model != "" {
			return r.Model
		}
	}
	return "gpt-5-codex"
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// RemoveSession implements SessionRemover.
func (a *Adapter) RemoveSession(ctx context.Context, h kit.NativeHandle) error {
	err := deleteThread(ctx, h.ID)
	if e := removeIndex(h.ID); err == nil {
		err = e
	}
	if e := os.Remove(h.Path); err == nil {
		err = e
	}
	return err
}

func indexPath() string { return filepath.Join(root(), "session_index.jsonl") }

func appendIndex(id, title string, now time.Time) error {
	path := indexPath()
	if _, err := fsutil.Backup(path); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	b := common.MustJSON(map[string]string{"id": id, "thread_name": title, "updated_at": now.Format("2006-01-02T15:04:05.000000Z")})
	_, err = f.Write(append(b, '\n'))
	return err
}

func removeIndex(id string) error {
	b, err := os.ReadFile(indexPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var out bytes.Buffer
	for _, l := range bytes.SplitAfter(b, []byte("\n")) {
		var e struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(l, &e) == nil && e.ID == id {
			continue
		}
		out.Write(l)
	}
	return fsutil.AtomicWrite(indexPath(), out.Bytes(), 0o600)
}

var stateRe = regexp.MustCompile(`state_(\d+)\.sqlite$`)

// stateDB returns the highest-numbered state database, or "" if none exists.
func stateDB() string {
	files, _ := filepath.Glob(filepath.Join(root(), "state_*.sqlite"))
	best, bestN := "", -1
	for _, f := range files {
		if m := stateRe.FindStringSubmatch(f); m != nil {
			if n, _ := strconv.Atoi(m[1]); n > bestN {
				best, bestN = f, n
			}
		}
	}
	return best
}

func openState(path string) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
}

// insertThread registers the session. It is a single-row transactional insert
// undone by deleteThread, so the live database is not copied beforehand.
// Only columns present in the installed schema are written.
func insertThread(ctx context.Context, id, path string, s model.Session, version, title, first string, now time.Time) error {
	dbPath := stateDB()
	if dbPath == "" {
		return nil // older installs have no state database
	}
	db, err := openState(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info('threads')`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		have[n] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if !have["id"] {
		return nil
	}
	sec, ms := now.Unix(), now.UnixMilli()
	vals := map[string]any{
		"id": id, "rollout_path": path, "created_at": sec, "updated_at": sec, "created_at_ms": ms, "updated_at_ms": ms,
		"recency_at": sec, "recency_at_ms": ms, "source": "cli", "model_provider": "openai", "cwd": s.CWD, "title": title,
		"sandbox_policy": `{"type":"read-only"}`, "approval_mode": "on-request", "has_user_event": 1, "cli_version": version,
		"first_user_message": clip(first, 200), "preview": clip(first, 200), "model": s.Model, "thread_source": "user", "originator": "bridge",
	}
	var cols, marks []string
	var args []any
	for k, v := range vals {
		if have[k] {
			cols, marks, args = append(cols, `"`+k+`"`), append(marks, "?"), append(args, v)
		}
	}
	_, err = db.ExecContext(ctx, `INSERT INTO threads(`+strings.Join(cols, ",")+`) VALUES(`+strings.Join(marks, ",")+`)`, args...)
	return err
}

func deleteThread(ctx context.Context, id string) error {
	dbPath := stateDB()
	if dbPath == "" {
		return nil
	}
	db, err := openState(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, `DELETE FROM threads WHERE id=?`, id)
	return err
}

// Render produces the rollout JSONL and the first user message (pure apart
// from the supplied id and clock).
func Render(s model.Session, id, version string, now time.Time, events []model.Event) ([]byte, string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	ordinal := 0
	write := func(ts time.Time, typ string, payload any) error {
		if ts.IsZero() {
			ts = now
		}
		ordinal++
		return enc.Encode(map[string]any{"timestamp": ts.UTC().Format("2006-01-02T15:04:05.000Z"), "ordinal": ordinal, "type": typ, "payload": payload})
	}
	if err := write(now, "session_meta", map[string]any{
		"id": id, "timestamp": now.Format("2006-01-02T15:04:05.000Z"), "cwd": s.CWD, "originator": "bridge",
		"cli_version": version, "source": "cli", "model_provider": "openai", "thread_source": "user",
	}); err != nil {
		return nil, "", err
	}
	if err := write(now, "turn_context", map[string]any{
		"cwd": s.CWD, "approval_policy": "on-request", "sandbox_policy": map[string]string{"type": "read-only"},
		"model": s.Model, "summary": "auto",
	}); err != nil {
		return nil, "", err
	}
	first := ""
	for _, e := range events {
		var err error
		switch e.Kind {
		case model.KindMessage:
			text := e.Text()
			switch e.Role {
			case model.RoleAssistant:
				err = write(e.TS, "response_item", map[string]any{"type": "message", "role": "assistant",
					"content": []map[string]string{{"type": "output_text", "text": text}}})
				if err == nil {
					err = write(e.TS, "event_msg", map[string]any{"type": "agent_message", "message": text})
				}
			case model.RoleSystem:
				err = write(e.TS, "response_item", map[string]any{"type": "message", "role": "developer",
					"content": []map[string]string{{"type": "input_text", "text": text}}})
			default:
				if first == "" {
					first = text
				}
				err = write(e.TS, "response_item", map[string]any{"type": "message", "role": "user",
					"content": []map[string]string{{"type": "input_text", "text": text}}})
				if err == nil {
					err = write(e.TS, "event_msg", map[string]any{"type": "user_message", "message": text, "images": []string{}})
				}
			}
		case model.KindToolCall:
			name, args, ok := FromCanonical(e.Tool)
			if !ok {
				return nil, "", fmt.Errorf("tool %q reached writer without a mapping", e.Tool.CanonicalName)
			}
			err = write(e.TS, "response_item", map[string]any{"type": "function_call", "name": name, "arguments": string(args), "call_id": e.Tool.ID})
		case model.KindToolResult:
			err = write(e.TS, "response_item", map[string]any{"type": "function_call_output", "call_id": e.Tool.ID, "output": e.Tool.Result})
		}
		if err != nil {
			return nil, "", err
		}
	}
	return buf.Bytes(), first, nil
}

var hookSpecs = []common.HookSpec{
	{Native: "SessionStart", Bridge: "session_start"},
	{Native: "UserPromptSubmit", Bridge: "prompt_submit"},
	{Native: "PreToolUse", Bridge: "pre_tool"},
	{Native: "PostToolUse", Bridge: "post_tool"},
	{Native: "Stop", Bridge: "stop"},
}

func hooksPath() string  { return filepath.Join(root(), "hooks.json") }
func configPath() string { return filepath.Join(root(), "config.toml") }

const (
	mcpBegin = "# bridge:begin mcp (managed by bridge)"
	mcpEnd   = "# bridge:end mcp"
)

// InstallHooks implements Adapter; it is idempotent.
func (a *Adapter) InstallHooks(ctx context.Context, cfg kit.HookConfig) error {
	if err := common.EditHooksFile(hooksPath(), cfg.Command, ID, hookSpecs); err != nil {
		return err
	}
	return editConfig(cfg.MCPCommand)
}

// UninstallHooks implements Adapter.
func (a *Adapter) UninstallHooks(ctx context.Context) error {
	if err := common.EditHooksFile(hooksPath(), "", ID, nil); err != nil {
		return err
	}
	return editConfig(nil)
}

// editConfig maintains a marked block in config.toml, so the user's comments
// and formatting stay byte-for-byte intact.
func editConfig(argv []string) error {
	b, err := os.ReadFile(configPath())
	if os.IsNotExist(err) && len(argv) == 0 {
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	text := string(b)
	if i := strings.Index(text, mcpBegin); i >= 0 {
		if j := strings.Index(text[i:], mcpEnd); j >= 0 {
			text = strings.TrimRight(text[:i], "\n") + "\n" + strings.TrimLeft(text[i+j+len(mcpEnd):], "\n")
			if strings.TrimSpace(text) == "" {
				text = ""
			}
		}
	}
	if len(argv) > 0 {
		q := func(s string) string { return strconv.Quote(s) }
		args := make([]string, 0, len(argv)-1)
		for _, x := range argv[1:] {
			args = append(args, q(x))
		}
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text += fmt.Sprintf("\n%s\n[mcp_servers.bridge]\ncommand = %s\nargs = [%s]\n%s\n", mcpBegin, q(argv[0]), strings.Join(args, ", "), mcpEnd)
	}
	if text == string(b) {
		return nil
	}
	return fsutil.SafeWrite(configPath(), []byte(text), 0o600)
}

// HooksInstalled implements adapterkit.HookChecker.
func (a *Adapter) HooksInstalled() bool { return common.HooksInstalled(hooksPath()) }

// HooksTrusted asks Codex for its own trust decision. If the installed client
// cannot report it, fail closed instead of treating a stale hash as approval.
func (a *Adapter) HooksTrusted() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "codex", "app-server")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return false
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false
	}
	if err = cmd.Start(); err != nil {
		return false
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	enc := json.NewEncoder(stdin)
	requests := []any{
		map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]string{"name": "bridge", "version": "1"}, "capabilities": map[string]bool{"experimentalApi": true}}},
		map[string]any{"method": "initialized"},
		map[string]any{"id": 2, "method": "hooks/list", "params": map[string]any{"cwds": []string{fsutil.Home()}}},
	}
	for _, req := range requests {
		if enc.Encode(req) != nil {
			return false
		}
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	for scanner.Scan() {
		var response struct {
			ID     int             `json:"id"`
			Error  json.RawMessage `json:"error"`
			Result struct {
				Data []struct {
					Errors []json.RawMessage `json:"errors"`
					Hooks  []struct {
						EventName   string `json:"eventName"`
						SourcePath  string `json:"sourcePath"`
						Command     string `json:"command"`
						Enabled     bool   `json:"enabled"`
						TrustStatus string `json:"trustStatus"`
					} `json:"hooks"`
				} `json:"data"`
			} `json:"result"`
		}
		if json.Unmarshal(scanner.Bytes(), &response) != nil || response.ID != 2 {
			continue
		}
		if len(response.Error) > 0 || len(response.Result.Data) == 0 {
			return false
		}
		found := map[string]bool{}
		for _, group := range response.Result.Data {
			if len(group.Errors) > 0 {
				return false
			}
			for _, hook := range group.Hooks {
				if filepath.Clean(hook.SourcePath) != filepath.Clean(hooksPath()) || !common.IsBridgeCommand(hook.Command) {
					continue
				}
				if !hook.Enabled || (hook.TrustStatus != "trusted" && hook.TrustStatus != "managed") {
					return false
				}
				found[hook.EventName] = true
			}
		}
		return found["sessionStart"] && found["userPromptSubmit"] && found["preToolUse"] && found["postToolUse"] && found["stop"]
	}
	return false
}

// ProbeVersion implements adapterkit.VersionProber: the real client's
// app-server loads a session written to a throwaway CODEX_HOME. Reading a
// thread needs neither a login nor a model call.
func (a *Adapter) ProbeVersion(ctx context.Context, version string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	bin, err := common.FindBinary(ctx, version, []string{"codex", "/opt/homebrew/bin/codex", "/usr/local/bin/codex",
		"/Applications/ChatGPT.app/Contents/Resources/codex-cli/bin/codex", "/Applications/Codex.app/Contents/Resources/codex"})
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
	id := fsutil.NewUUID()
	const asked, answered = "bridge-probe-question", "bridge-probe-answer"
	data, _, err := Render(model.Session{CWD: home, Model: "probe"}, id, version, now, []model.Event{
		model.TextEvent(model.RoleUser, asked, now), model.TextEvent(model.RoleAssistant, answered, now)})
	if err != nil {
		return err
	}
	p := filepath.Join(home, "sessions", now.Format("2006"), now.Format("01"), now.Format("02"),
		"rollout-"+now.Format("2006-01-02T15-04-05")+"-"+id+".jsonl")
	if err := fsutil.AtomicWrite(p, data, 0o600); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, bin, "app-server")
	cmd.Env = common.ProbeEnv("HOME="+home, "CODEX_HOME="+home)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	r := bufio.NewReaderSize(stdout, 1<<20)
	call := func(n int, method string, params any) (string, error) {
		b, _ := json.Marshal(map[string]any{"id": n, "method": method, "params": params})
		if _, err := stdin.Write(append(b, '\n')); err != nil {
			return "", err
		}
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return "", fmt.Errorf("app-server: %w", err)
			}
			var m struct {
				ID    *int            `json:"id"`
				Error json.RawMessage `json:"error"`
			}
			if json.Unmarshal([]byte(line), &m) == nil && m.ID != nil && *m.ID == n {
				if len(m.Error) > 0 {
					return "", fmt.Errorf("%s: %s", method, m.Error)
				}
				return line, nil
			}
		}
	}
	if _, err := call(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "bridge", "title": "bridge", "version": "1"}}); err != nil {
		return err
	}
	if _, err := stdin.Write([]byte(`{"method":"initialized"}` + "\n")); err != nil {
		return err
	}
	out, err := call(2, "thread/read", map[string]any{"threadId": id, "includeTurns": true})
	if err != nil {
		return err
	}
	if !strings.Contains(out, asked) || !strings.Contains(out, answered) {
		return errors.New("客户端加载了探测会话，但历史内容缺失")
	}
	return nil
}

// latestCWD returns the working directory of the newest turn.
func latestCWD(p string) string {
	lines := common.TailLines(p, 1<<20)
	for i := len(lines) - 1; i >= 0; i-- {
		var l line
		if json.Unmarshal(lines[i], &l) != nil || l.Type != "turn_context" {
			continue
		}
		var t struct {
			CWD string `json:"cwd"`
		}
		if json.Unmarshal(l.Payload, &t) == nil && t.CWD != "" {
			return t.CWD
		}
	}
	return ""
}

// activeGrace is how recently a rollout must have changed to count as in use;
// Codex has no registry of running threads to consult.
const activeGrace = 60 * time.Second

// SessionActive implements adapterkit.SessionAppender.
func (a *Adapter) SessionActive(ref kit.SessionRef) bool {
	st, err := os.Stat(ref.Path)
	return err == nil && time.Since(st.ModTime()) < activeGrace
}

// historyMode returns the thread's storage mode, or "" when not indexed.
func historyMode(ctx context.Context, id string) (string, error) {
	path := stateDB()
	if path == "" {
		return "", nil
	}
	db, err := openState(path)
	if err != nil {
		return "", err
	}
	defer db.Close()
	var mode string
	err = db.QueryRowContext(ctx, `SELECT history_mode FROM threads WHERE id=?`, id).Scan(&mode)
	if err == sql.ErrNoRows || (err != nil && strings.Contains(err.Error(), "no such")) {
		return "", nil
	}
	return mode, err
}

// RenderAppend implements adapterkit.SessionAppender. Both storage modes
// grow by appending to the rollout: a paginated thread's history pages are an
// index Codex builds from the rollout, continuing from where it stopped, so
// appended turns only need the structure Codex writes itself.
func (a *Adapter) RenderAppend(ctx context.Context, ref kit.SessionRef, events []model.Event) ([]byte, error) {
	mode, err := historyMode(ctx, ref.NativeID)
	if err != nil {
		return nil, err
	}
	if mode == "" {
		mode = rolloutMode(ref.Path)
	}
	switch mode {
	case "paginated":
		return renderPaginated(ref, lastOrdinal(ref.Path), time.Now().UTC(), events)
	case "", "legacy":
	default:
		return nil, kit.ErrNotSupported
	}
	last := 0
	lines := common.TailLines(ref.Path, 1<<20)
	for i := len(lines) - 1; i >= 0; i-- {
		var l struct {
			Ordinal int `json:"ordinal"`
		}
		if json.Unmarshal(lines[i], &l) == nil && l.Ordinal > 0 {
			last = l.Ordinal
			break
		}
	}
	data, _, err := Render(model.Session{CWD: ref.CWD, Model: ref.Model}, ref.NativeID, ref.Version, time.Now().UTC(), events)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	for i, l := range bytes.SplitAfter(data, []byte("\n")) {
		if i < 2 || len(bytes.TrimSpace(l)) == 0 { // session_meta and turn_context
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(l, &m); err != nil {
			return nil, err
		}
		last++
		m["ordinal"] = common.MustJSON(last)
		out.Write(common.MustJSON(m))
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

// AfterAppend implements adapterkit.SessionAppender, moving the thread to the
// top of Codex's recent lists.
func (a *Adapter) AfterAppend(ctx context.Context, ref kit.SessionRef) error {
	path := stateDB()
	if path == "" {
		return nil
	}
	db, err := openState(path)
	if err != nil {
		return err
	}
	defer db.Close()
	now := time.Now()
	_, err = db.ExecContext(ctx, `UPDATE threads SET updated_at=?, updated_at_ms=?, recency_at=?, recency_at_ms=? WHERE id=?`,
		now.Unix(), now.UnixMilli(), now.Unix(), now.UnixMilli(), ref.NativeID)
	if err != nil && strings.Contains(err.Error(), "no such") {
		return nil
	}
	return err
}
