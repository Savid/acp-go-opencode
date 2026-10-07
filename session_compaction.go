package opencodeacp

import (
	"context"

	"github.com/savid/acp-go-core/wire"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

func (s *session) projectCompaction(ctx context.Context, event opencode.Event, props eventProperties) error {
	if event.SessionID() != "" && event.SessionID() != s.nativeID {
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

		if s.compactionMessages == nil {
			s.compactionMessages = make(map[string]bool)
		}

		if !s.compactionMessages[key] {
			s.compactionMessages[key] = true
			s.compactionMessage = key
		}

		if info.Error != nil {
			if s.compactionMessage == key {
				s.compactionMessage = ""
			}

			value.Status = wire.CompactionFailed
			if info.Error.Name == "MessageAbortedError" {
				value.Status = wire.CompactionCancelled
			}
		} else {
			if info.Time.Completed != 0 {
				return nil
			}

			value.Status = wire.CompactionInProgress
		}
	case event.Type == "session.compacted":
		if event.ID != "" {
			if s.compactionEvents[event.ID] {
				return nil
			}

			if s.compactionEvents == nil {
				s.compactionEvents = make(map[string]bool)
			}

			s.compactionEvents[event.ID] = true
		}

		key = s.compactionMessage
		if key == "" {
			key = event.ID
		}

		value.Status = wire.CompactionCompleted
		s.compactionMessage = ""
	default:
		return nil
	}

	return s.compactions.Publish(ctx, s.agent.connection(), s.id, key, value)
}
