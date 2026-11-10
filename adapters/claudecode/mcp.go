package claudecode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"bridge/adapters/internal/common"
	"bridge/internal/fsutil"
	kit "bridge/pkg/adapterkit"
)

var mcpFields = common.MCPFields{Command: "command", Args: "args", Env: "env", URL: "url", Headers: "headers"}

// MCPConfigPath implements adapterkit.MCPConfigurer: user-scope servers live
// in ~/.claude.json, project servers in the repository's .mcp.json.
func (a *Adapter) MCPConfigPath(scope, projectRoot string) string {
	if scope == "project" {
		if projectRoot == "" {
			return ""
		}
		return filepath.Join(projectRoot, ".mcp.json")
	}
	return globalJSON()
}

// SupportsMCP reports why a server can't be written to Claude Code, or "".
func (a *Adapter) SupportsMCP(s kit.MCPServer) string {
	if s.Cwd != "" {
		return "Claude Code 的 MCP 配置不支持 cwd"
	}
	return ""
}

func readDoc(path string) (map[string]any, []byte, error) {
	orig, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]any{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	doc := map[string]any{}
	if len(bytes.TrimSpace(orig)) > 0 {
		if err := json.Unmarshal(orig, &doc); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	return doc, orig, nil
}

// ReadMCP implements adapterkit.MCPConfigurer.
func (a *Adapter) ReadMCP(path string) (map[string]kit.MCPServer, error) {
	doc, _, err := readDoc(path)
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
		if t, _ := m["type"].(string); t == "http" || t == "sse" {
			s.Transport = t
		} else if t == "" && s.URL != "" && s.Command == "" {
			s.Transport = "http"
		}
		out[name] = s
	}
	return out, nil
}

// WriteMCP implements adapterkit.MCPConfigurer.
func (a *Adapter) WriteMCP(path string, set map[string]kit.MCPServer, remove []string) error {
	doc, _, err := readDoc(path)
	if err != nil {
		return err
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	for _, name := range remove {
		delete(servers, name)
	}
	for name, s := range set {
		m := common.MCPToNative(s, mcpFields)
		m["type"] = "stdio"
		if s.Transport != "" {
			m["type"] = s.Transport
		}
		servers[name] = m
	}
	doc["mcpServers"] = servers
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	e.SetIndent("", "  ")
	if err := e.Encode(doc); err != nil {
		return err
	}
	return fsutil.SafeWrite(path, b.Bytes(), 0o600)
}
