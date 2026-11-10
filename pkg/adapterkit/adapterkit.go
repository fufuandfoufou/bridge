// Package adapterkit is the adapter SDK: the capability declaration, the
// Adapter interface every agent integration implements, and a conformance
// suite. Core code only depends on these types, never on a concrete agent.
package adapterkit

import (
	"context"
	"errors"
	"sort"
	"time"

	"bridge/internal/model"
)

// ProtocolVersion is the external adapter JSON-RPC protocol version.
const ProtocolVersion = "1"

// HookKind enumerates the lifecycle hooks an agent may offer.
type HookKind string

const (
	HookSessionStart HookKind = "session_start"
	HookPromptSubmit HookKind = "prompt_submit"
	HookPreTool      HookKind = "pre_tool"
	HookPostTool     HookKind = "post_tool"
	HookStop         HookKind = "stop"
)

// Replica strategies for rule files and skills.
const (
	FormatSymlink  = "symlink"
	FormatGenerate = "generate"
	FormatSource   = "source" // the agent reads the source location directly
)

// PathSpec names a rule file or skill directory. A leading ~ means home;
// otherwise the path is relative to the project root.
type PathSpec struct {
	Scope  string `json:"scope"` // global | project
	Path   string `json:"path"`
	Format string `json:"format,omitempty"`
}

// ToolMapping pairs a canonical tool name with the native name.
type ToolMapping struct {
	Canonical string `json:"canonical"`
	Native    string `json:"native"`
}

// Capabilities declares what an adapter supports for one agent version.
type Capabilities struct {
	SessionRead   bool          `json:"session_read"`
	SessionWrite  bool          `json:"session_write"`
	LiveTail      bool          `json:"live_tail"`
	Hooks         []HookKind    `json:"hooks,omitempty"`
	MCP           bool          `json:"mcp"`
	RulesFiles    []PathSpec    `json:"rules_files,omitempty"`
	SkillsDirs    []PathSpec    `json:"skills_dirs,omitempty"`
	ContextWindow int           `json:"context_window"`
	Tools         []ToolMapping `json:"tools,omitempty"`
}

// HasHook reports whether hook k is available.
func (c Capabilities) HasHook(k HookKind) bool {
	for _, h := range c.Hooks {
		if h == k {
			return true
		}
	}
	return false
}

// SupportsTool reports whether a canonical tool maps to a native tool.
func (c Capabilities) SupportsTool(canonical string) bool {
	for _, t := range c.Tools {
		if t.Canonical == canonical {
			return true
		}
	}
	return false
}

// Version is an agent version string; empty means unknown.
type Version string

// Install is the result of detecting an agent.
type Install struct {
	Installed bool    `json:"installed"`
	Version   Version `json:"version"`
	DataDir   string  `json:"data_dir"`
}

// Filter narrows session listing.
type Filter struct {
	CWD   string    `json:"cwd,omitempty"`
	Since time.Time `json:"since,omitempty"`
	Limit int       `json:"limit,omitempty"`
}

