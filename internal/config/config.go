// Package config loads four merged layers (defaults, global, project, command
// line) and remembers which layer, file and line produced every value.
package config

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"bridge/internal/fsutil"
	"bridge/internal/privacy"
)

// Schema is the published JSON Schema for the configuration file.
//
//go:embed config.schema.json
var Schema []byte

// AgentOptions are per-agent settings.
type AgentOptions struct {
	Direction string `toml:"direction"`
	// Inject decides whether the agent receives other agents' progress and
	// memory automatically. Unset means yes.
	Inject *bool `toml:"inject"`
}

// Skills configures skill syncing.
type Skills struct {
	Enabled bool     `toml:"enabled"`
	Exclude []string `toml:"exclude"`
}

// Sync holds the category switches.
type Sync struct {
	Sessions      bool     `toml:"sessions"`
	Memory        bool     `toml:"memory"`
	GlobalMemory  bool     `toml:"global_memory"`
	ProjectMemory bool     `toml:"project_memory"`
	Rules         bool     `toml:"rules"`
	RulesSource   string   `toml:"rules_source"`
	MCPConfigs    bool     `toml:"mcp_configs"`
	MCPExclude    []string `toml:"mcp_exclude"`
	ExcludeFiles  []string `toml:"exclude_files"`
	Skills        Skills   `toml:"skills"`
}

// Handoff configures session migration.
type Handoff struct {
	KeepRecentTurns int     `toml:"keep_recent_turns"`
	MaxContextRatio float64 `toml:"max_context_ratio"`
}

// Live configures real-time collaboration.
type Live struct {
	InjectOnPrompt    bool   `toml:"inject_on_prompt"`
	InjectTokenBudget int    `toml:"inject_token_budget"`
	ConflictPolicy    string `toml:"conflict_policy"`
}

// Memory configures extraction.
type Memory struct {
	AutoExtract      bool     `toml:"auto_extract"`
	Extractor        string   `toml:"extractor"`
	GlobalCategories []string `toml:"global_categories"`
	MinConfidence    float64  `toml:"min_confidence"`
	MaxGlobal        int      `toml:"max_global"`
}

// Privacy configures exclusion and redaction.
type Privacy struct {
	ExcludeProjects []string `toml:"exclude_projects"`
	IncludeProjects []string `toml:"include_projects"`
	Redact          []string `toml:"redact"`
}

// ExternalAdapter explicitly registers an external adapter executable.
type ExternalAdapter struct {
	Path string `toml:"path"`
}

// Config is the fully merged configuration.
type Config struct {
	Version  int
	Enabled  []string
	Agents   map[string]AgentOptions
	Sync     Sync
	Handoff  Handoff
	Live     Live
	Memory   Memory
	Privacy  Privacy
	Adapters map[string]ExternalAdapter

	// Origins maps a dotted key to where its value came from.
	Origins map[string]Origin
	// Warnings lists non-fatal problems such as unknown keys.
	Warnings []string
}

// Origin explains where a value came from.
type Origin struct {
	Layer string // default | global | project | cli
	File  string
	Line  int
}

func (o Origin) String() string {
	if o.File == "" {
		return o.Layer
	}
	return fmt.Sprintf("%s (%s:%d)", o.Layer, o.File, o.Line)
}

// Directions.
const (
	DirBoth    = "both"
	DirSend    = "send-only"
	DirReceive = "receive-only"
)

const defaultsTOML = `
version = 1

[agents]
enabled = ["claude-code", "codex", "pi", "dsh"]

# DeepSeek Harness uploads its session logs by default, which would include
# injected progress from other agents, so injection there is opt-in.
[agents.dsh]
inject = false

[sync]
sessions = true
memory = true
global_memory = true
project_memory = true
rules = true
rules_source = "AGENTS.md"
mcp_configs = false
# bridge registers itself; node_repl is provided by the ChatGPT app.
mcp_exclude = ["bridge", "node_repl"]
exclude_files = []

[sync.skills]
enabled = true
exclude = []

[handoff]
keep_recent_turns = 20
max_context_ratio = 0.7

[live]
inject_on_prompt = true
inject_token_budget = 800
conflict_policy = "warn"

[memory]
auto_extract = false
extractor = "claude-code"
global_categories = ["coding_style", "tools", "communication"]
min_confidence = 0.7
max_global = 50

[privacy]
exclude_projects = []
include_projects = []
redact = []
`

// Options selects the layers to load.
type Options struct {
	GlobalPath  string   // defaults to ~/.config/bridge/config.toml
	ProjectRoot string   // when set, <root>/.bridge.toml is loaded
	Set         []string // command-line overrides, "dotted.key=value"
}

// GlobalPath is the default global configuration location.
func GlobalPath() string { return filepath.Join(fsutil.ConfigDir(), "config.toml") }

