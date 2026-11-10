package common

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestWithoutReasoning(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		path []string
		want string
	}{
		{
			name: "pi mixed content",
			raw:  `{"id":"entry","message":{"role":"assistant","content":[{"type":"thinking","thinking":"private","thinkingSignature":"private-signature"},{"type":"text","text":"thinking is a word"},{"type":"toolCall","id":"call","arguments":{"type":"thinking","count":9007199254740993}}]},"extra":"kept"}`,
			path: []string{"message"},
			want: `{"id":"entry","message":{"role":"assistant","content":[{"type":"text","text":"thinking is a word"},{"type":"toolCall","id":"call","arguments":{"type":"thinking","count":9007199254740993}}]},"extra":"kept"}`,
		},
		{
			name: "dsh nested message",
			raw:  `{"data":{"message":{"content":[{"type":"reasoning","text":"private"},{"type":"redacted_thinking","data":"private"},{"type":"text","text":"answer"}]}},"seq":17}`,
			path: []string{"data", "message"},
			want: `{"data":{"message":{"content":[{"type":"text","text":"answer"}]}},"seq":17}`,
		},
		{
			name: "escaped block type",
			raw:  `{"message":{"content":[{"type":"\u0074hinking","thinking":"private"}]}}`,
			path: []string{"message"},
			want: `{"message":{"content":[]}}`,
		},
		{name: "text content", raw: `{"message":{"content":"literal reasoning and thinking"}}`, path: []string{"message"}, want: `{"message":{"content":"literal reasoning and thinking"}}`},
		{name: "bash execution", raw: `{"message":{"role":"bashExecution","command":"echo thinking","output":"reasoning"}}`, path: []string{"message"}, want: `{"message":{"role":"bashExecution","command":"echo thinking","output":"reasoning"}}`},
		{name: "missing message", raw: `{"data":{}}`, path: []string{"data", "message"}},
		{name: "unknown content shape", raw: `{"message":{"content":{"thinking":"private"}}}`, path: []string{"message"}},
		{name: "invalid content block", raw: `{"message":{"content":["private"]}}`, path: []string{"message"}},
		{name: "invalid json", raw: `{"message":`, path: []string{"message"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := WithoutReasoning([]byte(tc.raw), tc.path...)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("unknown shape retained: %s", got)
				}
				return
			}
			decode := func(raw []byte) any {
				t.Helper()
				dec := json.NewDecoder(bytes.NewReader(raw))
				dec.UseNumber()
				var value any
				if err := dec.Decode(&value); err != nil {
					t.Fatal(err)
				}
				return value
			}
			if !reflect.DeepEqual(decode(got), decode([]byte(tc.want))) {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}
