package filesync

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"bridge/internal/fsutil"
	"bridge/internal/privacy"
	kit "bridge/pkg/adapterkit"
)

// Global skills live in one shared directory, ~/.agents/skills, the Agent
// Skills location Codex, pi and DeepSeek Harness read themselves. Agents
// that don't (Claude Code) get a symlink per skill. A skill found only in an
// agent's own directory is collected: moved into the shared directory, with a
// symlink left behind for agents that need one. Skills of the same name but
// different content are never merged automatically.

// SharedSkills is the shared global skill directory.
func SharedSkills() string { return filepath.Join(fsutil.Home(), ".agents", "skills") }

// legacyAgent stands for earlier versions' shared directory in actions.
const legacyAgent = "bridge"

// legacySkills is where earlier versions kept the shared skills.
func legacySkills() string { return filepath.Join(fsutil.ConfigDir(), "skills") }

// planCollect plans the collection of the skills in an agent's own
// directory. linkBack is true for agents that read that directory rather
// than the shared one, so a symlink must replace each collected skill.
func planCollect(in Input, a Agent, own, shared string, linkBack bool) ([]Action, error) {
	cfg := in.Config
	entries, err := os.ReadDir(own)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	format := kit.FormatSource
	if linkBack {
		format = kit.FormatSymlink
	}
	var out []Action
	for _, e := range entries {
		name := e.Name()
		// Hidden directories hold skills the agent manages itself (Codex's
		// .system); containers without a SKILL.md (Claude's synced) aren't
		// skills either.
		if strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(own, name)
		st, err := os.Lstat(path)
		if err != nil || st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
			continue // links, including bridge's own, are not the agent's skills
		}
		if _, err := os.Stat(filepath.Join(path, "SKILL.md")); err != nil {
			continue
		}
		act := Action{Category: "skill", Agent: a.ID, Source: filepath.Join(shared, name), Target: path, Scope: "global", Format: format}
		switch {
		case a.ID != legacyAgent && gate(cfg, a.ID) != "", !cfg.Sync.Skills.Enabled, privacy.MatchName(name, cfg.Sync.Skills.Exclude):
			continue
		}
		out = append(out, classifyCollect(act, linkBack))
	}
	return out, nil
}

// classifyCollect decides what to do with an agent's own skill, given the
// shared directory's current state.
func classifyCollect(act Action, linkBack bool) Action {
	st, err := os.Lstat(act.Source)
	shared := act.Source
	switch {
	case os.IsNotExist(err):
		return with(act, Collect, "只在 "+act.Agent+" 中，收入公共目录")
	case err != nil:
		return with(act, Conflict, err.Error())
	case st.Mode()&os.ModeSymlink != 0:
		resolved, err := filepath.EvalSymlinks(act.Source)
		if err != nil {
			return with(act, Conflict, "公共目录里的同名项是失效的软链接")
		}
		if real, err := filepath.EvalSymlinks(act.Target); err == nil && resolved == real {
			return with(act, Collect, "公共目录里只是指向它的软链接，收入公共目录")
		}
		// A link into another agent's directory, collected in the same run:
		// compare with what it points at.
		shared = resolved
	case !st.IsDir():
		return with(act, Conflict, "公共目录里有同名文件")
	}
	switch {
	case sameTree(shared, act.Target):
		if linkBack {
			return with(act, Relink, "内容与公共目录相同，替换为软链接")
		}
		return with(act, Dedupe, "与公共目录内容相同，删除重复的副本")
	}
	return with(act, Conflict, "与公共目录中的同名 skill 内容不同；合回源以 "+act.Agent+" 的为准，或以源为准覆盖")
}

// planLegacy collects skills from the shared directory of earlier versions.
func planLegacy(in Input) []Action {
	if !in.Config.Sync.Skills.Enabled {
		return nil
	}
	acts, _ := planCollect(in, Agent{ID: legacyAgent}, legacySkills(), SharedSkills(), false)
	return acts
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(l, target)
		case info.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

// moveDir renames, falling back to copy and remove across file systems.
func moveDir(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyTree(src, dst); err != nil {
		_ = os.RemoveAll(dst)
		return err
	}
	return os.RemoveAll(src)
}

// backupDir keeps a copy of a skill before it is replaced or removed.
func backupDir(path string) (string, error) {
	dst := filepath.Join(fsutil.BackupDir(), "skills", fmt.Sprintf("%s-%s", filepath.Base(path), fsutil.NewUUID()))
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", err
	}
	return dst, copyTree(path, dst)
}

// collect moves an agent's own skill into the shared directory, rechecking
// the shared side first: another agent may have collected the same name in
// this run.
func collect(a Action, j *journal) (Action, error) {
	linkBack := a.Format == kit.FormatSymlink
	if again := classifyCollect(a, linkBack); again.Kind != Collect {
		return again, nil
	}
	if st, err := os.Lstat(a.Source); err == nil && st.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(a.Source); err != nil { // the old link pointed at a.Target
			return a, err
		}
	}
	if err := moveDir(a.Target, a.Source); err != nil {
		return a, err
	}
	if linkBack {
		if err := os.Symlink(a.Source, a.Target); err != nil {
			return a, err
		}
		j.add(entry{Path: a.Target, Kind: "collected", Link: a.Source})
	}
	return a, nil
}

// dedupe removes an agent's copy identical to the shared skill (kept as a
// backup); the agent reads the shared one.
func dedupe(a Action) error {
	if _, err := backupDir(a.Target); err != nil {
		return err
	}
	return os.RemoveAll(a.Target)
}

// adoptSkill makes the agent's version of a skill the shared one. The shared
// version it replaces is backed up.
func adoptSkill(a Action, j *journal) error {
	st, err := os.Lstat(a.Target)
	if err != nil || st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
		return errors.New("no own copy of the skill to adopt")
	}
	if _, err := os.Lstat(a.Source); err == nil {
		if _, err := backupDir(a.Source); err != nil {
			return err
		}
		if err := os.RemoveAll(a.Source); err != nil {
			return err
		}
	}
	a.Kind = Collect
	_, err = collect(a, j)
	return err
}

// restoreCollected gives an agent that read its own directory a real copy of
// a collected skill again, for uninstall.
func restoreCollected(e entry) (string, error) {
	st, err := os.Lstat(e.Path)
	if err == nil {
		if st.Mode()&os.ModeSymlink == 0 {
			return "保留（已不是软链接）：" + e.Path, nil
		}
		if l, err := os.Readlink(e.Path); err != nil || l != e.Link {
			return "保留（软链接已被改指向）：" + e.Path, nil
		}
		if err := os.Remove(e.Path); err != nil {
			return "", err
		}
	}
	if _, err := os.Stat(e.Link); err != nil {
		return "公共目录里已没有这个 skill，无法还原：" + e.Path, nil
	}
	return "", copyTree(e.Link, e.Path)
}
