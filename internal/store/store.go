// Package store persists the unified model in SQLite (WAL, FTS5). The events
// table is append-only, which is what later enables incremental replication.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // SQLite driver

	"bridge/internal/fsutil"
	"bridge/internal/model"
)

var migrations = []string{`
CREATE TABLE projects(id TEXT PRIMARY KEY, path TEXT NOT NULL, git_remote TEXT NOT NULL DEFAULT '');
CREATE TABLE sessions(
  id TEXT PRIMARY KEY, project_id TEXT NOT NULL DEFAULT '', agent TEXT NOT NULL, native_id TEXT NOT NULL,
  model TEXT NOT NULL DEFAULT '', cwd TEXT NOT NULL DEFAULT '', title TEXT NOT NULL DEFAULT '',
  started_at INTEGER NOT NULL DEFAULT 0, ended_at INTEGER NOT NULL DEFAULT 0, parent TEXT NOT NULL DEFAULT '');
CREATE INDEX sessions_project ON sessions(project_id, ended_at);
CREATE TABLE events(
  session_id TEXT NOT NULL, seq INTEGER NOT NULL, kind TEXT NOT NULL, role TEXT NOT NULL DEFAULT '',
  content TEXT, tool TEXT, file TEXT, ts INTEGER NOT NULL, native_ref TEXT,
  PRIMARY KEY(session_id, seq)) WITHOUT ROWID;
CREATE VIRTUAL TABLE events_fts USING fts5(session_id UNINDEXED, seq UNINDEXED, body, tokenize='trigram');
CREATE TABLE cursors(
  agent TEXT NOT NULL, native_file TEXT NOT NULL, offset INTEGER NOT NULL, inode INTEGER NOT NULL,
  format_version TEXT NOT NULL DEFAULT '', next_seq INTEGER NOT NULL DEFAULT 0,
  size INTEGER NOT NULL DEFAULT 0, mtime INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(agent, native_file));
CREATE TABLE watermarks(reader TEXT NOT NULL, source TEXT NOT NULL, seq INTEGER NOT NULL, PRIMARY KEY(reader, source));
CREATE TABLE participants(session_id TEXT PRIMARY KEY, project_id TEXT NOT NULL, started_at INTEGER NOT NULL, ended_at INTEGER NOT NULL DEFAULT 0);
CREATE TABLE memories(
  id TEXT PRIMARY KEY, scope TEXT NOT NULL, project_id TEXT NOT NULL DEFAULT '', category TEXT NOT NULL DEFAULT '',
  content TEXT NOT NULL, source_events TEXT NOT NULL DEFAULT '[]', confidence REAL NOT NULL DEFAULT 1,
  supersedes TEXT NOT NULL DEFAULT '', status TEXT NOT NULL, created_at INTEGER NOT NULL);
CREATE VIRTUAL TABLE memories_fts USING fts5(id UNINDEXED, body, tokenize='trigram');
CREATE TABLE forget_list(hash TEXT PRIMARY KEY, content TEXT NOT NULL, created_at INTEGER NOT NULL);
CREATE TABLE extraction(session_id TEXT PRIMARY KEY, last_seq INTEGER NOT NULL DEFAULT 0, queued_at INTEGER NOT NULL DEFAULT 0);
CREATE TABLE artifacts(kind TEXT NOT NULL, canonical_path TEXT NOT NULL, hash TEXT NOT NULL, PRIMARY KEY(kind, canonical_path));
`, `
CREATE TABLE baselines(session_id TEXT PRIMARY KEY, seq INTEGER NOT NULL);
INSERT INTO baselines(session_id, seq)
  SELECT x.session_id, x.last_seq FROM extraction x JOIN sessions s ON s.id=x.session_id WHERE s.parent<>'' AND x.last_seq>0;
`, `
CREATE TABLE conversation_links(
  source_id TEXT NOT NULL,
  target_id TEXT NOT NULL,
  source_seq INTEGER NOT NULL,
  target_seq INTEGER NOT NULL,
  PRIMARY KEY(source_id, target_id)
);
`}

// Store wraps the database.
type Store struct{ db *sql.DB }

