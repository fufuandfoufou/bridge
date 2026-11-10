package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"bridge/adapters/internal/common"
	"bridge/internal/fsutil"
	"bridge/internal/model"
	kit "bridge/pkg/adapterkit"
)

// rolloutMode reads the storage mode from the rollout's session_meta, for
// threads Codex hasn't indexed.
func rolloutMode(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<16)
	line, err := r.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return ""
	}
	var m struct {
		Type    string `json:"type"`
		Payload struct {
			HistoryMode string `json:"history_mode"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &m) != nil || m.Type != "session_meta" {
		return ""
	}
	return m.Payload.HistoryMode
}

func lastOrdinal(path string) int {
	lines := common.TailLines(path, 1<<20)
	for i := len(lines) - 1; i >= 0; i-- {
		var l struct {
			Ordinal *int `json:"ordinal"`
		}
		if json.Unmarshal(lines[i], &l) == nil && l.Ordinal != nil {
			return *l.Ordinal
		}
	}
	return -1
}

// renderPaginated writes events as completed turns in the form Codex itself
// uses for paginated threads: task_started, the model-visible response items,
// an item_completed event for each message shown in the transcript, and
// task_complete. A turn starts at each user message.
func renderPaginated(ref kit.SessionRef, last int, now time.Time, events []model.Event) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	write := func(ts time.Time, typ string, payload any) error {
		if ts.IsZero() || ts.After(now) {
			ts = now
		}
		last++
		return enc.Encode(map[string]any{"timestamp": ts.UTC().Format("2006-01-02T15:04:05.000Z"), "ordinal": last, "type": typ, "payload": payload})
	}
	thread := ref.NativeID
	var turn, lastAgent string
	var started time.Time
	open := func(ts time.Time) error {
		if ts.IsZero() || ts.After(now) {
			ts = now
		}
		turn, lastAgent, started = fsutil.NewUUID(), "", ts
		return write(ts, "event_msg", map[string]any{
			"type": "task_started", "turn_id": turn, "root_turn_id": turn, "started_at": ts.Unix(),
			"turn_attribution": map[string]any{"turn_id": turn, "root_turn_id": turn, "turn_trigger": "composer",
				"parent_turn_id": nil, "initiating_agent_path": nil},
			"collaboration_mode_kind": "default",
		})
	}
	closeTurn := func(ts time.Time) error {
		if turn == "" {
			return nil
		}
		if ts.IsZero() || ts.After(now) || ts.Before(started) {
			ts = started
		}
		err := write(ts, "event_msg", map[string]any{
			"type": "task_complete", "turn_id": turn, "root_turn_id": turn, "last_agent_message": lastAgent,
			"started_at": started.Unix(), "completed_at": ts.Unix(), "duration_ms": ts.Sub(started).Milliseconds(),
		})
		turn = ""
		return err
	}
	item := func(ts time.Time, v map[string]any) error {
		if ts.IsZero() || ts.After(now) {
			ts = now
		}
		return write(ts, "event_msg", map[string]any{"type": "item_completed", "thread_id": thread, "turn_id": turn,
			"item": v, "started_at_ms": ts.UnixMilli(), "completed_at_ms": ts.UnixMilli()})
	}
	var lastTS time.Time
	for _, e := range events {
		var err error
		lastTS = e.TS
		isUser := e.Kind == model.KindMessage && e.Role != model.RoleAssistant && e.Role != model.RoleSystem
		if isUser {
			if err = closeTurn(e.TS); err != nil {
				return nil, err
			}
		}
		if turn == "" {
			if err = open(e.TS); err != nil {
				return nil, err
			}
		}
		switch e.Kind {
		case model.KindMessage:
			text := e.Text()
			switch e.Role {
			case model.RoleAssistant:
				id := "msg_" + strings.ReplaceAll(fsutil.NewUUID(), "-", "")
				err = write(e.TS, "response_item", map[string]any{"type": "message", "id": id, "role": "assistant", "status": "completed",
					"content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}}})
				if err == nil {
					err = item(e.TS, map[string]any{"type": "AgentMessage", "id": id, "content": []map[string]string{{"type": "Text", "text": text}}})
				}
				lastAgent = text
			case model.RoleSystem:
				err = write(e.TS, "response_item", map[string]any{"type": "message", "role": "developer",
					"content": []map[string]string{{"type": "input_text", "text": text}}})
			default:
				err = write(e.TS, "response_item", map[string]any{"type": "message", "role": "user",
					"content": []map[string]string{{"type": "input_text", "text": text}}})
				if err == nil {
					err = item(e.TS, map[string]any{"type": "UserMessage", "id": fsutil.NewUUID(),
						"content": []map[string]any{{"type": "text", "text": text, "text_elements": []any{}}}})
				}
			}
		case model.KindToolCall:
			name, args, ok := FromCanonical(e.Tool)
			if !ok {
				return nil, fmt.Errorf("tool %q reached writer without a mapping", e.Tool.CanonicalName)
			}
			err = write(e.TS, "response_item", map[string]any{"type": "function_call", "name": name, "arguments": string(args), "call_id": e.Tool.ID})
		case model.KindToolResult:
			err = write(e.TS, "response_item", map[string]any{"type": "function_call_output", "call_id": e.Tool.ID, "output": e.Tool.Result})
		}
		if err != nil {
			return nil, err
		}
	}
	if err := closeTurn(lastTS); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
