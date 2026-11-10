package acceptance

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bridge/internal/project"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.test"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestL6_WorktreeMode(t *testing.T) {
	e := newEnv(t)
	repo := filepath.Join(e.home, "repo")
	must(t, os.MkdirAll(repo, 0o755))
	gitIn(t, repo, "init", "-q", "-b", "main")
	write(t, filepath.Join(repo, "AGENTS.md"), "# rules\n")
	write(t, filepath.Join(repo, "app.go"), "package app\n")
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-qm", "init")
	must(t, os.MkdirAll(filepath.Join(e.home, ".claude/projects"), 0o755))

	r := e.run("", "worktree", "add", "codex")
	if r.err == nil {
		t.Fatal("worktree add outside a repository should fail")
	}
	cmd := exec.Command(binPath, "worktree", "add", "codex")
	cmd.Dir, cmd.Env = repo, append(os.Environ(), "HOME="+e.home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("add: %s", out)
	}
	wt := filepath.Join(e.home, "repo-codex")
	if !strings.Contains(string(out), wt) || gitIn(t, wt, "branch", "--show-current") != "bridge/codex" {
		t.Fatalf("worktree: %s", out)
	}

	// The checkout gets the project's rules, which don't count as changes.
	if _, err := os.Lstat(filepath.Join(wt, "CLAUDE.md")); err != nil {
		t.Error("rules not synced into the worktree")
	}
	if s := gitIn(t, wt, "status", "--porcelain"); s != "" {
		t.Errorf("generated files show as changes: %q", s)
	}

	// Every worktree is the same project, each with its own checkout.
	pm, pw := project.Identify(repo), project.Identify(filepath.Join(wt, "sub"))
	if pm.ID != pw.ID || pw.Path != repo || pw.Worktree != wt {
		t.Fatalf("identity: %+v vs %+v", pm, pw)
	}

	// Progress from the other worktree says where it happened.
	s := e.store()
	seedSession(t, s, "claude-code:main", "claude-code", repo, "主目录的工作")
	seedSession(t, s, "codex:wt", "codex", wt, "在 worktree 里重构")
	svc := liveSvc(s)
	got, err := svc.PromptSubmit("claude-code", "main", repo)
	must(t, err)
	if !strings.Contains(got, "在 worktree 里重构") || !strings.Contains(got, "工作位置：worktree "+wt) || !strings.Contains(got, "分支 bridge/codex") {
		t.Errorf("digest: %s", got)
	}
	got, err = svc.PromptSubmit("codex", "wt", wt)
	must(t, err)
	if !strings.Contains(got, "工作位置：主目录 "+repo) || !strings.Contains(got, "分支 main") {
		t.Errorf("worktree reader: %s", got)
	}

	// Disk changes in another checkout aren't the reader's files.
	must(t, s.AddWorkspaceChange(pm.ID, filepath.Join(wt, "app.go"), "modify", time.Now().UTC()))
	svc.Ack("claude-code", "main")
	if got, _ := svc.PromptSubmit("claude-code", "main", repo); strings.Contains(got, "app.go") {
		t.Errorf("another worktree's change reported: %s", got)
	}

	// The summary shows each worktree's progress and the latest instruction.
	write(t, filepath.Join(wt, "app.go"), "package app\n\nfunc New() {}\n")
	gitIn(t, wt, "commit", "-qam", "add New")
	write(t, filepath.Join(wt, "wip.go"), "package app\n")
	r = e.run("", "api", "worktrees", "--dir", repo)
	for _, want := range []string{`"branch":"bridge/codex"`, `"ahead":1`, `"changed":1`, `"agent":"codex"`, "在 worktree 里重构", `"main":true`} {
		if !strings.Contains(r.out, want) {
			t.Errorf("worktrees lacks %s: %s", want, r.out)
		}
	}

	// Uncommitted work is never discarded without asking; the branch stays.
	r = e.run("", "api", "worktree-remove", "codex", "--dir", repo)
	if !strings.Contains(r.out, "未提交") {
		t.Fatalf("removed with uncommitted changes: %s", r.out)
	}
	must(t, os.Remove(filepath.Join(wt, "wip.go")))
	if r = e.run("", "api", "worktree-remove", "codex", "--dir", repo); !strings.Contains(r.out, `"ok":true`) {
		t.Fatal(r.out)
	}
	if _, err := os.Stat(wt); err == nil {
		t.Error("directory left")
	}
	if gitIn(t, repo, "log", "-1", "--format=%s", "bridge/codex") != "add New" {
		t.Error("branch lost")
	}
}
