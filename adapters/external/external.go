// Package external talks to out-of-process adapters over JSON-RPC 2.0 on
// stdio. Each adapter is untrusted code: it must be approved once, is started
// on demand, exits when idle, and its crashes never reach the main process.
package external

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"bridge/internal/config"
	"bridge/internal/fsutil"
	"bridge/internal/model"
	kit "bridge/pkg/adapterkit"
)

// Prefix is the executable name prefix used for PATH discovery.
const Prefix = "bridge-adapter-"

const (
	callTimeout = 30 * time.Second
	idleTimeout = 60 * time.Second
)

// Client implements adapterkit.Adapter over JSON-RPC.
type Client struct {
	id, path string

	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	nextID int
	idle   *time.Timer
}

// New creates a client for the executable at path.
func New(id, path string) *Client { return &Client{id: id, path: path} }

// ID implements Adapter.
func (c *Client) ID() string { return c.id }

// Close terminates the child process, if any.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stop()
}

func (c *Client) stop() {
	if c.idle != nil {
		c.idle.Stop()
		c.idle = nil
	}
	if c.cmd != nil {
		_ = c.stdin.Close()
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
		c.cmd = nil
	}
}

func (c *Client) start() error {
	cmd := exec.Command(c.path)
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	c.cmd, c.stdin, c.stdout = cmd, in, bufio.NewReaderSize(out, 1<<20)
	return nil
}

// maxMessage bounds one response so an adapter cannot exhaust memory.
const maxMessage = 32 << 20

func readLine(r *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		chunk, err := r.ReadSlice('\n')
		out = append(out, chunk...)
		if len(out) > maxMessage {
			return nil, fmt.Errorf("response exceeds %d bytes", maxMessage)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return out, err
	}
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// call runs one request. On timeout or broken pipe the child is killed, so a
// misbehaving adapter fails the call and nothing else.
func (c *Client) call(ctx context.Context, method string, params, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd == nil {
		if err := c.start(); err != nil {
			return fmt.Errorf("start adapter %s: %w", c.id, err)
		}
	}
	if c.idle != nil {
		c.idle.Stop()
	}
	c.nextID++
	id := c.nextID
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return err
	}
	type reply struct {
		resp rpcResponse
		err  error
	}
	stdin, stdout := c.stdin, c.stdout
	ch := make(chan reply, 1)
	go func() {
		if _, err := stdin.Write(append(b, '\n')); err != nil {
			ch <- reply{err: err}
			return
		}
		for {
			line, err := readLine(stdout)
			if err != nil {
				ch <- reply{err: err}
				return
			}
			var r rpcResponse
			if err := json.Unmarshal(line, &r); err != nil {
				ch <- reply{err: fmt.Errorf("invalid response: %w", err)}
				return
			}
			if r.ID == id {
				ch <- reply{resp: r}
				return
			}
		}
	}()
	timer := time.NewTimer(callTimeout)
	defer timer.Stop()
	var r reply
	select {
	case r = <-ch:
	case <-timer.C:
		c.stop()
		return fmt.Errorf("adapter %s: %s timed out", c.id, method)
	case <-ctx.Done():
		c.stop()
		return ctx.Err()
	}
	if r.err != nil {
		c.stop()
		return fmt.Errorf("adapter %s: %s: %w", c.id, method, r.err)
	}
	c.idle = time.AfterFunc(idleTimeout, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.stop()
	})
	if r.resp.Error != nil {
		if r.resp.Error.Code == -32601 {
			return fmt.Errorf("%s: %w", method, kit.ErrNotSupported)
		}
		return fmt.Errorf("adapter %s: %s: %s", c.id, method, r.resp.Error.Message)
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(r.resp.Result, result)
}

