// Package filesync keeps rule files and skills consistent across agents. Each
// has one source; other locations are symlinks when the format matches and
// generated copies when it does not.
package filesync

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"bridge/internal/config"
	"bridge/internal/fsutil"
	"bridge/internal/memory"
	"bridge/internal/privacy"
	kit "bridge/pkg/adapterkit"
)

// Action kinds.
const (
	Noop       = "noop"        // already correct
	Skip       = "skip"        // not synced, Reason says why
	Link       = "link"        // create a symlink
	Relink     = "relink"      // replace an identical regular file by a symlink
	Generate   = "generate"    // write a generated copy
	Drift      = "drift"       // generated copy edited by hand
	Conflict   = "conflict"    // pre-existing different content
	NeedSource = "need_source" // replicas exist but no source yet
)

// Action is one planned synchronization step.
type Action struct {
	Kind     string `json:"kind"`
	Category string `json:"category"` // rule | skill
	Agent    string `json:"agent"`
	Source   string `json:"source"`
	Target   string `json:"target"`
	Scope    string `json:"scope"`
	Reason   string `json:"reason"`
	Format   string `json:"format"`
}

// Agent pairs an adapter ID with its capabilities.
type Agent struct {
	ID   string
	Caps kit.Capabilities
}

// Input describes what to plan.
type Input struct {
	ProjectRoot string // empty plans only global scope
	Agents      []Agent
	Config      *config.Config
	GlobalDir   string // defaults to ~/.config/bridge
}

// Plan computes the actions without touching the file system. It covers every
// enabled-or-not agent so `why` can explain what was skipped.
func Plan(in Input) ([]Action, error) {
	var out []Action
	gd := in.GlobalDir
	if gd == "" {
		gd = fsutil.ConfigDir()
	}
	cfg := in.Config
	scopes := []string{"global"}
	if in.ProjectRoot != "" {
		scopes = append(scopes, "project")
	}
	for _, a := range in.Agents {
		for _, sc := range scopes {
			srcRule := filepath.Join(gd, "AGENTS.md")
			srcSkills := filepath.Join(gd, "skills")
			if sc == "project" {
				srcRule = filepath.Join(in.ProjectRoot, cfg.Sync.RulesSource)
				srcSkills = filepath.Join(in.ProjectRoot, ".agents", "skills")
			}
			for _, spec := range a.Caps.RulesFiles {
				if spec.Scope != sc {
					continue
				}
				out = append(out, planRule(in, a, spec, srcRule))
			}
			for _, spec := range a.Caps.SkillsDirs {
				if spec.Scope != sc {
					continue
				}
				acts, err := planSkills(in, a, spec, srcSkills)
				if err != nil {
					return nil, err
				}
				out = append(out, acts...)
			}
		}
	}
	return out, nil
}

func resolve(root string, spec kit.PathSpec) string {
	if spec.Scope == "project" && !strings.HasPrefix(spec.Path, "~") {
		return filepath.Join(root, spec.Path)
	}
	return fsutil.Expand(spec.Path)
}

func gate(cfg *config.Config, agent string) string {
	if !cfg.AgentEnabled(agent) {
		return "agent 未启用（agents.enabled）"
	}
	if !cfg.CanReceive(agent) {
		return "agent 同步方向为 " + cfg.Direction(agent) + "，不接收同步"
	}
	return ""
}

func planRule(in Input, a Agent, spec kit.PathSpec, src string) Action {
	target := resolve(in.ProjectRoot, spec)
	act := Action{Category: "rule", Agent: a.ID, Source: src, Target: target, Scope: spec.Scope, Format: spec.Format}
	cfg := in.Config
	if r := gate(cfg, a.ID); r != "" {
		return with(act, Skip, r)
	}
	if !cfg.Sync.Rules {
		return with(act, Skip, "规则同步已关闭（sync.rules）")
	}
	if privacy.MatchName(filepath.Base(target), cfg.Sync.ExcludeFiles) {
		return with(act, Skip, "被 sync.exclude_files 排除")
	}
	if filepath.Clean(target) == filepath.Clean(src) {
		return with(act, Noop, "这个文件就是规则源")
	}
	if _, err := os.Stat(src); err != nil {
		if _, e := os.Lstat(target); e == nil {
			return with(act, NeedSource, "规则源不存在，但这里已有内容；可用 bridge sync --adopt "+a.ID+" 把它作为源")
		}
		return with(act, Skip, "规则源不存在")
	}
	if !knownFormat(spec.Format) {
		return with(act, Skip, "格式未经验证（"+orNone(spec.Format)+"），不同步")
	}
	if spec.Format == kit.FormatGenerate {
		return planGenerated(act, src, target)
	}
	return planLink(act, src, target)
}

