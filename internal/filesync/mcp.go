package filesync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"

	"bridge/internal/fsutil"
	"bridge/internal/privacy"
	"bridge/internal/tomledit"
	kit "bridge/pkg/adapterkit"
)

// Remove deletes an MCP server bridge wrote once its source entry is gone.
const Remove = "remove"

// MCP servers are synced entry by entry inside each agent's own config file,
// never as whole files. Bridge remembers a fingerprint of every entry it
// wrote or matched, which tells its own unchanged entries apart from ones the
// user edited, and from same-named servers the user had already.

type mcpRecord struct {
	Hash  string `json:"hash"`
	Owned bool   `json:"owned"` // bridge added it, so uninstall takes it away
}

type mcpTarget struct {
	Agent   string               `json:"agent"`
	Servers map[string]mcpRecord `json:"servers"`
}

type mcpState struct {
	Targets map[string]*mcpTarget `json:"targets"`
}

// MCPStatePath records the MCP entries bridge manages.
func MCPStatePath() string { return filepath.Join(fsutil.DataDir(), "mcp-sync.json") }

func loadMCPState() (*mcpState, error) {
	st := &mcpState{Targets: map[string]*mcpTarget{}}
	b, err := os.ReadFile(MCPStatePath())
	if os.IsNotExist(err) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, err
	}
	if st.Targets == nil {
		st.Targets = map[string]*mcpTarget{}
	}
	return st, nil
}

func (st *mcpState) save() error {
	if err := fsutil.EnsurePrivateDir(fsutil.DataDir()); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	return fsutil.AtomicWrite(MCPStatePath(), b, 0o600)
}

func (st *mcpState) records(target string) map[string]mcpRecord {
	if t := st.Targets[target]; t != nil {
		return t.Servers
	}
	return nil
}

func (st *mcpState) record(target, agent, name string, r mcpRecord) {
	t := st.Targets[target]
	if t == nil {
		t = &mcpTarget{Agent: agent, Servers: map[string]mcpRecord{}}
		st.Targets[target] = t
	}
	t.Servers[name] = r
}

func (st *mcpState) forget(target, name string) {
	if t := st.Targets[target]; t != nil {
		delete(t.Servers, name)
		if len(t.Servers) == 0 {
			delete(st.Targets, target)
		}
	}
}

// Apply has only the actions, so Plan leaves the agents' configurers here.
var mcpAgents sync.Map // agent ID -> kit.MCPConfigurer

// RegisterMCP makes agents' MCP configurations reachable without a plan, as
// uninstall needs.
func RegisterMCP(agents []Agent) {
	for _, a := range agents {
		if a.MCP != nil {
			mcpAgents.Store(a.ID, a.MCP)
		}
	}
}

func configurer(agent string) (kit.MCPConfigurer, error) {
	v, ok := mcpAgents.Load(agent)
	if !ok {
		return nil, fmt.Errorf("agent %s can't sync MCP configuration", agent)
	}
	return v.(kit.MCPConfigurer), nil
}

// srcServer is one entry of mcp.toml: the shared definition plus, per agent,
// fields only that agent understands (a subtable named after the agent).
type srcServer struct {
	base kit.MCPServer
	per  map[string]map[string]any
}

func (s srcServer) for_(agent string) kit.MCPServer {
	out := s.base
	out.Extra = s.per[agent]
	return out
}

func mcpSourcePath(scope, root, globalDir string) string {
	if scope == "project" {
		return filepath.Join(root, ".agents", "mcp.toml")
	}
	return filepath.Join(globalDir, "mcp.toml")
}

func strings_(v any, key string) ([]string, error) {
	a, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s: expected an array of strings", key)
	}
	out := make([]string, 0, len(a))
	for _, x := range a {
		s, ok := x.(string)
		if !ok {
			return nil, fmt.Errorf("%s: expected an array of strings", key)
		}
		out = append(out, s)
	}
	return out, nil
}

func table(v any, key string) (map[string]string, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: expected a table of strings", key)
	}
	out := map[string]string{}
	for k, x := range m {
		s, ok := x.(string)
		if !ok {
			return nil, fmt.Errorf("%s.%s: expected a string", key, k)
		}
		out[k] = s
	}
	return out, nil
}

