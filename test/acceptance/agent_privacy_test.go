package acceptance

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"bridge/adapters/dsh"
	"bridge/adapters/pi"
	"bridge/internal/daemon"
	"bridge/internal/fsutil"
	"bridge/internal/model"
	"bridge/internal/store"
	kit "bridge/pkg/adapterkit"
)

func TestAgentReasoningNeverStored(t *testing.T) {
	for _, format := range []string{"pi", "dsh", "dsh-zstd"} {
		for _, reasoningType := range []string{"thinking", "reasoning", "redacted_thinking"} {
			t.Run(format+"/"+reasoningType, func(t *testing.T) {
				e := newEnv(t)
				marker, signature := "PRIVATE_REASONING_SENTINEL", "PRIVATE_SIGNATURE_SENTINEL"
				thought := map[string]any{"type": reasoningType, "text": marker, "thinking": marker, "signature": signature, "thinkingSignature": signature}
				call := map[string]any{"type": "toolCall", "id": "call-1", "name": "edit", "arguments": map[string]any{"path": "a.go", "edits": []any{map[string]any{"oldText": "old", "newText": "new"}}}}
				if format != "pi" {
					call = map[string]any{"type": "tool-call", "id": "call-1", "name": "edit", "arguments": `{"file_path":"a.go","old_string":"old","new_string":"new"}`}
				}
				messages := []map[string]any{
					{"role": "user", "content": []any{map[string]any{"type": "text", "text": "explain thinking and reasoning"}}},
					{"role": "assistant", "content": []any{thought, map[string]any{"type": "text", "text": "public answer"}, call}},
					{"role": "toolResult", "toolCallId": "call-1", "toolName": "edit", "content": []any{map[string]any{"type": "text", "text": "edited"}}},
					{"role": "assistant", "content": []any{thought}},
				}
				var source bytes.Buffer
				for i, message := range messages {
					record := map[string]any{"type": "message", "id": "message", "timestamp": "2026-11-10T00:00:00Z", "message": message}
					if format != "pi" {
						recordType := "assistant/message"
						data := map[string]any{"message": message}
						if message["role"] == "assistant" {
							data["stream"] = []any{map[string]any{"thinking": marker, "signature": signature}}
							message["source"] = map[string]any{"kind": "model", "provider": "probe", "model": "probe", "replayState": map[string]any{"thinking": marker, "signature": signature}}
						}
						if i == 0 {
							recordType = "user/message"
							message["source"] = map[string]any{"kind": "user"}
							data = message
						} else if i == 2 {
							recordType = "tool/result"
							message["role"] = "tool"
						}
						record = map[string]any{"type": recordType, "seq": i + 1, "time": 1794268800000, "data": data}
					}
					must(t, json.NewEncoder(&source).Encode(record))
				}
				path := filepath.Join(e.home, "session.jsonl")
				original := append([]byte(nil), source.Bytes()...)
				if format == "dsh-zstd" {
					var compressed bytes.Buffer
					enc, err := zstd.NewWriter(&compressed)
					must(t, err)
					_, err = enc.Write(original)
					must(t, err)
					must(t, enc.Close())
					original = compressed.Bytes()
					path += ".zstd"
				}
				write(t, path, string(original))
				var adapter kit.Adapter = pi.New()
				if format != "pi" {
					adapter = dsh.New()
				}
				s := e.store()
				ingester := daemon.Ingester{Store: s, Config: cfgFn}
				ref := kit.SessionRef{NativeID: "private-test", Path: path, CWD: e.proj, StartedAt: time.Now(), UpdatedAt: time.Now()}
				_, err := ingester.IngestRef(context.Background(), adapter, ref)
				must(t, err)
				events, err := s.Events(adapter.ID()+":private-test", 0, 0)
				must(t, err)
				encoded, err := json.Marshal(events)
				must(t, err)
				for _, secret := range []string{marker, signature} {
					if bytes.Contains(encoded, []byte(secret)) || len(scanData(t, secret)) != 0 {
						t.Errorf("private %s survived ingestion", secret)
					}
				}
				if got := strings.Join(texts(events, ""), "|"); got != "explain thinking and reasoning|public answer" {
					t.Errorf("conversation changed: %q", got)
				}
				counts := map[model.EventKind]int{}
				for _, event := range events {
					counts[event.Kind]++
					if event.Kind == model.KindReasoning && (len(event.NativeRef) != 0 || len(event.Content) != 0) {
						t.Error("reasoning placeholder retained a payload")
					}
					if event.Kind != model.KindReasoning && !json.Valid(event.NativeRef) {
						t.Errorf("%s lost safe native metadata", event.Kind)
					}
					if event.Kind == model.KindToolCall && (event.Tool.ID != "call-1" || event.Tool.CanonicalName != model.ToolEditFile) {
						t.Errorf("tool call changed: %+v", event.Tool)
					}
					if event.Kind == model.KindToolResult && (event.Tool.ID != "call-1" || event.Tool.Result != "edited") {
						t.Errorf("tool result changed: %+v", event.Tool)
					}
				}
				if counts[model.KindReasoning] != 2 || counts[model.KindToolCall] != 1 || counts[model.KindToolResult] != 1 || counts[model.KindFileChange] != 1 {
					t.Errorf("event kinds changed: %v", counts)
				}
				n, err := ingester.IngestRef(context.Background(), adapter, ref)
				must(t, err)
				if n != 0 {
					t.Errorf("unchanged source reingested %d events", n)
				}
				got, err := os.ReadFile(path)
				must(t, err)
				if !bytes.Equal(got, original) {
					t.Error("native session was modified")
				}
			})
		}
	}
}

