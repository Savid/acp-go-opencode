package opencode

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStripSessionCarrierFromSyncEvent(t *testing.T) {
	for _, eventType := range []string{syncEventTypeSessionCreated, syncEventTypeSessionUpdated} {
		t.Run(eventType, func(t *testing.T) {
			event := SyncEvent{
				Type: eventType,
				Data: map[string]json.RawMessage{
					syncEventFieldInfo: json.RawMessage(`{
						"id":"native",
						"metadata":{
							"native":{"kept":true},
							"acp-go-opencode":{"ref":"opaque-reference"}
						}
					}`),
				},
			}

			require.NoError(t, StripSessionCarrierFromSyncEvent(&event))
			require.JSONEq(t, `{
				"id":"native",
				"metadata":{"native":{"kept":true}}
			}`, string(event.Data[syncEventFieldInfo]))
		})
	}
}

func TestStripSessionCarrierFromSyncEventLeavesOtherNativeDataUntouched(t *testing.T) {
	ordinary := json.RawMessage(`{"id":"native","metadata":{"native":"kept"}}`)
	for _, event := range []SyncEvent{
		{Type: "message.updated.1", Data: map[string]json.RawMessage{syncEventFieldInfo: append(json.RawMessage(nil), ordinary...)}},
		{Type: syncEventTypeSessionCreated, Data: map[string]json.RawMessage{syncEventFieldInfo: append(json.RawMessage(nil), ordinary...)}},
		{Type: syncEventTypeSessionCreated, Data: map[string]json.RawMessage{syncEventFieldInfo: json.RawMessage(`{"metadata":null}`)}},
		{Type: syncEventTypeSessionUpdated, Data: map[string]json.RawMessage{}},
	} {
		before, err := json.Marshal(event)
		require.NoError(t, err)
		require.NoError(t, StripSessionCarrierFromSyncEvent(&event))
		after, err := json.Marshal(event)
		require.NoError(t, err)
		require.JSONEq(t, string(before), string(after))
	}
}

func TestStripSessionCarrierFromSyncEventRejectsMalformedNativeSessionMetadata(t *testing.T) {
	for _, info := range []json.RawMessage{
		json.RawMessage(`{`),
		json.RawMessage(`{"metadata":"not-an-object"}`),
	} {
		event := SyncEvent{
			Type: syncEventTypeSessionCreated,
			Data: map[string]json.RawMessage{syncEventFieldInfo: info},
		}
		require.Error(t, StripSessionCarrierFromSyncEvent(&event))
	}
}
