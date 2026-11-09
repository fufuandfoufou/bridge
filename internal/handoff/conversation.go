package handoff

import (
	"bridge/internal/store"
	"errors"
)

// ConversationState is a read-only preflight, not permission to write. Native
// files and client activity must be checked again at the actual write boundary.
type ConversationState string

const (
	ConversationCurrent     ConversationState = "current"
	ConversationSourceAhead ConversationState = "source_ahead"
	ConversationTargetAhead ConversationState = "target_ahead"
	ConversationDiverged    ConversationState = "diverged"
)

// CompareConversation refuses regressed or absent baselines. Both sides
// advancing is a branch conflict, never an invitation to concatenate histories.
func CompareConversation(link store.ConversationLink, sourceSeq, targetSeq int64) (ConversationState, error) {
	if link.SourceSeq <= 0 || link.TargetSeq <= 0 || sourceSeq < link.SourceSeq || targetSeq < link.TargetSeq {
		return "", errors.New("conversation history or sync boundary is missing or has regressed")
	}
	sourceAhead, targetAhead := sourceSeq > link.SourceSeq, targetSeq > link.TargetSeq
	switch {
	case sourceAhead && targetAhead:
		return ConversationDiverged, nil
	case sourceAhead:
		return ConversationSourceAhead, nil
	case targetAhead:
		return ConversationTargetAhead, nil
	default:
		return ConversationCurrent, nil
	}
}