// Capabilities implements Adapter via the initialize handshake. A failed or
// protocol-incompatible handshake yields no capabilities.
func (c *Client) Capabilities(v kit.Version) kit.Capabilities {
	var res struct {
		ProtocolVersion string           `json:"protocol_version"`
		Capabilities    kit.Capabilities `json:"capabilities"`
	}
	err := c.call(context.Background(), "initialize", map[string]string{"protocol_version": kit.ProtocolVersion, "agent_version": string(v)}, &res)
	if err != nil || res.ProtocolVersion != kit.ProtocolVersion {
		return kit.Capabilities{}
	}
	return res.Capabilities
}

// Detect implements Adapter.
func (c *Client) Detect(ctx context.Context) (kit.Install, error) {
	var r kit.Install
	return r, c.call(ctx, "detect", struct{}{}, &r)
}

// ListSessions implements Adapter.
func (c *Client) ListSessions(ctx context.Context, f kit.Filter) ([]kit.SessionRef, error) {
	var r struct {
		Sessions []kit.SessionRef `json:"sessions"`
	}
	return r.Sessions, c.call(ctx, "sessions/list", map[string]any{"filter": f}, &r)
}

// ReadSession implements Adapter.
func (c *Client) ReadSession(ctx context.Context, ref kit.SessionRef, from model.Cursor) ([]model.Event, model.Cursor, error) {
	var r struct {
		Events []model.Event `json:"events"`
		Cursor model.Cursor  `json:"cursor"`
	}
	err := c.call(ctx, "sessions/read", map[string]any{"ref": ref, "from": from}, &r)
	return r.Events, r.Cursor, err
}

// WriteSession implements Adapter. Native references are never disclosed.
func (c *Client) WriteSession(ctx context.Context, s model.Session, events []model.Event) (kit.NativeHandle, error) {
	clean := make([]model.Event, len(events))
	for i, e := range events {
		e.NativeRef = nil
		clean[i] = e
	}
	var h kit.NativeHandle
	return h, c.call(ctx, "sessions/write", map[string]any{"session": s, "events": clean}, &h)
}

// InstallHooks implements Adapter.
func (c *Client) InstallHooks(ctx context.Context, cfg kit.HookConfig) error {
	return c.call(ctx, "hooks/install", cfg, nil)
}

// UninstallHooks implements Adapter.
func (c *Client) UninstallHooks(ctx context.Context) error {
	return c.call(ctx, "hooks/uninstall", struct{}{}, nil)
}

// Candidate is a discovered external adapter.
type Candidate struct {
	ID, Path string
	Trusted  bool
}

func trustPath() string { return filepath.Join(fsutil.DataDir(), "trusted-adapters.json") }

func loadTrust() map[string]string {
	m := map[string]string{}
	if b, err := os.ReadFile(trustPath()); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

// Trust records approval of an executable's current content. Replacing the
// file invalidates the approval.
func Trust(path string) error {
	h, err := fsutil.HashFile(path)
	if err != nil {
		return err
	}
	m := loadTrust()
	m[path] = h
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := fsutil.EnsurePrivateDir(fsutil.DataDir()); err != nil {
		return err
	}
	return fsutil.AtomicWrite(trustPath(), b, 0o600)
}

// Discover finds adapters on PATH and in the configuration; config wins.
func Discover(cfg *config.Config) []Candidate {
	found := map[string]string{}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), Prefix) {
				continue
			}
			p := filepath.Join(dir, e.Name())
			if st, err := os.Stat(p); err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
				continue
			}
			id := strings.TrimPrefix(e.Name(), Prefix)
			if _, dup := found[id]; !dup {
				found[id] = p
			}
		}
	}
	for id, a := range cfg.Adapters {
		found[id] = fsutil.Expand(a.Path)
	}
	trust := loadTrust()
	out := make([]Candidate, 0, len(found))
	for id, p := range found {
		h, err := fsutil.HashFile(p)
		out = append(out, Candidate{ID: id, Path: p, Trusted: err == nil && trust[p] == h})
	}
	return out
}

// ErrUntrusted is returned for adapters awaiting approval.
var ErrUntrusted = errors.New("adapter is not trusted; run: bridge adapter trust <id>")
