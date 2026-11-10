package dsh

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"bridge/adapters/internal/common"
	"bridge/internal/fsutil"
	"bridge/internal/model"
)

// FromCanonical maps supported calls to the harness's native tools.
func FromCanonical(t *model.ToolCall) (string, json.RawMessage, bool) {
	var args map[string]any
	if t == nil || json.Unmarshal(t.Args, &args) != nil {
		return "", nil, false
	}
	var name string
	var native map[string]any
	switch t.CanonicalName {
	case model.ToolReadFile:
		name, native = "read", map[string]any{"file_path": args["path"], "offset": args["offset"], "limit": args["limit"]}
	case model.ToolWriteFile:
		name, native = "write", map[string]any{"file_path": args["path"], "content": args["content"]}
	case model.ToolEditFile:
		_, oldOK := args["old"].(string)
		_, newOK := args["new"].(string)
		if !oldOK || !newOK {
			return "", nil, false
		}
		name, native = "edit", map[string]any{"file_path": args["path"], "old_string": args["old"], "new_string": args["new"], "replace_all": args["replace_all"]}
	case model.ToolShell:
		if command, ok := args["command"].(string); !ok || command == "" {
			return "", nil, false
		}
		name, native = "bash", map[string]any{"command": args["command"], "description": args["description"]}
	case model.ToolSearchText:
		name, native = "grep", map[string]any{"pattern": args["pattern"], "path": args["path"], "include": args["glob"]}
	case model.ToolSearchFiles:
		name, native = "glob", map[string]any{"pattern": args["pattern"], "path": args["path"]}
	case model.ToolTodo:
		name, native = "todo_write", map[string]any{"todos": args["items"]}
	case model.ToolWebFetch:
		name, native = "web_fetch", map[string]any{"url": args["url"]}
	case model.ToolWebSearch:
		name, native = "web_search", map[string]any{"query": args["query"]}
	case model.ToolSubagent:
		name, native = "subagent", map[string]any{"description": args["description"], "prompt": args["prompt"]}
	default:
		return "", nil, false
	}
	for key, value := range native {
		if value == nil {
			delete(native, key)
		}
	}
	return name, common.MustJSON(native), true
}

// CanRender implements adapterkit.ToolRenderer.
func (a *Adapter) CanRender(t *model.ToolCall) bool {
	_, _, ok := FromCanonical(t)
	return ok
}

type renderedCall struct {
	original string
	id       string
	name     string
	args     string
}

type writer struct {
	buf       bytes.Buffer
	seq       int64
	turn      int
	step      int
	openTurn  bool
	openStep  bool
	hasSystem bool
	provider  string
	model     string
	now       time.Time
	assistant []map[string]any
	calls     []renderedCall
	results   map[string][]*model.ToolCall
	err       error
}

func newWriter(seq int64, lastTurn int, hasSystem bool, modelName string, now time.Time) *writer {
	provider, mdl := "bridge", "imported"
	if parts := strings.SplitN(modelName, "/", 2); len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		provider, mdl = parts[0], parts[1]
	}
	return &writer{seq: seq, turn: lastTurn, hasSystem: hasSystem, provider: provider, model: mdl, now: now,
		results: map[string][]*model.ToolCall{}}
}

func (w *writer) put(kind string, data any, surface bool) int64 {
	seq := w.seq
	row := map[string]any{"type": kind, "seq": seq, "time": w.now.UnixMilli(), "data": data}
	if surface {
		row["surfaceOp"] = "append"
	}
	if w.err == nil {
		w.err = json.NewEncoder(&w.buf).Encode(row)
	}
	w.seq++
	return seq
}

func nativeMessage(role, source string, content []map[string]any) map[string]any {
	return map[string]any{"id": fsutil.NewUUID(), "role": role, "source": map[string]any{"kind": source}, "content": content}
}

func (w *writer) startStep() {
	if !w.openTurn {
		w.turn++
		w.step = 0
		w.put("turn/start", map[string]any{"turn": w.turn}, false)
		w.openTurn = true
	}
	if !w.openStep {
		w.step++
		w.put("step/start", map[string]any{"turn": w.turn, "step": w.step}, false)
		w.openStep = true
	}
	if !w.hasSystem {
		w.put("system/message", map[string]any{"turn": w.turn, "step": w.step,
			"message": nativeMessage("system", "system-prompt", []map[string]any{})}, true)
		w.hasSystem = true
	}
}

