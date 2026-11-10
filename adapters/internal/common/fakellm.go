package common

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// FakeLLM is a local stand-in for an OpenAI-compatible chat endpoint. Version
// probes point a real client at it: the client loads a session bridge wrote
// and sends its history here, nothing leaves the machine, and no account or
// key is involved. Every request gets the same short answer.
type FakeLLM struct {
	URL    string // base URL ending in /v1
	srv    *http.Server
	mu     sync.Mutex
	bodies []string
}

// StartFakeLLM listens on a random local port.
func StartFakeLLM() (*FakeLLM, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f := &FakeLLM{URL: "http://" + l.Addr().String() + "/v1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/", f.serve)
	f.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = f.srv.Serve(l) }()
	return f, nil
}

// Close stops the server.
func (f *FakeLLM) Close() { _ = f.srv.Close() }

// Requests returns the bodies received so far.
func (f *FakeLLM) Requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bodies...)
}

// Saw reports whether any request contained s.
func (f *FakeLLM) Saw(s string) bool {
	for _, b := range f.Requests() {
		if strings.Contains(b, s) {
			return true
		}
	}
	return false
}

func (f *FakeLLM) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"probe","object":"model"}]}`)
		return
	}
	b, _ := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	f.mu.Lock()
	f.bodies = append(f.bodies, string(b))
	f.mu.Unlock()
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(b, &req)
	const answer = "probe ok"
	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "probe", "object": "chat.completion", "created": time.Now().Unix(), "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": answer}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	chunk := func(v any) {
		j, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", j)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}
	base := map[string]any{"id": "probe", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": req.Model}
	with := func(extra map[string]any) map[string]any {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	chunk(with(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": answer}, "finish_reason": nil}}}))
	chunk(with(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		"usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}}))
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}
