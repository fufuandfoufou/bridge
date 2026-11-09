package live

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"bridge/internal/fsutil"
)

// ClaimTTL is the lease length; every edit renews it, so a crashed client
// cannot hold a file indefinitely.
const ClaimTTL = 10 * time.Minute

// Claims are turn-scoped leases. They coordinate participating agents only:
// manual edits and shell commands are not covered. With Path set, they survive
// a daemon restart.
type Claims struct {
	mu    sync.Mutex
	files map[string]Claim
	// Path is the file claims are persisted to; empty keeps them in memory.
	Path string
}

// Load restores persisted claims. A missing or damaged file means no claims:
// leases are advisory state that a later edit re-establishes.
func (c *Claims) Load() {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := os.ReadFile(c.Path)
	if err != nil {
		return
	}
	var list []Claim
	if json.Unmarshal(b, &list) != nil {
		return
	}
	c.files = make(map[string]Claim, len(list))
	for _, v := range list {
		c.files[v.Project+"\x00"+v.Path] = v
	}
}

func (c *Claims) save() {
	if c.Path == "" {
		return
	}
	list := make([]Claim, 0, len(c.files))
	for _, v := range c.files {
		list = append(list, v)
	}
	if b, err := json.Marshal(list); err == nil {
		_ = fsutil.AtomicWrite(c.Path, b, 0o600)
	}
}

type Claim struct {
	Project   string    `json:"project"`
	Path      string    `json:"path"`
	Agent     string    `json:"agent"`
	Session   string    `json:"session"`
	ClaimedAt time.Time `json:"claimed_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (c *Claims) cleanup(now time.Time) {
	if c.files == nil {
		c.files = make(map[string]Claim)
	}
	for k, v := range c.files {
		if !now.Before(v.ExpiresAt) {
			delete(c.files, k)
		}
	}
}

// TryAcquire takes every path or none, for the block policy: a denied edit
// must not leave claims behind on the paths that happened to be free.
func (c *Claims) TryAcquire(project, agent, session string, paths []string, now time.Time) []Claim {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cleanup(now)
	var conflicts []Claim
	for _, path := range paths {
		if old, ok := c.files[project+"\x00"+path]; ok && (old.Agent != agent || old.Session != session) {
			conflicts = append(conflicts, old)
		}
	}
	if len(conflicts) > 0 {
		return conflicts
	}
	return c.acquire(project, agent, session, paths, now)
}

// Acquire keeps the first owner on conflict and returns the conflicts.
func (c *Claims) Acquire(project, agent, session string, paths []string, now time.Time) []Claim {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cleanup(now)
	return c.acquire(project, agent, session, paths, now)
}

func (c *Claims) acquire(project, agent, session string, paths []string, now time.Time) []Claim {
	defer c.save()
	var conflicts []Claim
	for _, path := range paths {
		key := project + "\x00" + path
		old, ok := c.files[key]
		if ok && (old.Agent != agent || old.Session != session) {
			conflicts = append(conflicts, old)
			continue
		}
		if !ok {
			old = Claim{Project: project, Path: path, Agent: agent, Session: session, ClaimedAt: now}
		}
		old.ExpiresAt = now.Add(ClaimTTL)
		c.files[key] = old
	}
	return conflicts
}

func (c *Claims) Release(agent, session string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range c.files {
		if v.Agent == agent && v.Session == session {
			delete(c.files, k)
		}
	}
	c.save()
}

func (c *Claims) List(now time.Time) []Claim {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cleanup(now)
	out := make([]Claim, 0, len(c.files))
	for _, v := range c.files {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// EditPaths extracts only explicit file-edit tool paths. Shell commands and
// unknown tools are deliberately not interpreted as reliable file edits.
func EditPaths(tool string, raw json.RawMessage, cwd, root string) []string {
	var input struct {
		FilePath string `json:"file_path"`
		Command  string `json:"command"`
		Patch    string `json:"patch"`
	}
	if json.Unmarshal(raw, &input) != nil {
		return nil
	}
	var paths []string
	switch strings.ToLower(tool) {
	case "edit", "write", "multiedit":
		paths = append(paths, input.FilePath)
	case "apply_patch":
		patch := input.Command
		if patch == "" {
			patch = input.Patch
		}
		for _, line := range strings.Split(patch, "\n") {
			for _, prefix := range []string{"*** Add File: ", "*** Update File: ", "*** Delete File: ", "*** Move to: "} {
				if strings.HasPrefix(line, prefix) {
					paths = append(paths, strings.TrimSpace(strings.TrimPrefix(line, prefix)))
				}
			}
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, path := range paths {
		if path == "" || strings.ContainsRune(path, 0) {
			continue
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		path = filepath.Clean(path)
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = resolved
		} else if parent, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
			path = filepath.Join(parent, filepath.Base(path))
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) || seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	return out
}

// ClaimDenial explains a refused edit to the agent.
func ClaimDenial(conflicts []Claim) string {
	var b strings.Builder
	b.WriteString("Bridge 已拦截这次编辑，以下文件正由其他会话修改，请稍后重试或改做别的：\n")
	for _, v := range conflicts {
		fmt.Fprintf(&b, "- %s 已由 %s 会话 %s 认领\n", v.Path, v.Agent, v.Session)
	}
	return b.String()
}

func ClaimWarning(conflicts []Claim) string {
	if len(conflicts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Bridge 文件冲突提醒（仅提示，编辑仍会继续）：\n")
	for _, v := range conflicts {
		fmt.Fprintf(&b, "- %s 已由 %s 会话 %s 认领（%s）\n", v.Path, v.Agent, v.Session, v.ClaimedAt.Local().Format("15:04:05"))
	}
	return b.String()
}
