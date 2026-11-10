package acceptance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

const codexConfig = `# my codex settings
model = "gpt-5.5" # chosen by hand

# reverse engineering
[mcp_servers.rea]
command = "/opt/homebrew/bin/rea"
args = ["mcp"]
startup_timeout_sec = 30

[mcp_servers.rea.env]
JAVA_HOME = "/jdk"

[mcp_servers.bridge]
command = "/x/bridge"
args = ["mcp"]
`

func claudeServers(t *testing.T, e *env) map[string]map[string]any {
	t.Helper()
	var doc struct {
		Servers map[string]map[string]any `json:"mcpServers"`
	}
	must(t, json.Unmarshal([]byte(read(t, filepath.Join(e.home, ".claude.json"))), &doc))
	return doc.Servers
}

func codexServers(t *testing.T, e *env) map[string]map[string]any {
	t.Helper()
	var doc struct {
		Servers map[string]map[string]any `toml:"mcp_servers"`
	}
	_, err := toml.Decode(read(t, filepath.Join(e.home, ".codex/config.toml")), &doc)
	must(t, err)
	return doc.Servers
}

func TestMCPConfigSync(t *testing.T) {
	e := newEnv(t)
	e.base()
	claudeJSON := filepath.Join(e.home, ".claude.json")
	codexTOML := filepath.Join(e.home, ".codex/config.toml")
	write(t, claudeJSON, `{"numStartups": 7, "mcpServers": {"bridge": {"type": "stdio", "command": "/x/bridge", "args": ["mcp"]}, "mine": {"type": "stdio", "command": "mine"}}}`)
	write(t, codexTOML, codexConfig)
	source := filepath.Join(e.home, ".config/bridge/mcp.toml")

	// Off by default: nothing is touched.
	r := e.run("", "sync")
	if read(t, codexTOML) != codexConfig || strings.Contains(r.out, "rea") {
		t.Fatalf("synced while off: %s", r.out)
	}
	write(t, filepath.Join(e.home, ".config/bridge/config.toml"), "version = 1\n[sync]\nmcp_configs = true\n")

	// Servers living in only one agent are offered for adoption, bridge's own
	// registration never.
	r = e.run("", "plan")
	if !strings.Contains(r.out, "need_source") || !strings.Contains(r.out, "rea") || !strings.Contains(r.out, "mine") || strings.Contains(r.out, "· bridge") {
		t.Fatalf("plan: %s", r.out)
	}

	// Adopting Codex's servers writes the source and delivers them to Claude
	// in the same run; Codex's own file is left byte-for-byte alone.
	r = e.run("", "sync", "--adopt", "codex")
	if r.err != nil {
		t.Fatal(r.out)
	}
	src := read(t, source)
	if !strings.Contains(src, "[servers.rea]") || !strings.Contains(src, "[servers.rea.codex]") || !strings.Contains(src, "startup_timeout_sec = 30") {
		t.Fatalf("source: %s", src)
	}
	if read(t, codexTOML) != codexConfig {
		t.Fatalf("codex config rewritten:\n%s", read(t, codexTOML))
	}
	rea := claudeServers(t, e)["rea"]
	if rea["command"] != "/opt/homebrew/bin/rea" || rea["env"].(map[string]any)["JAVA_HOME"] != "/jdk" || rea["startup_timeout_sec"] != nil {
		t.Fatalf("claude rea: %v", rea)
	}
	var doc map[string]any
	must(t, json.Unmarshal([]byte(read(t, claudeJSON)), &doc))
	if doc["numStartups"] != float64(7) || claudeServers(t, e)["mine"] == nil {
		t.Fatal("other Claude settings lost")
	}

	// A source edit reaches both agents; Codex keeps its comments and the
	// fields only it understands.
	write(t, source, strings.Replace(src, `args = ["mcp"]`, `args = ["mcp", "--v2"]`, 1))
	if r = e.run("", "sync"); r.err != nil {
		t.Fatal(r.out)
	}
	cx := read(t, codexTOML)
	for _, keep := range []string{"# my codex settings", `model = "gpt-5.5" # chosen by hand`, "[mcp_servers.bridge]"} {
		if !strings.Contains(cx, keep) {
			t.Errorf("codex lost %q:\n%s", keep, cx)
		}
	}
	if s := codexServers(t, e)["rea"]; s["startup_timeout_sec"] != int64(30) || len(s["args"].([]any)) != 2 {
		t.Errorf("codex rea: %v", s)
	}
	if a := claudeServers(t, e)["rea"]["args"].([]any); len(a) != 2 {
		t.Errorf("claude rea args: %v", a)
	}

	// A hand edit in one agent is drift: kept, and the other agent still
	// receives the next source change.
	var cdoc map[string]any
	must(t, json.Unmarshal([]byte(read(t, claudeJSON)), &cdoc))
	cdoc["mcpServers"].(map[string]any)["rea"].(map[string]any)["command"] = "/hand/edited"
	b, _ := json.Marshal(cdoc)
	write(t, claudeJSON, string(b))
	write(t, source, strings.Replace(read(t, source), `"--v2"`, `"--v3"`, 1))
	r = e.run("", "sync")
	if !strings.Contains(r.out, "drift") || claudeServers(t, e)["rea"]["command"] != "/hand/edited" {
		t.Errorf("drift: %s", r.out)
	}
	if !strings.Contains(read(t, codexTOML), `"--v3"`) {
		t.Error("codex missed the update")
	}

	// A different same-named server the user already has is never overwritten
	// unless forced.
	write(t, source, read(t, source)+"\n[servers.mine]\ncommand = \"theirs\"\n")
	r = e.run("", "sync")
	if !strings.Contains(r.out, "conflict") || claudeServers(t, e)["mine"]["command"] != "mine" {
		t.Errorf("conflict: %s", r.out)
	}
	if codexServers(t, e)["mine"]["command"] != "theirs" {
		t.Error("new server not added to codex")
	}

	// Servers bridge added are removed when they leave the source.
	write(t, source, read(t, source)+"\n[servers.fresh]\ncommand = \"fresh\"\n")
	must(t, e.run("", "sync").err)
	if claudeServers(t, e)["fresh"] == nil || codexServers(t, e)["fresh"] == nil {
		t.Fatal("fresh not added")
	}
	write(t, source, strings.Replace(read(t, source), "\n[servers.fresh]\ncommand = \"fresh\"\n", "", 1))
	r = e.run("", "sync")
	if claudeServers(t, e)["fresh"] != nil || codexServers(t, e)["fresh"] != nil {
		t.Errorf("fresh not removed: %s", r.out)
	}

	// Uninstall takes away only what bridge added and nobody edited.
	write(t, source, read(t, source)+"\n[servers.fresh]\ncommand = \"fresh\"\n")
	must(t, e.run("", "sync").err)
	r = e.run("", "uninstall", "--yes")
	if r.err != nil {
		t.Fatal(r.out)
	}
	cs, xs := claudeServers(t, e), codexServers(t, e)
	if cs["fresh"] != nil || xs["fresh"] != nil || xs["mine"] != nil {
		t.Errorf("bridge's servers left: claude=%v codex=%v", cs["fresh"], xs["mine"])
	}
	if cs["rea"]["command"] != "/hand/edited" || cs["mine"]["command"] != "mine" || xs["rea"] == nil {
		t.Errorf("user's servers removed: %v %v %v", cs["rea"], cs["mine"], xs["rea"])
	}
	if _, err := os.Stat(source); err != nil {
		t.Error("uninstall deleted the user's source file")
	}
}

