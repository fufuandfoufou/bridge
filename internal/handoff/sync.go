package handoff

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"bridge/internal/fsutil"
	"bridge/internal/model"
	"bridge/internal/store"
	kit "bridge/pkg/adapterkit"
)

// SyncResult reports one bring-up-to-date pass of a linked conversation.
type SyncResult struct {
	State   string `json:"state"` // current | synced | blocked
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Applied int    `json:"applied"`
	Message string `json:"message"`
}

// SyncDeps are the collaborators of SyncConversation.
type SyncDeps struct {
	Deps
	// Ingest re-reads an adapter's sessions into the store.
	Ingest func(ctx context.Context, a kit.Adapter) error
}

// EnsureLink backfills the boundary of a copy made before boundaries were
// recorded. The copy's baseline is exact; the source side is the last event
// that existed when the copy was written.
func EnsureLink(st *store.Store, c model.Session) error {
	if c.Parent == "" || c.StartedAt.IsZero() {
		return nil
	}
	links, err := st.ConversationLinksFor(c.ID)
	if err != nil {
		return err
	}
	for _, l := range links {
		if l.TargetID == c.ID {
			return nil
		}
	}
	base, err := st.Baseline(c.ID)
	if err != nil || base <= 0 {
		return err
	}
	src, err := st.MaxSeqAt(c.Parent, c.StartedAt)
	if err != nil || src <= 0 {
		return err
	}
	return st.CreateConversationLink(c.Parent, c.ID, src, base)
}

// LinkFor returns the link of a session whose other side was active last.
func LinkFor(st *store.Store, id string) (store.ConversationLink, bool, error) {
	links, err := st.ConversationLinksFor(id)
	if err != nil || len(links) == 0 {
		return store.ConversationLink{}, false, err
	}
	var best store.ConversationLink
	var newest time.Time
	found := false
	for _, l := range links {
		other := l.TargetID
		if other == id {
			other = l.SourceID
		}
		s, err := st.GetSession(other)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return best, false, err
		}
		if !found || s.EndedAt.After(newest) {
			best, newest, found = l, s.EndedAt, true
		}
	}
	return best, found, nil
}

// LinkState compares both sides of a link with what the store holds now.
func LinkState(st *store.Store, l store.ConversationLink) (ConversationState, error) {
	a, err := st.MaxSeq(l.SourceID)
	if err != nil {
		return "", err
	}
	b, err := st.MaxSeq(l.TargetID)
	if err != nil {
		return "", err
	}
	return CompareConversation(l, a, b)
}

type syncJournal struct {
	Path     string `json:"path"`
	Before   int64  `json:"before"`
	Expected int64  `json:"expected"`
	Source   string `json:"source"`
	Target   string `json:"target"`
	To       string `json:"to"`
	ToAgent  string `json:"to_agent"`
	FromSeq  int64  `json:"from_seq"` // last event of the sending side included
}

func syncJournalPath() string { return filepath.Join(fsutil.DataDir(), "sync-journal.json") }

// recoverSync settles an interrupted append. A complete append is rolled
// forward, since the daemon may already have indexed it. A partial one is cut
// back. If the file has changed some other way syncing stops, rather than
// truncating someone's work.
func recoverSync(ctx context.Context, d SyncDeps) error {
	b, err := os.ReadFile(syncJournalPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var j syncJournal
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	st, err := os.Stat(j.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return os.Remove(syncJournalPath())
		}
		return err
	}
	switch {
	case st.Size() == j.Expected:
		return finishSync(ctx, d, j)
	case st.Size() > j.Before && st.Size() < j.Expected:
		if err := os.Truncate(j.Path, j.Before); err != nil {
			return err
		}
		return os.Remove(syncJournalPath())
	case st.Size() == j.Before:
		return os.Remove(syncJournalPath())
	}
	return fmt.Errorf("上次同步被中断，且 %s 之后又发生了变化，已停止同步，请手动检查", j.Path)
}

