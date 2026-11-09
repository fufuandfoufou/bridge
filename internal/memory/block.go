package memory

import (
	"fmt"
	"regexp"
	"strings"
)

// Block names.
const (
	BlockGlobal  = "global-preferences"
	BlockProject = "project-memory"
)

const blockNote = "(自动生成，请用 bridge memory 命令修改)"

func beginMarker(name string) string { return "<!-- bridge:begin " + name + " " + blockNote + " -->" }

const endMarker = "<!-- bridge:end -->"

var anyBlockRe = regexp.MustCompile(`(?s)\n?<!-- bridge:begin [^\n]*?-->.*?<!-- bridge:end -->\n?`)

func blockRe(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?s)<!-- bridge:begin ` + regexp.QuoteMeta(name) + `\b[^\n]*?-->.*?<!-- bridge:end -->`)
}

// UpsertBlock replaces the named block's body, or appends the block when it
// does not exist yet. Text outside the block is never touched. With no lines
// the block is removed.
func UpsertBlock(doc, name string, lines []string) string {
	if CheckBlocks(doc) != nil {
		return doc
	}
	if len(lines) == 0 {
		return RemoveBlock(doc, name)
	}
	block := beginMarker(name) + "\n" + strings.Join(lines, "\n") + "\n" + endMarker
	re := blockRe(name)
	if re.MatchString(doc) {
		return re.ReplaceAllLiteralString(doc, block)
	}
	if doc == "" {
		return block + "\n"
	}
	return strings.TrimRight(doc, "\n") + "\n\n" + block + "\n"
}

// CheckBlocks reports damaged markers: begin and end must alternate. Editing
// a damaged file could destroy text, since the block's extent is unknown.
func CheckBlocks(doc string) error {
	open := false
	for _, m := range markerRe.FindAllString(doc, -1) {
		begin := m == "<!-- bridge:begin"
		if begin == open {
			return fmt.Errorf("damaged bridge block markers")
		}
		open = begin
	}
	if open {
		return fmt.Errorf("damaged bridge block markers: block is not closed")
	}
	return nil
}

var markerRe = regexp.MustCompile(`<!-- bridge:begin|<!-- bridge:end`)

// RemoveBlock deletes the named block and the blank line that set it apart.
func RemoveBlock(doc, name string) string {
	loc := blockRe(name).FindStringIndex(doc)
	if loc == nil {
		return doc
	}
	before, after := strings.TrimRight(doc[:loc[0]], "\n"), strings.TrimLeft(doc[loc[1]:], "\n")
	switch {
	case before == "" && after == "":
		return ""
	case before == "":
		return after
	case after == "":
		return before + "\n"
	}
	return before + "\n\n" + after
}

// StripBlocks returns doc with every managed block removed.
func StripBlocks(doc string) string { return anyBlockRe.ReplaceAllString(doc, "\n") }

// Blocks returns the managed blocks found in doc, in order.
func Blocks(doc string) []string {
	var out []string
	for _, b := range anyBlockRe.FindAllString(doc, -1) {
		out = append(out, strings.Trim(b, "\n"))
	}
	return out
}
