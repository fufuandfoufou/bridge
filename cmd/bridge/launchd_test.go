package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeLaunchd models launchctl: bootout only finishes after a few polls, and
// bootstrapping while the old job is still loaded fails, as on macOS.
type fakeLaunchd struct {
	loaded       bool
	pendingPolls int // polls until an issued bootout completes
	calls        []string
	failAlways   bool
}

func (f *fakeLaunchd) run(args ...string) ([]byte, error) {
	f.calls = append(f.calls, args[0])
	switch args[0] {
	case "bootout":
		if f.loaded {
			f.pendingPolls = 3
		}
		return nil, nil
	case "print":
		if f.pendingPolls > 0 {
			f.pendingPolls--
			if f.pendingPolls == 0 {
				f.loaded = false
			}
		}
		if f.loaded {
			return nil, nil
		}
		return nil, errors.New("not found")
	case "bootstrap":
		if f.loaded || f.failAlways {
			return []byte("Bootstrap failed: 5: Input/output error"), errors.New("exit status 5")
		}
		f.loaded = true
		return nil, nil
	case "kickstart":
		return nil, nil
	}
	return nil, errors.New("unexpected " + args[0])
}

func withFake(t *testing.T, f *fakeLaunchd) {
	old := launchctl
	launchctl = f.run
	t.Cleanup(func() { launchctl = old })
}

func TestReloadWaitsForBootout(t *testing.T) {
	f := &fakeLaunchd{loaded: true}
	withFake(t, f)
	if err := reloadService("gui/1", "/p.plist", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if !f.loaded {
		t.Error("service left stopped")
	}
	if f.calls[len(f.calls)-1] != "bootstrap" {
		t.Errorf("calls: %v", f.calls)
	}
}

func TestReloadFreshInstall(t *testing.T) {
	f := &fakeLaunchd{}
	withFake(t, f)
	if err := reloadService("gui/1", "/p.plist", time.Millisecond); err != nil || !f.loaded {
		t.Fatalf("err=%v loaded=%v", err, f.loaded)
	}
}

func TestReloadReportsPersistentFailure(t *testing.T) {
	f := &fakeLaunchd{failAlways: true}
	withFake(t, f)
	err := reloadService("gui/1", "/p.plist", time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "Input/output error") {
		t.Fatalf("err=%v", err)
	}
}