// finishSync moves the link past what was appended. The sending side's
// boundary is the event actually sent, so work added meanwhile isn't skipped.
func finishSync(ctx context.Context, d SyncDeps, j syncJournal) error {
	target, ok := d.Registry.Get(j.ToAgent)
	if !ok {
		return fmt.Errorf("no adapter for agent %q", j.ToAgent)
	}
	if err := d.Ingest(ctx, target); err != nil {
		return err
	}
	toMax, err := d.Store.MaxSeq(j.To)
	if err != nil {
		return err
	}
	sourceSeq, targetSeq := j.FromSeq, toMax
	if j.To == j.Source {
		sourceSeq, targetSeq = toMax, j.FromSeq
	}
	if err := d.Store.UpdateConversationLink(j.Source, j.Target, sourceSeq, targetSeq); err != nil {
		return err
	}
	// What was copied in is not progress by this agent.
	if err := d.Store.SetBaseline(j.To, toMax); err != nil {
		return err
	}
	return os.Remove(syncJournalPath())
}

func findRef(ctx context.Context, a kit.Adapter, nativeID string) (kit.SessionRef, error) {
	refs, err := a.ListSessions(ctx, kit.Filter{})
	if err != nil {
		return kit.SessionRef{}, err
	}
	for _, r := range refs {
		if r.NativeID == nativeID {
			return r, nil
		}
	}
	return kit.SessionRef{}, fmt.Errorf("session %s not found in %s", nativeID, a.ID())
}

// SyncConversation brings a linked pair up to date. New work from one side is
// appended to the other, keeping session IDs. When both advanced, the side
// named by prefer (by default, the one active last) wins: its new work is
// appended and the other side's own new work stays where it is.
func SyncConversation(ctx context.Context, d SyncDeps, id, prefer string) (*SyncResult, error) {
	if err := recoverSync(ctx, d); err != nil {
		return nil, err
	}
	src, err := d.Store.FindSession(id)
	if err != nil {
		return nil, fmt.Errorf("session %q: %w", id, err)
	}
	l, ok, err := LinkFor(d.Store, src.ID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return &SyncResult{State: "blocked", Message: "这个会话没有可同步的迁移记录。"}, nil
	}
	state, err := LinkState(d.Store, l)
	if err != nil {
		return nil, err
	}
	if state == ConversationCurrent {
		return &SyncResult{State: "current", Message: "两边已经是最新的。"}, nil
	}
	a, err := d.Store.GetSession(l.SourceID)
	if err != nil {
		return nil, err
	}
	b, err := d.Store.GetSession(l.TargetID)
	if err != nil {
		return nil, err
	}
	from, to, fromSeq := a, b, l.SourceSeq
	if state == ConversationTargetAhead || (state == ConversationDiverged && targetWins(a, b, prefer)) {
		from, to, fromSeq = b, a, l.TargetSeq
	}
	cfg := d.Config
	if m, err := cfg.Matcher(); err != nil {
		return nil, err
	} else if m.Excluded(from.CWD) || m.Excluded(to.CWD) {
		return nil, errors.New("这个项目被隐私规则排除，不能同步")
	}
	if !cfg.Sync.Sessions || !cfg.CanSend(from.Agent) || !cfg.CanReceive(to.Agent) {
		return &SyncResult{State: "blocked", Message: "当前配置不允许这个方向的同步。"}, nil
	}
	target, ok := d.Registry.Get(to.Agent)
	if !ok {
		return nil, fmt.Errorf("no adapter for agent %q", to.Agent)
	}
	appender, ok := target.(kit.SessionAppender)
	if !ok {
		return &SyncResult{State: "blocked", Message: to.Agent + " 不支持更新已有会话。"}, nil
	}
	in, err := target.Detect(ctx)
	if err != nil {
		return nil, err
	}
	caps := target.Capabilities(in.Version)
	if !caps.SessionWrite {
		return &SyncResult{State: "blocked", Message: fmt.Sprintf("%s 版本 %s 未验证写入，请先在概览页检测或启用。", to.Agent, in.Version)}, nil
	}
	ref, err := findRef(ctx, target, to.NativeID)
	if err != nil {
		return nil, err
	}
	if appender.SessionActive(ref) {
		return &SyncResult{State: "blocked", From: from.Agent, To: to.Agent,
			Message: fmt.Sprintf("%s 的这个会话正在使用，退出后再更新，否则可能造成分叉。", to.Agent)}, nil
	}
	events, err := d.Store.Events(from.ID, fromSeq, 0)
	if err != nil {
		return nil, err
	}
	red, err := cfg.Redactor()
	if err != nil {
		return nil, err
	}
	canRender := func(t *model.ToolCall) bool {
		if !caps.SupportsTool(t.CanonicalName) {
			return false
		}
		if tr, ok := target.(kit.ToolRenderer); ok {
			return tr.CanRender(t)
		}
		return true
	}
	res := Transform(events, Options{SourceAgent: from.Agent, SourceModel: from.Model, TargetAgent: to.Agent,
		ContextWindow: caps.ContextWindow, MaxContextRatio: cfg.Handoff.MaxContextRatio, KeepRecentTurns: cfg.Handoff.KeepRecentTurns,
		CanRender: canRender, Redactor: red, Git: readGit(to.CWD), Now: time.Now().UTC(), Delta: true})
	if res.OverBudget {
		return &SyncResult{State: "blocked", Message: "新增内容超出目标 agent 的上下文预算，没有写入。"}, nil
	}
	data, err := appender.RenderAppend(ctx, ref, res.Events)
	if errors.Is(err, kit.ErrNotSupported) {
		return &SyncResult{State: "blocked", Message: "目标会话的存储格式暂不支持追加；可以用“迁移”生成新的副本。"}, nil
	}
	if err != nil {
		return nil, err
	}
	journal := syncJournal{Source: l.SourceID, Target: l.TargetID, To: to.ID, ToAgent: to.Agent, FromSeq: events[len(events)-1].Seq}
	if err := appendAndVerify(ctx, target, ref, data, res.Events, journal); err != nil {
		return nil, err
	}
	fsutil.Failpoint("sync.after_append")
	if err := appender.AfterAppend(ctx, ref); err != nil {
		return nil, err
	}
	if err := finishSync(ctx, d, journal); err != nil {
		return nil, err
	}
	msg := fmt.Sprintf("已把 %s 的 %d 条新内容更新到 %s。", from.Agent, len(events), to.Agent)
	if state == ConversationDiverged {
		msg += fmt.Sprintf("两边都有新内容，已以 %s 为准；%s 自己的新内容保留在原处，不会同步给 %s。", from.Agent, to.Agent, from.Agent)
	}
	return &SyncResult{State: "synced", From: from.Agent, To: to.Agent, Applied: len(events), Message: msg}, nil
}

