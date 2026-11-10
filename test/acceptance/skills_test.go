package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// skillsEnv lays out a home like a real one: Claude and Codex each with
// their own skills, plus the agents' internal directories bridge must leave
// alone.
func skillsEnv(t *testing.T) *env {
	e := newEnv(t)
	e.base()
	write(t, filepath.Join(e.home, ".config/bridge/config.toml"), "version = 1\n[agents]\nenabled = [\"claude-code\", \"codex\", \"pi\"]\n")
	must(t, os.MkdirAll(filepath.Join(e.home, ".pi/agent"), 0o755))
	write(t, filepath.Join(e.home, ".claude/skills/both/SKILL.md"), "claude version")
	write(t, filepath.Join(e.home, ".claude/skills/claude-only/SKILL.md"), "c")
	write(t, filepath.Join(e.home, ".claude/skills/synced/uuid/SKILL.md"), "managed by the app")
	write(t, filepath.Join(e.home, ".codex/skills/both/SKILL.md"), "codex version")
	write(t, filepath.Join(e.home, ".codex/skills/codex-only/SKILL.md"), "x")
	write(t, filepath.Join(e.home, ".codex/skills/.system/builtin/SKILL.md"), "built in")
	write(t, filepath.Join(e.home, ".pi/agent/skills/same/SKILL.md"), "same")
	write(t, filepath.Join(e.home, ".agents/skills/same/SKILL.md"), "same")
	return e
}

func linkTo(t *testing.T, p string) string {
	t.Helper()
	l, err := os.Readlink(p)
	if err != nil {
		t.Fatalf("%s is not a symlink: %v", p, err)
	}
	return l
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestSKILL01_CollectIntoSharedDirectory(t *testing.T) {
	e := skillsEnv(t)
	shared := filepath.Join(e.home, ".agents/skills")
	r := e.run("", "sync")
	// Claude's own skills move to the shared directory and are linked back.
	if read(t, filepath.Join(shared, "claude-only/SKILL.md")) != "c" {
		t.Error("claude-only not collected")
	}
	if linkTo(t, filepath.Join(e.home, ".claude/skills/claude-only")) != filepath.Join(shared, "claude-only") {
		t.Error("claude-only not linked back")
	}
	// Codex reads the shared directory itself: no link is left behind.
	if read(t, filepath.Join(shared, "codex-only/SKILL.md")) != "x" || exists(filepath.Join(e.home, ".codex/skills/codex-only")) {
		t.Error("codex-only not moved")
	}
	if linkTo(t, filepath.Join(e.home, ".claude/skills/codex-only")) != filepath.Join(shared, "codex-only") {
		t.Error("codex-only not linked for Claude")
	}
	// An identical copy is removed; pi reads the shared one.
	if exists(filepath.Join(e.home, ".pi/agent/skills/same")) {
		t.Error("duplicate kept")
	}
	// Directories the agents manage themselves are untouched.
	if read(t, filepath.Join(e.home, ".codex/skills/.system/builtin/SKILL.md")) != "built in" ||
		read(t, filepath.Join(e.home, ".claude/skills/synced/uuid/SKILL.md")) != "managed by the app" {
		t.Error("managed directory touched")
	}
	if exists(filepath.Join(shared, ".system")) || exists(filepath.Join(shared, "synced")) {
		t.Error("managed directory collected")
	}
	// Same name, different content: the first collected wins the shared
	// slot only because nothing was there; the other side waits.
	if read(t, filepath.Join(e.home, ".codex/skills/both/SKILL.md")) != "codex version" {
		t.Error("conflicting copy changed")
	}
	if !strings.Contains(r.out, "conflict") {
		t.Errorf("conflict not reported: %s", r.out)
	}
	// Nothing left to do for the agents that read the shared directory.
	for _, l := range strings.Split(e.run("", "plan").out, "\n") {
		if strings.Contains(l, "skill") && !strings.HasPrefix(l, "noop") && !strings.HasPrefix(l, "conflict") {
			t.Errorf("not settled: %s", l)
		}
	}
}

func TestSKILL02_InvertedLinkIsCollected(t *testing.T) {
	e := newEnv(t)
	e.base()
	own := filepath.Join(e.home, ".claude/skills/video")
	write(t, filepath.Join(own, "SKILL.md"), "v")
	shared := filepath.Join(e.home, ".agents/skills/video")
	must(t, os.MkdirAll(filepath.Dir(shared), 0o755))
	must(t, os.Symlink(own, shared))
	if r := e.run("", "sync"); r.err != nil {
		t.Fatal(r.out)
	}
	if st, err := os.Lstat(shared); err != nil || !st.IsDir() {
		t.Fatal("shared copy is not a real directory")
	}
	if read(t, filepath.Join(shared, "SKILL.md")) != "v" || linkTo(t, own) != shared {
		t.Error("link not inverted")
	}
}

func TestSKILL03_ConflictResolution(t *testing.T) {
	shared := func(e *env) string { return filepath.Join(e.home, ".agents/skills/both/SKILL.md") }
	t.Run("adopt", func(t *testing.T) {
		e := skillsEnv(t)
		e.run("", "sync")
		before := read(t, shared(e))
		other := map[string]string{"claude version": "codex", "codex version": "claude-code"}[before]
		if r := e.run("", "sync", "--adopt", other); r.err != nil {
			t.Fatal(r.out)
		}
		if read(t, shared(e)) == before {
			t.Error("adopted version not shared")
		}
		if !strings.Contains(e.run("", "plan").out, "noop") || strings.Contains(e.run("", "plan").out, "conflict") {
			t.Error("conflict not resolved")
		}
		backups, _ := filepath.Glob(filepath.Join(e.home, ".local/share/bridge/backups/skills/both-*/SKILL.md"))
		if len(backups) == 0 || read(t, backups[0]) != before {
			t.Error("replaced version not backed up")
		}
	})
	t.Run("force", func(t *testing.T) {
		e := skillsEnv(t)
		e.run("", "sync")
		before := read(t, shared(e))
		if r := e.run("", "sync", "--force"); r.err != nil {
			t.Fatal(r.out)
		}
		if read(t, shared(e)) != before {
			t.Error("shared version changed")
		}
		if strings.Contains(e.run("", "plan").out, "conflict") {
			t.Error("conflict not resolved")
		}
	})
}

func TestSKILL04_UninstallRestoresCopies(t *testing.T) {
	e := skillsEnv(t)
	e.run("", "sync")
	if r := e.run("", "uninstall", "--yes"); r.err != nil {
		t.Fatal(r.out)
	}
	p := filepath.Join(e.home, ".claude/skills/claude-only")
	if st, err := os.Lstat(p); err != nil || st.Mode()&os.ModeSymlink != 0 {
		t.Fatal("claude-only not restored as a real directory")
	}
	if read(t, filepath.Join(p, "SKILL.md")) != "c" {
		t.Error("restored content differs")
	}
	if exists(filepath.Join(e.home, ".claude/skills/codex-only")) {
		t.Error("link bridge created was left behind")
	}
}