// SessionRef locates a native session without loading it.
type SessionRef struct {
	Agent     string    `json:"agent"`
	NativeID  string    `json:"native_id"`
	Path      string    `json:"path"`
	CWD       string    `json:"cwd,omitempty"`
	Model     string    `json:"model,omitempty"`
	Title     string    `json:"title,omitempty"`
	Version   string    `json:"version,omitempty"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Size      int64     `json:"size,omitempty"`
	// Unreadable marks a session file that exists but cannot be opened.
	Unreadable bool `json:"unreadable,omitempty"`
}

// NativeHandle identifies a session written to the target agent.
type NativeHandle struct {
	ID            string `json:"id"`
	Path          string `json:"path"`
	ResumeCommand string `json:"resume_command"`
}

// HookConfig tells an adapter what to install.
type HookConfig struct {
	Command    string   `json:"command"`     // path to the bridge binary
	MCPCommand []string `json:"mcp_command"` // argv launching the stdio MCP server
}

// Adapter is the contract implemented once per agent.
type Adapter interface {
	ID() string
	Detect(ctx context.Context) (Install, error)
	Capabilities(v Version) Capabilities
	ListSessions(ctx context.Context, f Filter) ([]SessionRef, error)
	ReadSession(ctx context.Context, ref SessionRef, from model.Cursor) ([]model.Event, model.Cursor, error)
	WriteSession(ctx context.Context, s model.Session, events []model.Event) (NativeHandle, error)
	InstallHooks(ctx context.Context, cfg HookConfig) error
	UninstallHooks(ctx context.Context) error
}

// SessionRemover is optionally implemented to undo a WriteSession, including
// any session index entries, when post-write verification fails.
type SessionRemover interface {
	RemoveSession(ctx context.Context, h NativeHandle) error
}

// ToolRenderer is optionally implemented when support depends on a call's
// arguments, not only its canonical name. Calls it rejects degrade to text.
type ToolRenderer interface {
	CanRender(t *model.ToolCall) bool
}

// SessionRecoverer is optionally implemented to undo a write interrupted by a
// crash. It must refuse with ErrOwnership when the objects may have been
// modified since started.
type SessionRecoverer interface {
	RollbackSession(ctx context.Context, s model.Session, started time.Time) error
}

// SessionIDMinter is optionally implemented when an agent has its own session
// ID convention. WriteSession uses a Session.NativeID it is given.
type SessionIDMinter interface {
	NewSessionID() string
}

// HookChecker is optionally implemented so doctor can verify installation.
type HookChecker interface {
	HooksInstalled() bool
}

// SessionAppender is optionally implemented when an existing session can take
// new history. RenderAppend returns ErrNotSupported for sessions in a format
// that can't be extended.
type SessionAppender interface {
	SessionActive(ref SessionRef) bool
	RenderAppend(ctx context.Context, ref SessionRef, events []model.Event) ([]byte, error)
	AfterAppend(ctx context.Context, ref SessionRef) error
}

// SessionWriteLocker holds an agent's native cross-process lease while Bridge
// appends, verifies, or recovers a session. Closing the process releases it.
type SessionWriteLocker interface {
	LockSession(ref SessionRef) (release func(), err error)
}

// SessionAppendValidator validates native framing instead of the default JSONL
// newline check, for example when the file contains compressed frames.
type SessionAppendValidator interface {
	ValidateAppend(ref SessionRef) error
}

// VersionProber is optionally implemented when an adapter can check, with the
// real client and without touching user data, that an agent version loads the
// sessions it writes.
type VersionProber interface {
	ProbeVersion(ctx context.Context, version string) error
}

// HookTrustChecker is optionally implemented by agents that run hooks only
// after the user has approved them.
type HookTrustChecker interface {
	HooksTrusted() bool
}

// Watcher is optionally implemented to expose directories for live tailing.
type Watcher interface {
	WatchRoots() []string
}

// Errors shared by adapters.
var (
	ErrUnsupportedVersion = errors.New("agent version is not verified for writing; refusing to write")
	ErrNotSupported       = errors.New("operation not supported by this adapter")
	ErrConflict           = errors.New("a session with this id already exists")
	ErrOwnership          = errors.New("interrupted write may have been modified since; left in place")
)

// Registry holds the available adapters.
type Registry struct{ m map[string]Adapter }

// NewRegistry builds a registry; a later adapter with the same ID wins.
func NewRegistry(as ...Adapter) *Registry {
	r := &Registry{m: map[string]Adapter{}}
	for _, a := range as {
		r.Add(a)
	}
	return r
}

// Add registers an adapter.
func (r *Registry) Add(a Adapter) { r.m[a.ID()] = a }

// Get returns an adapter by ID.
func (r *Registry) Get(id string) (Adapter, bool) { a, ok := r.m[id]; return a, ok }

// All returns adapters sorted by ID.
func (r *Registry) All() []Adapter {
	out := make([]Adapter, 0, len(r.m))
	for _, a := range r.m {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

// Effective describes the channels actually in effect for an agent, per the
// degradation matrix in the design document.
type Effective struct {
	Handoff string `json:"handoff"`
	Live    string `json:"live"`
	Memory  string `json:"memory"`
}

// Degrade evaluates the degradation matrix for capabilities c.
func Degrade(c Capabilities) Effective {
	hooks := c.HasHook(HookSessionStart) && c.HasHook(HookPromptSubmit)
	e := Effective{}
	switch {
	case c.SessionWrite:
		e.Handoff = "原生写回"
	case !hooks:
		e.Handoff = "生成交接文档，用户手动引用"
	default:
		e.Handoff = "生成交接文档，启动时注入"
	}
	switch {
	case hooks && c.MCP:
		e.Live, e.Memory = "hook 注入增量 + MCP 查询", "hook 启动注入 + MCP 检索"
	case hooks:
		e.Live, e.Memory = "hook 注入增量", "hook 启动注入"
	case c.MCP:
		e.Live, e.Memory = "仅 MCP 按需查询", "写入规则文件的托管区块 + MCP"
	default:
		e.Live, e.Memory = "不支持", "仅写入规则文件托管区块"
	}
	if !c.SessionRead {
		e.Handoff += "（只能作为接收方）"
		e.Live += "（对方看不到它的进展）"
		e.Memory += "（无法从它提取记忆）"
	}
	return e
}

// MCPServer is an MCP server definition in agent-neutral form. Fields only one
// agent understands travel in Extra, keyed by their native names.
type MCPServer struct {
	Transport string            `json:"transport,omitempty" toml:"transport,omitempty"` // "" (stdio), "http" or "sse"
	Command   string            `json:"command,omitempty" toml:"command,omitempty"`
	Args      []string          `json:"args,omitempty" toml:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty" toml:"env,omitempty"`
	Cwd       string            `json:"cwd,omitempty" toml:"cwd,omitempty"`
	URL       string            `json:"url,omitempty" toml:"url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty" toml:"headers,omitempty"`
	Extra     map[string]any    `json:"extra,omitempty" toml:"-"`
}

// MCPConfigurer is implemented by adapters whose MCP server configuration
// bridge can keep in sync.
type MCPConfigurer interface {
	// MCPConfigPath is the file holding the MCP servers of a scope ("global"
	// or "project"), or "" when the agent has none for that scope.
	MCPConfigPath(scope, projectRoot string) string
	// ReadMCP returns the servers defined in path; a missing file has none.
	ReadMCP(path string) (map[string]MCPServer, error)
	// WriteMCP adds or replaces the servers in set and deletes those in
	// remove, leaving every other entry and setting as it was.
	WriteMCP(path string, set map[string]MCPServer, remove []string) error
}