// targetWins chooses the sending side of a diverged pair: the side the user
// named, otherwise the side active last.
func targetWins(a, b model.Session, prefer string) bool {
	switch prefer {
	case a.Agent:
		return false
	case b.Agent:
		return true
	}
	return b.EndedAt.After(a.EndedAt)
}

func lastText(es []model.Event) string {
	for i := len(es) - 1; i >= 0; i-- {
		if es[i].Kind == model.KindMessage && es[i].Role != model.RoleSystem && es[i].Text() != "" {
			return es[i].Text()
		}
	}
	return ""
}

// appendAndVerify journals, appends in one write, and reads the file back.
func appendAndVerify(ctx context.Context, a kit.Adapter, ref kit.SessionRef, data []byte, want []model.Event, j syncJournal) error {
	st, err := os.Stat(ref.Path)
	if err != nil {
		return err
	}
	if st.Size() > 0 {
		f, err := os.Open(ref.Path)
		if err != nil {
			return err
		}
		tail := make([]byte, 1)
		_, err = f.ReadAt(tail, st.Size()-1)
		f.Close()
		if err != nil {
			return err
		}
		if tail[0] != '\n' {
			return errors.New("目标会话末尾有未写完的内容，已停止，请稍后重试")
		}
	}
	if _, err := fsutil.Backup(ref.Path); err != nil {
		return err
	}
	j.Path, j.Before, j.Expected = ref.Path, st.Size(), st.Size()+int64(len(data))
	entry, _ := json.Marshal(j)
	if err := fsutil.EnsurePrivateDir(fsutil.DataDir()); err != nil {
		return err
	}
	if err := fsutil.AtomicWrite(syncJournalPath(), entry, 0o600); err != nil {
		return err
	}
	f, err := os.OpenFile(ref.Path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if e := f.Close(); err == nil {
		err = e
	}
	rollback := func(cause error) error {
		if e := os.Truncate(ref.Path, st.Size()); e != nil {
			return errors.Join(cause, e)
		}
		if e := os.Remove(syncJournalPath()); e != nil {
			return errors.Join(cause, e)
		}
		return cause
	}
	if err != nil {
		return rollback(err)
	}
	got, _, err := a.ReadSession(ctx, ref, model.Cursor{})
	if err != nil {
		return rollback(err)
	}
	if w := lastText(want); w != "" && lastText(got) != w {
		return rollback(errors.New("写入后读回校验失败，已回滚"))
	}
	return nil
}
