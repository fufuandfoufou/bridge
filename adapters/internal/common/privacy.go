package common

import (
	"bytes"
	"encoding/json"
)

// WithoutReasoning removes private reasoning blocks from the message at path.
// Ordinary content and native metadata are preserved. An unrecognized message
// shape is discarded so that it cannot carry reasoning into NativeRef.
func WithoutReasoning(raw []byte, path ...string) json.RawMessage {
	var record map[string]json.RawMessage
	if json.Unmarshal(raw, &record) != nil || record == nil {
		return nil
	}
	if len(path) > 0 {
		original := record[path[0]]
		clean := WithoutReasoning(original, path[1:]...)
		if len(clean) == 0 {
			return nil
		}
		if bytes.Equal(original, clean) {
			return Compact(raw)
		}
		record[path[0]] = clean
		return MustJSON(record)
	}
	content, ok := record["content"]
	if !ok {
		return Compact(raw)
	}
	var text string
	if json.Unmarshal(content, &text) == nil {
		return Compact(raw)
	}
	var blocks []json.RawMessage
	if json.Unmarshal(content, &blocks) != nil {
		return nil
	}
	kept := make([]json.RawMessage, 0, len(blocks))
	for _, rawBlock := range blocks {
		var block struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(rawBlock, &block) != nil {
			return nil
		}
		switch block.Type {
		case "thinking", "redacted_thinking", "reasoning":
			continue
		}
		kept = append(kept, rawBlock)
	}
	if len(kept) == len(blocks) {
		return Compact(raw)
	}
	record["content"] = MustJSON(kept)
	return MustJSON(record)
}