// knownFormat rejects undeclared formats: matching file names do not prove
// that two agents read a file the same way.
func knownFormat(f string) bool {
	return f == kit.FormatSymlink || f == kit.FormatGenerate || f == kit.FormatSource
}

func orNone(s string) string {
	if s == "" {
		return "未声明"
	}
	return s
}

func with(a Action, kind, reason string) Action { a.Kind, a.Reason = kind, reason; return a }

func planLink(act Action, src, target string) Action {
	act.Format = kit.FormatSymlink
	st, err := os.Lstat(target)
	if os.IsNotExist(err) {
		return with(act, Link, "副本不存在，创建软链接")
	}
	if err != nil {
		return with(act, Conflict, err.Error())
	}
	if st.Mode()&os.ModeSymlink != 0 {
		if resolved, err := filepath.EvalSymlinks(target); err == nil {
			if real, e := filepath.EvalSymlinks(src); e == nil && resolved == real {
				return with(act, Noop, "软链接已指向规则源")
			}
		}
		return with(act, Conflict, "已有指向别处的软链接")
	}
	if st.IsDir() {
		if sameTree(src, target) {
			return with(act, Relink, "内容与源相同，替换为软链接")
		}
		return with(act, Conflict, "已有内容不同的目录")
	}
	a, _ := fsutil.HashFile(src)
	b, _ := fsutil.HashFile(target)
	if a != "" && a == b {
		return with(act, Relink, "内容与源相同，替换为软链接")
	}
	return with(act, Conflict, "已有内容不同的文件，不自动覆盖")
}

func sameTree(a, b string) bool {
	x, y := treeHash(a), treeHash(b)
	return x != "" && x == y
}

func treeHash(root string) string {
	var lines []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		h, err := fsutil.HashFile(p)
		lines = append(lines, rel+":"+h)
		return err
	})
	if err != nil {
		return ""
	}
	sort.Strings(lines)
	return fsutil.HashString(strings.Join(lines, "\n"))
}

var headerRe = regexp.MustCompile(`^<!-- bridge:generated sha256=([0-9a-f]{64}) source=([^\n]*?) \(自动生成，请修改源文件\) -->\n`)

func header(hash, src string) string {
	return "<!-- bridge:generated sha256=" + hash + " source=" + src + " (自动生成，请修改源文件) -->\n"
}

// parseGenerated splits a generated file into recorded hash and hand-editable
// body (no header, no managed blocks). ok is false if it has no header.
func parseGenerated(data string) (hash, body string, ok bool) {
	m := headerRe.FindStringSubmatch(data)
	if m == nil {
		return "", "", false
	}
	return m[1], memory.StripBlocks(data[len(m[0]):]), true
}

func normalized(s string) string { return strings.TrimSpace(s) }

func planGenerated(act Action, src, target string) Action {
	srcBytes, err := os.ReadFile(src)
	if err != nil {
		return with(act, Conflict, err.Error())
	}
	want := fsutil.HashString(normalized(string(srcBytes)))
	data, err := os.ReadFile(target)
	if os.IsNotExist(err) {
		return with(act, Generate, "副本不存在，生成副本")
	}
	if err != nil {
		return with(act, Conflict, err.Error())
	}
	hash, body, ok := parseGenerated(string(data))
	if !ok {
		// A file holding nothing but our managed blocks is safe to adopt.
		if strings.TrimSpace(memory.StripBlocks(string(data))) == "" {
			return with(act, Generate, "副本只有托管区块，生成副本")
		}
		return with(act, Conflict, "已有用户手写的内容，不自动覆盖")
	}
	if fsutil.HashString(normalized(body)) != hash {
		return with(act, Drift, "副本被手工修改（hash 不符）；用 --adopt 合回源，或 --force 以源为准")
	}
	if hash != want {
		return with(act, Generate, "规则源已变化，重新生成")
	}
	return with(act, Noop, "副本与源一致")
}