// Load merges all layers. Later layers override earlier ones; tables merge
// key by key and arrays replace as a whole.
func Load(o Options) (*Config, error) {
	merged := map[string]any{}
	origins := map[string]Origin{}
	var warnings []string

	apply := func(layer, file string, text []byte) error {
		lines := scanLines(text)
		var m map[string]any
		if _, err := toml.Decode(string(text), &m); err != nil {
			if file != "" {
				return fmt.Errorf("%s: %w", file, err)
			}
			return err
		}
		if layer == "project" {
			applyFloor(merged, m)
		}
		walkLeaves(m, "", func(key string) {
			origins[key] = Origin{Layer: layer, File: file, Line: lines[key]}
		})
		deepMerge(merged, m)
		return nil
	}
	if err := apply("default", "", []byte(defaultsTOML)); err != nil {
		return nil, err
	}
	global := o.GlobalPath
	if global == "" {
		global = GlobalPath()
	}
	if b, err := os.ReadFile(global); err == nil {
		if err := apply("global", global, b); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if o.ProjectRoot != "" {
		p := filepath.Join(o.ProjectRoot, ".bridge.toml")
		if b, err := os.ReadFile(p); err == nil {
			if err := apply("project", p, b); err != nil {
				return nil, err
			}
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	for _, kv := range o.Set {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, fmt.Errorf("invalid override %q, want key=value", kv)
		}
		tmp := map[string]any{}
		setDotted(tmp, strings.TrimSpace(k), parseScalar(strings.TrimSpace(v)))
		applyFloor(merged, tmp)
		walkLeaves(tmp, "", func(key string) { origins[key] = Origin{Layer: "cli"} })
		deepMerge(merged, tmp)
	}
	cfg, w, err := decode(merged)
	if err != nil {
		return nil, err
	}
	cfg.Origins = origins
	cfg.Warnings = append(warnings, w...)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadForDir loads configuration for a working directory, using the git
// repository root (or the directory itself) as the project root.
func LoadForDir(dir string, set []string) (*Config, error) {
	return Load(Options{ProjectRoot: ProjectRoot(dir), Set: set})
}

// ProjectRoot walks up from dir to the nearest directory containing .git.
func ProjectRoot(dir string) string {
	if dir == "" {
		return ""
	}
	d := filepath.Clean(dir)
	for {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return filepath.Clean(dir)
		}
		d = parent
	}
}

func decode(m map[string]any) (*Config, []string, error) {
	// [agents] mixes the "enabled" array with per-agent subtables.
	agents, _ := m["agents"].(map[string]any)
	rest := map[string]any{}
	for k, v := range m {
		if k != "agents" {
			rest[k] = v
		}
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(rest); err != nil {
		return nil, nil, err
	}
	var raw struct {
		Version  int                        `toml:"version"`
		Sync     Sync                       `toml:"sync"`
		Handoff  Handoff                    `toml:"handoff"`
		Live     Live                       `toml:"live"`
		Memory   Memory                     `toml:"memory"`
		Privacy  Privacy                    `toml:"privacy"`
		Adapters map[string]ExternalAdapter `toml:"adapters"`
	}
	md, err := toml.Decode(buf.String(), &raw)
	if err != nil {
		return nil, nil, err
	}
	var warnings []string
	for _, k := range md.Undecoded() {
		warnings = append(warnings, "unknown config key: "+k.String())
	}
	sort.Strings(warnings)
	c := &Config{Version: raw.Version, Sync: raw.Sync, Handoff: raw.Handoff, Live: raw.Live,
		Memory: raw.Memory, Privacy: raw.Privacy, Adapters: raw.Adapters, Agents: map[string]AgentOptions{}}
	for k, v := range agents {
		switch x := v.(type) {
		case []any:
			if k == "enabled" {
				for _, e := range x {
					if s, ok := e.(string); ok {
						c.Enabled = append(c.Enabled, s)
					}
				}
			}
		case []string:
			if k == "enabled" {
				c.Enabled = append(c.Enabled, x...)
			}
		case map[string]any:
			opt := AgentOptions{}
			if d, ok := x["direction"].(string); ok {
				opt.Direction = d
			}
			if v, ok := x["inject"].(bool); ok {
				opt.Inject = &v
			}
			c.Agents[k] = opt
		default:
			warnings = append(warnings, "unknown config key: agents."+k)
		}
	}
	return c, warnings, nil
}

// Validate checks semantic constraints.
func (c *Config) Validate() error {
	if c.Version != 1 {
		return fmt.Errorf("unsupported config version %d", c.Version)
	}
	if p := c.Live.ConflictPolicy; p != "warn" && p != "block" {
		return fmt.Errorf("live.conflict_policy must be warn or block, got %q", p)
	}
	if r := c.Handoff.MaxContextRatio; r <= 0 || r > 1 {
		return fmt.Errorf("handoff.max_context_ratio must be in (0,1], got %v", r)
	}
	if c.Handoff.KeepRecentTurns < 1 {
		return fmt.Errorf("handoff.keep_recent_turns must be at least 1")
	}
	if c.Live.InjectTokenBudget < 0 {
		return fmt.Errorf("live.inject_token_budget must not be negative")
	}
	if m := c.Memory.MinConfidence; m < 0 || m > 1 {
		return fmt.Errorf("memory.min_confidence must be in [0,1], got %v", m)
	}
	for id, a := range c.Agents {
		switch a.Direction {
		case "", DirBoth, DirSend, DirReceive:
		default:
			return fmt.Errorf("agents.%s.direction: invalid value %q", id, a.Direction)
		}
	}
	if _, err := privacy.NewRedactor(c.Privacy.Redact); err != nil {
		return err
	}
	_, err := c.Matcher()
	return err
}

// Redactor builds the configured redactor.
func (c *Config) Redactor() (*privacy.Redactor, error) { return privacy.NewRedactor(c.Privacy.Redact) }

// Matcher builds the configured project matcher.
func (c *Config) Matcher() (*privacy.Matcher, error) {
	return privacy.NewMatcher(c.Privacy.IncludeProjects, c.Privacy.ExcludeProjects)
}

// AgentEnabled reports whether the agent takes part in syncing.
func (c *Config) AgentEnabled(id string) bool {
	for _, e := range c.Enabled {
		if e == id {
			return true
		}
	}
	return false
}

// InjectEnabled reports whether the agent receives progress and memory
// automatically through its hooks or plugin.
func (c *Config) InjectEnabled(agent string) bool {
	if o, ok := c.Agents[agent]; ok && o.Inject != nil {
		return *o.Inject
	}
	return true
}

// Direction returns the agent's sync direction, defaulting to both.
func (c *Config) Direction(id string) string {
	if d := c.Agents[id].Direction; d != "" {
		return d
	}
	return DirBoth
}

// CanSend reports whether the agent's data may flow out to others.
func (c *Config) CanSend(id string) bool {
	return c.AgentEnabled(id) && c.Direction(id) != DirReceive
}

// CanReceive reports whether others' data may flow into the agent.
func (c *Config) CanReceive(id string) bool {
	return c.AgentEnabled(id) && c.Direction(id) != DirSend
}

// GlobalMemoryOn reports whether global preferences are shared.
func (c *Config) GlobalMemoryOn() bool { return c.Sync.Memory && c.Sync.GlobalMemory }

// ProjectMemoryOn reports whether project memories are shared.
func (c *Config) ProjectMemoryOn() bool { return c.Sync.Memory && c.Sync.ProjectMemory }

// Why returns the origin of a dotted key.
func (c *Config) Why(key string) (Origin, bool) { o, ok := c.Origins[key]; return o, ok }

// floorKeys are safety settings that narrower layers may add to but never
// remove from: a project file cannot opt out of a global exclusion.
var floorKeys = []string{"exclude_projects", "redact"}

// applyFloor rewrites an override so it unions with the current value. An
// override adding nothing is dropped, leaving the wider layer as the origin.
func applyFloor(merged, override map[string]any) {
	priv, _ := override["privacy"].(map[string]any)
	current, _ := merged["privacy"].(map[string]any)
	for _, k := range floorKeys {
		v, ok := priv[k]
		if !ok {
			continue
		}
		have := asStrings(current[k])
		union := append([]string{}, have...)
		for _, s := range asStrings(v) {
			if !containsString(union, s) {
				union = append(union, s)
			}
		}
		if len(union) == len(have) {
			delete(priv, k)
			continue
		}
		priv[k] = union
	}
}

func asStrings(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{x}
	}
	return nil
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func walkLeaves(m map[string]any, prefix string, f func(string)) {
	for k, v := range m {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if sub, ok := v.(map[string]any); ok {
			walkLeaves(sub, key, f)
			continue
		}
		f(key)
	}
}

func deepMerge(dst, src map[string]any) {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				deepMerge(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
}

func setDotted(m map[string]any, key string, v any) {
	parts := strings.Split(key, ".")
	for _, p := range parts[:len(parts)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
	m[parts[len(parts)-1]] = v
}

func parseScalar(s string) any {
	if b, err := strconv.ParseBool(s); err == nil {
		return b
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	if strings.HasPrefix(s, "[") {
		var w struct{ V any }
		if _, err := toml.Decode("V = "+s, &w); err == nil {
			return w.V
		}
	}
	return strings.Trim(s, `"'`)
}

var (
	sectionRe = regexp.MustCompile(`^\s*\[([^\[\]]+)\]\s*(#.*)?$`)
	keyRe     = regexp.MustCompile(`^\s*([A-Za-z0-9_\-]+)\s*=`)
)

// scanLines maps dotted keys to 1-based line numbers. toml's metadata carries
// no key positions, and a line scan is enough for this flat file format.
func scanLines(text []byte) map[string]int {
	out := map[string]int{}
	section := ""
	for i, line := range strings.Split(string(text), "\n") {
		if m := sectionRe.FindStringSubmatch(line); m != nil {
			section = strings.TrimSpace(m[1])
			continue
		}
		if m := keyRe.FindStringSubmatch(line); m != nil {
			k := m[1]
			if section != "" {
				k = section + "." + k
			}
			if _, seen := out[k]; !seen {
				out[k] = i + 1
			}
		}
	}
	return out
}
