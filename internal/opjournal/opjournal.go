// Package opjournal durably records in-flight session writes. A write spans
// several resources that cannot share a transaction, so an entry exists from
// before the first write until the whole migration is verified; any entry
// found later describes an interrupted write to recover.
package opjournal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"bridge/internal/fsutil"
)

// Op is one in-flight write. The target ID is minted by bridge, which is what
// establishes that the half-written objects are bridge's to remove.
type Op struct {
	Agent   string    `json:"agent"`
	ID      string    `json:"id"`
	CWD     string    `json:"cwd"`
	Source  string    `json:"source"`
	Started time.Time `json:"started"`
}

func dir() string { return filepath.Join(fsutil.DataDir(), "ops") }

func path(agent, id string) string { return filepath.Join(dir(), agent+"_"+id+".json") }

// Begin persists an operation before its first side effect.
func Begin(op Op) error {
	if err := fsutil.EnsurePrivateDir(dir()); err != nil {
		return err
	}
	b, err := json.Marshal(op)
	if err != nil {
		return err
	}
	return fsutil.AtomicWrite(path(op.Agent, op.ID), b, 0o600)
}

// Done removes a finished operation.
func Done(agent, id string) error {
	err := os.Remove(path(agent, id))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// List returns all unfinished operations.
func List() ([]Op, error) {
	entries, err := os.ReadDir(dir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Op
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir(), e.Name()))
		if err != nil {
			return nil, err
		}
		var op Op
		if err := json.Unmarshal(b, &op); err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, nil
}
