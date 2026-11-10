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
// Every worktree of a repository is the same project, identified by its main
// checkout, so agents in different worktrees share memory, progress and the
// task board.
func Identify(cwd string) model.Project {
	if cwd == "" {
		return model.Project{}
	}
	root := config.ProjectRoot(cwd)
	main := MainCheckout(root)
	remote := gitRemote(root)
	return model.Project{ID: fsutil.HashString(remote + "|" + main)[:16], Path: main, GitRemote: remote, Worktree: root}
}

// gitDirs returns the checkout's own git directory and the repository's
// shared one; they differ in a linked worktree, whose .git is a file.
func gitDirs(root string) (own, common string) {
	gitDir := filepath.Join(root, ".git")
	st, err := os.Stat(gitDir)
	if err != nil {
		return "", ""
	}
	if st.IsDir() {
		return gitDir, gitDir
	}
	b, err := os.ReadFile(gitDir)
	if err != nil {
		return "", ""
	}
	d := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir:"))
	if !filepath.IsAbs(d) {
		d = filepath.Join(root, d)
	}
	if c, err := os.ReadFile(filepath.Join(d, "commondir")); err == nil {
		return d, filepath.Clean(filepath.Join(d, strings.TrimSpace(string(c))))
	}
	return d, d
}

// MainCheckout returns the main working tree of the repository root belongs
// to, or root itself.
func MainCheckout(root string) string {
	own, common := gitDirs(root)
	if own == "" || own == common || filepath.Base(common) != ".git" {
		return root
	}
	return filepath.Dir(common)
}

// Branch returns the branch checked out at root, or "" when detached.
func Branch(root string) string {
	own, _ := gitDirs(root)
	b, err := os.ReadFile(filepath.Join(own, "HEAD"))
	if err != nil {
		return ""
	}
	ref, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "ref: refs/heads/")
	if !ok {
		return ""
	}
	return ref
}

func gitRemote(root string) string {
	_, gitDir := gitDirs(root)
	if gitDir == "" {
		return ""
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
