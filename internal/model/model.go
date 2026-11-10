// Package model defines the unified, agent-neutral data model. Sessions are
// ordered event streams; memories and collaboration state derive from events.
// The model only evolves through backward-compatible extensions.
package model

import (
	"encoding/json"
	"strings"
	"time"
)

// SchemaVersion is the version of the unified model.
const SchemaVersion = 1

// EventKind classifies an event.
type EventKind string

const (
	KindMessage    EventKind = "message"
	KindToolCall   EventKind = "tool_call"
	KindToolResult EventKind = "tool_result"
	KindFileChange EventKind = "file_change"
	KindReasoning  EventKind = "reasoning_stub" // placeholder only, never content
	KindMeta       EventKind = "meta"
)

// Roles used by events.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleSystem    = "system"
	RoleTool      = "tool"
)

// Canonical tool vocabulary (v1).
const (
	ToolReadFile    = "read_file"
	ToolWriteFile   = "write_file"
	ToolEditFile    = "edit_file"
	ToolShell       = "shell"
	ToolSearchText  = "search_text"
	ToolSearchFiles = "search_files"
	ToolWebFetch    = "web_fetch"
	ToolWebSearch   = "web_search"
	ToolTodo        = "todo"
	ToolSubagent    = "subagent"
	ToolMCPCall     = "mcp_call"
	ToolOpaque      = "opaque"
)

// ContentPart is a piece of message content.
type ContentPart struct {
	Type string `json:"type"` // text | image_ref
	Text string `json:"text,omitempty"`
	Ref  string `json:"ref,omitempty"`
}

// ToolCall describes one tool invocation; a tool_call event and its
// tool_result event share the same ID.
type ToolCall struct {
	ID            string          `json:"id"`
	CanonicalName string          `json:"canonical_name"`
	NativeName    string          `json:"native_name"`
	Args          json.RawMessage `json:"args,omitempty"`
	Result        string          `json:"result,omitempty"`
	Status        string          `json:"status,omitempty"` // ok | error | interrupted
}

// FileChange records a file modification made by a tool.
type FileChange struct {
	Path   string `json:"path"`
	Op     string `json:"op"` // create | modify | delete
	Diff   string `json:"diff,omitempty"`
	GitSHA string `json:"git_sha,omitempty"`
}

// Event is one entry of a session.
type Event struct {
	SessionID string          `json:"session_id"`
	Seq       int64           `json:"seq"`
	Kind      EventKind       `json:"kind"`
	Role      string          `json:"role,omitempty"`
	Content   []ContentPart   `json:"content,omitempty"`
	Tool      *ToolCall       `json:"tool,omitempty"`
	File      *FileChange     `json:"file,omitempty"`
	TS        time.Time       `json:"ts"`
	NativeRef json.RawMessage `json:"native_ref,omitempty"`
}

// Text concatenates the text parts of the event.
func (e Event) Text() string {
	var parts []string
	for _, p := range e.Content {
		if p.Text != "" {
			parts = append(parts, p.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// TextEvent builds a plain message event.
func TextEvent(role, text string, ts time.Time) Event {
	return Event{Kind: KindMessage, Role: role, TS: ts, Content: []ContentPart{{Type: "text", Text: text}}}
}

// Project is identified by git remote + repository root.
type Project struct {
	ID        string `json:"id"`
	Path      string `json:"path"` // the main checkout
	GitRemote string `json:"git_remote,omitempty"`
	// Worktree is the checkout the directory is in: Path itself, or a linked
	// git worktree of the same repository. Not stored.
	Worktree string `json:"-"`
}

// Checkout returns the root of the working tree in use.
func (p Project) Checkout() string {
	if p.Worktree != "" {
		return p.Worktree
	}
	return p.Path
}

// Session is a conversation of one agent.
type Session struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id,omitempty"`
	Agent     string    `json:"agent"`
	NativeID  string    `json:"native_id"`
	Model     string    `json:"model,omitempty"`
	CWD       string    `json:"cwd,omitempty"`
	Title     string    `json:"title,omitempty"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	Parent    string    `json:"parent,omitempty"` // handoff lineage
}

// Memory statuses.
const (
	MemoryActive     = "active"
	MemoryPending    = "pending"
	MemorySuperseded = "superseded"
	MemoryForgotten  = "forgotten"
)

// Memory scopes.
const (
	ScopeGlobal  = "global"
	ScopeProject = "project"
)

// Memory is a durable fact traceable to the events that produced it.
type Memory struct {
	ID           string    `json:"id"`
	Scope        string    `json:"scope"`
	ProjectID    string    `json:"project_id,omitempty"`
	Category     string    `json:"category"`
	Content      string    `json:"content"`
	SourceEvents []string  `json:"source_events,omitempty"`
	Confidence   float64   `json:"confidence"`
	Supersedes   string    `json:"supersedes,omitempty"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
}

// Artifact tracks the source and replica state of a synced file.
type Artifact struct {
	Kind          string `json:"kind"` // rule | skill | mcp_config
	CanonicalPath string `json:"canonical_path"`
	Hash          string `json:"hash"`
}

// Cursor is a resumable incremental read position.
type Cursor struct {
	Agent         string `json:"agent"`
	NativeFile    string `json:"native_file"`
	Offset        int64  `json:"offset"`
	Inode         uint64 `json:"inode"`
	FormatVersion string `json:"format_version,omitempty"`
	// NextSeq is the sequence number the next event read will receive.
	NextSeq int64 `json:"next_seq"`
}

// EstimateTokens approximates token usage: CJK runes count one token each,
// other text about four characters per token.
func EstimateTokens(s string) int {
	cjk, other := 0, 0
	for _, r := range s {
		if r >= 0x2E80 {
			cjk++
		} else {
			other++
		}
	}
	return cjk + (other+3)/4
}

// EventTokens estimates the tokens consumed by an event.
func EventTokens(e Event) int {
	n := EstimateTokens(e.Text())
	if e.Tool != nil {
		n += EstimateTokens(string(e.Tool.Args)) + EstimateTokens(e.Tool.Result)
	}
	return n
}

// FileChangeFromTool derives a file change from a canonical tool call, or nil
// when the tool does not edit files.
func FileChangeFromTool(t *ToolCall) *FileChange {
	if t == nil {
		return nil
	}
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Old     string `json:"old"`
		New     string `json:"new"`
		Patch   string `json:"patch"`
	}
	_ = json.Unmarshal(t.Args, &a)
	if a.Path == "" {
		return nil
	}
	switch t.CanonicalName {
	case ToolWriteFile:
		return &FileChange{Path: a.Path, Op: "create", Diff: prefixLines("+", a.Content)}
	case ToolEditFile:
		if a.Patch != "" {
			return &FileChange{Path: a.Path, Op: "modify", Diff: a.Patch}
		}
		return &FileChange{Path: a.Path, Op: "modify", Diff: prefixLines("-", a.Old) + "\n" + prefixLines("+", a.New)}
	}
	return nil
}

func prefixLines(p, s string) string {
	if s == "" {
		return ""
	}
	return p + strings.ReplaceAll(s, "\n", "\n"+p)
}
