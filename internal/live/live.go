// Package live implements real-time collaboration: per-reader watermarks and
// a rule-based delta digest injected into each agent, with no model calls.
package live

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"bridge/internal/config"
	"bridge/internal/handoff"
	"bridge/internal/model"
	"bridge/internal/project"
	"bridge/internal/store"
)

// Footer is appended when an injection exceeds its token budget.
const Footer = "详情可用 bridge MCP 的 session_events 工具查询"

// Group is the new activity of one other session.
type Group struct {
	SessionID string
	Agent     string
	Title     string
	Events    []model.Event
}

func lineDelta(diff string) (add, del int) {
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"):
		case strings.HasPrefix(l, "+"):
			add++
		case strings.HasPrefix(l, "-"):
			del++
		}
	}
	return
}

func oneLine(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return string(r)
}

// Digest renders one group: changed files with line counts, the latest user
// instruction, and the opening of the latest assistant reply (pure).
func Digest(g Group) string {
	type stat struct{ add, del int }
	files := map[string]*stat{}
	var user, assistant string
	for _, e := range g.Events {
		switch {
		case e.Kind == model.KindFileChange && e.File != nil:
			s := files[e.File.Path]
			if s == nil {
				s = &stat{}
				files[e.File.Path] = s
			}
			a, d := lineDelta(e.File.Diff)
			s.add, s.del = s.add+a, s.del+d
		case e.Kind == model.KindMessage && e.Role == model.RoleUser:
			user = e.Text()
		case e.Kind == model.KindMessage && e.Role == model.RoleAssistant:
			assistant = e.Text()
		}
	}
	if len(files) == 0 && user == "" && assistant == "" {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "### %s", g.Agent)
	if g.Title != "" {
		fmt.Fprintf(&b, "（%s）", oneLine(g.Title, 40))
	}
	fmt.Fprintf(&b, " · 会话 %s\n", g.SessionID)
	if len(files) > 0 {
		names := make([]string, 0, len(files))
		for f := range files {
			names = append(names, f)
		}
		sort.Strings(names)
		b.WriteString("改动文件：")
		for i, f := range names {
			if i > 0 {
				b.WriteString("，")
			}
			fmt.Fprintf(&b, "%s (+%d/-%d)", f, files[f].add, files[f].del)
		}
		b.WriteString("\n")
	}
	if user != "" {
		fmt.Fprintf(&b, "用户最新指令：%s\n", oneLine(user, 200))
	}
	if assistant != "" {
		fmt.Fprintf(&b, "助手最新回复：%s\n", oneLine(assistant, 200))
	}
	return b.String()
}

// Summarize joins digests under a token budget; groups that do not fit are
// replaced by a pointer to the MCP tool. Budget 0 means no injection.
func Summarize(groups []Group, budget int) string { return summarize(groups, budget, Footer) }

// footerNoMCP replaces Footer for agents that have no MCP tools to call.
const footerNoMCP = "部分内容已省略"

const digestHeading = "## 其他 agent 的最新进展\n\n"

func footerText(f string) string { return "\n\n（内容较多，" + f + "）\n" }

func summarize(groups []Group, budget int, footer string) string {
	if budget <= 0 {
		return ""
	}
	var parts []string
	used, truncated := model.EstimateTokens(digestHeading+footerText(footer)), false
	for _, g := range groups {
		d := Digest(g)
		if d == "" {
			continue
		}
		n := model.EstimateTokens(d) + 1 // the joining newline
		if used+n > budget {
			truncated = true
			continue
		}
		used += n
		parts = append(parts, d)
	}
	if len(parts) == 0 && !truncated {
		return ""
	}
	out := digestHeading + strings.Join(parts, "\n")
	if truncated {
		out = strings.TrimRight(out, "\n") + footerText(footer)
	}
	return out
}

// Service answers hook events from the store.
type Service struct {
	Store  *store.Store
	Config func(cwd string) (*config.Config, error)
	Now    func() time.Time
	// HasMCP reports whether an agent can call MCP tools; nil means yes.
	HasMCP func(agent string) bool

	mu      sync.Mutex
	pending map[string]map[string]int64
}

