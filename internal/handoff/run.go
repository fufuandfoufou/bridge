package handoff

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"bridge/internal/config"
	"bridge/internal/fsutil"
	"bridge/internal/model"
	"bridge/internal/opjournal"
	"bridge/internal/store"
	kit "bridge/pkg/adapterkit"
)

func sortStrings(s []string) { sort.Strings(s) }

func jsonField(raw json.RawMessage, key string) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// Request describes one migration.
type Request struct {
	SessionID string
	Target    string
	Force     bool // write a document even if native writing is possible
}

// Outcome reports what happened.
type Outcome struct {
	Mode          string // native | document
	Handle        kit.NativeHandle
	DocumentPath  string
	Result        Result
	Message       string
	TargetSession string
	SourceSeq     int64 // last source event included in the handoff snapshot
}

// Deps are the collaborators of Run.
type Deps struct {
	Store    *store.Store
	Registry *kit.Registry
	Config   *config.Config
	// Refresh ingests the newest events of a session before migrating.
	Refresh func(ctx context.Context, s model.Session) error
}

// Run executes the migration end to end.
func Run(ctx context.Context, d Deps, req Request) (*Outcome, error) {
	src, err := d.Store.FindSession(req.SessionID)
	if err != nil {
		return nil, fmt.Errorf("session %q: %w", req.SessionID, err)
	}
	if err := Recover(ctx, d); err != nil {
		return nil, err
	}
	if d.Refresh != nil {
		if err := d.Refresh(ctx, src); err != nil {
			return nil, err
		}
	}
	cfg := d.Config
	if m, err := cfg.Matcher(); err != nil {
		return nil, err
	} else if m.Excluded(src.CWD) {
		return nil, fmt.Errorf("project %s is excluded from syncing", src.CWD)
	}
	if !cfg.Sync.Sessions {
		return nil, errors.New("session sync is disabled by configuration (sync.sessions)")
	}
	if !cfg.CanSend(src.Agent) {
		return nil, fmt.Errorf("agent %s is not allowed to send sessions (direction %s)", src.Agent, cfg.Direction(src.Agent))
	}
	if !cfg.CanReceive(req.Target) {
		return nil, fmt.Errorf("agent %s is not allowed to receive sessions (enabled=%v, direction %s)", req.Target, cfg.AgentEnabled(req.Target), cfg.Direction(req.Target))
	}
	target, ok := d.Registry.Get(req.Target)
	if !ok {
		return nil, fmt.Errorf("no adapter for agent %q", req.Target)
	}
	in, err := target.Detect(ctx)
	if err != nil {
		return nil, err
	}
	caps := target.Capabilities(in.Version)
	events, err := d.Store.Events(src.ID, 0, 0)
	if err != nil {
		return nil, err
	}
	if !hasConversation(events) {
		return nil, fmt.Errorf("session %s has no conversation to migrate", src.ID)
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
	res := Transform(events, Options{
		SourceAgent: src.Agent, SourceModel: src.Model, TargetAgent: req.Target,
		ContextWindow: caps.ContextWindow, MaxContextRatio: cfg.Handoff.MaxContextRatio,
		KeepRecentTurns: cfg.Handoff.KeepRecentTurns, CanRender: canRender,
		Redactor: red, Git: readGit(src.CWD), Now: time.Now().UTC(),
	})
	if res.OverBudget {
		return nil, fmt.Errorf("session cannot fit %s's context budget even after truncation (about %d tokens); nothing was written", req.Target, res.EstimatedTokens)
	}
	if !caps.SessionWrite || req.Force {
		return writeDocument(d, src, req.Target, caps, in, res)
	}
	dst := model.Session{CWD: src.CWD, Title: src.Title, Agent: req.Target, ProjectID: src.ProjectID, Parent: src.ID, NativeID: fsutil.NewUUID()}
	if m, ok := target.(kit.SessionIDMinter); ok {
		dst.NativeID = m.NewSessionID()
	}
	if src.Agent == req.Target {
		dst.Model = src.Model
	}
	if err := opjournal.Begin(opjournal.Op{Agent: req.Target, ID: dst.NativeID, CWD: dst.CWD, Source: src.ID, Started: time.Now().UTC()}); err != nil {
		return nil, err
	}
	fsutil.Failpoint("handoff.after_journal")
	h, err := target.WriteSession(ctx, dst, res.Events)
	if err != nil {
		if e := opjournal.Done(req.Target, dst.NativeID); e != nil {
			return nil, errors.Join(err, e)
		}
		if errors.Is(err, kit.ErrUnsupportedVersion) || errors.Is(err, kit.ErrNotSupported) {
			return writeDocument(d, src, req.Target, caps, in, res)
		}
		return nil, fmt.Errorf("write session: %w", err)
	}
	if err := verify(ctx, target, h, req.Target, res.Events); err != nil {
		rollback(ctx, target, h)
		if e := opjournal.Done(req.Target, dst.NativeID); e != nil {
			return nil, errors.Join(err, e)
		}
		return nil, fmt.Errorf("verification failed, write rolled back: %w", err)
	}
	fsutil.Failpoint("handoff.after_verify")
	now := time.Now().UTC()
	dst.ID, dst.NativeID, dst.StartedAt, dst.EndedAt = req.Target+":"+h.ID, h.ID, now, now
	if err := d.Store.UpsertSession(dst); err != nil {
		return nil, err
	}
	if err := opjournal.Done(req.Target, h.ID); err != nil {
		return nil, err
	}
	return &Outcome{Mode: "native", Handle: h, Result: res, TargetSession: dst.ID, SourceSeq: events[len(events)-1].Seq,
		Message: "恢复命令：" + h.ResumeCommand}, nil
}

// Recover rolls back writes interrupted by a crash. It stops and reports when
// a target's ownership cannot be established, so nothing is duplicated or
// removed by guesswork; the entry stays until the user resolves it.
func Recover(ctx context.Context, d Deps) error {
	ops, err := opjournal.List()
	if err != nil {
		return err
	}
	var errs []error
	for _, op := range ops {
		a, ok := d.Registry.Get(op.Agent)
		if !ok {
			errs = append(errs, fmt.Errorf("interrupted write to %s (%s): adapter unavailable", op.Agent, op.ID))
			continue
		}
		r, ok := a.(kit.SessionRecoverer)
		if !ok {
			errs = append(errs, fmt.Errorf("interrupted write to %s (%s): adapter cannot recover it; check the agent, then delete the entry in %s", op.Agent, op.ID, filepath.Join(fsutil.DataDir(), "ops")))
			continue
		}
		if err := r.RollbackSession(ctx, model.Session{Agent: op.Agent, NativeID: op.ID, CWD: op.CWD}, op.Started); err != nil {
			errs = append(errs, fmt.Errorf("recover %s %s: %w", op.Agent, op.ID, err))
			continue
		}
		if err := opjournal.Done(op.Agent, op.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func hasConversation(es []model.Event) bool {
	for _, e := range es {
		if e.Kind == model.KindMessage {
			return true
		}
	}
	return false
}

func rollback(ctx context.Context, a kit.Adapter, h kit.NativeHandle) {
	if r, ok := a.(kit.SessionRemover); ok {
		_ = r.RemoveSession(ctx, h)
		return
	}
	if h.Path != "" {
		_ = os.Remove(h.Path)
	}
}

// verify reads back the written session and compares user and assistant text
// plus the tool-call count with what was meant to be written.
func verify(ctx context.Context, a kit.Adapter, h kit.NativeHandle, agent string, want []model.Event) error {
	got, _, err := a.ReadSession(ctx, kit.SessionRef{Agent: agent, NativeID: h.ID, Path: h.Path}, model.Cursor{})
	if err != nil {
		return err
	}
	wt, wc := digest(want)
	gt, gc := digest(got)
	if wc != gc {
		return fmt.Errorf("tool call count %d, want %d", gc, wc)
	}
	if len(wt) != len(gt) {
		return fmt.Errorf("message count %d, want %d", len(gt), len(wt))
	}
	for i := range wt {
		if wt[i] != gt[i] {
			return fmt.Errorf("message %d differs after write", i)
		}
	}
	return nil
}

func digest(es []model.Event) ([]string, int) {
	var text []string
	calls := 0
	for _, e := range es {
		switch {
		case e.Kind == model.KindToolCall:
			calls++
		case e.Kind == model.KindMessage && e.Role != model.RoleSystem:
			if t := e.Text(); t != "" && !strings.HasPrefix(t, "[bridge 交接]") {
				text = append(text, t)
			}
		}
	}
	return text, calls
}

func handoffDir() string { return filepath.Join(fsutil.DataDir(), "handoffs") }

// PendingPath is where a document waits to be injected at the next session start.
func PendingPath(agent, projectID string) string {
	return filepath.Join(handoffDir(), "pending-"+agent+"-"+fsutil.HashString(projectID)[:12]+".md")
}

func writeDocument(d Deps, src model.Session, targetID string, caps kit.Capabilities, in kit.Install, res Result) (*Outcome, error) {
	doc := Document(src, res)
	if err := fsutil.EnsurePrivateDir(handoffDir()); err != nil {
		return nil, err
	}
	path := filepath.Join(handoffDir(), strings.ReplaceAll(src.ID, ":", "_")+"-to-"+targetID+".md")
	if err := fsutil.AtomicWrite(path, []byte(doc), 0o600); err != nil {
		return nil, err
	}
	msg := "目标 agent 不支持原生写回（版本 " + string(in.Version) + "），已生成交接文档：" + path + "\n请在目标 agent 中引用这个文件。"
	if caps.HasHook(kit.HookSessionStart) {
		if err := fsutil.AtomicWrite(PendingPath(targetID, src.ProjectID), []byte(doc), 0o600); err != nil {
			return nil, err
		}
		msg = "目标 agent 不支持原生写回（版本 " + string(in.Version) + "），已生成交接文档：" + path + "\n下次在该项目启动 " + targetID + " 时会自动注入。"
	}
	return &Outcome{Mode: "document", DocumentPath: path, Result: res, Message: msg}, nil
}

func readGit(cwd string) GitState {
	var g GitState
	run := func(args ...string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = cwd
		b, err := cmd.Output()
		if err != nil {
			return ""
		}
		return strings.TrimRight(string(b), "\n")
	}
	g.Branch = run("rev-parse", "--abbrev-ref", "HEAD")
	for _, l := range strings.Split(run("status", "--porcelain"), "\n") {
		if len(l) > 3 {
			g.Dirty = append(g.Dirty, l[3:])
		}
	}
	return g
}
