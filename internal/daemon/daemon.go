// Package daemon runs the background process: collection, the hook socket,
// file sync and the memory extraction queue.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"bridge/internal/config"
	"bridge/internal/filesync"
	"bridge/internal/fsutil"
	"bridge/internal/handoff"
	"bridge/internal/live"
	"bridge/internal/memory"
	"bridge/internal/model"
	"bridge/internal/project"
	"bridge/internal/store"
	"bridge/internal/versions"
	kit "bridge/pkg/adapterkit"
)

// ConfigFunc loads configuration for a working directory.
type ConfigFunc func(cwd string) (*config.Config, error)

// Ingester copies new native events into the store.
type Ingester struct {
	Store    *store.Store
	Registry *kit.Registry
	Config   ConfigFunc
}

// Sync ingests every adapter once and returns the number of new events.
// One agent's failure does not stop the others.
func (in *Ingester) Sync(ctx context.Context) (int, error) {
	total := 0
	var errs []error
	for _, a := range in.Registry.All() {
		n, err := in.SyncAdapter(ctx, a)
		total += n
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a.ID(), err))
		}
	}
	return total, errors.Join(errs...)
}

// SyncAdapter ingests one adapter.
func (in *Ingester) SyncAdapter(ctx context.Context, a kit.Adapter) (int, error) {
	inst, err := a.Detect(ctx)
	if err != nil || !inst.Installed || !a.Capabilities(inst.Version).SessionRead {
		return 0, err
	}
	refs, err := a.ListSessions(ctx, kit.Filter{})
	if err != nil {
		return 0, err
	}
	total := 0
	var errs []error
	for _, r := range refs {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, err := in.IngestRef(ctx, a, r)
		total += n
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.NativeID, err))
		}
	}
	return total, errors.Join(errs...)
}

func underDir(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
}

// IngestRef applies exclusion before reading anything, redacts before
// storing, and commits events and cursor together.
func (in *Ingester) IngestRef(ctx context.Context, a kit.Adapter, r kit.SessionRef) (int, error) {
	if r.Unreadable {
		return 0, fmt.Errorf("cannot read session file %s", r.Path)
	}
	if r.CWD == "" || underDir(filepath.Clean(r.CWD), memory.ExtractorDir()) {
		return 0, nil
	}
	cfg, err := in.Config(r.CWD)
	if err != nil {
		return 0, err
	}
	if !cfg.CanSend(a.ID()) || (!cfg.Sync.Sessions && !cfg.Sync.Memory) {
		return 0, nil
	}
	m, err := cfg.Matcher()
	if err != nil {
		return 0, err
	}
	if m.Excluded(r.CWD) {
		return 0, nil
	}
	prev, ok, err := in.Store.GetCursor(a.ID(), r.Path)
	if err != nil {
		return 0, err
	}
	var size, mtime int64
	if st, err := os.Stat(r.Path); err == nil {
		size, mtime = st.Size(), st.ModTime().UnixNano()
		if ok && prev.Size == size && prev.MTime == mtime && prev.Inode == fsutil.Inode(st) {
			return 0, in.refreshLocation(a.ID(), r)
		}
	}
	events, cur, err := a.ReadSession(ctx, r, prev.Cursor)
	if err != nil {
		return 0, err
	}
	red, err := cfg.Redactor()
	if err != nil {
		return 0, err
	}
	p := project.Identify(r.CWD)
	if err := in.Store.UpsertProject(p); err != nil {
		return 0, err
	}
	id := live.SessionKey(a.ID(), r.NativeID)
	title := r.Title
	if err := in.Store.UpsertSession(model.Session{ID: id, ProjectID: p.ID, Agent: a.ID(), NativeID: r.NativeID,
		Model: r.Model, CWD: r.CWD, Title: red.String(title), StartedAt: r.StartedAt, EndedAt: r.UpdatedAt}); err != nil {
		return 0, err
	}
	for i := range events {
		events[i] = red.Event(events[i])
		events[i].SessionID = id
	}
	cur.Agent, cur.NativeFile = a.ID(), r.Path
	if ok && prev.NextSeq > 0 && (cur.Offset < prev.Offset || cur.Inode != prev.Inode || (len(events) > 0 && events[0].Seq <= prev.NextSeq)) {
		if events, cur.NextSeq, err = in.rebase(id, events); err != nil {
			return 0, err
		}
	}
	return in.Store.Ingest(id, events, store.CursorState{Cursor: cur, Size: size, MTime: mtime})
}

