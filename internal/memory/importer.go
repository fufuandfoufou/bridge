package memory

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"bridge/internal/config"
	"bridge/internal/fsutil"
	"bridge/internal/model"
	"bridge/internal/store"
)

// Importing history extracts memories from sessions that happened before
// bridge was set up, or while extraction was off. It always runs on the
// user's explicit request, shows its cost first, and sends every result to
// the review queue.

// ImportFilter selects the sessions to import.
type ImportFilter struct {
	ProjectID string    // empty: every project
	Agent     string    // empty: every agent
	Since     time.Time // zero: all time
}

// ImportItem is one session the import would process.
type ImportItem struct {
	Session string    `json:"session"`
	Agent   string    `json:"agent"`
	Title   string    `json:"title"`
	CWD     string    `json:"cwd"`
	Last    time.Time `json:"last"`
	Tokens  int       `json:"tokens"`
	Calls   int       `json:"calls"`
}

// ImportPlan is what an import would cost.
type ImportPlan struct {
	Items     []ImportItem   `json:"items"`
	Sessions  int            `json:"sessions"`
	Calls     int            `json:"calls"`
	Tokens    int            `json:"tokens"`
	Skipped   map[string]int `json:"skipped"`
	Extractor string         `json:"extractor"`
}

// promptOverhead approximates the instructions and existing memories each
// call carries besides the conversation.
const promptOverhead = 800

// Skip reasons, also shown to the user.
const (
	SkipCopy    = "迁移生成的副本"
	SkipPrivacy = "被隐私规则排除"
	SkipOff     = "该项目关闭了记忆"
	SkipDone    = "已提取过"
	SkipShort   = "内容太少"
)

// PlanImport lists the sessions with conversation not yet extracted.
func PlanImport(st *store.Store, cfgFor func(cwd string) (*config.Config, error), f ImportFilter, extractor string) (ImportPlan, error) {
	p := ImportPlan{Items: []ImportItem{}, Skipped: map[string]int{}, Extractor: extractor}
	list, err := st.ListSessions(store.SessionFilter{ProjectID: f.ProjectID, Agent: f.Agent, Since: f.Since})
	if err != nil {
		return p, err
	}
	scratch := ExtractorDir()
	for _, s := range list {
		if s.Parent != "" {
			p.Skipped[SkipCopy]++
			continue
		}
		if s.CWD == scratch || strings.HasPrefix(s.CWD, scratch+string(filepath.Separator)) {
			continue // extraction's own runs
		}
		cfg, err := cfgFor(s.CWD)
		if err != nil {
			return p, err
		}
		if m, err := cfg.Matcher(); err != nil {
			return p, err
		} else if m.Excluded(s.CWD) {
			p.Skipped[SkipPrivacy]++
			continue
		}
		if !cfg.GlobalMemoryOn() && !cfg.ProjectMemoryOn() {
			p.Skipped[SkipOff]++
			continue
		}
		last, err := st.ExtractedUpTo(s.ID)
		if err != nil {
			return p, err
		}
		events, err := st.Events(s.ID, last, 0)
		if err != nil {
			return p, err
		}
		conv := ConversationEvents(events)
		if len(conv) == 0 {
			p.Skipped[SkipDone]++
			continue
		}
		if len(conv) < 2 {
			p.Skipped[SkipShort]++
			continue
		}
		item := ImportItem{Session: s.ID, Agent: s.Agent, Title: s.Title, CWD: s.CWD, Last: s.EndedAt}
		for _, c := range Chunks(conv, ChunkTokens) {
			item.Calls++
			item.Tokens += promptOverhead
			for _, e := range c {
				item.Tokens += model.EstimateTokens(e.Text()) + 8
			}
		}
		p.Items = append(p.Items, item)
		p.Sessions++
		p.Calls += item.Calls
		p.Tokens += item.Tokens
	}
	return p, nil
}

// Import states.
const (
	ImportRunning     = "running"
	ImportDone        = "done"
	ImportFailed      = "failed"
	ImportCancelled   = "cancelled"
	ImportInterrupted = "interrupted"
)

// ImportStatus is the progress of the latest import, kept in a file so the
// desktop app can follow a run started in the background.
type ImportStatus struct {
	State      string    `json:"state"`
	PID        int       `json:"pid"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	Total      int       `json:"total"`
	Done       int       `json:"done"`
	Added      int       `json:"added"`
	Current    string    `json:"current"`
	Error      string    `json:"error,omitempty"`
}

// ImportStatusPath is where the progress is kept.
func ImportStatusPath() string { return filepath.Join(fsutil.DataDir(), "memory-import.json") }

// LoadImportStatus reads the latest progress. A run whose process is gone
// without finishing reports as interrupted.
func LoadImportStatus() (ImportStatus, error) {
	var s ImportStatus
	b, err := os.ReadFile(ImportStatusPath())
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, err
	}
	if s.State == ImportRunning && !alive(s.PID) {
		s.State = ImportInterrupted
	}
	return s, nil
}

func alive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

func (s ImportStatus) save() error {
	if err := fsutil.EnsurePrivateDir(fsutil.DataDir()); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	return fsutil.AtomicWrite(ImportStatusPath(), b, 0o600)
}

// ErrImportRunning means another import is in progress.
var ErrImportRunning = errors.New("已有一个历史导入正在进行")

// Importer runs an import plan.
type Importer struct {
	Store        *store.Store
	ConfigFor    func(cwd string) (*config.Config, error)
	NewExtractor func(agent string) Extractor
	Progress     func(ImportStatus) // optional, after every session
}

// Run extracts each planned session in turn. Progress is stored per part of
// a session, so an interrupted import continues where it stopped. The first
// failure ends the run: it is usually one that would repeat, such as the
// extractor not being logged in.
func (im *Importer) Run(ctx context.Context, p ImportPlan) (ImportStatus, error) {
	if cur, err := LoadImportStatus(); err != nil {
		return cur, err
	} else if cur.State == ImportRunning && cur.PID != os.Getpid() {
		return cur, ErrImportRunning
	}
	st := ImportStatus{State: ImportRunning, PID: os.Getpid(), StartedAt: time.Now().UTC(), Total: len(p.Items)}
	report := func() {
		_ = st.save()
		if im.Progress != nil {
			im.Progress(st)
		}
	}
	report()
	var runErr error
	for _, item := range p.Items {
		if ctx.Err() != nil {
			break
		}
		st.Current = item.Title
		if st.Current == "" {
			st.Current = item.Session
		}
		report()
		cfg, err := im.ConfigFor(item.CWD)
		if err != nil {
			runErr = err
			break
		}
		e := Engine{Store: im.Store, Extractor: im.NewExtractor(cfg.Memory.Extractor), Config: cfg, Review: true}
		n, err := e.ExtractSession(ctx, item.Session)
		st.Added += n
		if err != nil {
			runErr = err
			break
		}
		st.Done++
	}
	st.Current = ""
	st.FinishedAt = time.Now().UTC()
	switch {
	case ctx.Err() != nil:
		st.State = ImportCancelled
	case runErr != nil:
		st.State, st.Error = ImportFailed, runErr.Error()
	default:
		st.State = ImportDone
	}
	report()
	if st.State == ImportCancelled {
		return st, nil
	}
	return st, runErr
}
