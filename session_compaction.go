package opencodeacp

import (
	"context"

	"github.com/savid/acp-go-core/wire"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

// projectCompaction publishes the compaction progress a native event reports.
// The pump owns the compaction state and forgets each request once its
// summary ends.
func (s *session) projectCompaction(ctx context.Context, event opencode.Event, props eventProperties) error {
	if event.SessionID != "" && event.SessionID != s.nativeID {
		return nil
	}

	info := props.Info
	value := wire.Compaction{}
	key := ""

	switch {
	case event.Type == eventPartUpdated && props.Part.Type == "compaction":
		part := props.Part
		if part.Auto != nil && part.MessageID != "" {
			if s.compactionTriggers == nil {
				s.compactionTriggers = make(map[string]string)
			}

			trigger := wire.CompactionTriggerManual
			if *part.Auto {
				trigger = wire.CompactionTriggerAuto
			}

			s.compactionTriggers[part.MessageID] = trigger
		}

		return nil
	case event.Type == eventMessageUpdated && info.Role == roleAssistant && info.CompactionSummary():
		key = info.ID
		value.Trigger = s.compactionTriggers[info.ParentID]

		if key == "" {
			return nil
		}

		switch {
		case info.Error != nil:
			if s.compactionMessage == key {
				s.compactionMessage = ""
			}

			delete(s.compactionTriggers, info.ParentID)

			value.Status = wire.CompactionFailed
			if info.Error.Name == "MessageAbortedError" {
				value.Status = wire.CompactionCancelled
			}
		case info.Time.Completed != 0:
			// Success arrives as the session's compaction event.
			if s.compactionMessage == "" {
				s.compactionMessage = key
			}

			return nil
		default:
			s.compactionMessage = key
			value.Status = wire.CompactionInProgress
		}
	case event.Type == "session.compacted":
		if event.ID != "" {
			if s.compactionEvent == event.ID {
				return nil
			}

			s.compactionEvent = event.ID
		}

		key = s.compactionMessage
		if key == "" {
			key = event.ID
		}

		value.Status = wire.CompactionCompleted
		s.compactionMessage = ""
		clear(s.compactionTriggers)
	default:
		return nil
	}

	return s.compactions.Publish(ctx, s.agent.connection(), s.id, key, value)
}
