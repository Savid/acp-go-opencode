//nolint:tagliatelle // Native OpenCode message events use sessionID.
package opencodeacp

import (
	"encoding/json"
	"fmt"
	"strings"
)

const sessionStoreRoleAssistant = "assistant"

// SessionStoreTerminalState is the provider-native terminal identity recovered
// from one committed opencode-sync-events-v1 snapshot entry.
type SessionStoreTerminalState struct {
	MessageID string
}

// InspectSessionStoreTerminalState validates the current-format snapshot for
// logicalSessionID and returns its latest finished assistant message. The
// snapshot owns the private native aggregate identity; callers do not need to
// know it. OpenCode may append repeated user-message events after the terminal
// assistant; those events are intentionally not terminal state.
//
// A valid snapshot with no terminal assistant returns the zero state. Invalid
// cardinality, JSON, snapshot formats, message events, and aggregate ordering
// fail closed.
func InspectSessionStoreTerminalState(
	logicalSessionID string,
	entries []SessionStoreEntry,
) (SessionStoreTerminalState, error) {
	if strings.TrimSpace(logicalSessionID) == "" {
		return SessionStoreTerminalState{}, fmt.Errorf("logical session id is required")
	}

	if len(entries) != 1 {
		return SessionStoreTerminalState{}, fmt.Errorf(
			"OpenCode session-store snapshot requires exactly one entry, got %d", len(entries),
		)
	}

	snapshot, err := decodeStateSnapshot(entries[0])
	if err != nil {
		return SessionStoreTerminalState{}, fmt.Errorf("decode OpenCode session-store snapshot: %w", err)
	}

	if err := validateSyncSnapshot(logicalSessionID, snapshot); err != nil {
		return SessionStoreTerminalState{}, fmt.Errorf("validate OpenCode session-store snapshot: %w", err)
	}

	nativeSessionID := snapshot.Session.NativeSessionID

	events := snapshot.Events[nativeSessionID]

	var terminal SessionStoreTerminalState

	for _, event := range events {
		if event.Type != syncTypeMessageUpdated {
			continue
		}

		var info sessionStoreMessageInfo
		if err := json.Unmarshal(event.Data[syncFieldInfo], &info); err != nil {
			return SessionStoreTerminalState{}, fmt.Errorf(
				"decode OpenCode message event %q: %w", event.ID, err,
			)
		}

		if strings.TrimSpace(info.ID) == "" || info.SessionID != nativeSessionID ||
			(info.Role != roleUser && info.Role != sessionStoreRoleAssistant) {
			return SessionStoreTerminalState{}, fmt.Errorf(
				"OpenCode message event %q has invalid message identity", event.ID,
			)
		}

		if info.Role != sessionStoreRoleAssistant || strings.TrimSpace(info.Finish) == "" {
			continue
		}

		terminal.MessageID = info.ID
	}

	return terminal, nil
}

type sessionStoreMessageInfo struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	Role      string `json:"role"`
	Finish    string `json:"finish"`
}
