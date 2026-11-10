package dsh

import (
	"strings"
	"testing"
)

func TestPatchBlockEditing(t *testing.T) {
	block := renderBlock("/x/bridge", true)
	user := "# mine\n- id: session-log-deepseek\n  disabled: true\n"
	cases := []struct{ name, in, want string }{
		{"missing", "", block},
		{"empty list", "[]\n", block},
		{"comments and empty list", "# keep me\n[]\n", "# keep me\n" + block},
		{"user rows", user, user + block},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := withBlock(c.in, block)
			if err != nil || got != c.want {
				t.Fatalf("withBlock:\n%q\nwant\n%q (%v)", got, c.want, err)
			}
			again, _ := withBlock(got, block)
			if again != got {
				t.Error("not idempotent")
			}
			back := withoutBlock(got)
			if strings.Contains(back, "bridge") {
				t.Errorf("block left: %q", back)
			}
			if empty, hasList := onlyEmptyList(back); empty && !hasList {
				t.Errorf("an emptied patch must keep [] or dsh fails to boot: %q", back)
			}
			if c.in == user && back != user {
				t.Errorf("user rows changed: %q", back)
			}
		})
	}
	if _, err := withBlock("[{id: x, disabled: true}]\n", block); err == nil {
		t.Error("a flow-style list must be refused, not edited")
	}
	if strings.Contains(renderBlock("/x/bridge", false), "bridge-live") {
		t.Error("plugin row written for an unverified version")
	}
}

func TestPluginEmbedsBridgePath(t *testing.T) {
	src := renderPlugin(`/Applications/Bridge.app/Contents/Resources/bri"dge`)
	if !strings.Contains(src, `const BRIDGE = "/Applications/Bridge.app/Contents/Resources/bri\"dge"`) {
		t.Fatalf("path not quoted safely:\n%s", src)
	}
}