func (w *writer) flushAssistant() {
	if len(w.assistant) == 0 {
		return
	}
	w.startStep()
	message := nativeMessage("assistant", "model", w.assistant)
	message["source"] = map[string]any{"kind": "model", "provider": w.provider, "model": w.model}
	w.put("assistant/message", map[string]any{"turn": w.turn, "step": w.step, "message": message, "stream": []any{}}, true)
	for _, call := range w.calls {
		w.put("tool/call", map[string]any{"turn": w.turn, "step": w.step, "callId": call.id, "name": call.name, "arguments": call.args}, false)
		result, status := common.InterruptedResult, "error"
		if queue := w.results[call.original]; len(queue) > 0 {
			result, status = queue[0].Result, queue[0].Status
			w.results[call.original] = queue[1:]
		}
		message := nativeMessage("tool", "tool", []map[string]any{{"type": "text", "text": result}})
		message["source"] = map[string]any{"kind": "tool", "callId": call.id}
		message["toolCallId"], message["isError"] = call.id, status == "error"
		w.put("tool/result", map[string]any{"turn": w.turn, "step": w.step, "message": message}, true)
	}
	w.put("step/end", map[string]any{"turn": w.turn, "step": w.step}, false)
	w.openStep = false
	w.assistant, w.calls = nil, nil
}

func (w *writer) finishTurn() {
	w.flushAssistant()
	if w.openStep {
		w.put("step/end", map[string]any{"turn": w.turn, "step": w.step}, false)
		w.openStep = false
	}
	if w.openTurn {
		w.put("turn/end", map[string]any{"turn": w.turn, "reason": map[string]any{"kind": "completed"}}, false)
		w.openTurn = false
	}
}

func (w *writer) events(events []model.Event) error {
	for _, event := range events {
		if event.Kind == model.KindToolResult && event.Tool != nil {
			w.results[event.Tool.ID] = append(w.results[event.Tool.ID], event.Tool)
		}
	}
	for _, event := range events {
		switch event.Kind {
		case model.KindMessage:
			if text := event.Text(); text != "" {
				if event.Role == model.RoleAssistant {
					w.assistant = append(w.assistant, map[string]any{"type": "text", "text": text})
				} else {
					w.finishTurn()
					w.startStep()
					w.put("user/message", nativeMessage("user", "user", []map[string]any{{"type": "text", "text": text}}), true)
				}
			}
		case model.KindToolCall:
			name, args, ok := FromCanonical(event.Tool)
			if !ok {
				return fmt.Errorf("tool reached dsh writer without a native mapping")
			}
			call := renderedCall{original: event.Tool.ID, id: fsutil.NewUUID(), name: name, args: string(args)}
			w.calls = append(w.calls, call)
			w.assistant = append(w.assistant, map[string]any{"type": "tool-call", "id": call.id, "name": name, "arguments": call.args})
		case model.KindToolResult:
			w.flushAssistant()
		}
	}
	w.finishTurn()
	for id, queue := range w.results {
		if len(queue) > 0 {
			return fmt.Errorf("tool result %q has no matching call", id)
		}
	}
	return w.err
}

// Render produces a current-generation log accepted by the real harness.
func Render(s model.Session, id string, now time.Time, events []model.Event) ([]byte, error) {
	w := newWriter(0, 0, false, s.Model, now)
	if err := json.NewEncoder(&w.buf).Encode(map[string]any{"type": "session", "version": formatVersion,
		"id": id, "createdAt": now.UnixMilli(), "cwd": s.CWD, "isSeeded": false, "delegationDepth": 0}); err != nil {
		return nil, err
	}
	if s.Title != "" {
		w.put("session/title", map[string]any{"title": s.Title, "source": map[string]any{"kind": "user"}, "messageSeqs": []any{}}, false)
	}
	if err := w.events(events); err != nil {
		return nil, err
	}
	return w.buf.Bytes(), nil
}
