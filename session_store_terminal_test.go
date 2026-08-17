package opencodeacp

import (
	"encoding/json"
	"testing"

	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

func TestInspectSessionStoreTerminalStateIgnoresLateDuplicateUser(t *testing.T) {
	snapshot := terminalTestSnapshot(t,
		terminalMessageEvent("native", 1, "user-created", "user", "", nil),
		terminalMessageEvent("native", 2, "assistant-terminal", "assistant", "stop", nil),
		terminalMessageEvent("native", 3, "assistant-terminal", "assistant", "stop", int64Pointer(300)),
		syncTestEvent("native", 4, "session.updated.1", nil),
		terminalMessageEvent("native", 5, "user-created", "user", "", nil),
	)

	terminal, err := InspectSessionStoreTerminalState("session", []SessionStoreEntry{snapshot})
	require.NoError(t, err)
	require.Equal(t, SessionStoreTerminalState{MessageID: "assistant-terminal"}, terminal)
}

func TestInspectSessionStoreTerminalStateNoTerminalAssistant(t *testing.T) {
	snapshot := terminalTestSnapshot(t,
		terminalMessageEvent("native", 1, "user", "user", "", nil),
		terminalMessageEvent("native", 2, "assistant-pending", "assistant", "", nil),
		terminalMessageEvent("native", 3, "assistant-completed-only", "assistant", "", int64Pointer(100)),
		terminalMessageEvent("native", 4, "user", "user", "", nil),
	)

	terminal, err := InspectSessionStoreTerminalState("session", []SessionStoreEntry{snapshot})
	require.NoError(t, err)
	require.Empty(t, terminal)
}

func TestInspectSessionStoreTerminalStateSelectsLatestAssistantTerminal(t *testing.T) {
	snapshot := terminalTestSnapshot(t,
		terminalMessageEvent("native", 1, "assistant-tool", "assistant", "tool-calls", int64Pointer(100)),
		terminalMessageEvent("native", 2, "user", "user", "", nil),
		terminalMessageEvent("native", 3, "assistant-final", "assistant", "stop", int64Pointer(200)),
		terminalMessageEvent("native", 4, "user", "user", "", nil),
	)

	terminal, err := InspectSessionStoreTerminalState("session", []SessionStoreEntry{snapshot})
	require.NoError(t, err)
	require.Equal(t, "assistant-final", terminal.MessageID)
}

func TestInspectSessionStoreTerminalStateRejectsMalformedOrUnsupportedEntry(t *testing.T) {
	valid := terminalTestSnapshot(t,
		terminalMessageEvent("native", 1, "assistant", "assistant", "stop", int64Pointer(100)),
	)

	tests := map[string]struct {
		entries   []SessionStoreEntry
		sessionID string
		wantErr   string
	}{
		"empty logical id": {
			entries: []SessionStoreEntry{valid}, wantErr: "logical session id is required",
		},
		"missing entry": {sessionID: "session", wantErr: "exactly one entry"},
		"multiple entries": {
			entries: []SessionStoreEntry{valid, valid}, sessionID: "session", wantErr: "exactly one entry",
		},
		"malformed json": {
			entries: []SessionStoreEntry{SessionStoreEntry(`{`)}, sessionID: "session", wantErr: "decode OpenCode",
		},
		"unsupported format": {
			entries: []SessionStoreEntry{mutateTerminalTestSnapshot(t, valid, func(snapshot *stateSnapshot) {
				snapshot.Format = "removed-format"
			})},
			sessionID: "session", wantErr: "unsupported opencode store format",
		},
		"logical session mismatch": {
			entries: []SessionStoreEntry{valid}, sessionID: "other", wantErr: "manifest identity mismatch",
		},
		"non-contiguous order": {
			entries: []SessionStoreEntry{mutateTerminalTestSnapshot(t, valid, func(snapshot *stateSnapshot) {
				snapshot.Events["native"][1].Sequence = 2
			})},
			sessionID: "session", wantErr: "non-contiguous",
		},
		"malformed info": {
			entries: []SessionStoreEntry{mutateTerminalTestSnapshot(t, valid, func(snapshot *stateSnapshot) {
				snapshot.Events["native"][1].Data[syncFieldInfo] = json.RawMessage(`[]`)
			})},
			sessionID: "session", wantErr: "decode OpenCode message event",
		},
		"wrong message session": {
			entries: []SessionStoreEntry{mutateTerminalTestSnapshot(t, valid, func(snapshot *stateSnapshot) {
				snapshot.Events["native"][1].Data[syncFieldInfo] = json.RawMessage(
					`{"id":"assistant","sessionID":"other","role":"assistant","finish":"stop"}`,
				)
			})},
			sessionID: "session", wantErr: "invalid message identity",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := InspectSessionStoreTerminalState(test.sessionID, test.entries)
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func terminalTestSnapshot(t *testing.T, events ...opencode.SyncEvent) SessionStoreEntry {
	t.Helper()

	snapshot := validSyncSnapshot("session", "native", "/source")
	snapshot.Events["native"] = append(snapshot.Events["native"], events...)
	entry, err := json.Marshal(snapshot)
	require.NoError(t, err)

	return SessionStoreEntry(entry)
}

func mutateTerminalTestSnapshot(
	t *testing.T,
	entry SessionStoreEntry,
	mutate func(*stateSnapshot),
) SessionStoreEntry {
	t.Helper()

	var snapshot stateSnapshot
	require.NoError(t, json.Unmarshal(entry, &snapshot))
	mutate(&snapshot)
	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)

	return SessionStoreEntry(encoded)
}

func terminalMessageEvent(
	nativeID string,
	sequence int64,
	messageID string,
	role string,
	finish string,
	completed *int64,
) opencode.SyncEvent {
	info := map[string]any{
		"id": messageID, "sessionID": nativeID, "role": role,
	}
	if finish != "" {
		info["finish"] = finish
	}
	if completed != nil {
		info["time"] = map[string]any{"completed": *completed}
	}
	encoded, _ := json.Marshal(info)

	return syncTestEvent(nativeID, sequence, "message.updated.1", map[string]json.RawMessage{
		syncFieldInfo: encoded,
	})
}

func int64Pointer(value int64) *int64 {
	return &value
}