// refreshLocation updates a session whose file is unchanged but whose recorded
// directory is out of date, such as one indexed by an older version.
func (in *Ingester) refreshLocation(agent string, r kit.SessionRef) error {
	x, err := in.Store.GetSession(live.SessionKey(agent, r.NativeID))
	if err != nil || x.CWD == r.CWD {
		return nil
	}
	p := project.Identify(r.CWD)
	if err := in.Store.UpsertProject(p); err != nil {
		return err
	}
	x.CWD, x.ProjectID = r.CWD, p.ID
	return in.Store.UpsertSession(x)
}

// rebase handles a replaced or truncated file: the event table is append-only,
// so records already held are dropped and genuinely new ones get fresh
// sequence numbers after the existing ones.
func (in *Ingester) rebase(id string, events []model.Event) ([]model.Event, int64, error) {
	held, err := in.Store.Events(id, 0, 0)
	if err != nil {
		return nil, 0, err
	}
	seen := map[string]int{}
	for _, e := range held {
		seen[fingerprint(e)]++
	}
	next, err := in.Store.MaxSeq(id)
	if err != nil {
		return nil, 0, err
	}
	kept := events[:0]
	for _, e := range events {
		if f := fingerprint(e); seen[f] > 0 {
			seen[f]--
			continue
		}
		next++
		e.Seq = next
		kept = append(kept, e)
	}
	return kept, next, nil
}

func fingerprint(e model.Event) string {
	b, _ := json.Marshal([]any{e.Kind, e.Role, e.Content, e.Tool, e.File})
	return string(b)
}

// Request is the socket protocol, one JSON object per connection.
type Request struct {
	Method    string          `json:"method"` // ping | hook | ack
	Event     string          `json:"event,omitempty"`
	Agent     string          `json:"agent,omitempty"`
	Session   string          `json:"session,omitempty"`
	CWD       string          `json:"cwd,omitempty"`
	ToolName  string          `json:"tool_name,omitempty"`
	ToolInput json.RawMessage `json:"tool_input,omitempty"`
}

// Response answers a Request.
type Response struct {
	Output string `json:"output,omitempty"`
	Deny   bool   `json:"deny,omitempty"` // refuse the tool call; Output is the reason
	Error  string `json:"error,omitempty"`
}

// Daemon ties all background services together.
type Daemon struct {
	Store    *store.Store
	Registry *kit.Registry
	Config   ConfigFunc
	Logger   *log.Logger

	ing    *Ingester
	liveS  *live.Service
	claims live.Claims
	nudge  chan struct{}
}

func (d *Daemon) global() (*config.Config, error) { return d.Config("") }

// Handle answers one socket request.
func (d *Daemon) Handle(req Request) Response {
	if req.Method == "ping" {
		return Response{Output: "pong"}
	}
	if req.Method == "claims" {
		b, err := json.Marshal(d.claims.List(time.Now()))
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{Output: string(b)}
	}
	if req.Method == "ack" {
		d.liveS.Ack(req.Agent, req.Session)
		return Response{}
	}
	if req.Method == "hook" && fsutil.Paused() {
		if req.Event == string(kit.HookStop) {
			d.claims.Release(req.Agent, req.Session)
		}
		return Response{}
	}
	if req.Method != "hook" {
		return Response{Error: "unknown method"}
	}
	var out string
	var err error
	switch req.Event {
	case string(kit.HookPreTool):
		if req.CWD == "" || req.Session == "" || req.Agent == "" {
			break
		}
		cfg, e := d.Config(req.CWD)
		if e != nil {
			err = e
			break
		}
		matcher, e := cfg.Matcher()
		if e != nil {
			err = e
			break
		}
		if !cfg.CanSend(req.Agent) || matcher.Excluded(req.CWD) {
			break
		}
		p := project.Identify(req.CWD)
		root := p.Path
		if canonical, e := filepath.EvalSymlinks(root); e == nil {
			root = canonical
		}
		paths := live.EditPaths(req.ToolName, req.ToolInput, req.CWD, root)
		allowed := paths[:0]
		for _, path := range paths {
			if !matcher.Excluded(path) {
				allowed = append(allowed, path)
			}
		}
		if cfg.Live.ConflictPolicy == "block" {
			if conflicts := d.claims.TryAcquire(p.ID, req.Agent, req.Session, allowed, time.Now()); len(conflicts) > 0 {
				return Response{Output: live.ClaimDenial(conflicts), Deny: true}
			}
			break
		}
		out = live.ClaimWarning(d.claims.Acquire(p.ID, req.Agent, req.Session, allowed, time.Now()))
	case string(kit.HookSessionStart):
		out, err = d.liveS.SessionStart(req.Agent, req.Session, req.CWD)
	case string(kit.HookPromptSubmit):
		out, err = d.liveS.PromptSubmit(req.Agent, req.Session, req.CWD)
	case string(kit.HookPostTool):
		d.poke()
	case string(kit.HookStop):
		d.claims.Release(req.Agent, req.Session)
		d.poke()
		err = d.liveS.Stop(req.Agent, req.Session, req.CWD)
	default:
		err = fmt.Errorf("unknown hook event %q", req.Event)
	}
	if err != nil {
		d.Logger.Printf("hook %s: %v", req.Event, err)
		return Response{Error: err.Error()}
	}
	return Response{Output: out}
}

