package dsh

import (
	"bridge/internal/fakellm"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"bridge/adapters/internal/common"
	"bridge/internal/fsutil"
	"bridge/internal/model"
	kit "bridge/pkg/adapterkit"
)

// ProbeVersion asks the installed client to restore a synthetic log and send
// its history to a loopback endpoint, using a disposable home and configuration.
func (a *Adapter) ProbeVersion(ctx context.Context, version string) error {
	bin, err := common.FindBinary(ctx, version, []string{"dsh", "/opt/homebrew/bin/dsh", "/usr/local/bin/dsh"})
	if err != nil {
		return err
	}
	marker := "bridge-dsh-" + fsutil.NewUUID()
	now := time.Now().UTC()
	return probe(ctx, bin, []model.Event{model.TextEvent(model.RoleUser, "remember "+marker, now),
		model.TextEvent(model.RoleAssistant, "recorded", now)}, marker)
}

func probe(ctx context.Context, bin string, events []model.Event, marker string) error {
	tmp, err := os.MkdirTemp("", "bridge-probe-dsh-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if tmp, err = filepath.EvalSymlinks(tmp); err != nil {
		return err
	}
	llm, err := fakellm.Start()
	if err != nil {
		return err
	}
	defer llm.Close()
	home, work := filepath.Join(tmp, "dsh"), filepath.Join(tmp, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		return err
	}
	id := New().NewSessionID()
	data, err := Render(model.Session{CWD: work, Title: "Bridge compatibility probe"}, id, time.Now().UTC(), events)
	if err != nil {
		return err
	}
	data, err = encodeLog(data)
	if err != nil {
		return err
	}
	path := filepath.Join(home, "sessions", projectKey(work), id, "session.v4.jsonl.zstd")
	if err := fsutil.AtomicWrite(path, data, 0o600); err != nil {
		return err
	}
	patch := `- id: session-log-deepseek
  disabled: true
- id: session-telemetry-otel
  disabled: true
- id: otel
  disabled: true
- id: web-search-deepseek
  disabled: true
- id: web-deepseek-api
  disabled: true
- id: llm-deepseek-account
  disabled: true
- id: plugin-package-inventory-deepseek
  disabled: true
- id: session-title-llm
  disabled: true
- id: llm-pi-ai
  config:
    providers:
      bridgeprobe:
        apiKeyEnv: BRIDGE_PROBE_KEY
        api: openai-completions
        baseURL: ` + llm.URL + `
        models:
          - id: probe
            contextWindow: 128000
- id: agent-default-model
  config:
    provider: bridgeprobe
    model: probe
`
	if err := fsutil.AtomicWrite(filepath.Join(home, "cordis.patch.yml"), []byte(patch), 0o600); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	run := func() error {
		cmd := exec.CommandContext(ctx, bin, "headless", "--session-id", id, "report that this session was restored")
		cmd.Dir = work
		cmd.Env = common.ProbeEnv("HOME="+tmp, "DSH_HOME="+home, "DSH_AGENTS_HOME="+filepath.Join(tmp, "agents"),
			"BRIDGE_PROBE_KEY=x", "DSH_TELEMETRY_MODE=OFF", "HTTPS_PROXY=http://127.0.0.1:1", "HTTP_PROXY=http://127.0.0.1:1", "NO_PROXY=127.0.0.1,localhost")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("dsh failed to resume the probe session: %w %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if err := run(); err != nil {
		return err
	}
	if !llm.Saw(marker) {
		return fmt.Errorf("dsh resumed the probe session without its migrated history")
	}
	// Append after the harness itself has continued the session, then restore it again.
	deltaMarker := "bridge-dsh-delta-" + fsutil.NewUUID()
	a := New()
	ref := kit.SessionRef{NativeID: id, Path: path}
	release, err := a.LockSession(ref)
	if err != nil {
		return err
	}
	defer release()
	delta, err := a.RenderAppend(ctx, ref, []model.Event{model.TextEvent(model.RoleUser, "remember "+deltaMarker, time.Now())})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(delta)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	release()
	if err := run(); err != nil {
		return err
	}
	if !llm.Saw(deltaMarker) {
		return fmt.Errorf("dsh resumed the probe session without its appended history")
	}
	return nil
}