// escapes reports whether p resolves outside dir.
func escapes(p, dir string) bool {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return true
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return true
	}
	return real != root && !strings.HasPrefix(real, root+string(filepath.Separator))
}

func planSkills(in Input, a Agent, spec kit.PathSpec, srcDir string) ([]Action, error) {
	cfg := in.Config
	base := resolve(in.ProjectRoot, spec)
	if !knownFormat(spec.Format) {
		return []Action{{Kind: Skip, Category: "skill", Agent: a.ID, Source: srcDir, Target: base, Scope: spec.Scope, Format: spec.Format,
			Reason: "格式未经验证（" + orNone(spec.Format) + "），不同步"}}, nil
	}
	if spec.Format == kit.FormatSource && filepath.Clean(base) == filepath.Clean(srcDir) {
		return nil, nil // this agent reads the source directory directly
	}
	mk := func(name string) Action {
		return Action{Category: "skill", Agent: a.ID, Source: filepath.Join(srcDir, name),
			Target: filepath.Join(base, name), Scope: spec.Scope, Format: kit.FormatSymlink}
	}
	entries, err := os.ReadDir(srcDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Action
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if _, err := os.Stat(filepath.Join(srcDir, name, "SKILL.md")); err != nil {
			continue
		}
		act := mk(name)
		if escapes(act.Source, srcDir) {
			out = append(out, with(act, Skip, "skill 源是指向源目录之外的软链接，拒绝同步"))
			continue
		}
		switch r := gate(cfg, a.ID); {
		case r != "":
			out = append(out, with(act, Skip, r))
		case !cfg.Sync.Skills.Enabled:
			out = append(out, with(act, Skip, "skill 同步已关闭（sync.skills）"))
		case privacy.MatchName(name, cfg.Sync.Skills.Exclude):
			out = append(out, with(act, Skip, "被 sync.skills.exclude 排除"))
		default:
			out = append(out, planLink(act, act.Source, act.Target))
		}
	}
	return out, nil
}

// Options controls Apply.
type Options struct {
	Force bool   // conflicts and drift yield to the source
	Adopt string // agent whose replica content flows back to the source
}

// Report summarizes an Apply.
type Report struct {
	Applied []Action
	Blocked []Action // conflicts and drift left alone
}

// Apply executes a plan. Files are backed up before replacement and every
// change is journaled so `bridge uninstall` can restore it.
func Apply(actions []Action, o Options) (Report, error) {
	var r Report
	j, err := loadJournal()
	if err != nil {
		return r, err
	}
	defer j.save()
	// Adoption updates sources first so that the links created next see them.
	if o.Adopt != "" {
		for i, a := range actions {
			if a.Agent != o.Adopt || (a.Kind != Drift && a.Kind != NeedSource && a.Kind != Conflict) {
				continue
			}
			if a.Kind == Drift && sourceMoved(a) {
				actions[i].Kind = Conflict
				actions[i].Reason = "规则源和副本都被修改过，两边都已保留；请手工合并后再同步"
				continue
			}
			if err := adopt(a); err != nil {
				return r, fmt.Errorf("adopt %s: %w", a.Target, err)
			}
			if a.Format == kit.FormatGenerate || a.Kind == Drift {
				actions[i].Kind = Generate
			} else {
				actions[i].Kind = forceLink(a)
			}
			actions[i].Reason = "已把副本内容合回规则源"
		}
	}
	for _, a := range actions {
		kind := a.Kind
		if o.Force && (kind == Drift || kind == Conflict) {
			kind = Generate
			if a.Format != kit.FormatGenerate {
				kind = Relink
			}
		}
		switch kind {
		case Noop, Skip:
			continue
		case Drift, Conflict, NeedSource:
			r.Blocked = append(r.Blocked, a)
			continue
		case Link:
			if err := createLink(a, j); err != nil {
				return r, err
			}
		case Relink:
			if err := replaceWithLink(a, j); err != nil {
				return r, err
			}
		case Generate:
			if err := writeGenerated(a, j); err != nil {
				return r, err
			}
		}
		r.Applied = append(r.Applied, a)
	}
	return r, nil
}

