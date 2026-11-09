package adapterkit

import (
	"context"
	"fmt"
	"os"
	"time"

	"bridge/internal/model"
)

// Check is one conformance result.
type Check struct {
	Name   string
	Passed bool
	Detail string
}

// ConformanceOptions controls the suite. Write enables the checks that write
// a session into the agent's real data directory and remove it afterwards.
type ConformanceOptions struct {
	Write bool
	CWD   string
}

var canonicalTools = map[string]bool{
	model.ToolReadFile: true, model.ToolWriteFile: true, model.ToolEditFile: true, model.ToolShell: true,
	model.ToolSearchText: true, model.ToolSearchFiles: true, model.ToolWebFetch: true, model.ToolWebSearch: true,
	model.ToolTodo: true, model.ToolSubagent: true, model.ToolMCPCall: true,
}

// Conformance runs the behaviors every adapter must satisfy, built-in or
// external. It uses no testing package so the CLI can run it directly.
func Conformance(ctx context.Context, a Adapter, o ConformanceOptions) []Check {
	var out []Check
	add := func(name string, err error) bool {
		c := Check{Name: name, Passed: err == nil}
		if err != nil {
			c.Detail = err.Error()
		}
		out = append(out, c)
		return err == nil
	}
	add("id is non-empty", func() error {
		if a.ID() == "" {
			return fmt.Errorf("empty id")
		}
		return nil
	}())
	in, err := a.Detect(ctx)
	if !add("detect succeeds", err) {
		return out
	}
	caps := a.Capabilities(in.Version)
	add("capabilities are consistent", func() error {
		if caps.LiveTail && !caps.SessionRead {
			return fmt.Errorf("live_tail requires session_read")
		}
		if caps.SessionWrite && caps.ContextWindow <= 0 {
			return fmt.Errorf("session_write requires a positive context_window")
		}
		return nil
	}())
	add("tools use the canonical vocabulary", func() error {
		for _, t := range caps.Tools {
			if !canonicalTools[t.Canonical] {
				return fmt.Errorf("unknown canonical tool %q", t.Canonical)
			}
		}
		return nil
	}())
	if caps.SessionRead && in.Installed {
		refs, err := a.ListSessions(ctx, Filter{Limit: 3})
		if add("list sessions succeeds", err) {
			for _, r := range refs {
				r := r
				add("read is incremental: "+r.NativeID, checkIncremental(ctx, a, r))
			}
		}
	}
	if o.Write && caps.SessionWrite && in.Installed {
		add("write then read back", checkWrite(ctx, a, o.CWD))
	}
	return out
}

func checkIncremental(ctx context.Context, a Adapter, r SessionRef) error {
	es, cur, err := a.ReadSession(ctx, r, model.Cursor{})
	if err != nil {
		return err
	}
	var last int64
	for _, e := range es {
		if e.Seq <= last {
			return fmt.Errorf("sequence not increasing at %d", e.Seq)
		}
		last = e.Seq
	}
	more, _, err := a.ReadSession(ctx, r, cur)
	if err != nil {
		return err
	}
	// A live session may legitimately have grown between the two reads.
	for _, e := range more {
		if e.Seq <= last {
			return fmt.Errorf("re-read returned already consumed seq %d", e.Seq)
		}
	}
	return nil
}

func checkWrite(ctx context.Context, a Adapter, cwd string) error {
	if cwd == "" {
		var err error
		if cwd, err = os.Getwd(); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	events := []model.Event{
		model.TextEvent(model.RoleUser, "bridge conformance: ping", now),
		model.TextEvent(model.RoleAssistant, "bridge conformance: pong", now),
	}
	h, err := a.WriteSession(ctx, model.Session{Agent: a.ID(), CWD: cwd, Title: "bridge conformance"}, events)
	if err != nil {
		return err
	}
	defer func() {
		if r, ok := a.(SessionRemover); ok {
			_ = r.RemoveSession(ctx, h)
		} else if h.Path != "" {
			_ = os.Remove(h.Path)
		}
	}()
	got, _, err := a.ReadSession(ctx, SessionRef{Agent: a.ID(), NativeID: h.ID, Path: h.Path}, model.Cursor{})
	if err != nil {
		return err
	}
	var texts []string
	for _, e := range got {
		if e.Kind == model.KindMessage {
			texts = append(texts, e.Text())
		}
	}
	if len(texts) != 2 || texts[0] != "bridge conformance: ping" || texts[1] != "bridge conformance: pong" {
		return fmt.Errorf("read back %q", texts)
	}
	return nil
}
