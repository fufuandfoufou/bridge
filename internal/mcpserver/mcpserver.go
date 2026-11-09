// Package mcpserver exposes shared context to agents as a stdio MCP server.
// Queries are confined to the project the agent is working in.
package mcpserver

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"bridge/internal/config"
	"bridge/internal/model"
	"bridge/internal/project"
	"bridge/internal/store"
)

// Handlers implement the tools, independent of the MCP transport.
type Handlers struct {
	Store   *store.Store
	Config  *config.Config
	Project model.Project
}

// SessionListIn are the session_list arguments.
type SessionListIn struct {
	Agent string `json:"agent,omitempty" jsonschema:"only sessions of this agent"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum results, default 20"`
}

// SessionInfo describes a session.
type SessionInfo struct {
	ID    string `json:"id"`
	Agent string `json:"agent"`
	Title string `json:"title,omitempty"`
	Model string `json:"model,omitempty"`
	Last  string `json:"last_activity"`
}

// SessionListOut is the session_list result.
type SessionListOut struct {
	Sessions []SessionInfo `json:"sessions"`
}

// SessionList lists sessions in the current project.
func (h *Handlers) SessionList(in SessionListIn) (SessionListOut, error) {
	out := SessionListOut{Sessions: []SessionInfo{}}
	if !h.Config.Sync.Sessions {
		return out, nil
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	list, err := h.Store.ListSessions(store.SessionFilter{ProjectID: h.Project.ID, Agent: in.Agent, Limit: limit})
	if err != nil {
		return out, err
	}
	for _, s := range list {
		if h.Config.CanSend(s.Agent) {
			out.Sessions = append(out.Sessions, SessionInfo{ID: s.ID, Agent: s.Agent, Title: s.Title, Model: s.Model,
				Last: s.EndedAt.Format("2006-01-02T15:04:05Z")})
		}
	}
	return out, nil
}

// SessionEventsIn are the session_events arguments.
type SessionEventsIn struct {
	SessionID string `json:"session_id" jsonschema:"session id from session_list"`
	After     int64  `json:"after,omitempty" jsonschema:"return events with a sequence number above this"`
	Limit     int    `json:"limit,omitempty" jsonschema:"maximum events, default 50"`
}

// EventInfo is a client-facing event view.
type EventInfo struct {
	Seq    int64  `json:"seq"`
	Kind   string `json:"kind"`
	Role   string `json:"role,omitempty"`
	Text   string `json:"text,omitempty"`
	Tool   string `json:"tool,omitempty"`
	Args   string `json:"args,omitempty"`
	Result string `json:"result,omitempty"`
	File   string `json:"file,omitempty"`
}

// SessionEventsOut is the session_events result.
type SessionEventsOut struct {
	Events []EventInfo `json:"events"`
}

const maxField = 4000

func clip(s string) string {
	r := []rune(s)
	if len(r) > maxField {
		return string(r[:maxField]) + "…"
	}
	return s
}

// ErrForbidden reports a session outside the caller's reach.
var ErrForbidden = errors.New("session is not accessible from this project")

// SessionEvents returns a session's events.
func (h *Handlers) SessionEvents(in SessionEventsIn) (SessionEventsOut, error) {
	out := SessionEventsOut{Events: []EventInfo{}}
	if !h.Config.Sync.Sessions {
		return out, errors.New("session sync is disabled")
	}
	s, err := h.Store.FindSession(in.SessionID)
	if err != nil {
		return out, fmt.Errorf("session %q: %w", in.SessionID, err)
	}
	if s.ProjectID != h.Project.ID || !h.Config.CanSend(s.Agent) {
		return out, ErrForbidden
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}
	es, err := h.Store.Events(s.ID, in.After, limit)
	if err != nil {
		return out, err
	}
	for _, e := range es {
		v := EventInfo{Seq: e.Seq, Kind: string(e.Kind), Role: e.Role, Text: clip(e.Text())}
		if e.Tool != nil {
			v.Tool, v.Args, v.Result = e.Tool.CanonicalName, clip(string(e.Tool.Args)), clip(e.Tool.Result)
		}
		if e.File != nil {
			v.File = e.File.Path
		}
		out.Events = append(out.Events, v)
	}
	return out, nil
}

// MemorySearchIn are the memory_search arguments.
type MemorySearchIn struct {
	Query string `json:"query" jsonschema:"search text"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum results, default 10"`
}

// MemoryInfo is a client-facing memory view.
type MemoryInfo struct {
	ID       string `json:"id"`
	Scope    string `json:"scope"`
	Category string `json:"category"`
	Content  string `json:"content"`
}

// MemorySearchOut is the memory_search result.
type MemorySearchOut struct {
	Memories []MemoryInfo `json:"memories"`
}

// MemorySearch finds active global and current-project memories.
func (h *Handlers) MemorySearch(in MemorySearchIn) (MemorySearchOut, error) {
	out := MemorySearchOut{Memories: []MemoryInfo{}}
	if !h.Config.Sync.Memory {
		return out, nil
	}
	ms, err := h.Store.SearchMemories(in.Query, h.Project.ID, in.Limit)
	if err != nil {
		return out, err
	}
	for _, m := range ms {
		if (m.Scope == model.ScopeGlobal && !h.Config.GlobalMemoryOn()) || (m.Scope == model.ScopeProject && !h.Config.ProjectMemoryOn()) {
			continue
		}
		out.Memories = append(out.Memories, MemoryInfo{ID: m.ID, Scope: m.Scope, Category: m.Category, Content: m.Content})
	}
	return out, nil
}

// Serve runs the server on stdio until the client disconnects.
func Serve(ctx context.Context, st *store.Store, cfg *config.Config, cwd string) error {
	h := &Handlers{Store: st, Config: cfg, Project: project.Identify(cwd)}
	s := mcp.NewServer(&mcp.Implementation{Name: "bridge", Version: "0.1.0"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "session_list", Description: "List sessions other agents have run in this project."},
		func(_ context.Context, _ *mcp.CallToolRequest, in SessionListIn) (*mcp.CallToolResult, SessionListOut, error) {
			o, err := h.SessionList(in)
			return nil, o, err
		})
	mcp.AddTool(s, &mcp.Tool{Name: "session_events", Description: "Read the events (messages, tool calls, file changes) of a session in this project."},
		func(_ context.Context, _ *mcp.CallToolRequest, in SessionEventsIn) (*mcp.CallToolResult, SessionEventsOut, error) {
			o, err := h.SessionEvents(in)
			return nil, o, err
		})
	mcp.AddTool(s, &mcp.Tool{Name: "memory_search", Description: "Search shared memories: personal preferences and decisions about this project."},
		func(_ context.Context, _ *mcp.CallToolRequest, in MemorySearchIn) (*mcp.CallToolResult, MemorySearchOut, error) {
			o, err := h.MemorySearch(in)
			return nil, o, err
		})
	return s.Run(ctx, &mcp.StdioTransport{})
}
