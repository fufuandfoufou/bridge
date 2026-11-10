package acceptance

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bridge/adapters/dsh"
	"bridge/internal/fakellm"
	kit "bridge/pkg/adapterkit"
)

// dshIsolation keeps the real harness offline: uploads and telemetry off,
// the model on a local stand-in. Written first, it also stands in for the
// user's own rows, which bridge must leave intact.
func dshIsolation(llmURL string) string {
	return `# my own settings
- id: session-log-deepseek
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
        baseURL: ` + llmURL + `
        models:
          - id: probe
            contextWindow: 128000
- id: agent-default-model
  config:
    provider: bridgeprobe
    model: probe
`
}

func dshRun(t *testing.T, e *env, prompt string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "dsh", "headless", prompt)
	cmd.Dir = e.proj
	cmd.Env = append(os.Environ(), "HOME="+e.home, "DSH_HOME="+filepath.Join(e.home, ".dsh"),
		"DSH_AGENTS_HOME="+filepath.Join(e.home, ".agents"), "BRIDGE_PROBE_KEY=x", "DSH_TELEMETRY_MODE=OFF",
		"HTTPS_PROXY=http://127.0.0.1:1", "HTTP_PROXY=http://127.0.0.1:1", "NO_PROXY=127.0.0.1,localhost")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("dsh headless: %v\n%s", err, out)
	}
}

func TestDeepSeekLiveCollaboration(t *testing.T) {
	if _, err := exec.LookPath("dsh"); err != nil {
		t.Skip("dsh is not installed")
	}
	e := newEnv(t)
	e.base() // a Claude Code session in the project with progress "q3"/"a3"
	home := filepath.Join(e.home, ".dsh")
	t.Setenv("DSH_HOME", home)
	llm, err := fakellm.Start()
	must(t, err)
	defer llm.Close()
	patch := filepath.Join(home, "cordis.patch.yml")
	user := dshIsolation(llm.URL)
	write(t, patch, user)

	a := dsh.New()
	must(t, a.InstallHooks(context.Background(), kit.HookConfig{Command: binPath, MCPCommand: []string{binPath, "mcp"}}))
	if !a.HooksInstalled() || !strings.HasPrefix(read(t, patch), user) {
		t.Fatalf("patch: %s", read(t, patch))
	}
	cfgPath := filepath.Join(e.home, ".config/bridge/config.toml")
	write(t, cfgPath, "version = 1\n[agents.dsh]\ninject = true\n")
	stop := startDaemon(t, e)
	defer stop()
	waitIngested(t, e)

	dshRun(t, e, "continue the work")
	if !llm.Saw("其他 agent 的最新进展") || !llm.Saw("q3") {
		t.Errorf("Claude Code's progress was not injected:\n%s", strings.Join(llm.Requests(), "\n---\n"))
	}
	if !llm.Saw("mcp__bridge__session_list") {
		t.Error("bridge's MCP tools were not offered to the model")
	}

	// Off by default and when switched off: nothing injected, MCP still there.
	write(t, cfgPath, "version = 1\n")
	appendFile(t, e.claudePath(e.proj, srcID), claudeLines(e.proj, srcID, msg{"user", "LATER_CLAUDE_WORK"}))
	quiet, err := fakellm.Start()
	must(t, err)
	defer quiet.Close()
	write(t, patch, strings.Replace(read(t, patch), llm.URL, quiet.URL, 1))
	time.Sleep(500 * time.Millisecond) // let the daemon ingest the new turn
	dshRun(t, e, "another task")
	if quiet.Saw("LATER_CLAUDE_WORK") || quiet.Saw("其他 agent 的最新进展") {
		t.Error("progress injected although agents.dsh.inject is off")
	}
	if !quiet.Saw("mcp__bridge__session_list") {
		t.Error("MCP tools gone with injection off")
	}

	must(t, a.UninstallHooks(context.Background()))
	if got := read(t, patch); strings.Contains(got, "bridge:begin") || strings.Contains(got, "bridge-mcp") || !strings.HasPrefix(got, "# my own settings") {
		t.Errorf("uninstall: %s", got)
	}
}
