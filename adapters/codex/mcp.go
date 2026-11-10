package codex

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/BurntSushi/toml"

	"bridge/adapters/internal/common"
	"bridge/internal/fsutil"
	"bridge/internal/tomledit"
	kit "bridge/pkg/adapterkit"
)

var mcpFields = common.MCPFields{Command: "command", Args: "args", Env: "env", Cwd: "cwd", URL: "url", Headers: "http_headers"}

const (
	syncBegin = "# bridge:begin mcp-sync (servers synced by bridge from mcp.toml)"
	syncEnd   = "# bridge:end mcp-sync"
)

// MCPConfigPath implements adapterkit.MCPConfigurer. Codex reads project
// config only for repositories the user has trusted.
func (a *Adapter) MCPConfigPath(scope, projectRoot string) string {
	if scope == "project" {
		if projectRoot == "" {
			return ""
		}
		return filepath.Join(projectRoot, ".codex", "config.toml")
	}
	return configPath()
}

// SupportsMCP reports why a server can't be written to Codex, or "".
func (a *Adapter) SupportsMCP(s kit.MCPServer) string {
	if s.Transport == "sse" {
		return "Codex 不支持 SSE 传输的 MCP server"
	}
	return ""
}

func readServers(text string) (map[string]kit.MCPServer, error) {
	var doc struct {
		Servers map[string]map[string]any `toml:"mcp_servers"`
	}
	if _, err := toml.Decode(text, &doc); err != nil {
		return nil, err
	}
	out := map[string]kit.MCPServer{}
	for name, m := range doc.Servers {
		s, err := common.MCPFromNative(m, mcpFields)
		if err != nil {
			return nil, fmt.Errorf("mcp_servers.%s.%w", name, err)
		}
		if s.URL != "" {
			s.Transport = "http"
		}
		out[name] = s
	}
	return out, nil
}

// ReadMCP implements adapterkit.MCPConfigurer.
func (a *Adapter) ReadMCP(path string) (map[string]kit.MCPServer, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]kit.MCPServer{}, nil
	}
	if err != nil {
		return nil, err
	}
	return readServers(string(b))
}

// WriteMCP implements adapterkit.MCPConfigurer. Synced servers live in a
// marked block; a server replaced or removed is first taken out wherever it
// is defined. The result is parsed back before it is written.
func (a *Adapter) WriteMCP(path string, set map[string]kit.MCPServer, remove []string) error {
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	text := string(b)
	for _, name := range append(append([]string{}, remove...), common.SortedNames(set)...) {
		text = tomledit.RemoveTables(text, "mcp_servers", name)
	}
	block := tomledit.Block(text, syncBegin, syncEnd)
	for _, name := range common.SortedNames(set) {
		t, err := tomledit.Encode(common.MCPToNative(set[name], mcpFields), "mcp_servers", name)
		if err != nil {
			return err
		}
		block = block + "\n\n" + t
	}
	text = tomledit.ReplaceBlock(text, syncBegin, syncEnd, block)
	got, err := readServers(text)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for _, name := range remove {
		if _, ok := got[name]; ok {
			return fmt.Errorf("%s: mcp_servers.%s is defined in a form bridge can't edit; change it by hand", path, name)
		}
	}
	for name, want := range set {
		if g := got[name]; !reflect.DeepEqual(normalize(g), normalize(want)) {
			return fmt.Errorf("%s: mcp_servers.%s is defined in a form bridge can't edit; change it by hand", path, name)
		}
	}
	if text == string(b) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return fsutil.SafeWrite(path, []byte(text), 0o600)
}

// normalize makes a decoded server comparable with one built in memory.
func normalize(s kit.MCPServer) string {
	out, _ := tomledit.Encode(common.MCPToNative(s, mcpFields), "x")
	return out
}