// sourceMoved reports whether the source changed since the replica was made.
func sourceMoved(a Action) bool {
	data, err := os.ReadFile(a.Target)
	if err != nil {
		return true
	}
	hash, _, ok := parseGenerated(string(data))
	if !ok {
		return true
	}
	src, err := os.ReadFile(a.Source)
	return err != nil || fsutil.HashString(normalized(string(src))) != hash
}

func forceLink(a Action) string { return Relink }

func adopt(a Action) error {
	if a.Category == "skill" {
		return errors.New("skill directories cannot be adopted automatically")
	}
	data, err := os.ReadFile(a.Target)
	if err != nil {
		return err
	}
	text := string(data)
	if _, body, ok := parseGenerated(text); ok {
		text = body
	} else {
		text = memory.StripBlocks(text)
	}
	return fsutil.SafeWrite(a.Source, []byte(strings.TrimSpace(text)+"\n"), 0o644)
}

func linkDest(a Action) string {
	if a.Scope != "project" {
		return a.Source
	}
	if rel, err := filepath.Rel(filepath.Dir(a.Target), a.Source); err == nil {
		return rel
	}
	return a.Source
}

func createLink(a Action, j *journal) error {
	if err := os.MkdirAll(filepath.Dir(a.Target), 0o755); err != nil {
		return err
	}
	if err := os.Symlink(linkDest(a), a.Target); err != nil {
		return err
	}
	j.add(entry{Path: a.Target, Kind: "symlink", Link: linkDest(a)})
	return nil
}

func replaceWithLink(a Action, j *journal) error {
	st, err := os.Lstat(a.Target)
	if err != nil {
		return err
	}
	var saved string
	if st.IsDir() {
		saved = a.Target + ".bridge-backup"
		if err := os.Rename(a.Target, saved); err != nil {
			return err
		}
	} else if st.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(a.Target); err != nil {
			return err
		}
	} else {
		if saved, err = fsutil.Backup(a.Target); err != nil {
			return err
		}
		if err := os.Remove(a.Target); err != nil {
			return err
		}
	}
	if err := os.Symlink(linkDest(a), a.Target); err != nil {
		return err
	}
	j.add(entry{Path: a.Target, Kind: "symlink", Link: linkDest(a), Backup: saved})
	return nil
}

func writeGenerated(a Action, j *journal) error {
	src, err := os.ReadFile(a.Source)
	if err != nil {
		return err
	}
	norm := normalized(string(src))
	text := header(fsutil.HashString(norm), a.Source) + norm + "\n"
	prev, err := os.ReadFile(a.Target)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, b := range memory.Blocks(string(prev)) {
		text = strings.TrimRight(text, "\n") + "\n\n" + b + "\n"
	}
	var saved string
	if err == nil {
		if saved, err = fsutil.Backup(a.Target); err != nil {
			return err
		}
	}
	if err := fsutil.AtomicWrite(a.Target, []byte(text), 0o644); err != nil {
		return err
	}
	j.add(entry{Path: a.Target, Kind: "generated", Backup: saved})
	return gitignore(a)
}

// gitignore adds a generated project copy to the repository's .gitignore.
func gitignore(a Action) error {
	if a.Scope != "project" {
		return nil
	}
	root := filepath.Dir(a.Source)
	rel, err := filepath.Rel(root, a.Target)
	if err != nil {
		return err
	}
	p := filepath.Join(root, ".gitignore")
	b, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) == rel {
			return nil
		}
	}
	if len(b) > 0 && !bytes.HasSuffix(b, []byte("\n")) {
		b = append(b, '\n')
	}
	return fsutil.SafeWrite(p, append(b, []byte(rel+"\n")...), 0o644)
}

