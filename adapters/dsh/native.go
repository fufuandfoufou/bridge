package dsh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"

	"github.com/klauspost/compress/zstd"

	"bridge/adapters/internal/common"
	"bridge/internal/fsutil"
	"bridge/internal/model"
	kit "bridge/pkg/adapterkit"
)

var sessionID = regexp.MustCompile(`^session-[A-Za-z0-9_-]+$`)

// projectKey mirrors the harness's UTF-16 project-directory encoding.
func projectKey(cwd string) string {
	var out strings.Builder
	separator := false
	for _, c := range utf16.Encode([]rune(cwd)) {
		switch {
		case c == '/' || c == '\\' || c == ':':
			if !separator {
				out.WriteByte('-')
			}
			separator = true
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-':
			out.WriteByte(byte(c))
			separator = false
		default:
			fmt.Fprintf(&out, "~%04X", c)
			separator = false
		}
	}
	slug := strings.TrimLeft(out.String(), "-")
	if slug == "" {
		slug = "root"
	}
	if len(slug) > 251 {
		slug = slug[:251]
	}
	return "--" + slug + "--"
}

// NewSessionID implements adapterkit.SessionIDMinter.
func (a *Adapter) NewSessionID() string { return "session-" + fsutil.NewUUID() }

func resumeCommand(cwd, id string) string {
	return "cd " + common.ShellQuote(cwd) + " && dsh headless --session-id " + common.ShellQuote(id) + " -"
}

// lockFile shares the harness's stable-inode, non-blocking flock protocol.
func lockFile(path string) (func(), error) {
	for attempt := 0; attempt < 3; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, err
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			f.Close()
			return nil, fmt.Errorf("%s: %w: %v", path, kit.ErrOwnership, err)
		}
		held, heldErr := f.Stat()
		current, currentErr := os.Stat(path)
		if heldErr == nil && currentErr == nil && os.SameFile(held, current) {
			return func() { _ = f.Close() }, nil
		}
		f.Close()
	}
	return nil, kit.ErrOwnership
}

// LockSession holds the real harness write lease until the caller releases it.
func (a *Adapter) LockSession(ref kit.SessionRef) (func(), error) {
	return lockFile(filepath.Join(filepath.Dir(ref.Path), "session.lock"))
}

// SessionActive implements adapterkit.SessionAppender using the native lock.
func (a *Adapter) SessionActive(ref kit.SessionRef) bool {
	release, err := a.LockSession(ref)
	if err != nil {
		return true
	}
	release()
	return false
}

// WriteSession publishes one complete log while holding its write lease.
func (a *Adapter) WriteSession(ctx context.Context, s model.Session, events []model.Event) (kit.NativeHandle, error) {
	in, err := a.Detect(ctx)
	if err != nil {
		return kit.NativeHandle{}, err
	}
	if !a.Capabilities(in.Version).SessionWrite {
		return kit.NativeHandle{}, kit.ErrUnsupportedVersion
	}
	if !filepath.IsAbs(s.CWD) {
		return kit.NativeHandle{}, fmt.Errorf("session working directory must be absolute")
	}
	id := s.NativeID
	if id == "" {
		id = a.NewSessionID()
	}
	if !sessionID.MatchString(id) {
		return kit.NativeHandle{}, fmt.Errorf("invalid dsh session id %q", id)
	}
	data, err := Render(s, id, time.Now().UTC(), events)
	if err != nil {
		return kit.NativeHandle{}, err
	}
	data, err = encodeLog(data)
	if err != nil {
		return kit.NativeHandle{}, err
	}
	if err := fsutil.EnsurePrivateDir(sessionsDir()); err != nil {
		return kit.NativeHandle{}, err
	}
	releaseCreate, err := lockFile(filepath.Join(sessionsDir(), ".bridge-create.lock"))
	if err != nil {
		return kit.NativeHandle{}, err
	}
	defer releaseCreate()
	if existing, err := filepath.Glob(filepath.Join(sessionsDir(), "*", id)); err != nil || len(existing) > 0 {
		return kit.NativeHandle{}, errors.Join(kit.ErrConflict, err)
	}
	projectDir := filepath.Join(sessionsDir(), projectKey(s.CWD))
	if err := fsutil.EnsurePrivateDir(projectDir); err != nil {
		return kit.NativeHandle{}, err
	}
	dir := filepath.Join(projectDir, id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return kit.NativeHandle{}, err
	}
	path := filepath.Join(dir, "session.v4.jsonl.zstd")
	release, err := a.LockSession(kit.SessionRef{Path: path})
	if err != nil {
		_ = os.Remove(dir)
		return kit.NativeHandle{}, err
	}
	defer release()
	if err := fsutil.AtomicWrite(filepath.Join(dir, ".bridge-origin"), []byte(fsutil.HashString(string(data))), 0o600); err != nil {
		return kit.NativeHandle{}, err
	}
	if err := fsutil.AtomicWrite(path, data, 0o600); err != nil {
		return kit.NativeHandle{}, err
	}
	fsutil.Failpoint("dsh.after_write")
	return kit.NativeHandle{ID: id, Path: path, ResumeCommand: resumeCommand(s.CWD, id)}, nil
}