func TestReasoningPrivacyUpgrade(t *testing.T) {
	e := newEnv(t)
	s := e.store()
	marker := "LEGACY_PRIVATE_REASONING_SENTINEL"
	for _, agent := range []string{"pi", "dsh", "claude-code"} {
		id := agent + ":legacy"
		must(t, s.UpsertSession(model.Session{ID: id, Agent: agent, NativeID: "legacy", CWD: e.proj}))
		event := model.TextEvent(model.RoleAssistant, "public legacy answer", time.Now())
		event.Seq = 1
		event.NativeRef = json.RawMessage(`{"message":{"content":[{"type":"thinking","thinking":"` + marker + `"},{"type":"text","text":"public legacy answer"}]}}`)
		if agent == "claude-code" {
			event.NativeRef = json.RawMessage(`{"type":"assistant","message":{"content":[{"type":"text","text":"safe native metadata"}]}}`)
		}
		_, err := s.Ingest(id, []model.Event{event}, store.CursorState{Cursor: model.Cursor{Agent: agent, NativeFile: agent + ".jsonl", Offset: 100, NextSeq: 1}})
		must(t, err)
	}
	must(t, s.Close())
	db, err := sql.Open("sqlite", "file:"+fsutil.DBPath())
	must(t, err)
	_, err = db.Exec(`PRAGMA user_version=4`)
	must(t, err)
	must(t, db.Close())
	s, err = store.Open(fsutil.DBPath())
	must(t, err)
	t.Cleanup(func() { _ = s.Close() })
	for _, agent := range []string{"pi", "dsh", "claude-code"} {
		events, err := s.Events(agent+":legacy", 0, 0)
		must(t, err)
		if len(events) != 1 || events[0].Seq != 1 || events[0].Text() != "public legacy answer" {
			t.Fatalf("legacy conversation changed: %+v", events)
		}
		if agent != "claude-code" && len(events[0].NativeRef) != 0 {
			t.Errorf("%s retained its legacy raw payload", agent)
		}
		if agent == "claude-code" && !bytes.Contains(events[0].NativeRef, []byte("safe native metadata")) {
			t.Error("unrelated native metadata changed")
		}
		cursor, ok, err := s.GetCursor(agent, agent+".jsonl")
		must(t, err)
		if !ok || cursor.Offset != 100 || cursor.NextSeq != 1 {
			t.Errorf("cursor changed: %+v", cursor)
		}
	}
	if files := scanData(t, marker); len(files) != 0 {
		t.Errorf("legacy reasoning remains in database files: %v", files)
	}
	// Reopening an upgraded database must keep newly sanitized native records.
	event := model.TextEvent(model.RoleAssistant, "new safe answer", time.Now())
	event.Seq = 2
	event.NativeRef = json.RawMessage(`{"message":{"content":[{"type":"text","text":"new safe answer"}]}}`)
	_, err = s.Ingest("pi:legacy", []model.Event{event}, store.CursorState{Cursor: model.Cursor{Agent: "pi", NativeFile: "pi.jsonl", Offset: 200, NextSeq: 2}})
	must(t, err)
	must(t, s.Close())
	s, err = store.Open(fsutil.DBPath())
	must(t, err)
	events, err := s.Events("pi:legacy", 1, 0)
	must(t, err)
	if len(events) != 1 || !bytes.Equal(events[0].NativeRef, event.NativeRef) {
		t.Errorf("upgrade cleanup ran again: %+v", events)
	}
}

func TestDeepSeekStreamPrivacyUpgrade(t *testing.T) {
	e := newEnv(t)
	s := e.store()
	id := "dsh:legacy-stream"
	must(t, s.UpsertSession(model.Session{ID: id, Agent: "dsh", NativeID: "legacy-stream", CWD: e.proj}))
	marker := "LEGACY_DSH_STREAM_SENTINEL"
	event := model.TextEvent(model.RoleAssistant, "safe answer", time.Now())
	event.Seq = 1
	event.NativeRef = json.RawMessage(`{"data":{"message":{"content":[{"type":"text","text":"safe answer"}],"source":{"kind":"model","replayState":"` + marker + `"}},"stream":[{"thinking":"` + marker + `"}]}}`)
	_, err := s.Ingest(id, []model.Event{event}, store.CursorState{Cursor: model.Cursor{Agent: "dsh", NativeFile: "legacy.zstd", NextSeq: 1}})
	must(t, err)
	must(t, s.Close())
	db, err := sql.Open("sqlite", "file:"+fsutil.DBPath())
	must(t, err)
	_, err = db.Exec(`PRAGMA user_version=5`)
	must(t, err)
	must(t, db.Close())
	s, err = store.Open(fsutil.DBPath())
	must(t, err)
	defer s.Close()
	events, err := s.Events(id, 0, 0)
	must(t, err)
	if len(events) != 1 || events[0].Text() != "safe answer" || len(events[0].NativeRef) != 0 {
		t.Errorf("stream repair changed canonical history or retained private data: %+v", events)
	}
	if files := scanData(t, marker); len(files) != 0 {
		t.Errorf("old provider stream remains in database files: %v", files)
	}
}