func (d *Daemon) poke() {
	select {
	case d.nudge <- struct{}{}:
	default:
	}
}

// Listen binds the Unix socket with mode 0600, refusing to start beside a
// running daemon and replacing only a stale socket file.
func Listen(path string) (net.Listener, error) {
	if c, err := net.DialTimeout("unix", path, 200*time.Millisecond); err == nil {
		c.Close()
		return nil, errors.New("daemon already running")
	}
	_ = os.Remove(path)
	if err := fsutil.EnsurePrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// Run blocks until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	l, err := Listen(fsutil.SocketPath())
	if err != nil {
		return err
	}
	defer l.Close()
	defer os.Remove(fsutil.SocketPath())
	d.claims.Path = filepath.Join(fsutil.DataDir(), "claims.json")
	d.claims.Load()
	d.ing = &Ingester{Store: d.Store, Registry: d.Registry, Config: d.Config}
	d.liveS = &live.Service{Store: d.Store, Config: d.Config, HasMCP: func(agent string) bool {
		a, ok := d.Registry.Get(agent)
		return ok && a.Capabilities("").MCP
	}}
	if cfg, err := d.global(); err != nil {
		d.Logger.Printf("recover: %v", err)
	} else if err := handoff.Recover(ctx, handoff.Deps{Store: d.Store, Registry: d.Registry, Config: cfg}); err != nil {
		d.Logger.Printf("recover: %v", err)
	}
	d.nudge = make(chan struct{}, 1)
	go d.serve(ctx, l)
	go d.collect(ctx)
	go d.extractLoop(ctx)
	go d.fileSyncLoop(ctx)
	go d.versionLoop(ctx)
	go d.watchWorkspaces(ctx)
	<-ctx.Done()
	return nil
}

func (d *Daemon) serve(ctx context.Context, l net.Listener) {
	go func() { <-ctx.Done(); l.Close() }()
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(2 * time.Second))
			var req Request
			if err := json.NewDecoder(c).Decode(&req); err != nil {
				return
			}
			_ = json.NewEncoder(c).Encode(d.Handle(req))
		}()
	}
}

// collect tails session directories. File events are debounced so a burst of
// writes becomes one pass; a slow timer covers anything the watcher missed.
func (d *Daemon) collect(ctx context.Context) {
	sync := func() {
		if fsutil.Paused() {
			return
		}
		if _, err := d.ing.Sync(ctx); err != nil && ctx.Err() == nil {
			d.Logger.Printf("sync: %v", err)
		}
	}
	sync()
	w, err := fsnotify.NewWatcher()
	if err != nil {
		d.Logger.Printf("watcher unavailable, polling only: %v", err)
	} else {
		defer w.Close()
		for _, a := range d.Registry.All() {
			if wr, ok := a.(kit.Watcher); ok {
				for _, r := range wr.WatchRoots() {
					addTree(w, r)
				}
			}
		}
	}
	var events chan fsnotify.Event
	var errs chan error
	if w != nil {
		events, errs = w.Events, w.Errors
	}
	const debounce = 300 * time.Millisecond
	fire := time.NewTimer(time.Hour)
	fire.Stop()
	slow := time.NewTicker(30 * time.Second)
	defer slow.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if ev.Op&fsnotify.Create != 0 {
				if st, err := os.Stat(ev.Name); err == nil && st.IsDir() {
					addTree(w, ev.Name)
				}
			}
			fire.Reset(debounce)
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			d.Logger.Printf("watcher: %v", err)
		case <-d.nudge:
			sync()
		case <-fire.C:
			sync()
		case <-slow.C:
			sync()
		}
	}
}

func addTree(w *fsnotify.Watcher, root string) {
	_ = filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
		if err == nil && e.IsDir() {
			_ = w.Add(p)
		}
		return nil
	})
}

const idleBeforeExtraction = 10 * time.Minute

func (d *Daemon) extractLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := d.extractOnce(ctx); err != nil && ctx.Err() == nil {
				d.Logger.Printf("memory extraction: %v", err)
			}
		}
	}
}

