// Package tomledit changes whole tables in a TOML file as text, so comments
// and formatting elsewhere in the file stay byte-for-byte intact.
package tomledit

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"
)

// headerKey splits a table header line such as `[a."b c".d]` into its key
// path, or returns nil when the line is not a header.
func headerKey(line string) []string {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "[") {
		return nil
	}
	s = strings.TrimPrefix(strings.TrimPrefix(s, "["), "[")
	var parts []string
	var cur strings.Builder
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' && i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
			} else if c == quote {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '.':
			parts = append(parts, strings.TrimSpace(cur.String()))
			cur.Reset()
		case c == ']':
			return append(parts, strings.TrimSpace(cur.String()))
		default:
			cur.WriteByte(c)
		}
	}
	return nil
}

func hasPrefix(key, prefix []string) bool {
	if len(key) < len(prefix) {
		return false
	}
	for i := range prefix {
		if key[i] != prefix[i] {
			return false
		}
	}
	return true
}

// RemoveTables deletes the table at prefix and all of its subtables. Comment
// lines directly above a removed header go with it.
func RemoveTables(text string, prefix ...string) string {
	lines := strings.SplitAfter(text, "\n")
	var out []string
	removing := false
	for _, l := range lines {
		if k := headerKey(l); k != nil {
			removing = hasPrefix(k, prefix)
			if removing {
				// Drop the comment block attached to this header.
				for len(out) > 0 && strings.HasPrefix(strings.TrimSpace(out[len(out)-1]), "#") &&
					!strings.HasPrefix(strings.TrimSpace(out[len(out)-1]), "# bridge:") {
					out = out[:len(out)-1]
				}
			}
		}
		if !removing {
			out = append(out, l)
		}
	}
	return strings.Join(out, "")
}

// Encode renders value as tables under the key path, e.g. Encode(v,
// "mcp_servers", "x") yields `[mcp_servers.x]` and its subtables.
func Encode(value map[string]any, path ...string) (string, error) {
	var root any = value
	for i := len(path) - 1; i >= 0; i-- {
		root = map[string]any{path[i]: root}
	}
	var b bytes.Buffer
	enc := toml.NewEncoder(&b)
	enc.Indent = ""
	if err := enc.Encode(root); err != nil {
		return "", err
	}
	// The encoder emits empty parent headers such as `[mcp_servers]`; they
	// would clash with a parent table defined elsewhere in the file.
	var out []string
	for _, l := range strings.Split(b.String(), "\n") {
		if k := headerKey(l); k != nil && len(k) < len(path) {
			continue
		}
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n")) + "\n", nil
}

// ReplaceBlock replaces the text between the begin and end marker lines, or
// appends a new marked block. Empty content removes the block.
func ReplaceBlock(text, begin, end, content string) string {
	if i := strings.Index(text, begin); i >= 0 {
		if j := strings.Index(text[i:], end); j >= 0 {
			text = strings.TrimRight(text[:i], "\n") + "\n" + strings.TrimLeft(text[i+j+len(end):], "\n")
		}
	}
	if strings.TrimSpace(text) == "" {
		text = ""
	}
	if strings.TrimSpace(content) == "" {
		return text
	}
	if text != "" {
		text = strings.TrimRight(text, "\n") + "\n\n"
	}
	return text + begin + "\n" + strings.TrimSpace(content) + "\n" + end + "\n"
}

// Block returns the content between the markers, or "".
func Block(text, begin, end string) string {
	i := strings.Index(text, begin)
	if i < 0 {
		return ""
	}
	j := strings.Index(text[i:], end)
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(text[i+len(begin) : i+j])
}

// Check parses text, so a broken edit is caught before it is written.
func Check(text string) error {
	var v map[string]any
	if _, err := toml.Decode(text, &v); err != nil {
		return fmt.Errorf("edit would leave invalid TOML: %w", err)
	}
	return nil
}
