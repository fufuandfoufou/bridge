// Package worktree runs the optional git worktree mode: each agent works in
// its own checkout of the repository, on its own branch, so agents never
// overwrite each other's files. Bridge treats every worktree as the same
// project and reports their progress side by side.
package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// BranchPrefix names the branches bridge creates.
const BranchPrefix = "bridge/"

// Worktree is one checkout of the repository.
type Worktree struct {
	Path    string `json:"path"`
	Branch  string `json:"branch"`
	Head    string `json:"head"`
	Main    bool   `json:"main"`
	Agent   string `json:"agent,omitempty"` // for branches bridge created
	Ahead   int    `json:"ahead"`           // commits not on the main checkout's branch
	Behind  int    `json:"behind"`          // main branch commits it doesn't have
	Changed int    `json:"changed"`         // uncommitted files
	Subject string `json:"subject"`         // last commit
	Missing bool   `json:"missing"`         // the directory is gone
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

// List returns every worktree of the repository main belongs to, the main
// checkout first, with its progress against the main checkout's branch.
func List(ctx context.Context, main string) ([]Worktree, error) {
	out, err := git(ctx, main, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var list []Worktree
	var cur *Worktree
	for _, l := range strings.Split(out+"\n", "\n") {
		switch {
		case strings.HasPrefix(l, "worktree "):
			list = append(list, Worktree{Path: strings.TrimPrefix(l, "worktree ")})
			cur = &list[len(list)-1]
		case cur == nil:
		case strings.HasPrefix(l, "HEAD "):
			cur.Head = strings.TrimPrefix(l, "HEAD ")
		case strings.HasPrefix(l, "branch "):
			cur.Branch = strings.TrimPrefix(strings.TrimPrefix(l, "branch "), "refs/heads/")
		case l == "prunable" || strings.HasPrefix(l, "prunable "):
			cur.Missing = true
		}
	}
	if len(list) == 0 {
		return nil, errors.New("not a git repository")
	}
	list[0].Main = true
	base := list[0].Branch
	for i := range list {
		w := &list[i]
		if a, ok := strings.CutPrefix(w.Branch, BranchPrefix); ok {
			w.Agent = a
		}
		if _, err := os.Stat(w.Path); err != nil {
			w.Missing = true
		}
		if w.Missing {
			continue
		}
		if s, err := git(ctx, w.Path, "status", "--porcelain"); err == nil && s != "" {
			w.Changed = len(strings.Split(s, "\n"))
		}
		w.Subject, _ = git(ctx, w.Path, "log", "-1", "--format=%s")
		if !w.Main && base != "" && w.Head != "" {
			if s, err := git(ctx, main, "rev-list", "--left-right", "--count", base+"..."+w.Head); err == nil {
				if f := strings.Fields(s); len(f) == 2 {
					w.Behind, _ = strconv.Atoi(f[0])
					w.Ahead, _ = strconv.Atoi(f[1])
				}
			}
		}
	}
	return list, nil
}

// DefaultPath is where an agent's worktree goes: next to the main checkout,
// named after it and the agent.
func DefaultPath(main, agent string) string {
	return filepath.Join(filepath.Dir(main), filepath.Base(main)+"-"+agent)
}

// Add creates a worktree for agent on a new branch from base (default: the
// main checkout's HEAD). An existing bridge branch for the agent is reused.
func Add(ctx context.Context, main, agent, base, path, branch string) (Worktree, error) {
	if agent == "" || strings.ContainsAny(agent, "/\\ ") {
		return Worktree{}, fmt.Errorf("invalid agent name %q", agent)
	}
	if path == "" {
		path = DefaultPath(main, agent)
	}
	if branch == "" {
		branch = BranchPrefix + agent
	}
	if _, err := os.Stat(path); err == nil {
		return Worktree{}, fmt.Errorf("%s already exists", path)
	}
	args := []string{"worktree", "add"}
	if _, err := git(ctx, main, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		args = append(args, path, branch)
	} else {
		if base == "" {
			base = "HEAD"
		}
		args = append(args, "-b", branch, path, base)
	}
	if _, err := git(ctx, main, args...); err != nil {
		return Worktree{}, err
	}
	list, err := List(ctx, main)
	if err != nil {
		return Worktree{}, err
	}
	for _, w := range list {
		if same(w.Path, path) {
			return w, nil
		}
	}
	return Worktree{Path: path, Branch: branch, Agent: agent}, nil
}

// Find resolves a worktree by agent name, branch or path.
func Find(list []Worktree, target string) (Worktree, error) {
	for _, w := range list {
		if !w.Main && (w.Agent == target || w.Branch == target || same(w.Path, target)) {
			return w, nil
		}
	}
	return Worktree{}, fmt.Errorf("no worktree %q", target)
}

// Remove deletes a worktree's directory. Its branch and commits stay; with
// uncommitted changes it refuses unless forced.
func Remove(ctx context.Context, main, target string, force bool) (Worktree, error) {
	list, err := List(ctx, main)
	if err != nil {
		return Worktree{}, err
	}
	w, err := Find(list, target)
	if err != nil {
		return w, err
	}
	if w.Changed > 0 && !force {
		return w, fmt.Errorf("%s 有 %d 个未提交的改动；提交后再删除，或强制删除（改动会丢失）", w.Path, w.Changed)
	}
	args := []string{"worktree", "remove", w.Path}
	if force {
		args = []string{"worktree", "remove", "--force", w.Path}
	}
	if w.Missing {
		args = []string{"worktree", "prune"}
	}
	_, err = git(ctx, main, args...)
	return w, err
}

func same(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, e1 := filepath.EvalSymlinks(a)
	rb, e2 := filepath.EvalSymlinks(b)
	return e1 == nil && e2 == nil && ra == rb
}

// Exclude lists paths, relative to a checkout, in the repository's shared
// info/exclude: files bridge generates in a worktree then don't count as
// uncommitted work, without touching any committed .gitignore.
func Exclude(ctx context.Context, dir string, rels []string) error {
	if len(rels) == 0 {
		return nil
	}
	common, err := git(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	p := filepath.Join(common, "info", "exclude")
	b, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	have := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		have[strings.TrimSpace(l)] = true
	}
	text := string(b)
	added := false
	for _, r := range rels {
		line := "/" + filepath.ToSlash(r)
		if have[line] {
			continue
		}
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		if !added {
			text += "# generated by bridge in worktrees\n"
			added = true
		}
		text += line + "\n"
		have[line] = true
	}
	if !added {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(text), 0o644)
}