func (s *Service) footer(agent string) string {
	if s.HasMCP != nil && !s.HasMCP(agent) {
		return footerNoMCP
	}
	return Footer
}

// Ack confirms the last injection reached the agent and only then advances
// the read watermarks. Delivery is therefore at-least-once: an injection whose
// confirmation is lost is sent again next time.
func (s *Service) Ack(agent, native string) {
	self := SessionKey(agent, native)
	s.mu.Lock()
	marks := s.pending[self]
	delete(s.pending, self)
	s.mu.Unlock()
	for source, seq := range marks {
		_ = s.Store.SetWatermark(self, source, seq)
	}
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

// activeWindow bounds how long a session without a stop event counts as live.
const activeWindow = 2 * time.Hour

// SessionKey is the unified ID of a native session.
func SessionKey(agent, native string) string { return agent + ":" + native }

func (s *Service) load(agent, cwd string) (*config.Config, model.Project, bool, error) {
	cfg, err := s.Config(cwd)
	if err != nil {
		return nil, model.Project{}, false, err
	}
	m, err := cfg.Matcher()
	if err != nil {
		return nil, model.Project{}, false, err
	}
	if m.Excluded(cwd) || !cfg.CanReceive(agent) {
		return cfg, model.Project{}, false, nil
	}
	return cfg, project.Identify(cwd), true, nil
}

// others returns other agents' recently active sessions in the project.
func (s *Service) others(cfg *config.Config, projectID, self, agent string) ([]model.Session, error) {
	list, err := s.Store.ListSessions(store.SessionFilter{ProjectID: projectID, Since: s.now().Add(-activeWindow)})
	if err != nil {
		return nil, err
	}
	active, err := s.Store.ActiveParticipants(projectID)
	if err != nil {
		return nil, err
	}
	live := map[string]bool{}
	for _, id := range active {
		live[id] = true
	}
	var out []model.Session
	for _, x := range list {
		if x.ID == self || x.Agent == agent || !cfg.CanSend(x.Agent) {
			continue
		}
		out = append(out, x)
	}
	// Live participants first, so they survive budget truncation.
	sort.SliceStable(out, func(i, j int) bool { return live[out[i].ID] && !live[out[j].ID] })
	return out, nil
}

// delta collects unread events and remembers how far they reach for Ack.
func (s *Service) delta(cfg *config.Config, projectID, self, agent string) ([]Group, error) {
	others, err := s.others(cfg, projectID, self, agent)
	if err != nil {
		return nil, err
	}
	var groups []Group
	marks := map[string]int64{}
	for _, o := range others {
		mark, err := s.Store.Watermark(self, o.ID)
		if err != nil {
			return nil, err
		}
		// A migrated copy's history is not progress made by that agent.
		base, err := s.Store.Baseline(o.ID)
		if err != nil {
			return nil, err
		}
		if base > mark {
			mark = base
		}
		es, err := s.Store.Events(o.ID, mark, 0)
		if err != nil {
			return nil, err
		}
		if len(es) == 0 {
			continue
		}
		groups = append(groups, Group{SessionID: o.ID, Agent: o.Agent, Title: o.Title, Events: es})
		marks[o.ID] = es[len(es)-1].Seq
	}
	s.mu.Lock()
	if s.pending == nil {
		s.pending = map[string]map[string]int64{}
	}
	s.pending[self] = marks
	s.mu.Unlock()
	return groups, nil
}

const maxHandoffTokens = 6000

// SessionStart registers the session and returns the startup injection:
// any pending handoff, project memories, in-flight work and resumable sessions.
func (s *Service) SessionStart(agent, native, cwd string) (string, error) {
	cfg, p, ok, err := s.load(agent, cwd)
	if err != nil || !ok {
		return "", err
	}
	self := SessionKey(agent, native)
	if err := s.Store.UpsertProject(p); err != nil {
		return "", err
	}
	if err := s.Store.JoinParticipant(self, p.ID, s.now()); err != nil {
		return "", err
	}
	var parts []string
	pending := handoff.PendingPath(agent, p.ID)
	if b, err := os.ReadFile(pending); err == nil {
		doc := string(b)
		if model.EstimateTokens(doc) > maxHandoffTokens {
			doc = string([]rune(doc)[:maxHandoffTokens]) + "\n…（交接文档已截断）"
		}
		parts = append(parts, doc)
		_ = os.Remove(pending)
	}
	budget := cfg.Live.InjectTokenBudget
	if cfg.ProjectMemoryOn() {
		ms, err := s.Store.ListMemories(store.MemoryFilter{Status: model.MemoryActive, Scope: model.ScopeProject, ProjectID: p.ID})
		if err != nil {
			return "", err
		}
		if t := memoryText(ms, budget/2, s.footer(agent) == Footer); t != "" {
			parts = append(parts, t)
		}
	}
	if cfg.Sync.Sessions {
		groups, err := s.delta(cfg, p.ID, self, agent)
		if err != nil {
			return "", err
		}
		if t := summarize(groups, budget/2, s.footer(agent)); t != "" {
			parts = append(parts, t)
		}
		if t, err := s.resumable(cfg, p.ID, agent); err != nil {
			return "", err
		} else if t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

func memoryText(ms []model.Memory, budget int, mcp bool) string {
	if len(ms) == 0 || budget <= 0 {
		return ""
	}
	out, used := "## 本项目记忆\n", 0
	for _, m := range ms {
		l := "- " + oneLine(m.Content, 300) + "\n"
		used += model.EstimateTokens(l)
		if used > budget {
			if mcp {
				return out + "（更多记忆可用 bridge MCP 的 memory_search 工具检索）\n"
			}
			return out + "（部分记忆已省略）\n"
		}
		out += l
	}
	return out
}

// resumable lists other agents' recent sessions that could be continued here.
func (s *Service) resumable(cfg *config.Config, projectID, agent string) (string, error) {
	list, err := s.Store.ListSessions(store.SessionFilter{ProjectID: projectID, Since: s.now().Add(-7 * 24 * time.Hour), Limit: 20})
	if err != nil {
		return "", err
	}
	var lines []string
	for _, x := range list {
		if x.Agent == agent || !cfg.CanSend(x.Agent) {
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s：%s（bridge handoff %s --to %s）", x.Agent, oneLine(x.Title, 40), x.ID, agent))
		if len(lines) == 3 {
			break
		}
	}
	if len(lines) == 0 {
		return "", nil
	}
	return "## 可接续的会话\n" + strings.Join(lines, "\n"), nil
}

// PromptSubmit returns other agents' progress since this session's last read.
func (s *Service) PromptSubmit(agent, native, cwd string) (string, error) {
	cfg, p, ok, err := s.load(agent, cwd)
	if err != nil || !ok || !cfg.Sync.Sessions || !cfg.Live.InjectOnPrompt {
		return "", err
	}
	self := SessionKey(agent, native)
	if err := s.Store.UpsertProject(p); err != nil {
		return "", err
	}
	if err := s.Store.JoinParticipant(self, p.ID, s.now()); err != nil {
		return "", err
	}
	groups, err := s.delta(cfg, p.ID, self, agent)
	if err != nil {
		return "", err
	}
	return summarize(groups, cfg.Live.InjectTokenBudget, s.footer(agent)), nil
}

// Stop ends participation and queues the session for memory extraction.
func (s *Service) Stop(agent, native, cwd string) error {
	cfg, err := s.Config(cwd)
	if err != nil {
		return err
	}
	self := SessionKey(agent, native)
	if err := s.Store.LeaveParticipant(self, s.now()); err != nil {
		return err
	}
	if cfg.Sync.Memory && cfg.Memory.AutoExtract {
		return s.Store.Enqueue(self, s.now())
	}
	return nil
}
