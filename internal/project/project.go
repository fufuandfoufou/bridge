// Package project identifies a project from a working directory. It reads
// .git/config directly instead of running git, keeping hook latency low.
package project

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"

	"bridge/internal/config"
	"bridge/internal/fsutil"
	"bridge/internal/model"
)

// Identify returns the project containing cwd: git remote + repository root.
func Identify(cwd string) model.Project {
	if cwd == "" {
		return model.Project{}
	}
	root := config.ProjectRoot(cwd)
	remote := gitRemote(root)
	return model.Project{ID: fsutil.HashString(remote + "|" + root)[:16], Path: root, GitRemote: remote}
}

func gitRemote(root string) string {
	gitDir := filepath.Join(root, ".git")
	if st, err := os.Stat(gitDir); err != nil {
		return ""
	} else if !st.IsDir() { // a worktree: .git is a file naming the real directory
		b, err := os.ReadFile(gitDir)
		if err != nil {
			return ""
		}
		d := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir:"))
		if !filepath.IsAbs(d) {
			d = filepath.Join(root, d)
		}
		if c, err := os.ReadFile(filepath.Join(d, "commondir")); err == nil {
			gitDir = filepath.Clean(filepath.Join(d, strings.TrimSpace(string(c))))
		} else {
			gitDir = d
		}
	}
	f, err := os.Open(filepath.Join(gitDir, "config"))
	if err != nil {
		return ""
	}
	defer f.Close()
	in, s := false, bufio.NewScanner(f)
	for s.Scan() {
		l := strings.TrimSpace(s.Text())
		if strings.HasPrefix(l, "[") {
			in = l == `[remote "origin"]`
			continue
		}
		if k, v, ok := strings.Cut(l, "="); in && ok && strings.TrimSpace(k) == "url" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
