package codex

import (
	"context"
	"os"
	"testing"
	"time"

	"bridge/internal/model"
	kit "bridge/pkg/adapterkit"
)

// TestAppendToRealPaginatedThread appends a turn to a paginated thread made
// by a real Codex client. It runs only when CODEX_PROBE_THREAD names such a
// thread's rollout under CODEX_HOME; see docs for the manual check.
func TestAppendToRealPaginatedThread(t *testing.T) {
	path, id := os.Getenv("CODEX_PROBE_ROLLOUT"), os.Getenv("CODEX_PROBE_THREAD")
	if path == "" || id == "" {
		t.Skip("set CODEX_HOME, CODEX_PROBE_ROLLOUT and CODEX_PROBE_THREAD to run against a real client")
	}
	now := time.Now().UTC()
	events := []model.Event{
		model.TextEvent(model.RoleUser, "bridge appended question", now),
		model.TextEvent(model.RoleAssistant, "bridge appended answer", now),
	}
	out, err := New().RenderAppend(context.Background(), kit.SessionRef{NativeID: id, Path: path}, events)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(out); err != nil {
		t.Fatal(err)
	}
	t.Logf("appended %d bytes", len(out))
}