// Open opens (creating if needed) the database with private file permissions.
func Open(path string) (*Store, error) {
	if err := fsutil.EnsurePrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=secure_delete(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Stat(f); err == nil {
			if err := os.Chmod(f, 0o600); err != nil {
				db.Close()
				return nil, err
			}
		}
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version=%d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromUnix(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// UpsertProject stores a project.
func (s *Store) UpsertProject(p model.Project) error {
	_, err := s.db.Exec(`INSERT INTO projects(id,path,git_remote) VALUES(?,?,?)
		ON CONFLICT(id) DO UPDATE SET path=excluded.path, git_remote=excluded.git_remote`, p.ID, p.Path, p.GitRemote)
	return err
}

// UpsertSession stores session metadata; lineage already recorded is kept.
func (s *Store) UpsertSession(x model.Session) error {
	_, err := s.db.Exec(`INSERT INTO sessions(id,project_id,agent,native_id,model,cwd,title,started_at,ended_at,parent)
		VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET project_id=excluded.project_id, model=excluded.model, cwd=excluded.cwd,
		title=excluded.title, started_at=excluded.started_at, ended_at=excluded.ended_at,
		parent=CASE WHEN excluded.parent<>'' THEN excluded.parent ELSE sessions.parent END`,
		x.ID, x.ProjectID, x.Agent, x.NativeID, x.Model, x.CWD, x.Title, unix(x.StartedAt), unix(x.EndedAt), x.Parent)
	return err
}

const sessionCols = `id,project_id,agent,native_id,model,cwd,title,started_at,ended_at,parent`

func scanSession(r interface{ Scan(...any) error }) (model.Session, error) {
	var x model.Session
	var st, en int64
	err := r.Scan(&x.ID, &x.ProjectID, &x.Agent, &x.NativeID, &x.Model, &x.CWD, &x.Title, &st, &en, &x.Parent)
	x.StartedAt, x.EndedAt = fromUnix(st), fromUnix(en)
	return x, err
}

// ErrNotFound reports a missing record.
var ErrNotFound = errors.New("not found")

// GetSession loads a session by ID.
func (s *Store) GetSession(id string) (model.Session, error) {
	x, err := scanSession(s.db.QueryRow(`SELECT `+sessionCols+` FROM sessions WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return x, ErrNotFound
	}
	return x, err
}

// FindSession resolves a full ID, a native ID, or a unique prefix of either.
func (s *Store) FindSession(q string) (model.Session, error) {
	if q == "" {
		return model.Session{}, ErrNotFound
	}
	like := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
	rows, err := s.db.Query(`SELECT `+sessionCols+` FROM sessions
		WHERE id=? OR native_id=? OR id LIKE ? ESCAPE '\' OR native_id LIKE ? ESCAPE '\' LIMIT 3`, q, q, like, like)
	if err != nil {
		return model.Session{}, err
	}
	defer rows.Close()
	var found []model.Session
	for rows.Next() {
		x, err := scanSession(rows)
		if err != nil {
			return x, err
		}
		if x.ID == q || x.NativeID == q {
			return x, nil
		}
		found = append(found, x)
	}
	if len(found) == 0 {
		return model.Session{}, ErrNotFound
	}
	if len(found) > 1 {
		return model.Session{}, fmt.Errorf("session id %q is ambiguous", q)
	}
	return found[0], rows.Err()
}

// SessionFilter narrows ListSessions.
type SessionFilter struct {
	ProjectID string
	Agent     string
	Since     time.Time
	Limit     int
}

// ListSessions returns sessions, most recently active first.
func (s *Store) ListSessions(f SessionFilter) ([]model.Session, error) {
	q := `SELECT ` + sessionCols + ` FROM sessions WHERE 1=1`
	var args []any
	if f.ProjectID != "" {
		q += ` AND project_id=?`
		args = append(args, f.ProjectID)
	}
	if f.Agent != "" {
		q += ` AND agent=?`
		args = append(args, f.Agent)
	}
	if !f.Since.IsZero() {
		q += ` AND ended_at>=?`
		args = append(args, unix(f.Since))
	}
	q += ` ORDER BY ended_at DESC`
	if f.Limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, f.Limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Session
	for rows.Next() {
		x, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func marshalOrNil(v any, isNil bool) any {
	if isNil {
		return nil
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// CursorState is a stored cursor plus the file stat used to skip unchanged files.
type CursorState struct {
	model.Cursor
	Size  int64
	MTime int64
}

// GetCursor loads the cursor for a file; ok is false if none exists.
func (s *Store) GetCursor(agent, file string) (CursorState, bool, error) {
	var c CursorState
	err := s.db.QueryRow(`SELECT agent,native_file,offset,inode,format_version,next_seq,size,mtime FROM cursors WHERE agent=? AND native_file=?`,
		agent, file).Scan(&c.Agent, &c.NativeFile, &c.Offset, &c.Inode, &c.FormatVersion, &c.NextSeq, &c.Size, &c.MTime)
	if errors.Is(err, sql.ErrNoRows) {
		return c, false, nil
	}
	return c, err == nil, err
}

// Ingest appends events and advances the cursor in one transaction, so a crash
// can never lose or double-apply a batch. Events whose (session, seq) already
// exist are ignored, which makes re-reading a rewritten file harmless.
func (s *Store) Ingest(sessionID string, events []model.Event, cur CursorState) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	ins, err := tx.Prepare(`INSERT OR IGNORE INTO events(session_id,seq,kind,role,content,tool,file,ts,native_ref) VALUES(?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return 0, err
	}
	defer ins.Close()
	fts, err := tx.Prepare(`INSERT INTO events_fts(session_id,seq,body) VALUES(?,?,?)`)
	if err != nil {
		return 0, err
	}
	defer fts.Close()
	added := 0
	for _, e := range events {
		var ref any
		if len(e.NativeRef) > 0 {
			ref = string(e.NativeRef)
		}
		r, err := ins.Exec(sessionID, e.Seq, string(e.Kind), e.Role, marshalOrNil(e.Content, len(e.Content) == 0),
			marshalOrNil(e.Tool, e.Tool == nil), marshalOrNil(e.File, e.File == nil), unix(e.TS), ref)
		if err != nil {
			return 0, err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			continue
		}
		added++
		if body := searchBody(e); body != "" {
			if _, err := fts.Exec(sessionID, e.Seq, body); err != nil {
				return 0, err
			}
		}
	}
	if _, err := tx.Exec(`INSERT INTO cursors(agent,native_file,offset,inode,format_version,next_seq,size,mtime) VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(agent,native_file) DO UPDATE SET offset=excluded.offset, inode=excluded.inode,
		format_version=excluded.format_version, next_seq=excluded.next_seq, size=excluded.size, mtime=excluded.mtime`,
		cur.Agent, cur.NativeFile, cur.Offset, cur.Inode, cur.FormatVersion, cur.NextSeq, cur.Size, cur.MTime); err != nil {
		return 0, err
	}
	return added, tx.Commit()
}

func searchBody(e model.Event) string {
	b := e.Text()
	if e.Tool != nil {
		b += " " + string(e.Tool.Args) + " " + e.Tool.Result
	}
	if e.File != nil {
		b += " " + e.File.Path
	}
	return strings.TrimSpace(b)
}

// Events returns a session's events with seq > after, ascending. limit <= 0
// means no limit.
func (s *Store) Events(sessionID string, after int64, limit int) ([]model.Event, error) {
	q := `SELECT session_id,seq,kind,role,content,tool,file,ts,native_ref FROM events WHERE session_id=? AND seq>? ORDER BY seq`
	if limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, limit)
	}
	rows, err := s.db.Query(q, sessionID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Event
	for rows.Next() {
		var e model.Event
		var kind string
		var content, tool, file, ref sql.NullString
		var ts int64
		if err := rows.Scan(&e.SessionID, &e.Seq, &kind, &e.Role, &content, &tool, &file, &ts, &ref); err != nil {
			return nil, err
		}
		e.Kind, e.TS = model.EventKind(kind), fromUnix(ts)
		if content.Valid {
			_ = json.Unmarshal([]byte(content.String), &e.Content)
		}
		if tool.Valid {
			e.Tool = &model.ToolCall{}
			_ = json.Unmarshal([]byte(tool.String), e.Tool)
		}
		if file.Valid {
			e.File = &model.FileChange{}
			_ = json.Unmarshal([]byte(file.String), e.File)
		}
		if ref.Valid {
			e.NativeRef = json.RawMessage(ref.String)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MaxSeq returns the highest sequence number stored for a session.
func (s *Store) MaxSeq(sessionID string) (int64, error) {
	var n sql.NullInt64
	err := s.db.QueryRow(`SELECT MAX(seq) FROM events WHERE session_id=?`, sessionID).Scan(&n)
	return n.Int64, err
}

// Hit is a full-text search result.
type Hit struct {
	SessionID string `json:"session_id"`
	Seq       int64  `json:"seq"`
	Snippet   string `json:"snippet"`
}

// ftsQuery quotes a user query as an FTS5 phrase. The trigram tokenizer needs
// at least three characters.
func ftsQuery(q string) string { return `"` + strings.ReplaceAll(q, `"`, `""`) + `"` }

// SearchEvents performs a full-text search across events.
func (s *Store) SearchEvents(q string, limit int) ([]Hit, error) {
	if limit <= 0 {
		limit = 20
	}
	if len([]rune(q)) < 3 {
		rows, err := s.db.Query(`SELECT session_id,seq,substr(body,1,200) FROM events_fts WHERE body LIKE ? LIMIT ?`, "%"+q+"%", limit)
		if err != nil {
			return nil, err
		}
		return scanHits(rows)
	}
	rows, err := s.db.Query(`SELECT session_id,seq,snippet(events_fts,2,'','','…',24) FROM events_fts WHERE body MATCH ? ORDER BY rank LIMIT ?`, ftsQuery(q), limit)
	if err != nil {
		return nil, err
	}
	return scanHits(rows)
}

func scanHits(rows *sql.Rows) ([]Hit, error) {
	defer rows.Close()
	var out []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.SessionID, &h.Seq, &h.Snippet); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Watermark returns how far reader has read source.
func (s *Store) Watermark(reader, source string) (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT seq FROM watermarks WHERE reader=? AND source=?`, reader, source).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// SetWatermark records read progress; it never moves backwards.
func (s *Store) SetWatermark(reader, source string, seq int64) error {
	_, err := s.db.Exec(`INSERT INTO watermarks(reader,source,seq) VALUES(?,?,?)
		ON CONFLICT(reader,source) DO UPDATE SET seq=MAX(seq,excluded.seq)`, reader, source, seq)
	return err
}

// JoinParticipant marks a session active in a project.
func (s *Store) JoinParticipant(sessionID, projectID string, at time.Time) error {
	_, err := s.db.Exec(`INSERT INTO participants(session_id,project_id,started_at,ended_at) VALUES(?,?,?,0)
		ON CONFLICT(session_id) DO UPDATE SET project_id=excluded.project_id, ended_at=0`, sessionID, projectID, unix(at))
	return err
}

// LeaveParticipant marks a session inactive.
func (s *Store) LeaveParticipant(sessionID string, at time.Time) error {
	_, err := s.db.Exec(`UPDATE participants SET ended_at=? WHERE session_id=?`, unix(at), sessionID)
	return err
}

// ActiveParticipants lists active sessions in a project.
func (s *Store) ActiveParticipants(projectID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT session_id FROM participants WHERE project_id=? AND ended_at=0`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// InsertMemory stores a memory, its search row and, when it supersedes another,
// retires the old one, all atomically.
func (s *Store) InsertMemory(m model.Memory) error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	src, _ := json.Marshal(m.SourceEvents)
	if _, err := tx.Exec(`INSERT INTO memories(id,scope,project_id,category,content,source_events,confidence,supersedes,status,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, m.ID, m.Scope, m.ProjectID, m.Category, m.Content, string(src), m.Confidence, m.Supersedes, m.Status, unix(m.CreatedAt)); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO memories_fts(id,body) VALUES(?,?)`, m.ID, m.Content); err != nil {
		return err
	}
	if m.Supersedes != "" && m.Status == model.MemoryActive {
		if _, err := tx.Exec(`UPDATE memories SET status=? WHERE id=?`, model.MemorySuperseded, m.Supersedes); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const memoryCols = `id,scope,project_id,category,content,source_events,confidence,supersedes,status,created_at`

func scanMemory(r interface{ Scan(...any) error }) (model.Memory, error) {
	var m model.Memory
	var src string
	var ts int64
	err := r.Scan(&m.ID, &m.Scope, &m.ProjectID, &m.Category, &m.Content, &src, &m.Confidence, &m.Supersedes, &m.Status, &ts)
	m.CreatedAt = fromUnix(ts)
	_ = json.Unmarshal([]byte(src), &m.SourceEvents)
	return m, err
}

// MemoryFilter narrows ListMemories. A Global + ProjectID combination returns
// global memories together with that project's.
type MemoryFilter struct {
	Status    string
	Scope     string
	ProjectID string
	Global    bool
}

// ListMemories returns memories, newest first.
func (s *Store) ListMemories(f MemoryFilter) ([]model.Memory, error) {
	q := `SELECT ` + memoryCols + ` FROM memories WHERE 1=1`
	var args []any
	if f.Status != "" {
		q += ` AND status=?`
		args = append(args, f.Status)
	}
	if f.Scope != "" {
		q += ` AND scope=?`
		args = append(args, f.Scope)
	}
	if f.ProjectID != "" {
		if f.Global {
			q += ` AND (project_id=? OR scope='global')`
		} else {
			q += ` AND project_id=?`
		}
		args = append(args, f.ProjectID)
	}
	q += ` ORDER BY created_at DESC, id DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetMemory loads a memory by ID or unique prefix.
func (s *Store) GetMemory(id string) (model.Memory, error) {
	rows, err := s.db.Query(`SELECT `+memoryCols+` FROM memories WHERE id=? OR id LIKE ? LIMIT 2`, id, id+"%")
	if err != nil {
		return model.Memory{}, err
	}
	defer rows.Close()
	var found []model.Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return m, err
		}
		found = append(found, m)
	}
	switch len(found) {
	case 0:
		return model.Memory{}, ErrNotFound
	case 1:
		return found[0], nil
	}
	return model.Memory{}, fmt.Errorf("memory id %q is ambiguous", id)
}

// SetMemoryStatus changes a memory's status; activating a memory that
// supersedes another retires the older one.
func (s *Store) SetMemoryStatus(id, status string) error {
	m, err := s.GetMemory(id)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(`UPDATE memories SET status=? WHERE id=?`, status, m.ID); err != nil {
		return err
	}
	if status == model.MemoryActive && m.Supersedes != "" {
		_, err = s.db.Exec(`UPDATE memories SET status=? WHERE id=?`, model.MemorySuperseded, m.Supersedes)
	}
	return err
}

// UpdateMemoryContent edits a memory in place and refreshes its search row.
func (s *Store) UpdateMemoryContent(id, content string) error {
	m, err := s.GetMemory(id)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(`UPDATE memories SET content=? WHERE id=?`, content, m.ID); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM memories_fts WHERE id=?`, m.ID); err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO memories_fts(id,body) VALUES(?,?)`, m.ID, content)
	return err
}

// SearchMemories ranks active memories by BM25 relevance. Queries shorter than
// the trigram minimum fall back to a substring scan.
func (s *Store) SearchMemories(q, projectID string, limit int) ([]model.Memory, error) {
	if limit <= 0 {
		limit = 10
	}
	cond := `m.status='active' AND (m.scope='global' OR m.project_id=?)`
	cols := "m." + strings.ReplaceAll(memoryCols, ",", ",m.")
	var rows *sql.Rows
	var err error
	if len([]rune(q)) < 3 {
		rows, err = s.db.Query(`SELECT `+cols+` FROM memories m WHERE `+cond+` AND m.content LIKE ? ORDER BY m.created_at DESC LIMIT ?`, projectID, "%"+q+"%", limit)
	} else {
		rows, err = s.db.Query(`SELECT `+cols+` FROM memories_fts f JOIN memories m ON m.id=f.id WHERE f.body MATCH ? AND `+cond+` ORDER BY f.rank LIMIT ?`, ftsQuery(q), projectID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func forgetHash(content string) string {
	return fsutil.HashString(strings.ToLower(strings.Join(strings.Fields(content), " ")))
}

// Forget deletes a memory and blocks its content from being re-extracted.
func (s *Store) Forget(id string) error {
	m, err := s.GetMemory(id)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Only a hash suppresses re-extraction, so the list cannot reveal the text.
	if _, err := tx.Exec(`INSERT OR IGNORE INTO forget_list(hash,content,created_at) VALUES(?,'',?)`, forgetHash(m.Content), unix(time.Now())); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE memories SET status=?, content='', source_events='[]' WHERE id=?`, model.MemoryForgotten, m.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM memories_fts WHERE id=?`, m.ID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if _, err := s.db.Exec(`INSERT INTO memories_fts(memories_fts) VALUES('optimize')`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return err
	}
	return fsutil.ScrubBackups(m.Content)
}

// IsForgotten reports whether content is on the do-not-extract list.
func (s *Store) IsForgotten(content string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM forget_list WHERE hash=?`, forgetHash(content)).Scan(&n)
	return n > 0, err
}

// Enqueue puts a session on the memory extraction queue.
func (s *Store) Enqueue(sessionID string, at time.Time) error {
	_, err := s.db.Exec(`INSERT INTO extraction(session_id,last_seq,queued_at) VALUES(?,0,?)
		ON CONFLICT(session_id) DO UPDATE SET queued_at=excluded.queued_at`, sessionID, unix(at))
	return err
}

// Queue lists sessions waiting for extraction, oldest first.
func (s *Store) Queue(limit int) ([]string, error) {
	rows, err := s.db.Query(`SELECT session_id FROM extraction WHERE queued_at>0 ORDER BY queued_at LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// IdleSessions returns sessions with events newer than their extraction point
// whose last activity is older than the given instant.
func (s *Store) IdleSessions(before time.Time, limit int) ([]string, error) {
	rows, err := s.db.Query(`SELECT s.id FROM sessions s LEFT JOIN extraction x ON x.session_id=s.id
		WHERE s.ended_at>0 AND s.ended_at<? AND IFNULL(x.queued_at,0)=0
		AND (SELECT MAX(seq) FROM events e WHERE e.session_id=s.id) > IFNULL(x.last_seq,0)
		ORDER BY s.ended_at LIMIT ?`, unix(before), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ExtractedUpTo returns the last event seq already sent for extraction.
func (s *Store) ExtractedUpTo(sessionID string) (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT last_seq FROM extraction WHERE session_id=?`, sessionID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// FinishExtraction records progress and clears the queue flag.
func (s *Store) FinishExtraction(sessionID string, seq int64) error {
	_, err := s.db.Exec(`INSERT INTO extraction(session_id,last_seq,queued_at) VALUES(?,?,0)
		ON CONFLICT(session_id) DO UPDATE SET last_seq=excluded.last_seq, queued_at=0`, sessionID, seq)
	return err
}

// Stats summarizes stored data for one agent.
type Stats struct {
	Sessions int
	Events   int
	LastSync time.Time
}

// AgentStats returns counts and last activity for an agent.
func (s *Store) AgentStats(agent string) (Stats, error) {
	var st Stats
	var last sql.NullInt64
	err := s.db.QueryRow(`SELECT COUNT(*), MAX(ended_at) FROM sessions WHERE agent=?`, agent).Scan(&st.Sessions, &last)
	if err != nil {
		return st, err
	}
	st.LastSync = fromUnix(last.Int64)
	err = s.db.QueryRow(`SELECT COUNT(*) FROM events e JOIN sessions s ON s.id=e.session_id WHERE s.agent=?`, agent).Scan(&st.Events)
	return st, err
}

// SaveArtifact records a synced artifact's source hash.
func (s *Store) SaveArtifact(a model.Artifact) error {
	_, err := s.db.Exec(`INSERT INTO artifacts(kind,canonical_path,hash) VALUES(?,?,?)
		ON CONFLICT(kind,canonical_path) DO UPDATE SET hash=excluded.hash`, a.Kind, a.CanonicalPath, a.Hash)
	return err
}

// ListProjects returns all known projects.
func (s *Store) ListProjects() ([]model.Project, error) {
	rows, err := s.db.Query(`SELECT id,path,git_remote FROM projects ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Project
	for rows.Next() {
		var p model.Project
		if err := rows.Scan(&p.ID, &p.Path, &p.GitRemote); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// FirstUserText returns the session's first user message, for display when
// the agent recorded no title.
func (s *Store) FirstUserText(sessionID string) (string, error) {
	rows, err := s.db.Query(`SELECT content FROM events WHERE session_id=? AND kind='message' AND role='user' AND content IS NOT NULL ORDER BY seq LIMIT 5`, sessionID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return "", err
		}
		var parts []model.ContentPart
		if json.Unmarshal([]byte(raw), &parts) != nil {
			continue
		}
		for _, p := range parts {
			if t := strings.Join(strings.Fields(p.Text), " "); t != "" && !strings.HasPrefix(t, "<") && !strings.HasPrefix(t, "[bridge") {
				return t, nil
			}
		}
	}
	return "", rows.Err()
}

// RecentChild returns the newest session migrated from parent to agent since
// the given instant.
func (s *Store) RecentChild(parent, agent string, since time.Time) (model.Session, bool, error) {
	x, err := scanSession(s.db.QueryRow(`SELECT `+sessionCols+` FROM sessions WHERE parent=? AND agent=? AND ended_at>=? ORDER BY ended_at DESC LIMIT 1`, parent, agent, unix(since)))
	if errors.Is(err, sql.ErrNoRows) {
		return x, false, nil
	}
	return x, err == nil, err
}

// SetBaseline marks the events up to seq as history copied in by a migration.
func (s *Store) SetBaseline(sessionID string, seq int64) error {
	_, err := s.db.Exec(`INSERT INTO baselines(session_id,seq) VALUES(?,?)
		ON CONFLICT(session_id) DO UPDATE SET seq=excluded.seq`, sessionID, seq)
	return err
}

// Baseline returns the last event that was copied in by a migration, or 0.
func (s *Store) Baseline(sessionID string) (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT seq FROM baselines WHERE session_id=?`, sessionID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// ConversationLink records the exact indexed histories represented at a
// handoff boundary. It must never be inferred from a title or a timestamp.
type ConversationLink struct {
	SourceID  string `json:"source_id"`
	TargetID  string `json:"target_id"`
	SourceSeq int64  `json:"source_seq"`
	TargetSeq int64  `json:"target_seq"`
}

func (s *Store) CreateConversationLink(source, target string, sourceSeq, targetSeq int64) error {
	if source == "" || target == "" || source == target || sourceSeq <= 0 || targetSeq <= 0 {
		return errors.New("invalid conversation link boundary")
	}
	_, err := s.db.Exec(`INSERT INTO conversation_links(source_id,target_id,source_seq,target_seq) VALUES(?,?,?,?)`, source, target, sourceSeq, targetSeq)
	return err
}

// ConversationLinksFor returns both incoming and outgoing links. Historical
// sessions without a recorded boundary are intentionally not guessed.
func (s *Store) ConversationLinksFor(id string) ([]ConversationLink, error) {
	rows, err := s.db.Query(`SELECT source_id,target_id,source_seq,target_seq FROM conversation_links WHERE source_id=? OR target_id=?`, id, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConversationLink
	for rows.Next() {
		var link ConversationLink
		if err := rows.Scan(&link.SourceID, &link.TargetID, &link.SourceSeq, &link.TargetSeq); err != nil {
			return nil, err
		}
		out = append(out, link)
	}
	return out, rows.Err()
}

// UpdateConversationLink moves a link's boundary after a sync.
func (s *Store) UpdateConversationLink(source, target string, sourceSeq, targetSeq int64) error {
	_, err := s.db.Exec(`UPDATE conversation_links SET source_seq=?, target_seq=? WHERE source_id=? AND target_id=?`, sourceSeq, targetSeq, source, target)
	return err
}

// MaxSeqAt returns the last event of a session recorded at or before t.
func (s *Store) MaxSeqAt(sessionID string, t time.Time) (int64, error) {
	var n sql.NullInt64
	err := s.db.QueryRow(`SELECT MAX(seq) FROM events WHERE session_id=? AND ts<=?`, sessionID, unix(t)).Scan(&n)
	return n.Int64, err
}
