package tomledit

import (
	"strings"
	"testing"
)

const sample = `# top comment
model = "x" # keep me

# the rea server
[mcp_servers.rea]
command = "rea"

[mcp_servers.rea.env]
A = "1"

[mcp_servers."my.server"]
command = "m"

[profiles.fast]
model = "y"
`

func TestRemoveTables(t *testing.T) {
	got := RemoveTables(sample, "mcp_servers", "rea")
	if strings.Contains(got, "rea") || strings.Contains(got, "A = ") {
		t.Fatalf("rea left behind:\n%s", got)
	}
	for _, keep := range []string{"# top comment", `model = "x" # keep me`, `[mcp_servers."my.server"]`, "[profiles.fast]"} {
		if !strings.Contains(got, keep) {
			t.Errorf("lost %q:\n%s", keep, got)
		}
	}
	got = RemoveTables(sample, "mcp_servers", "my.server")
	if strings.Contains(got, "my.server") || !strings.Contains(got, "[mcp_servers.rea.env]") {
		t.Fatalf("quoted key:\n%s", got)
	}
	if err := Check(got); err != nil {
		t.Fatal(err)
	}
}

func TestEncodeAndBlock(t *testing.T) {
	out, err := Encode(map[string]any{"command": "n", "env": map[string]string{"K": "v"}}, "mcp_servers", "a b")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "[mcp_servers]\n") || !strings.Contains(out, `[mcp_servers."a b"]`) {
		t.Fatalf("encode:\n%s", out)
	}
	text := ReplaceBlock(sample, "# b", "# e", out)
	if err := Check(text); err != nil {
		t.Fatal(err)
	}
	if Block(text, "# b", "# e") != strings.TrimSpace(out) {
		t.Fatal("block round trip")
	}
	if ReplaceBlock(text, "# b", "# e", "") != strings.TrimRight(sample, "\n")+"\n" {
		t.Fatalf("block removal:\n%q", ReplaceBlock(text, "# b", "# e", ""))
	}
}
