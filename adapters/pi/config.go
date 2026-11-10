package pi

import (
	"bytes"
	"context"
	"encoding/json"
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

func mcpPath() string { return filepath.Join(Root(), "mcp.json") }

func readJSON(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	doc := map[string]any{}
	if len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, &doc); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	return doc, nil
}

func writeJSON(path string, doc map[string]any) error {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	e.SetIndent("", "  ")
	if err := e.Encode(doc); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return fsutil.SafeWrite(path, b.Bytes(), 0o600)
}

// InstallHooks implements Adapter. pi has no shell hooks; connecting it
// registers bridge's MCP server so pi can query other agents' progress.
func (a *Adapter) InstallHooks(ctx context.Context, cfg kit.HookConfig) error {
	if len(cfg.MCPCommand) == 0 {
		return nil
	}
	return a.WriteMCP(mcpPath(), map[string]kit.MCPServer{"bridge": {Command: cfg.MCPCommand[0], Args: cfg.MCPCommand[1:]}}, nil)
}

// UninstallHooks implements Adapter.
func (a *Adapter) UninstallHooks(ctx context.Context) error {
	if _, err := os.Stat(mcpPath()); err != nil {
		return nil
	}
	return a.WriteMCP(mcpPath(), nil, []string{"bridge"})
}

// HooksInstalled implements adapterkit.HookChecker: connected means bridge's
// MCP server is registered.
func (a *Adapter) HooksInstalled() bool {
	s, err := a.ReadMCP(mcpPath())
	return err == nil && s["bridge"].Command != ""
}

var mcpFields = common.MCPFields{Command: "command", Args: "args", Env: "env", URL: "url", Headers: "headers"}

// MCPConfigPath implements adapterkit.MCPConfigurer.
func (a *Adapter) MCPConfigPath(scope, projectRoot string) string {
	if scope == model.ScopeProject {
		if projectRoot == "" {
			return ""
		}
		return filepath.Join(projectRoot, ".pi", "mcp.json")
	}
	return mcpPath()
}

// SupportsMCP reports why a server can't be written to pi, or "".
func (a *Adapter) SupportsMCP(s kit.MCPServer) string {
	switch {
	case s.Transport == "sse":
		return "pi 只支持 stdio 和 streamable HTTP 的 MCP server"
	case s.Cwd != "":
		return "pi 的 MCP 配置不支持 cwd"
	}
	return ""
}

// ReadMCP implements adapterkit.MCPConfigurer.
func (a *Adapter) ReadMCP(path string) (map[string]kit.MCPServer, error) {
	doc, err := readJSON(path)
	if err != nil {
		return nil, err
	}
	out := map[string]kit.MCPServer{}
	servers, _ := doc["mcpServers"].(map[string]any)
	for name, v := range servers {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		s, err := common.MCPFromNative(m, mcpFields, "type")
		if err != nil {
			return nil, fmt.Errorf("mcpServers.%s.%w", name, err)
		}
		if s.URL != "" && s.Command == "" {
			s.Transport = "http"
		}
		out[name] = s
	}
	return out, nil
}

// WriteMCP implements adapterkit.MCPConfigurer.
func (a *Adapter) WriteMCP(path string, set map[string]kit.MCPServer, remove []string) error {
	doc, err := readJSON(path)
	if err != nil {
		return err
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	for _, n := range remove {
		delete(servers, n)
	}
	for n, s := range set {
		servers[n] = common.MCPToNative(s, mcpFields)
	}
	doc["mcpServers"] = servers
	return writeJSON(path, doc)
}

// ProbeVersion implements adapterkit.VersionProber. The installed client
// continues a session bridge wrote, in a throwaway agent directory, against
// a model endpoint served from this process: the probe passes when the
// client sends the written history to that endpoint, which proves it loaded
// the session. Nothing leaves the machine.
func (a *Adapter) ProbeVersion(ctx context.Context, version string) error {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	bin, err := common.FindBinary(ctx, version, []string{"pi", "/opt/homebrew/bin/pi", "/usr/local/bin/pi"})
	if err != nil {
		return err
	}
	marker := "bridge-probe-" + entryID()
	now := time.Now().UTC()
	return probe(ctx, bin, marker, []model.Event{
		model.TextEvent(model.RoleUser, "remember the word "+marker, now),
		model.TextEvent(model.RoleAssistant, "noted: "+marker, now),
	})
}

// probe has the client continue a session made of events; the session must
// mention marker, and the client must send it to the model.
func probe(ctx context.Context, bin, marker string, events []model.Event) error {
	tmp, err := os.MkdirTemp("", "bridge-probe-pi-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if tmp, err = filepath.EvalSymlinks(tmp); err != nil {
		return err
	}
	llm, err := common.StartFakeLLM()
	if err != nil {
		return err
	}
	defer llm.Close()
	agentDir, work := filepath.Join(tmp, "agent"), filepath.Join(tmp, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		return err
	}
	models := map[string]any{"providers": map[string]any{"bridgeprobe": map[string]any{
		"baseUrl": llm.URL, "api": "openai-completions", "apiKey": "none",
		"models": []any{map[string]any{"id": "probe", "cost": map[string]int{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0}}}}}}
	if err := writeJSON(filepath.Join(agentDir, "models.json"), models); err != nil {
		return err
	}
	now := time.Now().UTC()
	id := fsutil.NewUUID()
	data, err := Render(model.Session{CWD: work}, id, now, "bridgeprobe", "probe", events)
	if err != nil {
		return err
	}
	stamp := fmt.Sprintf("%s-%03dZ", now.Format("2006-01-02T15-04-05"), now.Nanosecond()/int(time.Millisecond))
	path := filepath.Join(agentDir, "sessions", encodeCWD(work), stamp+"_"+id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, bin, "-p", "--session", id, "--provider", "bridgeprobe", "--model", "probe", "what word did I ask you to remember?")
	cmd.Dir = work
	cmd.Env = common.ProbeEnv("HOME="+tmp, "PI_CODING_AGENT_DIR="+agentDir)
	out, err := cmd.CombinedOutput()
	if !llm.Saw(marker) {
		msg := strings.TrimSpace(string(out))
		if err != nil {
			return fmt.Errorf("客户端未能继续探测会话：%v %s", err, clip(msg, 200))
		}
		return fmt.Errorf("客户端加载了探测会话，但历史内容缺失")
	}
	return nil
}
