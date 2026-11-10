package live

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"bridge/internal/model"
	"bridge/internal/store"
)

// Attribution windows: a tool call is recorded just before its write lands,
// and either side's clock and ingestion may lag a little.
const (
	attributeBefore = 2 * time.Minute
	attributeAfter  = 10 * time.Second
	workspaceWindow = 2 * time.Hour
	maxWorkspace    = 15
)

var opNames = map[string]string{"create": "新建", "modify": "修改", "delete": "删除"}

// workspaceDigest lists files changed in the project since the reader last
// looked, except its own edits. Attribution is never asserted: a file edited
// by one agent's tool around that time is "probably" that agent's, otherwise
// the source is unknown, which covers manual edits and shell commands.
func (s *Service) workspaceDigest(self string, p model.Project) (string, int64, error) {
	mark, err := s.Store.Watermark(self, workspaceSource(p.ID))
	if err != nil {
		return "", 0, err
	}
	changes, err := s.Store.WorkspaceChanges(p.ID, mark, s.now().Add(-workspaceWindow), 500)
	if err != nil || len(changes) == 0 {
		return "", mark, err
	}
	last := changes[len(changes)-1].ID
	root := p.Path
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	edits, err := s.Store.ToolEdits(p.ID, changes[0].At.Add(-attributeBefore), changes[len(changes)-1].At.Add(attributeAfter))
	if err != nil {
		return "", 0, err
	}
	// Tool paths may be relative to the session's directory, or reach the
	// project through a different symlinked prefix.
	touches := func(toolPath, abs, rel string) bool {
		return toolPath == abs || toolPath == rel || strings.HasSuffix(toolPath, string(filepath.Separator)+rel)
	}
	type entry struct{ op, by, rel string }
	latest := map[string]entry{}
	var order []string
	for _, c := range changes {
		rel, err := filepath.Rel(root, c.Path)
		if err != nil {
			rel = c.Path
		}
		own, agents := false, map[string]bool{}
		for _, e := range edits {
			if e.At.Before(c.At.Add(-attributeBefore)) || e.At.After(c.At.Add(attributeAfter)) || !touches(e.Path, c.Path, rel) {
				continue
			}
			if e.SessionID == self {
				own = true
			}
			agents[e.Agent] = true
		}
		if own {
			delete(latest, c.Path)
			continue
		}
		by := "来源未知（可能是手工修改或 Shell 命令）"
		switch len(agents) {
		case 0:
		case 1:
			for a := range agents {
				by = "可能来自 " + a + "（推测）"
			}
		default:
			by = "来源不确定（多个 agent 在附近编辑过）"
		}
		if _, seen := latest[c.Path]; !seen {
			order = append(order, c.Path)
		}
		latest[c.Path] = entry{op: c.Op, by: by, rel: rel}
	}
	var lines []string
	for _, path := range order {
		e, ok := latest[path]
		if !ok {
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s %s · %s", opNames[e.op], e.rel, e.by))
	}
	if len(lines) == 0 {
		return "", last, nil
	}
	more := ""
	if len(lines) > maxWorkspace {
		more = fmt.Sprintf("\n…另有 %d 个文件", len(lines)-maxWorkspace)
		lines = lines[:maxWorkspace]
	}
	return "## 工作区中的其他改动\n" + strings.Join(lines, "\n") + more + "\n", last, nil
}

func workspaceSource(projectID string) string { return "workspace:" + projectID }

func (s *Service) taskSummary(projectID string) (string, error) {
	tasks, err := s.Store.ListTasks(projectID, false)
	if err != nil || len(tasks) == 0 {
		return "", err
	}
	var b strings.Builder
	b.WriteString("## 任务看板\n")
	for i, t := range tasks {
		if i == 10 {
			fmt.Fprintf(&b, "…另有 %d 项，可用 bridge MCP 的 task_list 查看\n", len(tasks)-10)
			break
		}
		state := "待认领"
		if t.Status == "claimed" {
			state = "进行中（" + t.Owner + "）"
		}
		fmt.Fprintf(&b, "- [%s] %s · %s\n", store.ShortTaskID(t.ID), t.Title, state)
	}
	b.WriteString("可用 task_claim 认领，task_done 完成。\n")
	return b.String(), nil
}