func readMCPSource(path string) (map[string]srcServer, bool, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]srcServer{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var doc struct {
		Servers map[string]map[string]any `toml:"servers"`
	}
	if _, err := toml.Decode(string(b), &doc); err != nil {
		return nil, true, err
	}
	out := map[string]srcServer{}
	for name, m := range doc.Servers {
		s := srcServer{per: map[string]map[string]any{}}
		for k, v := range m {
			var err error
			key := "servers." + name + "." + k
			switch k {
			case "transport":
				s.base.Transport, _ = v.(string)
			case "command":
				s.base.Command, _ = v.(string)
			case "cwd":
				s.base.Cwd, _ = v.(string)
			case "url":
				s.base.URL, _ = v.(string)
			case "args":
				s.base.Args, err = strings_(v, key)
			case "env":
				s.base.Env, err = table(v, key)
			case "headers":
				s.base.Headers, err = table(v, key)
			default:
				extra, ok := v.(map[string]any)
				if !ok {
					return nil, true, fmt.Errorf("%s: unknown field; fields for one agent go in a [servers.%s.<agent>] table", key, name)
				}
				s.per[k] = extra
			}
			if err != nil {
				return nil, true, err
			}
		}
		out[name] = s
	}
	return out, true, nil
}

// canon is a stable rendering for comparing definitions from any format.
func canon(s kit.MCPServer) string {
	if s.Transport == "stdio" {
		s.Transport = ""
	}
	if s.Transport == "" && s.URL != "" && s.Command == "" {
		s.Transport = "http"
	}
	if len(s.Args) == 0 {
		s.Args = nil
	}
	if len(s.Env) == 0 {
		s.Env = nil
	}
	if len(s.Headers) == 0 {
		s.Headers = nil
	}
	if len(s.Extra) == 0 {
		s.Extra = nil
	}
	b, _ := json.Marshal(s)
	return string(b)
}

func fingerprint(s kit.MCPServer) string {
	h := sha256.Sum256([]byte(canon(s)))
	return hex.EncodeToString(h[:])
}

// mcpView is everything needed to decide one target's actions.
type mcpView struct {
	src       map[string]srcServer
	srcExists bool
	cur       map[string]kit.MCPServer
	recs      map[string]mcpRecord
	supporter interface{ SupportsMCP(kit.MCPServer) string }
}

func viewMCP(agent, source, target string, st *mcpState) (mcpView, error) {
	var v mcpView
	c, err := configurer(agent)
	if err != nil {
		return v, err
	}
	if v.src, v.srcExists, err = readMCPSource(source); err != nil {
		return v, fmt.Errorf("MCP 源文件 %s 无法解析：%w", source, err)
	}
	if v.cur, err = c.ReadMCP(target); err != nil {
		return v, fmt.Errorf("无法读取 %s：%w", target, err)
	}
	v.recs = st.records(target)
	v.supporter, _ = c.(interface{ SupportsMCP(kit.MCPServer) string })
	return v, nil
}

func (v mcpView) classify(act Action) Action {
	name := act.Name
	s, inSrc := v.src[name]
	c, inCur := v.cur[name]
	r, recd := v.recs[name]
	if inSrc {
		want := s.for_(act.Agent)
		if v.supporter != nil {
			if reason := v.supporter.SupportsMCP(want); reason != "" {
				return with(act, Skip, reason)
			}
		}
		switch {
		case !inCur && recd:
			return with(act, Drift, "已在这个 agent 中被删除；可以以源为准重新写入，或合回源（从源中删除）")
		case !inCur:
			return with(act, Generate, "新增 MCP server")
		case canon(c) == canon(want):
			return with(act, Noop, "与源一致")
		case recd && r.Hash == fingerprint(c):
			return with(act, Generate, "源已更新")
		case recd:
			return with(act, Drift, "被手工修改过；可合回源，或以源为准覆盖")
		}
		return with(act, Conflict, "已有同名但内容不同的配置，不会自动覆盖；可合回源，或以源为准覆盖")
	}
	switch {
	case !inCur:
		return with(act, Noop, "")
	case recd && r.Hash == fingerprint(c):
		return with(act, Remove, "已从源中删除")
	case recd:
		return with(act, Drift, "已从源中删除，但这里被手工修改过；可合回源，或以源为准删除")
	}
	return with(act, NeedSource, "只在 "+act.Agent+" 中配置；可合回源后同步给其他 agent")
}