// RemoveSession removes the file written by Bridge without removing other artifacts.
func (a *Adapter) RemoveSession(ctx context.Context, h kit.NativeHandle) error {
	release, err := a.LockSession(kit.SessionRef{Path: h.Path})
	if err != nil {
		return err
	}
	defer release()
	if err := checkOwned(h.Path); err != nil {
		return err
	}
	return os.Remove(h.Path)
}

func checkOwned(path string) error {
	expected, err := os.ReadFile(filepath.Join(filepath.Dir(path), ".bridge-origin"))
	if err != nil {
		return kit.ErrOwnership
	}
	actual, err := fsutil.HashFile(path)
	if err != nil {
		return err
	}
	if string(expected) != actual {
		return kit.ErrOwnership
	}
	return nil
}

// RollbackSession refuses cleanup once the harness may have continued a migration.
func (a *Adapter) RollbackSession(ctx context.Context, s model.Session, started time.Time) error {
	if !sessionID.MatchString(s.NativeID) {
		return kit.ErrOwnership
	}
	files, err := filepath.Glob(filepath.Join(sessionsDir(), "*", s.NativeID, "session.v*.jsonl*"))
	if err != nil || len(files) == 0 {
		return err
	}
	if len(files) != 1 || (filepath.Base(files[0]) != "session.v4.jsonl" && filepath.Base(files[0]) != "session.v4.jsonl.zstd") {
		return kit.ErrOwnership
	}
	release, err := a.LockSession(kit.SessionRef{Path: files[0]})
	if err != nil {
		return err
	}
	defer release()
	st, err := os.Stat(files[0])
	if err != nil {
		return err
	}
	if st.ModTime().After(started.Add(common.RecoveryGrace)) {
		return kit.ErrOwnership
	}
	if err := checkOwned(files[0]); err != nil {
		return err
	}
	return os.Remove(files[0])
}

type appendState struct {
	seq       int64
	turn      int
	hasSystem bool
	openTurn  bool
}

// readAppendState strictly decodes complete frames; the live reader tolerates torn tails.
func readAppendState(ref kit.SessionRef) (appendState, error) {
	var state appendState
	file, err := os.Open(ref.Path)
	if err != nil {
		return state, err
	}
	defer file.Close()
	var reader io.Reader = file
	if strings.HasSuffix(ref.Path, ".zstd") {
		decoder, err := zstd.NewReader(file, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return state, err
		}
		defer decoder.Close()
		reader = decoder
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		return state, err
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		return state, fmt.Errorf("dsh session has an incomplete tail")
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte{'\n'})
	var header event
	if json.Unmarshal(lines[0], &header) != nil || header.Type != "session" || header.Version != formatVersion || header.ID != ref.NativeID {
		return state, kit.ErrNotSupported
	}
	for _, line := range lines[1:] {
		var row event
		if json.Unmarshal(line, &row) != nil || row.Seq != state.seq {
			return state, fmt.Errorf("invalid dsh event sequence")
		}
		state.seq++
		var coordinates struct {
			Turn int `json:"turn"`
		}
		switch row.Type {
		case "turn/start", "turn/end":
			if err := json.Unmarshal(row.Data, &coordinates); err != nil {
				return state, err
			}
			state.turn = coordinates.Turn
			state.openTurn = row.Type == "turn/start"
		case "system/message":
			state.hasSystem = true
		}
	}
	if state.openTurn {
		return state, fmt.Errorf("dsh session has an unfinished turn; resume it in the harness before syncing")
	}
	return state, nil
}

// ValidateAppend implements adapterkit.SessionAppendValidator for either framing.
func (a *Adapter) ValidateAppend(ref kit.SessionRef) error {
	_, err := readAppendState(ref)
	return err
}

// RenderAppend preserves the native session id and continues its seq/turn counters.
func (a *Adapter) RenderAppend(ctx context.Context, ref kit.SessionRef, events []model.Event) ([]byte, error) {
	state, err := readAppendState(ref)
	if err != nil {
		return nil, err
	}
	w := newWriter(state.seq, state.turn, state.hasSystem, ref.Model, time.Now().UTC())
	if err := w.events(events); err != nil {
		return nil, err
	}
	if !strings.HasSuffix(ref.Path, ".zstd") {
		return w.buf.Bytes(), nil
	}
	return encodeFrame(w.buf.Bytes())
}

func encodeFrame(data []byte) ([]byte, error) {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderCRC(true))
	if err != nil {
		return nil, err
	}
	defer enc.Close()
	return enc.EncodeAll(data, nil), nil
}

// The harness requires its header to occupy a separate checksummed frame.
func encodeLog(data []byte) ([]byte, error) {
	end := bytes.IndexByte(data, '\n')
	if end < 0 {
		return nil, fmt.Errorf("dsh log has no complete header")
	}
	header, err := encodeFrame(data[:end+1])
	if err != nil || end+1 == len(data) {
		return header, err
	}
	body, err := encodeFrame(data[end+1:])
	return append(header, body...), err
}

// AfterAppend implements adapterkit.SessionAppender; the harness scans its logs directly.
func (a *Adapter) AfterAppend(ctx context.Context, ref kit.SessionRef) error { return nil }
