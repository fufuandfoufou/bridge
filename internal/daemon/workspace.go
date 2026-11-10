package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"bridge/internal/config"
	"bridge/internal/fsutil"
	"bridge/internal/store"
)

// Directories that are generated or private, never edited as project work.
var skipDirs = map[string]bool{"node_modules": true, "vendor": true, "dist": true, "build": true, "target": true, "__pycache__": true, "bin": true}

const maxWatchedDirs = 5000

func skipName(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasSuffix(name, "~") || strings.HasSuffix(name, ".swp") ||
		strings.HasSuffix(name, ".tmp") || strings.HasPrefix(name, ".bridge-tmp-")
}

// watchWorkspaces records every file change in the directories of active
// projects, whoever made it, so agents also learn about manual edits and
// shell commands. Only git repositories are watched, never home directories.
func (d *Daemon) watchWorkspaces(ctx context.Context) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		d.Logger.Printf("workspace watcher unavailable: %v", err)
		return
	}
	defer w.Close()
	roots := map[string]string{}  // project root -> project ID
	dirs := map[string][]string{} // project root -> watched directories
	addDir := func(root, dir string) {
		if len(dirs[root]) >= maxWatchedDirs {
			return
		}
		_ = filepath.WalkDir(dir, func(p string, e os.DirEntry, err error) error {
			if err != nil || !e.IsDir() {
				return nil
			}
			if p != root && (skipName(e.Name()) || skipDirs[e.Name()]) {
				return filepath.SkipDir
			}
			if len(dirs[root]) >= maxWatchedDirs {
				return filepath.SkipAll
			}
			if w.Add(p) == nil {
				dirs[root] = append(dirs[root], p)
			}
			return nil
		})
	}
	refresh := func() {
		active := map[string]string{}
		if !fsutil.Paused() {
			list, err := d.Store.ListSessions(store.SessionFilter{Since: time.Now().Add(-2 * time.Hour)})
			if err != nil {
				d.Logger.Printf("workspace: %v", err)
				return
			}
			projects, err := d.Store.ListProjects()
			if err != nil {
				d.Logger.Printf("workspace: %v", err)
				return
			}
			known := map[string]bool{}
			for _, p := range projects {
				known[p.ID] = true
			}
			// Each session's own checkout: worktrees of one project are
			// watched separately.
			for _, s := range list {
				if !known[s.ProjectID] || s.CWD == "" {
					continue
				}
				root := config.ProjectRoot(s.CWD)
				if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
					continue
				}
				if cfg, err := d.Config(root); err != nil || !cfg.Sync.Sessions {
					continue
				} else if m, err := cfg.Matcher(); err != nil || m.Excluded(root) {
					continue
				}
				if real, err := filepath.EvalSymlinks(root); err == nil {
					active[real] = s.ProjectID
				}
			}
		}
		for root, id := range active {
			if _, ok := roots[root]; !ok {
				roots[root] = id
				addDir(root, root)
			}
		}
		for root := range roots {
			if _, ok := active[root]; !ok {
				for _, p := range dirs[root] {
					_ = w.Remove(p)
				}
				delete(roots, root)
				delete(dirs, root)
			}
		}
	}
	projectOf := func(path string) (string, string) {
		best, id := "", ""
		for root, pid := range roots {
			if (path == root || strings.HasPrefix(path, root+string(filepath.Separator))) && len(root) > len(best) {
				best, id = root, pid
			}
		}
		return best, id
	}
	type change struct{ op, project, root string }
	pending := map[string]change{}
	flush := time.NewTimer(time.Hour)
	flush.Stop()
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	refresh()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			refresh()
			_ = d.Store.PruneWorkspaceChanges(time.Now().Add(-24 * time.Hour))
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			d.Logger.Printf("workspace watcher: %v", err)
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			root, pid := projectOf(ev.Name)
			if pid == "" || skipName(filepath.Base(ev.Name)) {
				continue
			}
			rel, _ := filepath.Rel(root, ev.Name)
			if parts := strings.Split(rel, string(filepath.Separator)); len(parts) > 1 {
				hidden := false
				for _, part := range parts[:len(parts)-1] {
					hidden = hidden || skipName(part) || skipDirs[part]
				}
				if hidden {
					continue
				}
			}
			if st, err := os.Stat(ev.Name); err == nil && st.IsDir() {
				if ev.Op&fsnotify.Create != 0 {
					addDir(root, ev.Name)
				}
				continue
			}
			op := "modify"
			switch {
			case ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0:
				if _, err := os.Stat(ev.Name); err == nil {
					continue // an atomic save renamed a file into place
				}
				op = "delete"
			case ev.Op&fsnotify.Create != 0:
				op = "create"
			case ev.Op&fsnotify.Write == 0:
				continue
			}
			if prev, ok := pending[ev.Name]; ok && prev.op == "create" && op == "modify" {
				op = "create"
			}
			pending[ev.Name] = change{op: op, project: pid, root: root}
			flush.Reset(time.Second)
		case <-flush.C:
			now := time.Now().UTC()
			byRoot := map[string][]string{}
			for path, c := range pending {
				byRoot[c.root] = append(byRoot[c.root], path)
			}
			for root, paths := range byRoot {
				status, err := gitStatus(ctx, root, paths)
				for _, path := range paths {
					c := pending[path]
					if err == nil { // without git, record what the watcher saw
						op, dirty := status[path]
						if !dirty {
							continue // same as the commit: a checkout, pull or reverted edit
						}
						c.op = op
					}
					if err := d.Store.AddWorkspaceChange(c.project, path, c.op, now); err != nil {
						d.Logger.Printf("workspace: %v", err)
					}
				}
			}
			pending = map[string]change{}
		}
	}
}

// gitStatus reports which of paths differ from the checkout's commit, keyed by
// absolute path, with the operation git sees: create for new files, delete,
// modify otherwise. Files git ignores and files identical to the commit are
// absent, which drops the files a checkout, pull or rebase rewrites, and
// temporary files that were created and removed again.
func gitStatus(ctx context.Context, root string, paths []string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	args := []string{"-C", root, "status", "--porcelain=v1", "-z", "--untracked-files=all"}
	if len(paths) <= 200 {
		args = append(args, "--")
		for _, p := range paths {
			if rel, err := filepath.Rel(root, p); err == nil {
				args = append(args, rel)
			}
		}
	}
	out, err := exec.CommandContext(ctx, "git", args...).Output()
	if err != nil {
		return nil, err
	}
	status := map[string]string{}
	fields := strings.Split(string(out), "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if len(f) < 4 {
			continue
		}
		code, rel := f[:2], f[3:]
		if code[0] == 'R' || code[0] == 'C' {
			i++ // the original path follows a rename or copy
		}
		op := "modify"
		switch {
		case code == "??" || code[0] == 'A':
			op = "create"
		case code[0] == 'D' || code[1] == 'D':
			op = "delete"
		}
		status[filepath.Join(root, rel)] = op
	}
	return status, nil
}
