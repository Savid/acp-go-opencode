package opencode

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDecodeActionRepliedReadsTheNativePayload drives the exact payload shapes the
// installed OpenCode server publishes for a server-confirmed resolution:
// `{sessionID, requestID, reply}` for a permission and `{sessionID, requestID,
// answers}` for a question. The request id is the id the matching `*.asked`
// announced, which is the whole correlation a resolution needs.
func TestDecodeActionRepliedReadsTheNativePayload(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name     string
		payload  string
		request  string
		accepted bool
	}{
		{
			name:     "permission allowed once",
			payload:  `{"sessionID":"ses_1","requestID":"per_1","reply":"once"}`,
			request:  "per_1",
			accepted: true,
		},
		{
			name:     "permission allowed always",
			payload:  `{"sessionID":"ses_1","requestID":"per_1","reply":"always"}`,
			request:  "per_1",
			accepted: true,
		},
		{
			name:    "permission rejected",
			payload: `{"sessionID":"ses_1","requestID":"per_1","reply":"reject"}`,
			request: "per_1",
		},
		{
			name:    "question answered",
			payload: `{"sessionID":"ses_1","requestID":"que_1","answers":[["yes"]]}`,
			request: "que_1",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			replied, ok := DecodeActionReplied(json.RawMessage(row.payload))
			require.True(t, ok)
			require.Equal(t, "ses_1", replied.SessionID)
			require.Equal(t, row.request, replied.RequestID)
			require.Equal(t, row.accepted, replied.Accepted())
		})
	}
}

// TestDecodeActionRepliedRefusesAnUncorrelatablePayload proves a resolution that
// names no request resolves nothing.
func TestDecodeActionRepliedRefusesAnUncorrelatablePayload(t *testing.T) {
	t.Parallel()

	for _, payload := range []string{
		`not json`,
		`{}`,
		`{"sessionID":"ses_1"}`,
		`{"requestID":"per_1"}`,
	} {
		_, ok := DecodeActionReplied(json.RawMessage(payload))
		require.False(t, ok, payload)
	}
}

// TestEventSessionIDRoutesByStructuredIdentity proves every event shape this
// adapter routes reports the native session it concerns, and that one naming none
// concerns no session.
func TestEventSessionIDRoutesByStructuredIdentity(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name       string
		properties string
		sessionID  string
	}{
		{"direct", `{"sessionID":"ses_1","requestID":"per_1"}`, "ses_1"},
		{"message info", `{"info":{"id":"msg_1","sessionID":"ses_1","role":"assistant"}}`, "ses_1"},
		{"message part", `{"part":{"id":"prt_1","sessionID":"ses_1","type":"text"}}`, "ses_1"},
		{"no session", `{"version":"1.18.18"}`, ""},
		{"not an object", `7`, ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			id, ok := EventSessionID(Event{Properties: json.RawMessage(row.properties)})
			require.Equal(t, row.sessionID != "", ok)
			require.Equal(t, row.sessionID, id)
		})
	}
}

// TestPermissionToolCallReadsEitherNativeShape proves the tool correlation is read
// from whichever member the native route carried it in. The session route names a
// `tool` object; the API route names a `source` whose type is tool, and a reader
// outside this package must not have to know which arrived.
func TestPermissionToolCallReadsEitherNativeShape(t *testing.T) {
	t.Parallel()

	var sessionRoute PermissionRequest
	require.NoError(t, json.Unmarshal([]byte(
		`{"id":"per_1","sessionID":"ses_1","permission":"edit","patterns":["*"],`+
			`"metadata":{},"always":[],"tool":{"messageID":"msg_1","callID":"call_1"}}`), &sessionRoute))
	require.Equal(t, PermissionTool{MessageID: "msg_1", CallID: "call_1"}, sessionRoute.ToolCall())

	var apiRoute PermissionRequest
	require.NoError(t, json.Unmarshal([]byte(
		`{"id":"per_1","sessionID":"ses_1","action":"edit","resources":["*"],`+
			`"source":{"type":"tool","messageID":"msg_1","callID":"call_1"}}`), &apiRoute))
	require.Equal(t, PermissionTool{MessageID: "msg_1", CallID: "call_1"}, apiRoute.ToolCall())

	var unsourced PermissionRequest
	require.NoError(t, json.Unmarshal([]byte(`{"id":"per_1","sessionID":"ses_1"}`), &unsourced))
	require.Equal(t, PermissionTool{}, unsourced.ToolCall())
}

// TestDecodeSessionStatusRequiresAnAddressableSession proves a status payload is
// routable only when it decodes and names its session, and that a valid payload
// carries the status through.
func TestDecodeSessionStatusRequiresAnAddressableSession(t *testing.T) {
	t.Parallel()

	if _, ok := DecodeSessionStatus(json.RawMessage(`{`)); ok {
		t.Fatal("malformed status payload was decoded")
	}

	if _, ok := DecodeSessionStatus(json.RawMessage(`{"status":{"type":"busy"}}`)); ok {
		t.Fatal("status payload naming no session was routable")
	}

	status, ok := DecodeSessionStatus(json.RawMessage(`{"sessionID":"ses_1","status":{"type":"busy"}}`))
	require.True(t, ok)
	require.Equal(t, "ses_1", status.SessionID)
	require.Equal(t, SessionStatusBusy, status.Status.Type)
}