func planMCP(in Input, a Agent, scope, globalDir string) ([]Action, error) {
	if scope == "project" && in.ProjectRoot == "" {
		return nil, nil
	}
	cfg := in.Config
	source := mcpSourcePath(scope, in.ProjectRoot, globalDir)
	act := Action{Category: "mcp", Agent: a.ID, Scope: scope, Source: source, Format: kit.FormatGenerate}
	if a.MCP == nil {
		return []Action{with(act, Skip, "这个 agent 不支持同步 MCP 配置")}, nil
	}
	act.Target = a.MCP.MCPConfigPath(scope, in.ProjectRoot)
	if act.Target == "" {
		return []Action{with(act, Skip, "这个 agent 没有该范围的 MCP 配置")}, nil
	}
	if r := gate(cfg, a.ID); r != "" {
		return []Action{with(act, Skip, r)}, nil
	}
	if !cfg.Sync.MCPConfigs {
		return []Action{with(act, Skip, "MCP 配置同步已关闭（sync.mcp_configs）")}, nil
	}
	mcpAgents.Store(a.ID, a.MCP)
	st, err := loadMCPState()
	if err != nil {
		return nil, err
	}
	v, err := viewMCP(a.ID, source, act.Target, st)
	if err != nil {
		return []Action{with(act, Conflict, err.Error())}, nil
	}
	names := map[string]bool{}
	for n := range v.src {
		names[n] = true
	}
	for n := range v.cur {
		names[n] = true
	}
	var sorted []string
	for n := range names {
		if !privacy.MatchName(n, cfg.Sync.MCPExclude) {
			sorted = append(sorted, n)
		}
	}
	sort.Strings(sorted)
	var out []Action
	for _, n := range sorted {
		x := act
		x.Name = n
		if y := v.classify(x); y.Kind != Noop || y.Reason != "" {
			out = append(out, y)
		}
	}
	if len(out) == 0 && !v.srcExists {
		out = append(out, with(act, Noop, "还没有 MCP 源（"+source+"），两边也都没有可同步的 server"))
	}
	return out, nil
}

func applyMCP(acts []Action, o Options) (Report, error) {
	var r Report
	st, err := loadMCPState()
	if err != nil {
		return r, err
	}
	defer st.save()
	if o.Adopt != "" {
		if err := adoptMCP(acts, o.Adopt, st); err != nil {
			return r, err
		}
		// The sources changed, so every decision is made again.
		views := map[string]mcpView{}
		for i, a := range acts {
			if a.Name == "" {
				continue
			}
			key := a.Agent + "\x00" + a.Target
			v, ok := views[key]
			if !ok {
				if v, err = viewMCP(a.Agent, a.Source, a.Target, st); err != nil {
					return r, err
				}
				views[key] = v
			}
			acts[i] = v.classify(a)
		}
	}
	type change struct {
		set    map[string]kit.MCPServer
		remove []string
		added  map[string]bool
		acts   []Action
		agent  string
		source string
	}
	changes := map[string]*change{}
	var order []string
	for _, a := range acts {
		kind := a.Kind
		if o.Force && (kind == Drift || kind == Conflict) {
			kind = Generate
		}
		switch kind {
		case Noop, Skip:
			continue
		case Drift, Conflict, NeedSource:
			r.Blocked = append(r.Blocked, a)
			continue
		}
		ch := changes[a.Target]
		if ch == nil {
			ch = &change{set: map[string]kit.MCPServer{}, added: map[string]bool{}, agent: a.Agent, source: a.Source}
			changes[a.Target] = ch
			order = append(order, a.Target)
		}
		ch.acts = append(ch.acts, a)
		if kind == Remove {
			ch.remove = append(ch.remove, a.Name)
		} else {
			ch.set[a.Name] = kit.MCPServer{} // filled below from the source
		}
	}
	for _, target := range order {
		ch := changes[target]
		c, err := configurer(ch.agent)
		if err != nil {
			return r, err
		}
		src, _, err := readMCPSource(ch.source)
		if err != nil {
			return r, err
		}
		before, err := c.ReadMCP(target)
		if err != nil {
			return r, err
		}
		for name := range ch.set {
			s, ok := src[name]
			if !ok { // forced over a hand edit of a server the source dropped
				delete(ch.set, name)
				ch.remove = append(ch.remove, name)
				continue
			}
			ch.set[name] = s.for_(ch.agent)
			_, existed := before[name]
			ch.added[name] = !existed
		}
		if err := c.WriteMCP(target, ch.set, ch.remove); err != nil {
			return r, err
		}
		after, err := c.ReadMCP(target)
		if err != nil {
			return r, err
		}
		for name := range ch.set {
			prev := st.records(target)[name]
			st.record(target, ch.agent, name, mcpRecord{Hash: fingerprint(after[name]), Owned: prev.Owned || ch.added[name]})
		}
		for _, name := range ch.remove {
			st.forget(target, name)
		}
		r.Applied = append(r.Applied, ch.acts...)
	}
	// Entries already matching the source are tracked from now on, so later
	// source edits reach them. They stay the user's: uninstall keeps them.
	for _, a := range acts {
		if a.Kind == Noop && a.Name != "" && a.Reason == "与源一致" {
			if _, ok := st.records(a.Target)[a.Name]; !ok {
				c, err := configurer(a.Agent)
				if err != nil {
					return r, err
				}
				cur, err := c.ReadMCP(a.Target)
				if err != nil {
					return r, err
				}
				if s, ok := cur[a.Name]; ok {
					st.record(a.Target, a.Agent, a.Name, mcpRecord{Hash: fingerprint(s)})
				}
			}
		}
	}
	return r, nil
}

