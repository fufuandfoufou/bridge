package fakellm

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

// Server is a local stand-in for an OpenAI-compatible chat endpoint. Version
// probes point a real client at it: the client loads a session bridge wrote
// and sends its history here, nothing leaves the machine, and no account or
// key is involved. Every request gets the same short answer.
type Server struct {
	URL    string // base URL ending in /v1
	srv    *http.Server
	mu     sync.Mutex
	bodies []string
	// ToolCall, when set, is answered to the first request that carries no
	// tool result yet: {"name": ..., "arguments": {...}}.
	ToolCall map[string]any
}

// Start listens on a random local port.
func Start() (*Server, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f := &Server{URL: "http://" + l.Addr().String() + "/v1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/", f.serve)
	f.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = f.srv.Serve(l) }()
	return f, nil
}

// Close stops the server.
func (f *Server) Close() { _ = f.srv.Close() }

// Requests returns the bodies received so far.
func (f *Server) Requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bodies...)
}

// Saw reports whether any request contained s.
func (f *Server) Saw(s string) bool {
	for _, b := range f.Requests() {
		if strings.Contains(b, s) {
			return true
		}
	}
	return false
}

func (f *Server) serve(w http.ResponseWriter, r *http.Request) {
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
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(b, &req)
	const answer = "probe ok"
	toolDone := false
	for _, m := range req.Messages {
		toolDone = toolDone || m.Role == "tool"
	}
	f.mu.Lock()
	call := f.ToolCall
	f.mu.Unlock()
	if call != nil && !toolDone && req.Stream {
		args, _ := json.Marshal(call["arguments"])
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(v any) {
			j, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", j)
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
		base := map[string]any{"id": "probe-call", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": req.Model}
		chunk := func(choice map[string]any) map[string]any {
			m := map[string]any{"choices": []any{choice}}
			for k, v := range base {
				m[k] = v
			}
			return m
		}
		send(chunk(map[string]any{"index": 0, "finish_reason": nil, "delta": map[string]any{"role": "assistant",
			"tool_calls": []any{map[string]any{"index": 0, "id": "call_probe", "type": "function",
				"function": map[string]any{"name": call["name"], "arguments": string(args)}}}}}))
		send(chunk(map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		return
	}
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
