// Package handoff migrates a session between agents. Transform is a pure
// pipeline; Run in run.go performs the single side-effecting step.
package handoff

import (
	"fmt"
	"strings"
	"time"

	"bridge/internal/model"
	"bridge/internal/privacy"
)

// GitState describes the working tree at handoff time.
type GitState struct {
	Branch string
	Dirty  []string
}

// Options parameterizes Transform.
type Options struct {
	SourceAgent     string
	SourceModel     string
	TargetAgent     string
	ContextWindow   int
	MaxContextRatio float64
	KeepRecentTurns int
	// CanRender decides whether the target has an equivalent for a tool call.
	CanRender func(*model.ToolCall) bool
	// MCPServers lists servers configured on the target. Nil means unknown,
	// in which case MCP calls degrade to text.
	MCPServers map[string]bool
	Redactor   *privacy.Redactor
	Git        GitState
	Now        time.Time
	// Delta marks an update to an existing session rather than a new one.
	Delta bool
}

// Result carries the transformed events and statistics.
type Result struct {
	Events          []model.Event
	ReasoningOmit   int
	Degraded        int
	SummarizedTurns int
	TruncatedTools  int
	// TruncatedMessages counts messages or results cut to fit the budget.
	TruncatedMessages int
	// OverBudget means the session cannot fit even after every reduction.
	OverBudget      bool
	EstimatedTokens int
}

const maxToolResultChars = 4000

// Transform converts source events to what the target should receive. For a
// same-agent handoff events pass through for lossless native rewrite.
func Transform(in []model.Event, o Options) Result {
	events := in
	if o.Redactor != nil {
		events = o.Redactor.Events(events)
	}
	if o.SourceAgent == o.TargetAgent {
		kept := make([]model.Event, 0, len(events))
		for _, e := range events {
			if e.Kind != model.KindReasoning {
				kept = append(kept, e)
			}
		}
		return Result{Events: kept, EstimatedTokens: tokens(kept)}
	}
	var r Result
	events = closeToolCalls(events, o.Now)
	events, r.ReasoningOmit = stripNonConversation(events)
	events, r.Degraded = mapTools(events, o)
	if o.ContextWindow > 0 {
		// The header counts against the budget, so reserve room for it first.
		avail := int(float64(o.ContextWindow)*o.MaxContextRatio) - model.EstimateTokens(header(o, r).Text())
		if tokens(events) > avail {
			events, r.TruncatedTools = truncateResults(events)
		}
		if tokens(events) > avail {
			events, r.SummarizedTurns = compress(events, o.KeepRecentTurns, avail)
		}
		if tokens(events) > avail {
			events, r.TruncatedMessages = shrink(events, avail)
		}
		r.OverBudget = tokens(events) > avail
	}
	out := make([]model.Event, 0, len(events)+1)
	out = append(out, header(o, r))
	for _, e := range events {
		e.NativeRef = nil
		out = append(out, e)
	}
	r.Events = out
	r.EstimatedTokens = tokens(out)
	return r
}

func tokens(es []model.Event) int {
	n := 0
	for _, e := range es {
		n += model.EventTokens(e)
	}
	return n
}

// closeToolCalls drops results with no call, and adds a synthetic result to
// calls that never finished; targets reject unpaired tool calls.
func closeToolCalls(in []model.Event, now time.Time) []model.Event {
	calls, results := map[string]bool{}, map[string]bool{}
	for _, e := range in {
		if e.Tool == nil {
			continue
		}
		if e.Kind == model.KindToolCall {
			calls[e.Tool.ID] = true
		} else if e.Kind == model.KindToolResult {
			results[e.Tool.ID] = true
		}
	}
	out := make([]model.Event, 0, len(in))
	for _, e := range in {
		if e.Kind == model.KindToolResult && !calls[e.Tool.ID] {
			continue
		}
		out = append(out, e)
		if e.Kind == model.KindToolCall && !results[e.Tool.ID] {
			out = append(out, model.Event{SessionID: e.SessionID, Kind: model.KindToolResult, Role: model.RoleTool, TS: e.TS,
				Tool: &model.ToolCall{ID: e.Tool.ID, Status: "interrupted", Result: "(该工具调用被中断，没有结果)"}})
		}
	}
	return out
}

// stripNonConversation removes reasoning (keeping only a count), meta events
// and file changes; file changes are already represented by their tool calls.
func stripNonConversation(in []model.Event) ([]model.Event, int) {
	out, n := make([]model.Event, 0, len(in)), 0
	for _, e := range in {
		switch e.Kind {
		case model.KindReasoning:
			n++
		case model.KindMeta, model.KindFileChange:
		default:
			out = append(out, e)
		}
	}
	return out, n
}

func supported(t *model.ToolCall, o Options) bool {
	if t.CanonicalName == model.ToolOpaque {
		return false
	}
	if t.CanonicalName == model.ToolMCPCall {
		var server string
		if s := jsonField(t.Args, "server"); s != "" {
			server = s
		}
		if !o.MCPServers[server] {
			return false
		}
	}
	return o.CanRender == nil || o.CanRender(t)
}

