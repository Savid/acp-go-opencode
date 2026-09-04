package lifecycle

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecodedDuplicateComparisonPreservesIntegersAboveFloatPrecision(t *testing.T) {
	negotiated := Negotiated{Version: Version}
	frame := func(marker string) json.RawMessage {
		return json.RawMessage(`{"sessionId":"s","update":{"sessionUpdate":"session_info_update"},"_meta":{"acp-go.dev/lifecycle":{"version":1,"streamId":"stream","sequence":1,"event":{"type":"lifecycle_snapshot","foreground":{"state":"idle","cycleId":"idle"},"activities":[],"actions":[],"quiescence":{"quiescent":false}}},"marker":` + marker + `}}`)
	}
	first, err := DecodeSessionUpdate(frame("9007199254740992"), negotiated)
	require.NoError(t, err)
	second, err := DecodeSessionUpdate(frame("9007199254740993"), negotiated)
	require.NoError(t, err)
	r := NewReducer(Options{Negotiated: negotiated})
	require.NoError(t, r.Reduce(first))
	require.ErrorContains(t, r.Reduce(second), string(ViolationConflictingDuplicate))
}

func decodeNegotiated() Negotiated {
	return Negotiated{
		Version:                 1,
		AuthoritativeQuiescence: true,
		QuiescenceSource:        ProofClassProcessContainment,
		ActivityKinds:           []ActivityKind{ActivityTask},
	}
}

// frame builds one session/update payload from a raw extension value and a raw
// carrier, so a test can inject a shape the encoder would never produce.
func frame(value, carrier string) json.RawMessage {
	return json.RawMessage(`{"sessionId":"session-1","update":` + carrier +
		`,"_meta":{"acp-go.dev/lifecycle":` + value + `}}`)
}

// envelopeAround wraps one event object in a well-formed envelope.
func envelopeAround(event string) string {
	return `{"version":1,"streamId":"stream-1","sequence":7,"event":` + event + `}`
}

const infoCarrier = `{"sessionUpdate":"session_info_update"}`

// TestDecodeReportsNoEnvelopeForOrdinaryContent proves an ordinary notification is
// not a violation: content rides its own surfaces without an envelope.
func TestDecodeReportsNoEnvelopeForOrdinaryContent(t *testing.T) {
	t.Parallel()

	for _, params := range []string{
		`{"sessionId":"session-1","update":` + infoCarrier + `}`,
		`{"sessionId":"session-1","update":` + infoCarrier + `,"_meta":{"acp-go.dev/route":{"version":1}}}`,
	} {
		_, err := DecodeSessionUpdate(json.RawMessage(params), decodeNegotiated())
		require.ErrorIs(t, err, ErrNoEnvelope)
	}
}

// TestDecodeRefusesAnEnvelopeOnAnUnnegotiatedConnection proves the answer is the
// contract: with the key omitted, any envelope at all is refused.
func TestDecodeRefusesAnEnvelopeOnAnUnnegotiatedConnection(t *testing.T) {
	t.Parallel()

	_, err := DecodeSessionUpdate(frame(envelopeAround(`{"type":"prompt_accepted"}`), infoCarrier), Negotiated{})
	require.ErrorIs(t, err, &ViolationError{Kind: ViolationUnnegotiatedFact})
}

// TestDecodeNamesTheRefusedIdentity proves a refusal reports the ordering identity
// the envelope managed to state, so a report can say which frame failed closed.
func TestDecodeNamesTheRefusedIdentity(t *testing.T) {
	t.Parallel()

	_, err := DecodeSessionUpdate(
		frame(envelopeAround(`{"type":"nonsense"}`), infoCarrier), decodeNegotiated())

	var refusal *ViolationError

	require.ErrorAs(t, err, &refusal)
	require.Equal(t, "stream-1", refusal.StreamID)
	require.Equal(t, uint64(7), refusal.Sequence)
	require.Contains(t, refusal.Error(), "stream-1#7")
	require.Contains(t, refusal.Error(), "nonsense")
	require.False(t, refusal.Is(ErrNoEnvelope))
}

