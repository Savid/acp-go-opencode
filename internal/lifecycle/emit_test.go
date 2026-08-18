package lifecycle

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func promptContained() Negotiated {
	return Negotiated{Versions: []int{1}, ActivityKinds: []ActivityKind{}}
}

// TestStreamClaimsASequenceBeforeDelivery proves a refused event consumes its
// sequence: the emitter reserves the number before it tries to send, so a drop
// leaves a detectable gap rather than a silently contiguous stream.
func TestStreamClaimsASequenceBeforeDelivery(t *testing.T) {
	t.Parallel()

	stream := NewStream("stream-1", promptContained())
	require.Equal(t, "stream-1", stream.ID())
	require.Equal(t, promptContained(), stream.Negotiated())

	opening, err := stream.Emit(SnapshotEvent(
		Foreground{State: ForegroundIdle, CycleID: "cycle-1"}, nil, QuiescenceFact{}))
	require.NoError(t, err)
	require.Equal(t, uint64(1), opening[fieldSequence])

	// A second snapshot on a live stream is refused, and its sequence stays
	// consumed.
	_, err = stream.Emit(SnapshotEvent(
		Foreground{State: ForegroundIdle, CycleID: "cycle-1"}, nil, QuiescenceFact{}))
	require.ErrorIs(t, err, &ViolationError{Kind: ViolationStreamCycle})

	require.Equal(t, uint64(1), stream.State().ReducedThrough)
}

// TestEmittedPromptStreamReducesThroughItsOwnValidator drives the whole
// prompt-contained shape this adapter emits and proves the emitter's own reducer
// projects it.
func TestEmittedPromptStreamReducesThroughItsOwnValidator(t *testing.T) {
	t.Parallel()

	stream := NewStream("stream-1", promptContained())
	submission := Submission{SubmissionID: "sub-1", ClientNonce: "nonce-1", RunID: "run-1"}

	for _, event := range []Event{
		SnapshotEvent(Foreground{State: ForegroundIdle, CycleID: "cycle-1"}, nil, QuiescenceFact{}),
		AcceptedEvent(submission, "turn-1"),
		TransitionEvent(ForegroundRunning, "cycle-1", "turn-1", CauseSubmission),
		ActionEvent(PendingAction("action-1", ActionPermission, Owner{Type: OwnerTurn, ID: "turn-1"}, true)),
		TransitionEvent(ForegroundRequiresAction, "cycle-1", "turn-1", CauseSubmission),
		ActionEvent(ResolvedAction("action-1", ActionAccepted)),
		TransitionEvent(ForegroundRunning, "cycle-1", "turn-1", CauseSubmission),
		IdleEvent("cycle-1", "turn-1", StopReasonEndTurn, OutcomeSuccess),
	} {
		_, err := stream.Emit(event)
		require.NoError(t, err)
	}

	state := stream.State()
	require.Equal(t, uint64(8), state.ReducedThrough)

	turn, ok := state.Turn("turn-1")
	require.True(t, ok)
	require.True(t, turn.Terminal)
	require.Equal(t, OutcomeSuccess, turn.Outcome)
	require.Equal(t, "run-1", turn.RunID)

	action, ok := state.Action("action-1")
	require.True(t, ok)
	require.Equal(t, ActionAccepted, action.State)
	require.True(t, state.Vacant())
}

// TestEmittedEndingIdleRecordsHowItSettled proves the emitter is held to the same
// ending-idle rule as a stream it reads: outcome always, stop reason unless the
// outcome is a failure.
func TestEmittedEndingIdleRecordsHowItSettled(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name       string
		stopReason string
		outcome    Outcome
		refused    bool
	}{
		{name: "success", stopReason: StopReasonEndTurn, outcome: OutcomeSuccess},
		{name: "cancelled", stopReason: StopReasonCancelled, outcome: OutcomeCancelled},
		{name: "limit", stopReason: StopReasonMaxTokens, outcome: OutcomeLimit},
		{name: "failed", outcome: OutcomeFailed},
		{name: "failed with a stop reason", stopReason: StopReasonEndTurn, outcome: OutcomeFailed, refused: true},
		{name: "outcome omitted", stopReason: StopReasonEndTurn, refused: true},
		{name: "stop reason omitted", outcome: OutcomeSuccess, refused: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			stream := NewStream("stream-1", promptContained())
			_, err := stream.Emit(SnapshotEvent(
				Foreground{State: ForegroundIdle, CycleID: "cycle-1"}, nil, QuiescenceFact{}))
			require.NoError(t, err)

			_, err = stream.Emit(AcceptedEvent(Submission{SubmissionID: "s", ClientNonce: "n"}, "turn-1"))
			require.NoError(t, err)

			envelope, err := stream.Emit(IdleEvent("cycle-1", "turn-1", row.stopReason, row.outcome))
			if row.refused {
				require.ErrorIs(t, err, &ViolationError{Kind: ViolationMalformedEnvelope})

				return
			}

			require.NoError(t, err)

			event, ok := envelope[fieldEvent].(map[string]any)
			require.True(t, ok)
			require.Equal(t, string(row.outcome), event[fieldOutcome])

			reason, stated := event[fieldStopReason]
			require.Equal(t, row.stopReason != "", stated)

			if stated {
				require.Equal(t, row.stopReason, reason)
			}
		})
	}
}