// mapTools converts unmappable calls, together with their results, into one
// assistant text at the position of the call.
func mapTools(in []model.Event, o Options) ([]model.Event, int) {
	degraded := map[string]*model.ToolCall{}
	result, status := map[string]string{}, map[string]string{}
	for _, e := range in {
		if e.Tool == nil {
			continue
		}
		if e.Kind == model.KindToolCall && !supported(e.Tool, o) {
			degraded[e.Tool.ID] = e.Tool
		}
		if e.Kind == model.KindToolResult {
			result[e.Tool.ID], status[e.Tool.ID] = e.Tool.Result, e.Tool.Status
		}
	}
	out := make([]model.Event, 0, len(in))
	for _, e := range in {
		switch {
		case e.Kind == model.KindToolCall && degraded[e.Tool.ID] != nil:
			out = append(out, model.TextEvent(model.RoleAssistant, describeTool(e.Tool, result[e.Tool.ID], status[e.Tool.ID], o.SourceAgent), e.TS))
		case e.Kind == model.KindToolResult && degraded[e.Tool.ID] != nil:
		default:
			out = append(out, e)
		}
	}
	return out, len(degraded)
}

func describeTool(t *model.ToolCall, res, st, agent string) string {
	name := t.NativeName
	if name == "" {
		name = t.CanonicalName
	}
	s := fmt.Sprintf("（在 %s 中调用了 %s：%s", agent, name, clip(string(t.Args), 600))
	if res != "" {
		s += "，结果：" + clip(res, 1500)
	}
	switch st {
	case "error":
		s += "，该调用失败"
	case "interrupted":
		s += "，该调用被中断"
	}
	return s + "）"
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func truncateResults(in []model.Event) ([]model.Event, int) {
	out, n := make([]model.Event, len(in)), 0
	copy(out, in)
	for i, e := range out {
		if e.Kind != model.KindToolResult || len([]rune(e.Tool.Result)) <= maxToolResultChars {
			continue
		}
		t := *e.Tool
		r := []rune(t.Result)
		t.Result = string(r[:maxToolResultChars]) + fmt.Sprintf("\n…（已截断，原文共 %d 字符）", len(r))
		out[i].Tool = &t
		n++
	}
	return out, n
}

// turnStarts returns the indexes of user messages, each opening a turn.
func turnStarts(in []model.Event) []int {
	var s []int
	for i, e := range in {
		if e.Kind == model.KindMessage && e.Role == model.RoleUser {
			s = append(s, i)
		}
	}
	return s
}

// compress keeps at most keep recent turns verbatim, fewer if needed to fit
// the budget, and folds everything earlier into one summary message at the
// start. At least the final turn is always kept.
func compress(in []model.Event, keep, budget int) ([]model.Event, int) {
	starts := turnStarts(in)
	if len(starts) == 0 {
		return in, 0
	}
	if keep > len(starts) {
		keep = len(starts)
	}
	for ; keep >= 1; keep-- {
		cut := starts[len(starts)-keep]
		if cut == 0 && keep == len(starts) && keep > 1 {
			continue // nothing would be summarized, so shrink further
		}
		summary := clipTokens(summarize(in[:cut], len(starts)-keep), budget/4)
		kept := in[cut:]
		if keep == 1 || tokens(kept)+model.EstimateTokens(summary) <= budget {
			out := make([]model.Event, 0, len(kept)+1)
			if cut > 0 {
				out = append(out, model.TextEvent(model.RoleUser, summary, in[0].TS))
			}
			return append(out, kept...), len(starts) - keep
		}
	}
	return in, 0
}

func clipTokens(s string, limit int) string {
	r := []rune(s)
	for model.EstimateTokens(string(r)) > limit && len(r) > 0 {
		r = r[:len(r)*limit/(model.EstimateTokens(string(r))+1)]
	}
	return string(r)
}

const cutMarker = "\n…（内容已截断）"

// shrink halves the largest message or tool result until the events fit, the
// last resort after summarizing. It reports how many items it cut.
func shrink(in []model.Event, avail int) ([]model.Event, int) {
	out := make([]model.Event, len(in))
	copy(out, in)
	cut := map[int]bool{}
	size := func(e model.Event) int {
		switch e.Kind {
		case model.KindMessage:
			return model.EstimateTokens(e.Text())
		case model.KindToolResult:
			return model.EstimateTokens(e.Tool.Result)
		}
		return 0
	}
	for tokens(out) > avail {
		best := -1
		for i, e := range out {
			if size(e) >= 32 && (best < 0 || size(e) > size(out[best])) {
				best = i
			}
		}
		if best < 0 {
			break // what remains is tool arguments, which cannot be cut safely
		}
		cut[best] = true
		e := &out[best]
		if e.Kind == model.KindMessage {
			r := []rune(strings.TrimSuffix(e.Text(), cutMarker))
			e.Content = []model.ContentPart{{Type: "text", Text: string(r[:len(r)/2]) + cutMarker}}
		} else {
			t := *e.Tool
			r := []rune(strings.TrimSuffix(t.Result, cutMarker))
			t.Result = string(r[:len(r)/2]) + cutMarker
			e.Tool = &t
		}
	}
	return out, len(cut)
}

// summarize is a rule-based digest of early turns; no model is called.
func summarize(in []model.Event, turns int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[历史摘要] 以下是更早 %d 轮对话的压缩记录：\n", turns)
	files := map[string]bool{}
	for _, e := range in {
		switch {
		case e.Kind == model.KindMessage && e.Role == model.RoleUser:
			fmt.Fprintf(&b, "- 用户：%s\n", oneLine(e.Text(), 200))
		case e.Kind == model.KindMessage && e.Role == model.RoleAssistant:
			fmt.Fprintf(&b, "- 助手：%s\n", oneLine(e.Text(), 200))
		case e.Kind == model.KindToolCall:
			if p := jsonField(e.Tool.Args, "path"); p != "" && (e.Tool.CanonicalName == model.ToolEditFile || e.Tool.CanonicalName == model.ToolWriteFile) {
				files[p] = true
			}
		}
	}
	if len(files) > 0 {
		names := make([]string, 0, len(files))
		for f := range files {
			names = append(names, f)
		}
		sortStrings(names)
		fmt.Fprintf(&b, "- 改动过的文件：%s\n", strings.Join(names, ", "))
	}
	return b.String()
}

func oneLine(s string, n int) string { return clip(strings.Join(strings.Fields(s), " "), n) }

func header(o Options, r Result) model.Event {
	var b strings.Builder
	if o.Delta {
		fmt.Fprintf(&b, "[bridge 同步] 以下内容是这个会话在 %s 中的后续工作，同步时间 %s。其中的工具调用来自其他 agent，你当前可用的工具可能不同。", o.SourceAgent, o.Now.Format(time.RFC3339))
		return model.TextEvent(model.RoleSystem, b.String(), o.Now)
	}
	fmt.Fprintf(&b, "[bridge 交接] 这个会话从 %s", o.SourceAgent)
	if o.SourceModel != "" {
		fmt.Fprintf(&b, "（模型 %s）", o.SourceModel)
	}
	fmt.Fprintf(&b, " 迁移而来，迁移时间 %s。\n", o.Now.Format(time.RFC3339))
	if o.Git.Branch != "" {
		fmt.Fprintf(&b, "当前 git 分支：%s。", o.Git.Branch)
		if len(o.Git.Dirty) > 0 {
			fmt.Fprintf(&b, "未提交改动（%d 个文件）：%s。", len(o.Git.Dirty), strings.Join(firstN(o.Git.Dirty, 30), ", "))
		} else {
			b.WriteString("工作区干净。")
		}
		b.WriteString("\n")
	}
	b.WriteString("以下工具调用来自其他 agent，你当前可用的工具可能不同，请不要假定它们可以直接调用。")
	if r.ReasoningOmit > 0 {
		fmt.Fprintf(&b, "\n已省略 %d 段推理内容。", r.ReasoningOmit)
	}
	return model.TextEvent(model.RoleSystem, b.String(), o.Now)
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// Document renders a structured handoff document, the fallback for targets
// that cannot accept a native session.
func Document(s model.Session, r Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# 会话交接：%s\n\n来源 agent：%s\n工作目录：%s\n\n", s.Title, s.Agent, s.CWD)
	fmt.Fprintf(&b, "## 迁移损失\n\n这是降级生成的交接文档，不是完整的原生会话。\n\n- 省略推理：%d 段\n- 降级为文本的工具调用：%d 个\n- 压缩为摘要的轮次：%d 轮\n- 截断的工具结果：%d 个\n- 截断的消息：%d 条\n\n",
		r.ReasoningOmit, r.Degraded, r.SummarizedTurns, r.TruncatedTools, r.TruncatedMessages)
	b.WriteString("## 对话记录\n\n")
	inList := false
	for _, e := range r.Events {
		switch e.Kind {
		case model.KindMessage:
			if inList { // end the tool list, or Markdown folds the message into it
				b.WriteString("\n")
				inList = false
			}
			fmt.Fprintf(&b, "**%s**：%s\n\n", e.Role, e.Text())
		case model.KindToolCall:
			fmt.Fprintf(&b, "- 工具 `%s`：`%s`\n", e.Tool.CanonicalName, clip(string(e.Tool.Args), 300))
			inList = true
		case model.KindToolResult:
			if e.Tool.Result != "" {
				fmt.Fprintf(&b, "  - 结果：%s\n", oneLine(e.Tool.Result, 300))
			}
		}
	}
	return b.String()
}
