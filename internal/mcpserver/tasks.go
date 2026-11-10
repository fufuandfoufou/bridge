package mcpserver

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"bridge/internal/store"
)

// Owner names the agent session holding a task. Each agent session runs its
// own MCP server process, so the parent process tells sessions apart.
func Owner(clientName string) string {
	agent := strings.ToLower(clientName)
	switch {
	case strings.Contains(agent, "claude"):
		agent = "claude-code"
	case strings.Contains(agent, "codex"):
		agent = "codex"
	case agent == "":
		agent = "agent"
	}
	return fmt.Sprintf("%s#%d", agent, os.Getppid())
}

// TaskListIn are the task_list arguments.
type TaskListIn struct {
	IncludeDone bool `json:"include_done,omitempty" jsonschema:"also list finished tasks"`
}

// TaskAddIn are the task_add arguments.
type TaskAddIn struct {
	Title  string `json:"title" jsonschema:"what needs doing, one line"`
	Detail string `json:"detail,omitempty" jsonschema:"context, acceptance criteria"`
}

// TaskRef names a task.
type TaskRef struct {
	ID string `json:"id" jsonschema:"task ID or its first characters"`
}

// TaskUpdateIn are the task_update arguments.
type TaskUpdateIn struct {
	ID      string `json:"id" jsonschema:"task ID or its first characters"`
	Note    string `json:"note,omitempty" jsonschema:"progress note"`
	Release bool   `json:"release,omitempty" jsonschema:"give the task back so others can claim it"`
}

// TaskDoneIn are the task_done arguments.
type TaskDoneIn struct {
	ID   string `json:"id" jsonschema:"task ID or its first characters"`
	Note string `json:"note,omitempty" jsonschema:"what was done"`
}

// TasksOut lists tasks.
type TasksOut struct {
	Tasks []store.Task `json:"tasks"`
}

// TaskOut reports a task after a change. When OK is false the change was
// refused and Task holds its current state, for example who holds it.
type TaskOut struct {
	OK      bool       `json:"ok"`
	Message string     `json:"message,omitempty"`
	Task    store.Task `json:"task"`
}

// TaskList lists the project's tasks.
func (h *Handlers) TaskList(in TaskListIn) (TasksOut, error) {
	t, err := h.Store.ListTasks(h.Project.ID, in.IncludeDone)
	return TasksOut{Tasks: t}, err
}

// TaskAdd adds a task to the board.
func (h *Handlers) TaskAdd(in TaskAddIn) (TaskOut, error) {
	t, err := h.Store.AddTask(h.Project.ID, in.Title, in.Detail)
	return TaskOut{OK: err == nil, Task: t}, err
}

func (h *Handlers) task(id string) (store.Task, error) {
	t, err := h.Store.GetTask(id)
	if err == nil && t.ProjectID != h.Project.ID {
		return t, store.ErrNotFound
	}
	return t, err
}

func refusal(t store.Task) string {
	switch t.Status {
	case store.TaskDone:
		return "task is already done"
	case store.TaskClaimed:
		return "task is held by " + t.Owner
	}
	return "task is not claimed by you; claim it first"
}

// TaskClaim takes an open task. Only one claimant can win.
func (h *Handlers) TaskClaim(in TaskRef, owner string) (TaskOut, error) {
	if _, err := h.task(in.ID); err != nil {
		return TaskOut{}, err
	}
	t, ok, err := h.Store.ClaimTask(in.ID, owner)
	if err != nil || ok {
		return TaskOut{OK: ok, Task: t}, err
	}
	return TaskOut{Message: refusal(t), Task: t}, nil
}

// TaskUpdate records progress on, or releases, a task the caller holds.
func (h *Handlers) TaskUpdate(in TaskUpdateIn, owner string) (TaskOut, error) {
	status := store.TaskClaimed
	if in.Release {
		status = store.TaskOpen
	}
	return h.change(in.ID, owner, status, in.Note)
}

// TaskDone completes a task the caller holds.
func (h *Handlers) TaskDone(in TaskDoneIn, owner string) (TaskOut, error) {
	return h.change(in.ID, owner, store.TaskDone, in.Note)
}

func (h *Handlers) change(id, owner, status, note string) (TaskOut, error) {
	if _, err := h.task(id); err != nil {
		return TaskOut{}, err
	}
	t, ok, err := h.Store.UpdateTask(id, owner, status, note)
	if err != nil || ok {
		return TaskOut{OK: ok, Task: t}, err
	}
	return TaskOut{Message: refusal(t), Task: t}, nil
}

func client(req *mcp.CallToolRequest) string {
	if req != nil && req.Session != nil {
		if p := req.Session.InitializeParams(); p != nil && p.ClientInfo != nil {
			return p.ClientInfo.Name
		}
	}
	return ""
}

func addTaskTools(s *mcp.Server, h *Handlers) {
	mcp.AddTool(s, &mcp.Tool{Name: "task_list", Description: "List the shared task board of this project, which all agents working here see."},
		func(_ context.Context, _ *mcp.CallToolRequest, in TaskListIn) (*mcp.CallToolResult, TasksOut, error) {
			o, err := h.TaskList(in)
			return nil, o, err
		})
	mcp.AddTool(s, &mcp.Tool{Name: "task_add", Description: "Add a task to the shared board so another agent can pick it up."},
		func(_ context.Context, _ *mcp.CallToolRequest, in TaskAddIn) (*mcp.CallToolResult, TaskOut, error) {
			o, err := h.TaskAdd(in)
			return nil, o, err
		})
	mcp.AddTool(s, &mcp.Tool{Name: "task_claim", Description: "Claim an open task before working on it. If ok is false, someone else holds it; pick another."},
		func(_ context.Context, req *mcp.CallToolRequest, in TaskRef) (*mcp.CallToolResult, TaskOut, error) {
			o, err := h.TaskClaim(in, Owner(client(req)))
			return nil, o, err
		})
	mcp.AddTool(s, &mcp.Tool{Name: "task_update", Description: "Add a progress note to a task you hold, or release it."},
		func(_ context.Context, req *mcp.CallToolRequest, in TaskUpdateIn) (*mcp.CallToolResult, TaskOut, error) {
			o, err := h.TaskUpdate(in, Owner(client(req)))
			return nil, o, err
		})
	mcp.AddTool(s, &mcp.Tool{Name: "task_done", Description: "Mark a task you hold as done."},
		func(_ context.Context, req *mcp.CallToolRequest, in TaskDoneIn) (*mcp.CallToolResult, TaskOut, error) {
			o, err := h.TaskDone(in, Owner(client(req)))
			return nil, o, err
		})
}
