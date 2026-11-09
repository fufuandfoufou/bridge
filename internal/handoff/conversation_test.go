package handoff

import (
	"bridge/internal/store"
	"testing"
)

func TestCompareConversation(t *testing.T) {
	link := store.ConversationLink{SourceID: "a", TargetID: "b", SourceSeq: 10, TargetSeq: 20}
	for _, tc := range []struct {
		source, target int64
		state          ConversationState
		bad            bool
	}{
		{10, 20, ConversationCurrent, false}, {11, 20, ConversationSourceAhead, false},
		{10, 21, ConversationTargetAhead, false}, {11, 21, ConversationDiverged, false},
		{9, 20, "", true}, {10, 19, "", true},
	} {
		state, err := CompareConversation(link, tc.source, tc.target)
		if state != tc.state || (err != nil) != tc.bad {
			t.Errorf("%+v: got %q, %v", tc, state, err)
		}
	}
	if _, err := CompareConversation(store.ConversationLink{}, 1, 1); err == nil {
		t.Fatal("missing baseline accepted")
	}
}