func TestDecodeCarrierLegality(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name    string
		carrier string
		class   CarrierClass
	}{
		{"identity only", infoCarrier, CarrierSessionInfo},
		{"not an object", `7`, CarrierUnknown},
		{"null", `null`, CarrierUnknown},
		{"variant missing", `{}`, CarrierUnknown},
		{"variant not a string", `{"sessionUpdate":1}`, CarrierUnknown},
		{"another variant", `{"sessionUpdate":"agent_message_chunk"}`, CarrierIneligible},
		{"titled session info", `{"sessionUpdate":"session_info_update","title":"t"}`, CarrierIneligible},
		{"stamped session info", `{"sessionUpdate":"session_info_update","updatedAt":"now"}`, CarrierIneligible},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, row.class, CarrierClassForSessionUpdate(json.RawMessage(row.carrier)))

			_, err := DecodeSessionUpdate(
				frame(envelopeAround(`{"type":"state_update","state":"running","cycleId":"c","turnId":"t","cause":"submission"}`),
					row.carrier), decodeNegotiated())

			if row.class == CarrierSessionInfo {
				require.NoError(t, err)

				return
			}

			require.ErrorIs(t, err, &ViolationError{Kind: ViolationIllegalCarrier})
		})
	}
}

// TestDecodeStrictness pins every structural rule the decoder enforces. Each row
// names exactly one defect so a refusal a decoder can reach two ways never stands
// in for two rules.
func TestDecodeStrictness(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name  string
		value string
		kind  ViolationKind
	}{
		{"extension value is an array", `[]`, ViolationIllegalCarrier},
		{"extension value is a scalar", `7`, ViolationIllegalCarrier},
		{"envelope unknown member", `{"version":1,"streamId":"s","sequence":1,"event":{"type":"prompt_accepted","submissionId":"a","clientNonce":"b","turnId":"c"},"extra":1}`, ViolationUnknownField},
		{"stream id missing", `{"version":1,"sequence":1,"event":{"type":"prompt_accepted"}}`, ViolationMalformedEnvelope},
		{"stream id empty", `{"version":1,"streamId":"","sequence":1,"event":{"type":"prompt_accepted"}}`, ViolationMalformedEnvelope},
		{"stream id not a string", `{"version":1,"streamId":7,"sequence":1,"event":{"type":"prompt_accepted"}}`, ViolationMalformedEnvelope},
		{"stream id over bound", `{"version":1,"streamId":"` + strings.Repeat("x", IdentifierBound+1) + `","sequence":1,"event":{"type":"prompt_accepted"}}`, ViolationMalformedEnvelope},
		{"sequence missing", `{"version":1,"streamId":"s","event":{"type":"prompt_accepted"}}`, ViolationMalformedEnvelope},
		{"sequence zero", `{"version":1,"streamId":"s","sequence":0,"event":{"type":"prompt_accepted"}}`, ViolationMalformedEnvelope},
		{"sequence not an integer", `{"version":1,"streamId":"s","sequence":"1","event":{"type":"prompt_accepted"}}`, ViolationMalformedEnvelope},
		{"version missing", `{"streamId":"s","sequence":1,"event":{"type":"prompt_accepted"}}`, ViolationMalformedEnvelope},
		{"version not an integer", `{"version":"1","streamId":"s","sequence":1,"event":{"type":"prompt_accepted"}}`, ViolationMalformedEnvelope},
		{"unsupported version", `{"version":2,"streamId":"s","sequence":1,"event":{"type":"prompt_accepted"}}`, ViolationUnsupportedVersion},
		{"event missing", `{"version":1,"streamId":"s","sequence":1}`, ViolationMalformedEnvelope},
		{"event not an object", `{"version":1,"streamId":"s","sequence":1,"event":7}`, ViolationMalformedEnvelope},
		{"event type missing", envelopeAround(`{}`), ViolationMalformedEnvelope},
		{"event type unknown", envelopeAround(`{"type":"turn_started"}`), ViolationUnknownEventType},

		{"acceptance unknown member", envelopeAround(`{"type":"prompt_accepted","submissionId":"a","clientNonce":"b","turnId":"c","extra":1}`), ViolationUnknownField},
		{"acceptance turn missing", envelopeAround(`{"type":"prompt_accepted","submissionId":"a","clientNonce":"b"}`), ViolationMalformedEnvelope},

		{"snapshot unknown member", envelopeAround(`{"type":"lifecycle_snapshot","foreground":{"state":"idle","cycleId":"c"},"activities":[],"actions":[],"quiescence":{"quiescent":false},"extra":1}`), ViolationUnknownField},
		{"snapshot activities missing", envelopeAround(`{"type":"lifecycle_snapshot","foreground":{"state":"idle","cycleId":"c"},"actions":[],"quiescence":{"quiescent":false}}`), ViolationMalformedEnvelope},
		{"snapshot activities not an array", envelopeAround(`{"type":"lifecycle_snapshot","foreground":{"state":"idle","cycleId":"c"},"activities":{},"actions":[],"quiescence":{"quiescent":false}}`), ViolationMalformedEnvelope},
		{"snapshot quiescence missing", envelopeAround(`{"type":"lifecycle_snapshot","foreground":{"state":"idle","cycleId":"c"},"activities":[],"actions":[]}`), ViolationMalformedEnvelope},
		{"snapshot quiescence unknown member", envelopeAround(`{"type":"lifecycle_snapshot","foreground":{"state":"idle","cycleId":"c"},"activities":[],"actions":[],"quiescence":{"quiescent":false,"extra":1}}`), ViolationUnknownField},
		{"foreground missing", envelopeAround(`{"type":"lifecycle_snapshot","activities":[],"actions":[],"quiescence":{"quiescent":false}}`), ViolationMalformedEnvelope},
		{"foreground unknown member", envelopeAround(`{"type":"lifecycle_snapshot","foreground":{"state":"idle","cycleId":"c","extra":1},"activities":[],"actions":[],"quiescence":{"quiescent":false}}`), ViolationUnknownField},
		{"foreground state invalid", envelopeAround(`{"type":"lifecycle_snapshot","foreground":{"state":"waiting","cycleId":"c"},"activities":[],"actions":[],"quiescence":{"quiescent":false}}`), ViolationMalformedEnvelope},
		{"idle foreground names a turn", envelopeAround(`{"type":"lifecycle_snapshot","foreground":{"state":"idle","cycleId":"c","turnId":"t","origin":"submission"},"activities":[],"actions":[],"quiescence":{"quiescent":false}}`), ViolationMalformedEnvelope},
		{"foreground turn without origin", envelopeAround(`{"type":"lifecycle_snapshot","foreground":{"state":"running","cycleId":"c","turnId":"t"},"activities":[],"actions":[],"quiescence":{"quiescent":false}}`), ViolationMalformedEnvelope},
		{"foreground origin invalid", envelopeAround(`{"type":"lifecycle_snapshot","foreground":{"state":"running","cycleId":"c","turnId":"t","origin":"session"},"activities":[],"actions":[],"quiescence":{"quiescent":false}}`), ViolationMalformedEnvelope},

		{"transition unknown member", envelopeAround(`{"type":"state_update","state":"running","cycleId":"c","turnId":"t","cause":"submission","extra":1}`), ViolationUnknownField},
		{"transition state invalid", envelopeAround(`{"type":"state_update","state":"waiting","cycleId":"c","turnId":"t","cause":"submission"}`), ViolationMalformedEnvelope},
		{"transition cause invalid", envelopeAround(`{"type":"state_update","state":"running","cycleId":"c","turnId":"t","cause":"native"}`), ViolationMalformedEnvelope},
		{"caused transition without a turn", envelopeAround(`{"type":"state_update","state":"running","cycleId":"c","cause":"submission"}`), ViolationMalformedEnvelope},
		{"stop reason on a live transition", envelopeAround(`{"type":"state_update","state":"running","cycleId":"c","turnId":"t","cause":"submission","stopReason":"end_turn"}`), ViolationMalformedEnvelope},
		{"stop reason invalid", envelopeAround(`{"type":"state_update","state":"idle","cycleId":"c","turnId":"t","cause":"submission","stopReason":"done","outcome":"success"}`), ViolationMalformedEnvelope},
		{"outcome invalid", envelopeAround(`{"type":"state_update","state":"idle","cycleId":"c","turnId":"t","cause":"submission","stopReason":"end_turn","outcome":"finished"}`), ViolationMalformedEnvelope},
		{"ending idle without an outcome", envelopeAround(`{"type":"state_update","state":"idle","cycleId":"c","turnId":"t","cause":"submission","stopReason":"end_turn"}`), ViolationMalformedEnvelope},

		{"activity update unknown member", envelopeAround(`{"type":"activity_update","activity":{"activityId":"a","state":"running"},"extra":1}`), ViolationUnknownField},
		{"activity update carries no activity", envelopeAround(`{"type":"activity_update"}`), ViolationMalformedEnvelope},
		{"activity unknown member", envelopeAround(`{"type":"activity_update","activity":{"activityId":"a","state":"running","extra":1}}`), ViolationUnknownField},
		{"activity state missing", envelopeAround(`{"type":"activity_update","activity":{"activityId":"a"}}`), ViolationMalformedEnvelope},
		{"activity kind invalid", envelopeAround(`{"type":"activity_update","activity":{"activityId":"a","state":"running","kind":"job"}}`), ViolationMalformedEnvelope},
		{"activity state invalid", envelopeAround(`{"type":"activity_update","activity":{"activityId":"a","state":"waiting"}}`), ViolationMalformedEnvelope},
		{"activity cause invalid", envelopeAround(`{"type":"activity_update","activity":{"activityId":"a","state":"running","cause":"native"}}`), ViolationMalformedEnvelope},
		{"activity progress not an object", envelopeAround(`{"type":"activity_update","activity":{"activityId":"a","state":"running","progress":7}}`), ViolationMalformedEnvelope},
		{"activity progress over bound", envelopeAround(`{"type":"activity_update","activity":{"activityId":"a","state":"running","progress":{"pad":"` + strings.Repeat("x", IdentifierBound) + `"}}}`), ViolationMalformedEnvelope},

		{"action update unknown member", envelopeAround(`{"type":"action_update","action":{"actionId":"a","state":"pending"},"extra":1}`), ViolationUnknownField},
		{"action update carries no action", envelopeAround(`{"type":"action_update"}`), ViolationMalformedEnvelope},
		{"action unknown member", envelopeAround(`{"type":"action_update","action":{"actionId":"a","state":"pending","extra":1}}`), ViolationUnknownField},
		{"action state missing", envelopeAround(`{"type":"action_update","action":{"actionId":"a"}}`), ViolationMalformedEnvelope},
		{"action kind invalid", envelopeAround(`{"type":"action_update","action":{"actionId":"a","state":"pending","kind":"approval"}}`), ViolationMalformedEnvelope},
		{"action state invalid", envelopeAround(`{"type":"action_update","action":{"actionId":"a","state":"answered"}}`), ViolationMalformedEnvelope},
		{"action blocks not a boolean", envelopeAround(`{"type":"action_update","action":{"actionId":"a","state":"pending","blocksForeground":"yes"}}`), ViolationMalformedEnvelope},
		{"action owner not an object", envelopeAround(`{"type":"action_update","action":{"actionId":"a","state":"pending","owner":7}}`), ViolationMalformedEnvelope},
		{"action owner unknown member", envelopeAround(`{"type":"action_update","action":{"actionId":"a","state":"pending","owner":{"type":"turn","id":"t","extra":1}}}`), ViolationUnknownField},
		{"action owner type invalid", envelopeAround(`{"type":"action_update","action":{"actionId":"a","state":"pending","owner":{"type":"session","id":"t"}}}`), ViolationMalformedEnvelope},
		{"action owner id missing", envelopeAround(`{"type":"action_update","action":{"actionId":"a","state":"pending","owner":{"type":"turn"}}}`), ViolationMalformedEnvelope},

		{"quiescence unknown member", envelopeAround(`{"type":"quiescence_update","quiescent":false,"extra":1}`), ViolationUnknownField},
		{"quiescent missing", envelopeAround(`{"type":"quiescence_update"}`), ViolationMalformedEnvelope},
		{"quiescent not a boolean", envelopeAround(`{"type":"quiescence_update","quiescent":"yes"}`), ViolationMalformedEnvelope},
		{"negative fact names a source", envelopeAround(`{"type":"quiescence_update","quiescent":false,"source":"process-containment"}`), ViolationMalformedEnvelope},
		{"negative fact carries a watermark", envelopeAround(`{"type":"quiescence_update","quiescent":false,"watermark":1}`), ViolationMalformedEnvelope},
		{"positive fact without a watermark", envelopeAround(`{"type":"quiescence_update","quiescent":true,"source":"process-containment"}`), ViolationMalformedEnvelope},
		{"positive fact without a source", envelopeAround(`{"type":"quiescence_update","quiescent":true,"watermark":1}`), ViolationMalformedEnvelope},
		{"proof class invalid", envelopeAround(`{"type":"quiescence_update","quiescent":true,"source":"prompt-return","watermark":1}`), ViolationMalformedEnvelope},
		{"watermark not an integer", envelopeAround(`{"type":"quiescence_update","quiescent":true,"source":"process-containment","watermark":"1"}`), ViolationMalformedEnvelope},
		{"watermark claims its own delivery", envelopeAround(`{"type":"quiescence_update","quiescent":true,"source":"process-containment","watermark":7}`), ViolationMalformedEnvelope},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			_, err := DecodeSessionUpdate(frame(row.value, infoCarrier), decodeNegotiated())
			require.ErrorIs(t, err, &ViolationError{Kind: row.kind})
		})
	}
}

