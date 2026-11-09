// Package versions records, per exact agent version, whether native session
// writing was verified on this machine or approved by the user. It extends
// the versions each adapter ships as verified.
package versions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"bridge/internal/fsutil"
	"bridge/internal/model"
	kit "bridge/pkg/adapterkit"
)

// States of a recorded version.
const (
	Verified = "verified" // passed the automatic probe
	Approved = "approved" // the user chose to allow it
	Failed   = "failed"   // the probe failed; writing stays off
)

// Record is what is known about one version.
type Record struct {
	State     string    `json:"state"`
	Error     string    `json:"error,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

var mu sync.Mutex

func path() string { return filepath.Join(fsutil.DataDir(), "agent-versions.json") }

func load() map[string]map[string]Record {
	m := map[string]map[string]Record{}
	if b, err := os.ReadFile(path()); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

// Get returns the record for an exact version.
func Get(agent, version string) (Record, bool) {
	mu.Lock()
	defer mu.Unlock()
	r, ok := load()[agent][version]
	return r, ok
}

// Allowed reports whether writing to this exact version has been enabled.
func Allowed(agent, version string) bool {
	if version == "" {
		return false
	}
	r, ok := Get(agent, version)
	return ok && (r.State == Verified || r.State == Approved)
}

// Set stores a record.
func Set(agent, version string, r Record) error {
	mu.Lock()
	defer mu.Unlock()
	m := load()
	if m[agent] == nil {
		m[agent] = map[string]Record{}
	}
	m[agent][version] = r
	return save(m)
}

// Clear forgets a version, returning it to unverified.
func Clear(agent, version string) error {
	mu.Lock()
	defer mu.Unlock()
	m := load()
	delete(m[agent], version)
	return save(m)
}

func save(m map[string]map[string]Record) error {
	if err := fsutil.EnsurePrivateDir(fsutil.DataDir()); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWrite(path(), b, 0o600)
}

// ErrNoProbe reports an adapter that cannot verify itself.
var ErrNoProbe = errors.New("这个 agent 不支持自动检测，只能手动启用")

// Probe checks a version and records the outcome: existing sessions of that
// version must still parse, and the real client must load a written session.
func Probe(ctx context.Context, a kit.Adapter, version string) (Record, error) {
	p, ok := a.(kit.VersionProber)
	if !ok {
		return Record{}, ErrNoProbe
	}
	err := readable(ctx, a, version)
	if err == nil {
		err = p.ProbeVersion(ctx, version)
	}
	if ctx.Err() != nil {
		return Record{}, ctx.Err() // cancelled, so no verdict either way
	}
	r := Record{State: Verified, CheckedAt: time.Now().UTC()}
	if err != nil {
		r.State, r.Error = Failed, err.Error()
	}
	if e := Set(a.ID(), version, r); e != nil {
		return r, e
	}
	return r, nil
}

// readable confirms the newest session of this version still yields events,
// catching a read format change that the write probe cannot see.
func readable(ctx context.Context, a kit.Adapter, version string) error {
	refs, err := a.ListSessions(ctx, kit.Filter{Limit: 50})
	if err != nil {
		return err
	}
	for _, r := range refs {
		if r.Version != version || r.Unreadable {
			continue
		}
		es, _, err := a.ReadSession(ctx, r, model.Cursor{})
		if err != nil {
			return fmt.Errorf("读取现有会话失败：%w", err)
		}
		if len(es) == 0 {
			return fmt.Errorf("现有会话 %s 解析不出内容", r.NativeID)
		}
		return nil
	}
	return nil // no session of this version yet
}