func TestMCPProjectScope(t *testing.T) {
	e := newEnv(t)
	e.base()
	write(t, filepath.Join(e.home, ".config/bridge/config.toml"), "version = 1\n[sync]\nmcp_configs = true\n")
	write(t, filepath.Join(e.proj, ".agents/mcp.toml"), "[servers.db]\ncommand = \"dbtool\"\nenv = { DSN = \"local\" }\n\n[servers.web]\nurl = \"https://example.test/mcp\"\ntransport = \"sse\"\n")
	r := e.run("", "sync")
	if r.err != nil {
		t.Fatal(r.out)
	}
	var mcpJSON struct {
		Servers map[string]map[string]any `json:"mcpServers"`
	}
	must(t, json.Unmarshal([]byte(read(t, filepath.Join(e.proj, ".mcp.json"))), &mcpJSON))
	if mcpJSON.Servers["db"]["command"] != "dbtool" || mcpJSON.Servers["web"]["type"] != "sse" {
		t.Errorf(".mcp.json: %v", mcpJSON.Servers)
	}
	var cx struct {
		Servers map[string]map[string]any `toml:"mcp_servers"`
	}
	_, err := toml.Decode(read(t, filepath.Join(e.proj, ".codex/config.toml")), &cx)
	must(t, err)
	if cx.Servers["db"]["command"] != "dbtool" || cx.Servers["web"] != nil {
		t.Errorf("codex project config: %v", cx.Servers)
	}
	if !strings.Contains(r.out, "SSE") && !strings.Contains(e.run("", "plan").out, "SSE") {
		t.Error("unsupported transport not explained")
	}
	if _, err := os.Stat(filepath.Join(e.home, ".claude.json")); err == nil {
		if strings.Contains(read(t, filepath.Join(e.home, ".claude.json")), "dbtool") {
			t.Error("project server leaked into global config")
		}
	}
}