// TestDecodeRefusesAnUnreadableNotification proves the notification itself is
// judged before anything inside it.
func TestDecodeRefusesAnUnreadableNotification(t *testing.T) {
	t.Parallel()

	for _, params := range []string{`{`, `[]`, `null`, `7`} {
		_, err := DecodeSessionUpdate(json.RawMessage(params), decodeNegotiated())
		require.ErrorIs(t, err, &ViolationError{Kind: ViolationMalformedEnvelope})
	}
}

// TestDecodeReadsANullMetaAsNoEnvelope proves a null `_meta` carries nothing
// rather than failing closed: an absent envelope is not a violation.
func TestDecodeReadsANullMetaAsNoEnvelope(t *testing.T) {
	t.Parallel()

	_, err := DecodeSessionUpdate(
		json.RawMessage(`{"sessionId":"session-1","update":`+infoCarrier+`,"_meta":null}`), decodeNegotiated())
	require.ErrorIs(t, err, ErrNoEnvelope)
}

// TestDecodeAcceptsTheOptionalMembersItAllows proves the optional members decode
// rather than merely failing to be refused.
func TestDecodeAcceptsTheOptionalMembersItAllows(t *testing.T) {
	t.Parallel()

	delivery, err := DecodeSessionUpdate(frame(envelopeAround(
		`{"type":"lifecycle_snapshot","foreground":{"state":"running","cycleId":"c","turnId":"t","origin":"activity"},`+
			`"activities":[{"activityId":"a","kind":"task","state":"running","cause":"submission","originTurnId":"t","runId":"r","toolCallId":"tool","progress":{}}],`+
			`"actions":[{"actionId":"x","kind":"permission","state":"pending","owner":{"type":"turn","id":"t"},"blocksForeground":true}],`+
			`"quiescence":{"quiescent":false}}`), infoCarrier), decodeNegotiated())
	require.NoError(t, err)
	require.Equal(t, CauseActivity, delivery.Event.Snapshot.Foreground.Origin)
	require.Equal(t, "tool", delivery.Event.Snapshot.Activities[0].ToolCallID)
	require.True(t, *delivery.Event.Snapshot.Actions[0].BlocksForeground)
}
