package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInjectDefaultsAndLayers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c, err := LoadForDir(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !c.InjectEnabled("claude-code") || !c.InjectEnabled("pi") || c.InjectEnabled("dsh") {
		t.Fatalf("defaults: claude=%v pi=%v dsh=%v", c.InjectEnabled("claude-code"), c.InjectEnabled("pi"), c.InjectEnabled("dsh"))
	}
	// A user table for dsh that only sets the direction keeps the default.
	g := filepath.Join(home, ".config", "bridge", "config.toml")
	_ = os.MkdirAll(filepath.Dir(g), 0o755)
	_ = os.WriteFile(g, []byte("version = 1\n[agents.dsh]\ndirection = \"send-only\"\n"), 0o644)
	if c, err = LoadForDir(t.TempDir(), nil); err != nil || c.InjectEnabled("dsh") || c.Direction("dsh") != "send-only" {
		t.Fatalf("partial table: inject=%v dir=%s err=%v", c.InjectEnabled("dsh"), c.Direction("dsh"), err)
	}
	_ = os.WriteFile(g, []byte("version = 1\n[agents.dsh]\ninject = true\n"), 0o644)
	if c, err = LoadForDir(t.TempDir(), nil); err != nil || !c.InjectEnabled("dsh") {
		t.Fatalf("opt in: %v %v", c.InjectEnabled("dsh"), err)
	}
}
