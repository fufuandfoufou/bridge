package acceptance

import (
	"bridge/internal/fakellm"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bridge/adapters/pi"
	kit "bridge/pkg/adapterkit"
)

// piRun has the real pi CLI answer one prompt in the project, talking to a
// local stand-in model, with bridge's extension installed.
func piRun(t *testing.T, e *env, llm *fakellm.Server, prompt string) {
	t.Helper()
	agentDir := filepath.Join(e.home, ".pi/agent")
	models := map[string]any{"providers": map[string]any{"bridgeprobe": map[string]any{
		"baseUrl": llm.URL, "api": "openai-completions", "apiKey": "none",
		"models": []any{map[string]any{"id": "probe", "cost": map[string]int{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0}}}}}}
	b, _ := json.Marshal(models)
	write(t, filepath.Join(agentDir, "models.json"), string(b))
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "pi", "-p", "--provider", "bridgeprobe", "--model", "probe", prompt)
	cmd.Dir = e.proj
	cmd.Env = append(os.Environ(), "HOME="+e.home, "PI_CODING_AGENT_DIR="+agentDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pi: %v\n%s", err, out)
	}
}

func TestPiLiveCollaboration(t *testing.T) {
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skip("pi is not installed")
	}
	e := newEnv(t)
	e.base() // a Claude Code session in the project with progress "q3"/"a3"
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(e.home, ".pi/agent"))
	a := pi.New()
	must(t, a.InstallHooks(context.Background(), kit.HookConfig{Command: binPath, MCPCommand: []string{binPath, "mcp"}}))
	if !a.HooksInstalled() {
		t.Fatal("extension not installed")
	}
	stop := startDaemon(t, e)
	defer stop()
	waitIngested(t, e)

	t.Run("progress_reaches_the_model", func(t *testing.T) {
		llm, err := fakellm.Start()
		must(t, err)
		defer llm.Close()
		piRun(t, e, llm, "continue the work")
		if !llm.Saw("其他 agent 的最新进展") || !llm.Saw("q3") {
			t.Fatalf("Claude Code's progress was not injected:\n%s", strings.Join(llm.Requests(), "\n---\n"))
		}
	})

	t.Run("claimed_file_is_blocked", func(t *testing.T) {
		write(t, filepath.Join(e.proj, ".bridge.toml"), "[live]\nconflict_policy = \"block\"\n")
		target := filepath.Join(e.proj, "shared.go")
		write(t, target, "package demo // x\n")
		claim, _ := json.Marshal(map[string]any{"session_id": "claude-holder", "cwd": e.proj, "tool_name": "Edit",
			"tool_input": map[string]any{"file_path": target}})
		if r := e.run(string(claim), "hook", "pre_tool", "--agent", "claude-code"); r.out != "" {
			t.Fatalf("claim: %s", r.out)
		}
		llm, err := fakellm.Start()
		must(t, err)
		defer llm.Close()
		llm.ToolCall = map[string]any{"name": "edit", "arguments": map[string]any{"path": target,
			"edits": []any{map[string]any{"oldText": "x", "newText": "y"}}}}
		piRun(t, e, llm, "edit shared.go")
		if read(t, target) != "package demo // x\n" {
			t.Error("pi edited a file Claude Code holds under the block policy")
		}
		// The block reason must come back as the edit's tool result, not just
		// appear somewhere (the progress digest also names claude-code).
		found := false
		for _, body := range llm.Requests() {
			var req struct {
				Messages []struct {
					Role    string `json:"role"`
					Content any    `json:"content"`
				} `json:"messages"`
			}
			_ = json.Unmarshal([]byte(body), &req)
			for _, m := range req.Messages {
				c, _ := json.Marshal(m.Content)
				if m.Role == "tool" && strings.Contains(string(c), "claude-code") && strings.Contains(string(c), "shared.go") {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("the block reason did not come back as the tool result:\n%s", strings.Join(llm.Requests(), "\n---\n"))
		}
	})

	t.Run("claimed_file_warns", func(t *testing.T) {
		write(t, filepath.Join(e.proj, ".bridge.toml"), "[live]\nconflict_policy = \"warn\"\n")
		target := filepath.Join(e.proj, "warned.go")
		write(t, target, "package demo // x\n")
		claim, _ := json.Marshal(map[string]any{"session_id": "claude-holder", "cwd": e.proj, "tool_name": "Edit",
			"tool_input": map[string]any{"file_path": target}})
		e.run(string(claim), "hook", "pre_tool", "--agent", "claude-code")
		llm, err := fakellm.Start()
		must(t, err)
		defer llm.Close()
		llm.ToolCall = map[string]any{"name": "edit", "arguments": map[string]any{"path": target,
			"edits": []any{map[string]any{"oldText": "x", "newText": "y"}}}}
		piRun(t, e, llm, "edit warned.go")
		if read(t, target) != "package demo // y\n" {
			t.Errorf("the warn policy should let the edit through: %q", read(t, target))
		}
		if !llm.Saw("warned.go") || !llm.Saw("claude-holder") {
			t.Error("the warning did not reach the model")
		}
	})

	must(t, a.UninstallHooks(context.Background()))
	if _, err := os.Stat(filepath.Join(e.home, ".pi/agent/extensions/bridge.js")); err == nil {
		t.Error("uninstall left the extension")
	}
}

// waitIngested waits until the daemon has indexed the Claude Code fixture.
func waitIngested(t *testing.T, e *env) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if strings.Contains(e.run("", "api", "sessions").out, `"agent":"claude-code"`) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("daemon never ingested the fixture")
}
