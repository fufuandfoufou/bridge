package common

import (
	"fmt"
	"sort"

	kit "bridge/pkg/adapterkit"
)

// MCPFields maps the neutral fields of kit.MCPServer to an agent's native
// key names; an empty name means the agent has no such field.
type MCPFields struct {
	Command, Args, Env, Cwd, URL, Headers string
}

func strMap(v any) (map[string]string, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expected a table, got %T", v)
	}
	out := map[string]string{}
	for k, x := range m {
		s, ok := x.(string)
		if !ok {
			return nil, fmt.Errorf("%s: expected a string, got %T", k, x)
		}
		out[k] = s
	}
	return out, nil
}

func strSlice(v any) ([]string, error) {
	switch a := v.(type) {
	case []string:
		return a, nil
	case []any:
		out := make([]string, 0, len(a))
		for _, x := range a {
			s, ok := x.(string)
			if !ok {
				return nil, fmt.Errorf("expected strings, got %T", x)
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, fmt.Errorf("expected an array, got %T", v)
}

// MCPFromNative converts a native entry. Keys outside f, other than those in
// skip, are kept in Extra.
func MCPFromNative(m map[string]any, f MCPFields, skip ...string) (kit.MCPServer, error) {
	var s kit.MCPServer
	skipped := map[string]bool{}
	for _, k := range skip {
		skipped[k] = true
	}
	for k, v := range m {
		var err error
		switch {
		case skipped[k]:
		case k == f.Command && f.Command != "":
			s.Command, _ = v.(string)
		case k == f.Args && f.Args != "":
			s.Args, err = strSlice(v)
		case k == f.Env && f.Env != "":
			s.Env, err = strMap(v)
		case k == f.Cwd && f.Cwd != "":
			s.Cwd, _ = v.(string)
		case k == f.URL && f.URL != "":
			s.URL, _ = v.(string)
		case k == f.Headers && f.Headers != "":
			s.Headers, err = strMap(v)
		default:
			if s.Extra == nil {
				s.Extra = map[string]any{}
			}
			s.Extra[k] = v
		}
		if err != nil {
			return s, fmt.Errorf("%s: %w", k, err)
		}
	}
	if len(s.Env) == 0 {
		s.Env = nil
	}
	if len(s.Headers) == 0 {
		s.Headers = nil
	}
	if len(s.Args) == 0 {
		s.Args = nil
	}
	return s, nil
}

// MCPToNative is the inverse of MCPFromNative.
func MCPToNative(s kit.MCPServer, f MCPFields) map[string]any {
	m := map[string]any{}
	for k, v := range s.Extra {
		m[k] = v
	}
	set := func(k string, v any, empty bool) {
		if k != "" && !empty {
			m[k] = v
		}
	}
	set(f.Command, s.Command, s.Command == "")
	set(f.Args, s.Args, len(s.Args) == 0)
	set(f.Env, s.Env, len(s.Env) == 0)
	set(f.Cwd, s.Cwd, s.Cwd == "")
	set(f.URL, s.URL, s.URL == "")
	set(f.Headers, s.Headers, len(s.Headers) == 0)
	return m
}

// SortedNames returns the keys of a server set in order.
func SortedNames(m map[string]kit.MCPServer) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