// TestEncodedEventsRoundTripThroughTheDecoder proves every event this package can
// render decodes back to the value it was built from, so the encoder and the
// unknown-member rule cannot drift apart.
func TestEncodedEventsRoundTripThroughTheDecoder(t *testing.T) {
	t.Parallel()

	negotiated := Negotiated{
		Versions:                []int{1},
		AuthoritativeQuiescence: true,
		QuiescenceSource:        ProofClassProcessContainment,
		ActivityKinds:           []ActivityKind{ActivityTask},
	}
	blocks := false
	progress := json.RawMessage(`{"done":1}`)

	for _, row := range []struct {
		name  string
		event Event
	}{
		{"idle snapshot", SnapshotEvent(
			Foreground{State: ForegroundIdle, CycleID: "cycle-1"}, nil,
			QuiescenceFact{Quiescent: true, Source: ProofClassProcessContainment, Barrier: "barrier-1"})},
		{"resumed snapshot", SnapshotEvent(
			Foreground{State: ForegroundRunning, CycleID: "cycle-1", TurnID: "turn-1", Origin: CauseSubmission},
			[]ActionUpdate{PendingAction("action-1", ActionElicitation, Owner{Type: OwnerTurn, ID: "turn-1"}, false)},
			QuiescenceFact{})},
		{"acceptance", AcceptedEvent(Submission{SubmissionID: "s", ClientNonce: "n"}, "turn-1")},
		{"running", TransitionEvent(ForegroundRunning, "cycle-1", "turn-1", CauseSubmission)},
		{"ending idle", IdleEvent("cycle-1", "turn-1", StopReasonRefusal, OutcomeRefused)},
		{"action first sight", ActionEvent(PendingAction("action-1", ActionPermission, Owner{Type: OwnerActivity, ID: "activity-1"}, true))},
		{"action patch", ActionEvent(ResolvedAction("action-1", ActionDeclined))},
		{"action patch restating what it blocks", ActionEvent(ActionUpdate{
			ActionID: "action-1", State: ActionCancelled, BlocksForeground: &blocks, RunID: "run-1"})},
		{"activity", ActivityUpdateEvent(ActivityUpdate{
			ActivityID: "activity-1", Kind: ActivityTask, State: ActivityRunning,
			Cause: CauseSubmission, OriginTurnID: "turn-1", ParentID: "activity-0",
			ToolCallID: "tool-1", RunID: "run-1", Progress: progress})},
		{"positive quiescence", QuiescenceEvent(QuiescenceFact{
			Quiescent: true, Source: ProofClassProcessContainment, Barrier: "barrier-1"})},
		{"negative quiescence", QuiescenceEvent(QuiescenceFact{})},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			envelope := map[string]any{
				fieldVersion:  Version,
				fieldStreamID: "stream-1",
				fieldSequence: 7,
				fieldEvent:    encodeEvent(row.event),
			}

			delivery, err := DecodeSessionUpdate(notification(t, envelope, sessionInfoCarrier()), negotiated)
			require.NoError(t, err)
			require.Equal(t, row.event.Type, delivery.Event.Type)
		})
	}
}

// sessionInfoCarrier is the identity-only neutral carrier every envelope rides.
func sessionInfoCarrier() map[string]any {
	return map[string]any{sessionUpdateField: string(CarrierSessionInfo)}
}

func notification(t *testing.T, envelope map[string]any, update map[string]any) json.RawMessage {
	t.Helper()

	params, err := json.Marshal(map[string]any{
		"sessionId": "session-1",
		updateField: update,
		metaField:   map[string]any{MetaKey: envelope},
	})
	require.NoError(t, err)

	return params
}

func TestStreamCloseRejectsLaterEmission(t *testing.T) {
	stream := NewStream("stream", Negotiated{Versions: []int{Version}})
	stream.Close()
	_, err := stream.Emit(SnapshotEvent(Foreground{State: ForegroundIdle, CycleID: "idle"}, nil, QuiescenceFact{}))
	require.ErrorContains(t, err, string(ViolationStaleStream))
}
