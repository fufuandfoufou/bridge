package live

import (
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestEditPathsAndClaims(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	input, _ := json.Marshal(map[string]string{"command": "*** Begin Patch\n*** Update File: src/a.go\n*** Move to: src/b.go\n*** End Patch"})
	paths := EditPaths("apply_patch", input, root, root)
	if len(paths) != 2 || paths[0] != filepath.Join(root, "src/a.go") {
		t.Fatalf("paths: %v", paths)
	}
	if got := EditPaths("Bash", input, root, root); len(got) != 0 {
		t.Fatal(got)
	}
	outside, _ := json.Marshal(map[string]string{"file_path": "../outside"})
	if got := EditPaths("Write", outside, root, root); len(got) != 0 {
		t.Fatal(got)
	}
	var c Claims
	if got := c.Acquire("p", "claude", "one", paths, now); len(got) != 0 {
		t.Fatal(got)
	}
	if got := c.Acquire("p", "codex", "two", paths, now); len(got) != 2 || got[0].Agent != "claude" {
		t.Fatal(got)
	}
	c.Release("claude", "one")
	if got := c.Acquire("p", "codex", "two", paths, now); len(got) != 0 {
		t.Fatal(got)
	}
	if len(c.List(now.Add(16*time.Minute))) != 0 {
		t.Fatal("lease did not expire")
	}
}

func TestClaimsConcurrent(t *testing.T) {
	var c Claims
	var wg sync.WaitGroup
	now := time.Now()
	for _, id := range []string{"a", "b"} {
		wg.Add(1)
		go func(id string) { defer wg.Done(); c.Acquire("p", id, id, []string{"/x"}, now) }(id)
	}
	wg.Wait()
	if got := c.List(now); len(got) != 1 {
		t.Fatalf("multiple owners: %v", got)
	}
}
