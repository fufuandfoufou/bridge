// Package privacy implements redaction and project exclusion. Redaction runs
// before anything reaches SQLite, so the database never stores plain secrets.
package privacy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"bridge/internal/fsutil"
	"bridge/internal/model"
)

type rule struct {
	name string
	re   *regexp.Regexp
	// keep, when non-empty, is a replacement template prefix (e.g. "${1}=").
	keep string
}

// Most specific rules come first so they name the secret type accurately.
var builtin = []rule{
	{name: "private_key", re: regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`)},
	{name: "anthropic_key", re: regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{20,}`)},
	{name: "openai_key", re: regexp.MustCompile(`sk-(?:proj-|svcacct-)?[A-Za-z0-9_\-]{20,}`)},
	{name: "github_token", re: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`)},
	{name: "aws_access_key", re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{name: "google_api_key", re: regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{30,}`)},
	{name: "slack_token", re: regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9\-]{10,}`)},
	{name: "jwt", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{8,}\.eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`)},
	{name: "env", keep: "${1}=", re: regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*(?i:secret|token|passwd|password|api_?key|access_?key|private_?key|credential)[A-Za-z0-9_]*)[ \t]*=[ \t]*["']?[^\s"']+["']?`)},
}

// Redactor applies built-in and user-supplied rules.
type Redactor struct{ rules []rule }

// NewRedactor compiles user patterns on top of the built-in rules.
func NewRedactor(patterns []string) (*Redactor, error) {
	r := &Redactor{rules: append([]rule{}, builtin...)}
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("invalid redact pattern %q: %w", p, err)
		}
		r.rules = append(r.rules, rule{name: "custom", re: re})
	}
	return r, nil
}

// String redacts s, replacing each secret with [REDACTED:<type>].
func (r *Redactor) String(s string) string {
	for _, ru := range r.rules {
		repl := "[REDACTED:" + ru.name + "]"
		if ru.keep != "" {
			repl = ru.keep + repl
		}
		s = ru.re.ReplaceAllString(s, repl)
	}
	return s
}

// JSON redacts every string in a JSON document while keeping it valid. Bytes
// that are not valid JSON are treated as opaque text and redacted as a whole,
// then encoded as a JSON string, so the result is always valid JSON.
func (r *Redactor) JSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		out, _ := json.Marshal(r.String(string(raw)))
		return out
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r.walk(v)); err != nil {
		return json.RawMessage(`null`)
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n"))
}

func (r *Redactor) walk(v any) any {
	switch x := v.(type) {
	case string:
		return r.String(x)
	case []any:
		for i := range x {
			x[i] = r.walk(x[i])
		}
	case map[string]any:
		// Secrets also appear as key/value pairs whose value alone looks benign.
		for k, val := range x {
			if s, ok := val.(string); ok && s != "" && sensitiveKey.MatchString(k) {
				x[k] = "[REDACTED:env]"
				continue
			}
			x[k] = r.walk(val)
		}
	}
	return v
}

var sensitiveKey = regexp.MustCompile(`(?i)^(api[_-]?key|.*secret|.*password|passwd|access[_-]?token|auth[_-]?token|private[_-]?key)$`)

// Event redacts all text-bearing fields of an event and returns the copy.
func (r *Redactor) Event(e model.Event) model.Event {
	if len(e.Content) > 0 {
		c := make([]model.ContentPart, len(e.Content))
		copy(c, e.Content)
		for i := range c {
			c[i].Text = r.String(c[i].Text)
		}
		e.Content = c
	}
	if e.Tool != nil {
		t := *e.Tool
		t.Args = r.JSON(t.Args)
		t.Result = r.String(t.Result)
		e.Tool = &t
	}
	if e.File != nil {
		f := *e.File
		f.Diff = r.String(f.Diff)
		e.File = &f
	}
	e.NativeRef = r.JSON(e.NativeRef)
	return e
}

// Events redacts a slice of events.
func (r *Redactor) Events(es []model.Event) []model.Event {
	out := make([]model.Event, len(es))
	for i, e := range es {
		out[i] = r.Event(e)
	}
	return out
}

// Matcher decides whether a project path takes part in syncing.
type Matcher struct{ include, exclude []*regexp.Regexp }

// NewMatcher compiles include and exclude globs. ** crosses directories, *
// and ? stay within one path component, and a leading ~ means home.
func NewMatcher(include, exclude []string) (*Matcher, error) {
	m := &Matcher{}
	var err error
	if m.include, err = compileAll(include); err != nil {
		return nil, err
	}
	if m.exclude, err = compileAll(exclude); err != nil {
		return nil, err
	}
	return m, nil
}

func compileAll(globs []string) ([]*regexp.Regexp, error) {
	var out []*regexp.Regexp
	for _, g := range globs {
		re, err := GlobRegexp(fsutil.Expand(g))
		if err != nil {
			return nil, fmt.Errorf("invalid glob %q: %w", g, err)
		}
		out = append(out, re)
		// Also match the resolved home, since paths are compared resolved too.
		home, expanded := fsutil.Home(), fsutil.Expand(g)
		if real, err := filepath.EvalSymlinks(home); err == nil && real != home && strings.HasPrefix(expanded, home) {
			re, err := GlobRegexp(real + strings.TrimPrefix(expanded, home))
			if err != nil {
				return nil, fmt.Errorf("invalid glob %q: %w", g, err)
			}
			out = append(out, re)
		}
	}
	return out, nil
}

// GlobRegexp translates a glob to an anchored regular expression. A trailing
// /** also matches the directory itself.
func GlobRegexp(g string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	if strings.HasSuffix(g, "/**") {
		translate(&b, strings.TrimSuffix(g, "/**"))
		b.WriteString("(?:/.*)?$")
	} else {
		translate(&b, g)
		b.WriteString("$")
	}
	return regexp.Compile(b.String())
}

func translate(b *strings.Builder, g string) {
	for i := 0; i < len(g); i++ {
		switch c := g[i]; {
		case strings.HasPrefix(g[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(g[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
}

// Excluded reports whether path must not be collected. Exclusion wins over
// inclusion; with an include list, anything unlisted is excluded.
func (m *Matcher) Excluded(path string) bool {
	if path == "" {
		return false
	}
	p := filepath.Clean(fsutil.Expand(path))
	candidates := []string{p}
	// A symlink into an excluded tree must not get around the exclusion.
	if real, err := filepath.EvalSymlinks(p); err == nil && real != p {
		candidates = append(candidates, real)
	}
	for _, c := range candidates {
		for _, re := range m.exclude {
			if re.MatchString(c) {
				return true
			}
		}
	}
	if len(m.include) == 0 {
		return false
	}
	for _, re := range m.include {
		if re.MatchString(candidates[len(candidates)-1]) {
			return false
		}
	}
	return true
}

// MatchName reports whether name matches any glob, for skill and file excludes.
func MatchName(name string, globs []string) bool {
	for _, g := range globs {
		if ok, err := filepath.Match(g, name); err == nil && ok {
			return true
		}
	}
	return false
}