// adoptMCP copies agent's version of the entries needing attention into the
// source. A server the agent no longer has is removed from the source.
func adoptMCP(acts []Action, agent string, st *mcpState) error {
	bySource := map[string][]Action{}
	for _, a := range acts {
		if a.Agent == agent && a.Name != "" && (a.Kind == Drift || a.Kind == NeedSource || a.Kind == Conflict) {
			bySource[a.Source] = append(bySource[a.Source], a)
		}
	}
	for source, list := range bySource {
		c, err := configurer(agent)
		if err != nil {
			return err
		}
		src, _, err := readMCPSource(source)
		if err != nil {
			return fmt.Errorf("MCP 源文件 %s 无法解析：%w", source, err)
		}
		b, err := os.ReadFile(source)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		text := string(b)
		if text == "" {
			text = "# MCP servers shared by every agent. Bridge writes them into each agent's own config.\n" +
				"# Fields only one agent understands go in a [servers.<name>.<agent>] table.\n"
		}
		for _, a := range list {
			cur, err := c.ReadMCP(a.Target)
			if err != nil {
				return err
			}
			text = tomledit.RemoveTables(text, "servers", a.Name)
			s, ok := cur[a.Name]
			if !ok {
				continue
			}
			m := map[string]any{}
			if s.Transport != "" && s.Transport != "stdio" {
				m["transport"] = s.Transport
			}
			put := func(k string, v any, empty bool) {
				if !empty {
					m[k] = v
				}
			}
			put("command", s.Command, s.Command == "")
			put("args", s.Args, len(s.Args) == 0)
			put("env", s.Env, len(s.Env) == 0)
			put("cwd", s.Cwd, s.Cwd == "")
			put("url", s.URL, s.URL == "")
			put("headers", s.Headers, len(s.Headers) == 0)
			for other, extra := range src[a.Name].per {
				if other != agent && len(extra) > 0 {
					m[other] = extra
				}
			}
			if len(s.Extra) > 0 {
				m[agent] = s.Extra
			}
			t, err := tomledit.Encode(m, "servers", a.Name)
			if err != nil {
				return err
			}
			text = strings.TrimRight(text, "\n") + "\n\n" + t
			prev := st.records(a.Target)[a.Name]
			st.record(a.Target, agent, a.Name, mcpRecord{Hash: fingerprint(s), Owned: prev.Owned})
		}
		if err := tomledit.Check(text); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
			return err
		}
		if err := fsutil.SafeWrite(source, []byte(text), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// UninstallMCP removes the servers bridge added that are still as it wrote
// them. Servers the user had before, or edited since, stay.
func UninstallMCP(agents []Agent) ([]string, error) {
	RegisterMCP(agents)
	st, err := loadMCPState()
	if err != nil {
		return nil, err
	}
	var notes []string
	for target, t := range st.Targets {
		c, err := configurer(t.Agent)
		if err != nil {
			notes = append(notes, "未处理 "+target+"："+err.Error())
			continue
		}
		cur, err := c.ReadMCP(target)
		if err != nil {
			return notes, err
		}
		var remove []string
		for name, rec := range t.Servers {
			s, ok := cur[name]
			switch {
			case !ok || !rec.Owned:
			case fingerprint(s) == rec.Hash:
				remove = append(remove, name)
			default:
				notes = append(notes, "保留了被手工修改过的 MCP server "+name+"（"+target+"）")
			}
		}
		if len(remove) > 0 {
			sort.Strings(remove)
			if err := c.WriteMCP(target, nil, remove); err != nil {
				return notes, err
			}
		}
	}
	if err := os.Remove(MCPStatePath()); err != nil && !os.IsNotExist(err) {
		return notes, err
	}
	return notes, nil
}