type entry struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"` // symlink | generated
	Backup string `json:"backup,omitempty"`
	Link   string `json:"link,omitempty"` // symlink destination bridge created
}

type journal struct {
	Entries []entry `json:"entries"`
	path    string
}

// JournalPath is the record of everything bridge created.
func JournalPath() string { return filepath.Join(fsutil.DataDir(), "filesync-journal.json") }

func loadJournal() (*journal, error) {
	j := &journal{path: JournalPath()}
	b, err := os.ReadFile(j.path)
	if os.IsNotExist(err) {
		return j, nil
	}
	if err != nil {
		return nil, err
	}
	return j, json.Unmarshal(b, j)
}

// add records a change. The first entry for a path owns the backup: only it
// describes the state before bridge touched the file.
func (j *journal) add(e entry) {
	for i := range j.Entries {
		if j.Entries[i].Path == e.Path {
			e.Backup = j.Entries[i].Backup
			j.Entries[i] = e
			return
		}
	}
	j.Entries = append(j.Entries, e)
}

func (j *journal) save() {
	if err := fsutil.EnsurePrivateDir(filepath.Dir(j.path)); err != nil {
		return
	}
	b, _ := json.MarshalIndent(j, "", "  ")
	_ = fsutil.AtomicWrite(j.path, b, 0o600)
}

// Uninstall removes everything recorded in the journal and restores any
// originals that were replaced. A generated file edited by hand is kept.
func Uninstall() ([]string, error) {
	j, err := loadJournal()
	if err != nil {
		return nil, err
	}
	var notes []string
	for _, e := range j.Entries {
		switch e.Kind {
		case "symlink":
			if st, err := os.Lstat(e.Path); err != nil || st.Mode()&os.ModeSymlink == 0 {
				notes = append(notes, "保留（已不是软链接）："+e.Path)
				continue
			}
			if l, err := os.Readlink(e.Path); err != nil || (e.Link != "" && l != e.Link) {
				notes = append(notes, "保留（软链接已被改指向）："+e.Path)
				continue
			}
			if err := os.Remove(e.Path); err != nil {
				return notes, err
			}
			// Drop directories that held only our links; Remove refuses non-empty ones.
			if d := filepath.Dir(e.Path); os.Remove(d) == nil {
				_ = os.Remove(filepath.Dir(d))
			}
		case "generated":
			data, err := os.ReadFile(e.Path)
			if err != nil {
				continue
			}
			if hash, body, ok := parseGenerated(string(data)); !ok || fsutil.HashString(normalized(body)) != hash {
				notes = append(notes, "保留（已被手工修改）："+e.Path)
				continue
			}
			if err := os.Remove(e.Path); err != nil {
				return notes, err
			}
		}
		if e.Backup == "" {
			continue
		}
		if st, err := os.Stat(e.Backup); err == nil && st.IsDir() {
			if err := os.Rename(e.Backup, e.Path); err != nil {
				return notes, err
			}
			continue
		}
		b, err := os.ReadFile(e.Backup)
		if err != nil {
			notes = append(notes, "备份缺失，无法还原："+e.Path)
			continue
		}
		if err := fsutil.AtomicWrite(e.Path, b, 0o644); err != nil {
			return notes, err
		}
	}
	j.Entries = nil
	j.save()
	return notes, nil
}

// StripManagedBlocks removes bridge's blocks from the given files.
func StripManagedBlocks(paths []string) error {
	for _, p := range paths {
		real := p
		if r, err := filepath.EvalSymlinks(p); err == nil {
			real = r
		}
		b, err := os.ReadFile(real)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		next := memory.RemoveBlock(memory.RemoveBlock(string(b), memory.BlockGlobal), memory.BlockProject)
		if next == string(b) {
			continue
		}
		if strings.TrimSpace(next) == "" {
			if _, err := fsutil.Backup(real); err != nil {
				return err
			}
			if err := os.Remove(real); err != nil {
				return err
			}
			continue
		}
		if err := fsutil.SafeWrite(real, []byte(next), 0o644); err != nil {
			return err
		}
	}
	return nil
}