// extractOnce queues idle sessions and processes one batch. A failure stops
// the batch and leaves the rest queued for the next tick.
func (d *Daemon) extractOnce(ctx context.Context) error {
	if fsutil.Paused() {
		return nil
	}
	cfg, err := d.global()
	if err != nil {
		return err
	}
	if !cfg.Sync.Memory || !cfg.Memory.AutoExtract {
		return nil
	}
	idle, err := d.Store.IdleSessions(time.Now().UTC().Add(-idleBeforeExtraction), 20)
	if err != nil {
		return err
	}
	for _, id := range idle {
		if err := d.Store.Enqueue(id, time.Now().UTC()); err != nil {
			return err
		}
	}
	queue, err := d.Store.Queue(5)
	if err != nil {
		return err
	}
	if len(queue) == 0 {
		return nil
	}
	changed := false
	for _, id := range queue {
		s, err := d.Store.GetSession(id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) { // hook arrived before the first ingest
				continue
			}
			return err
		}
		sc, err := d.Config(s.CWD)
		if err != nil {
			return err
		}
		if !sc.Sync.Memory || !sc.Memory.AutoExtract {
			max, _ := d.Store.MaxSeq(id)
			if err := d.Store.FinishExtraction(id, max); err != nil {
				return err
			}
			continue
		}
		e := memory.Engine{Store: d.Store, Extractor: memory.CLIExtractor{Agent: sc.Memory.Extractor}, Config: sc}
		n, err := e.ExtractSession(ctx, id)
		if n > 0 {
			changed = true
		}
		if err != nil {
			if changed {
				_, _ = memory.RenderBlocks(ctx, d.Store, d.Registry, cfg)
			}
			return fmt.Errorf("%s: %w", id, err)
		}
	}
	if changed {
		_, err = memory.RenderBlocks(ctx, d.Store, d.Registry, cfg)
	}
	return err
}

// versionLoop checks newly seen agent versions so an upgrade doesn't leave
// native migration off. Each version is probed once; "重新检测" retries it.
func (d *Daemon) versionLoop(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		d.probeNewVersions(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (d *Daemon) probeNewVersions(ctx context.Context) {
	if fsutil.Paused() {
		return
	}
	for _, a := range d.Registry.All() {
		if _, ok := a.(kit.VersionProber); !ok {
			continue
		}
		in, err := a.Detect(ctx)
		if err != nil || !in.Installed || in.Version == "" || a.Capabilities(in.Version).SessionWrite {
			continue
		}
		if _, known := versions.Get(a.ID(), string(in.Version)); known {
			continue
		}
		r, err := versions.Probe(ctx, a, string(in.Version))
		if err != nil {
			d.Logger.Printf("version check %s %s: %v", a.ID(), in.Version, err)
			continue
		}
		d.Logger.Printf("version check %s %s: %s %s", a.ID(), in.Version, r.State, r.Error)
	}
}

func (d *Daemon) fileSyncLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := d.fileSyncOnce(ctx); err != nil && ctx.Err() == nil {
				d.Logger.Printf("file sync: %v", err)
			}
		}
	}
}

func (d *Daemon) fileSyncOnce(ctx context.Context) error {
	if fsutil.Paused() {
		return nil
	}
	agents, err := DetectAgents(ctx, d.Registry)
	if err != nil {
		return err
	}
	roots := []string{""}
	projects, err := d.Store.ListProjects()
	if err != nil {
		return err
	}
	for _, p := range projects {
		// Only real repositories: a plain cwd is not a project to write into.
		if _, err := os.Stat(filepath.Join(p.Path, ".git")); err == nil {
			roots = append(roots, p.Path)
		}
	}
	var errs []error
	for _, root := range roots {
		cfg, err := d.Config(root)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if root != "" {
			if m, err := cfg.Matcher(); err != nil || m.Excluded(root) {
				continue
			}
		}
		acts, err := filesync.Plan(filesync.Input{ProjectRoot: root, Agents: agents, Config: cfg})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := filesync.Apply(acts, filesync.Options{}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// DetectAgents returns the installed agents with their capabilities.
func DetectAgents(ctx context.Context, reg *kit.Registry) ([]filesync.Agent, error) {
	var out []filesync.Agent
	for _, a := range reg.All() {
		in, err := a.Detect(ctx)
		if err != nil {
			return nil, err
		}
		if in.Installed {
			out = append(out, filesync.Agent{ID: a.ID(), Caps: a.Capabilities(in.Version)})
		}
	}
	return out, nil
}

// Ask sends one request to the daemon within the deadline.
func Ask(req Request, dial, total time.Duration) (Response, error) {
	var r Response
	c, err := net.DialTimeout("unix", fsutil.SocketPath(), dial)
	if err != nil {
		return r, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(total))
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return r, err
	}
	return r, json.NewDecoder(c).Decode(&r)
}
