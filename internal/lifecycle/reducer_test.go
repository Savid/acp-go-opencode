package lifecycle

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// reduction drives deliveries straight into a reducer, so a test can assert a
// rule the wire decoder would have refused first and can reach the payload-shape
// guards the decoder never produces.
type reduction struct {
	reducer  *Reducer
	sequence uint64
	streamID string
}

func newReduction(negotiated Negotiated) *reduction {
	return &reduction{reducer: NewReducer(Options{Negotiated: negotiated}), streamID: "stream-1"}
}

func (r *reduction) push(event Event) error {
	r.sequence++

	return r.reducer.Reduce(Delivery{
		StreamID: r.streamID,
		Sequence: r.sequence,
		Carrier:  CarrierSessionInfo,
		Event:    event,
	})
}

// open starts the stream on an idle foreground with nothing live.
func (r *reduction) open(t *testing.T) {
	t.Helper()

	require.NoError(t, r.push(SnapshotEvent(
		Foreground{State: ForegroundIdle, CycleID: "cycle-1"}, nil, QuiescenceFact{})))
}

// openTurn opens the stream and accepts one prompt-origin turn.
func (r *reduction) openTurn(t *testing.T) {
	t.Helper()

	r.open(t)
	require.NoError(t, r.push(AcceptedEvent(Submission{SubmissionID: "s", ClientNonce: "n"}, "turn-1")))
	require.NoError(t, r.push(TransitionEvent(ForegroundRunning, "cycle-1", "turn-1", CauseSubmission)))
}

func fullyProven() Negotiated {
	return Negotiated{
		Versions:                []int{1},
		AuthoritativeQuiescence: true,
		QuiescenceSource:        ProofClassProcessContainment,
		ActivityKinds:           []ActivityKind{ActivityTask, ActivitySubagent},
	}
}

// TestReducerLatchesOnItsFirstRefusal proves a stream that failed closed never
// reduces again and keeps reporting the verdict it already reached.
func TestReducerLatchesOnItsFirstRefusal(t *testing.T) {
	t.Parallel()

	r := newReduction(promptContained())
	require.Nil(t, r.reducer.Failed())

	err := r.push(TransitionEvent(ForegroundRunning, "cycle-1", "turn-1", CauseSubmission))
	require.ErrorIs(t, err, &ViolationError{Kind: ViolationDeltaBeforeSnapshot})

	latched := r.reducer.Failed()
	require.NotNil(t, latched)

	require.Equal(t, latched, r.push(SnapshotEvent(
		Foreground{State: ForegroundIdle, CycleID: "cycle-1"}, nil, QuiescenceFact{})))
	require.Equal(t, latched, r.reducer.ReduceSessionUpdate(
		frame(envelopeAround(`{"type":"prompt_accepted","submissionId":"a","clientNonce":"b","turnId":"c"}`), infoCarrier)))
	require.Empty(t, r.reducer.State().Turns)
}

// TestReducerRefusesAnIneligibleCarrierBeforeOrdering proves carrier legality is
// structural: a coalescable carrier is no evidence the sequence it claims was
// delivered, so there is nothing for an ordering check to be about.
func TestReducerRefusesAnIneligibleCarrierBeforeOrdering(t *testing.T) {
	t.Parallel()

	reducer := NewReducer(Options{Negotiated: promptContained()})
	err := reducer.Reduce(Delivery{
		StreamID: "stream-1",
		Sequence: 99,
		Carrier:  CarrierIneligible,
		Event:    SnapshotEvent(Foreground{State: ForegroundIdle, CycleID: "cycle-1"}, nil, QuiescenceFact{}),
	})
	require.ErrorIs(t, err, &ViolationError{Kind: ViolationIllegalCarrier})
}

// TestReducerRefusesAForeignDeltaAsStale proves only an opening snapshot may
// arrive on a stream identity the reducer has not seen.
func TestReducerRefusesAForeignDeltaAsStale(t *testing.T) {
	t.Parallel()

	r := newReduction(promptContained())
	r.open(t)

	err := r.reducer.Reduce(Delivery{
		StreamID: "stream-2",
		Sequence: 1,
		Carrier:  CarrierSessionInfo,
		Event:    TransitionEvent(ForegroundRunning, "cycle-2", "turn-2", CauseSubmission),
	})
	require.ErrorIs(t, err, &ViolationError{Kind: ViolationStaleStream})
}

// TestReducerRefusesAMissingPayload proves the reducer never trusts a
// discriminant without its payload, whatever produced the delivery.
func TestReducerRefusesAMissingPayload(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name  string
		event Event
		open  bool
	}{
		{name: "snapshot", event: Event{Type: EventSnapshot}},
		{name: "acceptance", event: Event{Type: EventPromptAccepted}, open: true},
		{name: "transition", event: Event{Type: EventStateUpdate}, open: true},
		{name: "activity", event: Event{Type: EventActivityUpdate}, open: true},
		{name: "action", event: Event{Type: EventActionUpdate}, open: true},
		{name: "quiescence", event: Event{Type: EventQuiescenceUpdate}, open: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			r := newReduction(fullyProven())
			if row.open {
				r.open(t)
			}

			require.ErrorIs(t, r.push(row.event), &ViolationError{Kind: ViolationMalformedEnvelope})
		})
	}
}

// TestReducerRefusesAnIncompleteSnapshotForeground proves a snapshot with no
// truthful foreground opens nothing.
func TestReducerRefusesAnIncompleteSnapshotForeground(t *testing.T) {
	t.Parallel()

	for _, foreground := range []Foreground{
		{State: "waiting", CycleID: "cycle-1"},
		{State: ForegroundIdle},
	} {
		r := newReduction(promptContained())
		require.ErrorIs(t, r.push(SnapshotEvent(foreground, nil, QuiescenceFact{})),
			&ViolationError{Kind: ViolationMalformedEnvelope})
		require.Nil(t, r.reducer.State().Foreground)
	}
}

// TestReducerRefusesAMisshapenSnapshotForegroundTurn proves the reducer holds an
// in-memory snapshot to the same turn and origin rules the wire decoder holds a
// received one to. An emitted snapshot never passes through the decoder, so a
// rule enforced only there would let this adapter emit a foreground it would
// refuse to read.
func TestReducerRefusesAMisshapenSnapshotForegroundTurn(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name       string
		foreground Foreground
	}{
		{
			name:       "idle naming a turn",
			foreground: Foreground{State: ForegroundIdle, CycleID: "cycle-1", TurnID: "turn-1", Origin: CauseSubmission},
		},
		{
			name:       "turn without an origin",
			foreground: Foreground{State: ForegroundRunning, CycleID: "cycle-1", TurnID: "turn-1"},
		},
		{
			name:       "origin without a turn",
			foreground: Foreground{State: ForegroundRunning, CycleID: "cycle-1", Origin: CauseSubmission},
		},
		{
			name:       "origin outside the closed pair",
			foreground: Foreground{State: ForegroundRunning, CycleID: "cycle-1", TurnID: "turn-1", Origin: Cause("close")},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			r := newReduction(promptContained())
			require.ErrorIs(t, r.push(SnapshotEvent(row.foreground, nil, QuiescenceFact{})),
				&ViolationError{Kind: ViolationMalformedEnvelope})
			require.Nil(t, r.reducer.State().Foreground)
			require.Empty(t, r.reducer.State().Turns)
		})
	}
}

// TestSnapshotActionSetIsJudgedWhole proves each rule the asserted action set is
// held to, and that a refused snapshot projects nothing.
func TestSnapshotActionSetIsJudgedWhole(t *testing.T) {
	t.Parallel()

	turnOwner := Owner{Type: OwnerTurn, ID: "turn-1"}
	blocking := PendingAction("action-1", ActionPermission, turnOwner, true)

	for _, row := range []struct {
		name    string
		actions []ActionUpdate
		kind    ViolationKind
	}{
		{
			name:    "incomplete first sight",
			actions: []ActionUpdate{{ActionID: "action-1", State: ActionPending, Owner: turnOwner}},
			kind:    ViolationMalformedEnvelope,
		},
		{
			name:    "terminal entry",
			actions: []ActionUpdate{PendingAction("action-1", ActionPermission, turnOwner, false)},
			kind:    ViolationMalformedEnvelope,
		},
		{
			name:    "owner not introduced",
			actions: []ActionUpdate{PendingAction("action-1", ActionPermission, Owner{Type: OwnerActivity, ID: "activity-9"}, false)},
			kind:    ViolationUnknownEntity,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			actions := row.actions
			if row.name == "terminal entry" {
				actions[0].State = ActionAccepted
			}

			r := newReduction(promptContained())
			err := r.push(SnapshotEvent(
				Foreground{State: ForegroundRunning, CycleID: "cycle-1", TurnID: "turn-1", Origin: CauseSubmission},
				actions, QuiescenceFact{}))
			require.ErrorIs(t, err, &ViolationError{Kind: row.kind})
			require.Empty(t, r.reducer.State().Actions)
			require.Nil(t, r.reducer.State().Foreground)
		})
	}

	r := newReduction(promptContained())
	require.NoError(t, r.push(SnapshotEvent(
		Foreground{State: ForegroundRequiresAction, CycleID: "cycle-1", TurnID: "turn-1", Origin: CauseSubmission},
		[]ActionUpdate{blocking}, QuiescenceFact{})))

	action, ok := r.reducer.State().Action("action-1")
	require.True(t, ok)
	require.True(t, action.BlocksForeground)
}

// TestTurnIdentityIsIntroducedOnce proves acceptance introduces its turn and a
// terminal turn never reopens, with the terminal token winning where both rules
// meet.
func TestTurnIdentityIsIntroducedOnce(t *testing.T) {
	t.Parallel()

	r := newReduction(promptContained())
	r.openTurn(t)

	require.ErrorIs(t,
		r.push(AcceptedEvent(Submission{SubmissionID: "s2", ClientNonce: "n2"}, "turn-1")),
		&ViolationError{Kind: ViolationImmutableIdentityChange})

	settled := newReduction(promptContained())
	settled.openTurn(t)
	require.NoError(t, settled.push(IdleEvent("cycle-1", "turn-1", StopReasonEndTurn, OutcomeSuccess)))

	for _, event := range []Event{
		AcceptedEvent(Submission{SubmissionID: "s2", ClientNonce: "n2"}, "turn-1"),
		TransitionEvent(ForegroundRunning, "cycle-2", "turn-1", CauseSubmission),
		IdleEvent("cycle-2", "turn-1", StopReasonEndTurn, OutcomeSuccess),
	} {
		reopened := newReduction(promptContained())
		reopened.openTurn(t)
		require.NoError(t, reopened.push(IdleEvent("cycle-1", "turn-1", StopReasonEndTurn, OutcomeSuccess)))
		require.ErrorIs(t, reopened.push(event), &ViolationError{Kind: ViolationPostTerminalMutation})
	}
}

// TestSessionCausedIdleEndsNoTurn proves an idle naming no turn reports the
// foreground without settling anything.
func TestSessionCausedIdleEndsNoTurn(t *testing.T) {
	t.Parallel()

	r := newReduction(promptContained())
	r.open(t)

	require.NoError(t, r.push(Event{Type: EventStateUpdate, State: &StateTransition{
		State: ForegroundIdle, CycleID: "cycle-2", Cause: CauseSession,
	}}))
	require.Equal(t, "cycle-2", r.reducer.State().Foreground.CycleID)
	require.Empty(t, r.reducer.State().Turns)
}

// TestUnknownTurnReferencesFailClosed proves an ending or session-caused
// transition never invents the turn it names.
func TestUnknownTurnReferencesFailClosed(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name  string
		event Event
	}{
		{"ending idle", IdleEvent("cycle-1", "ghost", StopReasonEndTurn, OutcomeSuccess)},
		{"session-caused running", Event{Type: EventStateUpdate, State: &StateTransition{
			State: ForegroundRunning, CycleID: "cycle-1", TurnID: "ghost", Cause: CauseSession}}},
		{"activity-caused requires action", Event{Type: EventStateUpdate, State: &StateTransition{
			State: ForegroundRequiresAction, CycleID: "cycle-1", TurnID: "ghost", Cause: CauseActivity}}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			r := newReduction(promptContained())
			r.open(t)
			require.Error(t, r.push(row.event))
		})
	}
}

// TestActivityImmutableIdentityIsFixedOnFirstSight proves every immutable member
// is compared against its first-sight value, and that a patch may restate one.
func TestActivityImmutableIdentityIsFixedOnFirstSight(t *testing.T) {
	t.Parallel()

	firstSight := ActivityUpdate{
		ActivityID: "activity-1", Kind: ActivityTask, State: ActivityRunning,
		Cause: CauseSubmission, OriginTurnID: "turn-1", ToolCallID: "tool-1", RunID: "run-1",
	}

	for _, row := range []struct {
		name  string
		patch ActivityUpdate
	}{
		{"kind", ActivityUpdate{ActivityID: "activity-1", State: ActivityRunning, Kind: ActivitySubagent}},
		{"parent", ActivityUpdate{ActivityID: "activity-1", State: ActivityRunning, ParentID: "activity-9"}},
		{"tool link", ActivityUpdate{ActivityID: "activity-1", State: ActivityRunning, ToolCallID: "tool-9"}},
		{"cause", ActivityUpdate{ActivityID: "activity-1", State: ActivityRunning, Cause: CauseSession}},
		{"origin turn", ActivityUpdate{ActivityID: "activity-1", State: ActivityRunning, OriginTurnID: "turn-9"}},
		{"ownership root", ActivityUpdate{ActivityID: "activity-1", State: ActivityRunning, RunID: "run-9"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			r := newReduction(fullyProven())
			r.openTurn(t)
			require.NoError(t, r.push(ActivityUpdateEvent(firstSight)))
			require.ErrorIs(t, r.push(ActivityUpdateEvent(row.patch)),
				&ViolationError{Kind: ViolationImmutableIdentityChange})
		})
	}

	r := newReduction(fullyProven())
	r.openTurn(t)
	require.NoError(t, r.push(ActivityUpdateEvent(firstSight)))

	restated := firstSight
	restated.State = ActivityRequiresAction
	restated.Progress = json.RawMessage(`{"step":2}`)
	require.NoError(t, r.push(ActivityUpdateEvent(restated)))

	activity, ok := r.reducer.State().Activity("activity-1")
	require.True(t, ok)
	require.Equal(t, ActivityRequiresAction, activity.State)
	require.JSONEq(t, `{"step":2}`, string(activity.Progress))
	require.False(t, r.reducer.State().Vacant())
}

// TestParentTerminalizesAfterEveryOwnedEntity proves an owned action holds its
// parent activity open exactly as an owned child does.
func TestParentTerminalizesAfterEveryOwnedEntity(t *testing.T) {
	t.Parallel()

	r := newReduction(fullyProven())
	r.openTurn(t)
	require.NoError(t, r.push(ActivityUpdateEvent(ActivityUpdate{
		ActivityID: "activity-1", Kind: ActivityTask, State: ActivityRunning,
		Cause: CauseSubmission, OriginTurnID: "turn-1",
	})))
	require.NoError(t, r.push(ActionEvent(PendingAction(
		"action-1", ActionElicitation, Owner{Type: OwnerActivity, ID: "activity-1"}, false))))

	require.ErrorIs(t, r.push(ActivityUpdateEvent(ActivityUpdate{
		ActivityID: "activity-1", State: ActivityCompleted,
	})), &ViolationError{Kind: ViolationParentTerminalBeforeChild})
}

// TestActionImmutableMembersAreFixedOnFirstSight proves a later patch restates an
// immutable member only with its first-sight value, and that a resolved action
// never changes again.
func TestActionImmutableMembersAreFixedOnFirstSight(t *testing.T) {
	t.Parallel()

	blocks := true
	other := false
	owner := Owner{Type: OwnerTurn, ID: "turn-1"}

	for _, row := range []struct {
		name  string
		patch ActionUpdate
	}{
		{"kind", ActionUpdate{ActionID: "action-1", State: ActionAccepted, Kind: ActionElicitation}},
		{"owner", ActionUpdate{ActionID: "action-1", State: ActionAccepted, Owner: Owner{Type: OwnerActivity, ID: "activity-1"}}},
		{"ownership root", ActionUpdate{ActionID: "action-1", State: ActionAccepted, RunID: "run-9"}},
		{"what it blocks", ActionUpdate{ActionID: "action-1", State: ActionAccepted, BlocksForeground: &other}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			r := newReduction(promptContained())
			r.openTurn(t)
			require.NoError(t, r.push(ActionEvent(ActionUpdate{
				ActionID: "action-1", Kind: ActionPermission, State: ActionPending,
				Owner: owner, BlocksForeground: &blocks,
			})))
			require.NoError(t, r.push(TransitionEvent(ForegroundRequiresAction, "cycle-1", "turn-1", CauseSubmission)))
			require.ErrorIs(t, r.push(ActionEvent(row.patch)),
				&ViolationError{Kind: ViolationImmutableIdentityChange})
		})
	}

	r := newReduction(promptContained())
	r.openTurn(t)
	require.NoError(t, r.push(ActionEvent(PendingAction("action-1", ActionPermission, owner, false))))
	require.NoError(t, r.push(ActionEvent(ResolvedAction("action-1", ActionDeclined))))
	require.ErrorIs(t, r.push(ActionEvent(ResolvedAction("action-1", ActionCancelled))),
		&ViolationError{Kind: ViolationPostTerminalMutation})

	// Restating the state a resolved action already reached changes nothing.
	restated := newReduction(promptContained())
	restated.openTurn(t)
	require.NoError(t, restated.push(ActionEvent(PendingAction("action-1", ActionPermission, owner, false))))
	require.NoError(t, restated.push(ActionEvent(ResolvedAction("action-1", ActionDeclined))))
	require.NoError(t, restated.push(ActionEvent(ResolvedAction("action-1", ActionDeclined))))
}

// TestAnActionResolvedOnFirstSightNeverBlocks proves an action whose first sight is
// already terminal records nothing to release.
func TestAnActionResolvedOnFirstSightNeverBlocks(t *testing.T) {
	t.Parallel()

	blocks := true

	r := newReduction(promptContained())
	r.openTurn(t)
	require.NoError(t, r.push(ActionEvent(ActionUpdate{
		ActionID: "action-1", Kind: ActionPermission, State: ActionCancelled,
		Owner: Owner{Type: OwnerTurn, ID: "turn-1"}, BlocksForeground: &blocks,
	})))
	require.NoError(t, r.push(IdleEvent("cycle-1", "turn-1", StopReasonCancelled, OutcomeCancelled)))
	require.True(t, r.reducer.State().Vacant())
}

// TestQuiescenceCertifiesOnlyWhatTheStreamProves pins the certification rules the
// battery's positive vectors do not reach directly.
func TestQuiescenceCertifiesOnlyWhatTheStreamProves(t *testing.T) {
	t.Parallel()

	r := newReduction(fullyProven())
	r.openTurn(t)
	require.NoError(t, r.push(IdleEvent("cycle-1", "turn-1", StopReasonEndTurn, OutcomeSuccess)))

	// A watermark that stops short of the last recorded work is a lie about the
	// boundary rather than a weaker claim.
	require.ErrorIs(t, r.push(QuiescenceEvent(QuiescenceFact{
		Quiescent: true, Source: ProofClassProcessContainment, Watermark: 1,
	})), &ViolationError{Kind: ViolationFalseQuiescence})

	certified := newReduction(fullyProven())
	certified.openTurn(t)
	require.NoError(t, certified.push(IdleEvent("cycle-1", "turn-1", StopReasonEndTurn, OutcomeSuccess)))
	require.NoError(t, certified.push(QuiescenceEvent(QuiescenceFact{
		Quiescent: true, Source: ProofClassProcessContainment, Watermark: 4, Barrier: "barrier-1",
	})))

	state := certified.reducer.State()
	require.True(t, state.Quiescence.Certified)
	require.Equal(t, "barrier-1", state.Quiescence.Barrier)

	// A negative fact revokes the boundary that stood.
	require.NoError(t, certified.push(QuiescenceEvent(QuiescenceFact{})))
	require.False(t, certified.reducer.State().Quiescence.Certified)
	require.Equal(t, uint64(6), certified.reducer.State().Quiescence.InvalidatedAt)

	// A second negative fact against no certification records nothing new.
	require.NoError(t, certified.push(QuiescenceEvent(QuiescenceFact{})))
	require.Equal(t, uint64(6), certified.reducer.State().Quiescence.InvalidatedAt)
}

// TestStateAccessorsReportAbsence proves the projection answers for an identity it
// never saw rather than inventing one.
func TestStateAccessorsReportAbsence(t *testing.T) {
	t.Parallel()

	var state State

	_, ok := state.Activity("activity-1")
	require.False(t, ok)

	_, ok = state.Action("action-1")
	require.False(t, ok)

	_, ok = state.Turn("turn-1")
	require.False(t, ok)
	require.True(t, state.Vacant())
}

// TestQuiescenceProjectionShape proves the projected certification carries the
// proof it stands on, or the sequence that revoked it, and never both.
func TestQuiescenceProjectionShape(t *testing.T) {
	t.Parallel()

	certified, err := json.Marshal(QuiescenceState{
		Certified: true, Source: ProofClassNativeSettledBarrier, Watermark: 3,
	})
	require.NoError(t, err)
	require.JSONEq(t, `{"certified":true,"source":"native-settled-barrier","watermark":3}`, string(certified))

	fresh, err := json.Marshal(QuiescenceState{})
	require.NoError(t, err)
	require.JSONEq(t, `{"certified":false}`, string(fresh))
}

// TestClosedVocabularyRejectsUnknownValues proves every closed enumeration answers
// no to a value outside it.
func TestClosedVocabularyRejectsUnknownValues(t *testing.T) {
	t.Parallel()

	require.False(t, ForegroundState("waiting").Valid())
	require.False(t, Cause("native").Valid())
	require.False(t, Outcome("finished").Valid())
	require.False(t, ActivityKind("job").Valid())
	require.False(t, ActivityState("waiting").Valid())
	require.False(t, ActionKind("approval").Valid())
	require.False(t, ActionState("answered").Valid())
	require.False(t, OwnerType("session").Valid())
	require.False(t, ProofClass("prompt-return").Valid())
	require.False(t, ValidStopReason("done"))
	require.False(t, ActivityState("waiting").Terminal())
	require.True(t, ActionState("accepted").Terminal())
}

// TestSnapshotEntrySetsAreDecodedStrictly proves a set entry that is not an object
// is refused rather than read as an empty entity.
func TestSnapshotEntrySetsAreDecodedStrictly(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name  string
		value string
	}{
		{"activity entry", envelopeAround(`{"type":"lifecycle_snapshot","foreground":{"state":"idle","cycleId":"c"},` +
			`"activities":[7],"actions":[],"quiescence":{"quiescent":false}}`)},
		{"action entry", envelopeAround(`{"type":"lifecycle_snapshot","foreground":{"state":"idle","cycleId":"c"},` +
			`"activities":[],"actions":[7],"quiescence":{"quiescent":false}}`)},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			_, err := DecodeSessionUpdate(frame(row.value, infoCarrier), decodeNegotiated())
			require.ErrorIs(t, err, &ViolationError{Kind: ViolationMalformedEnvelope})
		})
	}
}

// TestSnapshotActivityIdentityIsJudgedWhole proves a snapshot's own activity set is
// held to the first-sight identity rule.
func TestSnapshotActivityIdentityIsJudgedWhole(t *testing.T) {
	t.Parallel()

	incomplete := newReduction(fullyProven())
	err := incomplete.reducer.Reduce(Delivery{
		StreamID: "stream-1",
		Sequence: 1,
		Carrier:  CarrierSessionInfo,
		Event: Event{Type: EventSnapshot, Snapshot: &Snapshot{
			Foreground: Foreground{State: ForegroundRunning, CycleID: "cycle-1", TurnID: "turn-1", Origin: CauseSubmission},
			Activities: []ActivityUpdate{{ActivityID: "activity-1", State: ActivityRunning}},
		}},
	})
	require.ErrorIs(t, err, &ViolationError{Kind: ViolationImmutableIdentityChange})
}

// TestSnapshotProjectsItsOwnActivitySet proves the emitted snapshot renders the
// nonterminal activity set and the reducer resolves later patches against it.
func TestSnapshotProjectsItsOwnActivitySet(t *testing.T) {
	t.Parallel()

	stream := NewStream("stream-1", fullyProven())
	envelope, err := stream.Emit(Event{Type: EventSnapshot, Snapshot: &Snapshot{
		Foreground: Foreground{State: ForegroundRunning, CycleID: "cycle-1", TurnID: "turn-1", Origin: CauseActivity},
		Activities: []ActivityUpdate{{
			ActivityID: "activity-1", Kind: ActivityTask, State: ActivityRunning,
			Cause: CauseActivity, OriginTurnID: "turn-1",
		}},
	}})
	require.NoError(t, err)

	event, ok := envelope[fieldEvent].(map[string]any)
	require.True(t, ok)
	require.Len(t, event[fieldActivities], 1)

	activity, ok := stream.State().Activity("activity-1")
	require.True(t, ok)
	require.Equal(t, ActivityRunning, activity.State)
	require.False(t, stream.State().Vacant())
}

// TestQuiescenceRefusesAnUnclaimedProofClass proves a class the answer never
// claimed is refused before the fact's content is judged.
func TestQuiescenceRefusesAnUnclaimedProofClass(t *testing.T) {
	t.Parallel()

	r := newReduction(fullyProven())
	r.open(t)

	require.ErrorIs(t, r.push(QuiescenceEvent(QuiescenceFact{
		Quiescent: true, Source: ProofClassNativeSettledBarrier, Watermark: 1,
	})), &ViolationError{Kind: ViolationUnnegotiatedFact})
}

// TestPendingActionPatchKeepsTheBoundaryRevoked proves a patch that leaves an
// action pending is still live work.
func TestPendingActionPatchKeepsTheBoundaryRevoked(t *testing.T) {
	t.Parallel()

	r := newReduction(promptContained())
	r.openTurn(t)
	require.NoError(t, r.push(ActionEvent(PendingAction(
		"action-1", ActionPermission, Owner{Type: OwnerTurn, ID: "turn-1"}, false))))
	require.NoError(t, r.push(ActionEvent(ResolvedAction("action-1", ActionPending))))

	action, ok := r.reducer.State().Action("action-1")
	require.True(t, ok)
	require.Equal(t, ActionPending, action.State)
	require.False(t, r.reducer.State().Vacant())
}

// TestVacancyRequiresEveryOwnedEntityTerminal proves a live activity and a pending
// action each keep the projection non-vacant on their own.
func TestVacancyRequiresEveryOwnedEntityTerminal(t *testing.T) {
	t.Parallel()

	live := State{
		Foreground: &Foreground{State: ForegroundIdle, CycleID: "cycle-1"},
		Activities: []ActivityRecord{{ActivityID: "activity-1", State: ActivityRunning}},
	}
	require.False(t, live.Vacant())

	held := State{
		Foreground: &Foreground{State: ForegroundIdle, CycleID: "cycle-1"},
		Activities: []ActivityRecord{{ActivityID: "activity-1", State: ActivityCompleted}},
		Actions:    []ActionRecord{{ActionID: "action-1", State: ActionPending}},
	}
	require.False(t, held.Vacant())

	settled := held
	settled.Actions = []ActionRecord{{ActionID: "action-1", State: ActionCancelled}}
	require.True(t, settled.Vacant())
}

// restatedAction rebuilds one action's full first-sight shape, which is what a
// native source replaying its own state emits rather than a minimal patch.
func restatedAction(state ActionState, blocks bool) ActionUpdate {
	return ActionUpdate{
		ActionID:         "action-1",
		Kind:             ActionPermission,
		State:            state,
		Owner:            Owner{Type: OwnerTurn, ID: "turn-1"},
		BlocksForeground: &blocks,
	}
}

// TestTerminalActionRestatementIsSuppressed proves a resolved action restated
// member-for-member says nothing: the sequence is consumed, the projection is
// untouched, and the restatement records no work, so a proof whose watermark
// covers the terminal idle but stops below it still certifies the boundary.
func TestTerminalActionRestatementIsSuppressed(t *testing.T) {
	t.Parallel()

	r := newReduction(fullyProven())
	r.openTurn(t)
	require.NoError(t, r.push(ActionEvent(restatedAction(ActionPending, false))))
	require.NoError(t, r.push(ActionEvent(ResolvedAction("action-1", ActionAccepted))))
	require.NoError(t, r.push(IdleEvent("cycle-1", "turn-1", "end_turn", OutcomeSuccess)))

	settled := r.sequence
	require.NoError(t, r.push(ActionEvent(restatedAction(ActionAccepted, false))))

	state := r.reducer.State()
	require.Equal(t, r.sequence, state.ReducedThrough)
	require.Zero(t, state.SuppressedRetransmissions)

	action, ok := state.Action("action-1")
	require.True(t, ok)
	require.Equal(t, ActionAccepted, action.State)

	require.NoError(t, r.push(QuiescenceEvent(QuiescenceFact{
		Quiescent: true, Source: ProofClassProcessContainment, Watermark: settled,
	})))
	require.True(t, r.reducer.State().Quiescence.Certified)
}

// TestTerminalActionDifferenceIsRefused proves every member a restatement carries
// counts, and that the terminal token wins: a delivery that both restates a
// resolved action and changes an immutable reports post_terminal_mutation rather
// than the immutable-identity token a live action would have earned.
func TestTerminalActionDifferenceIsRefused(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name    string
		carried ActionUpdate
	}{
		{"state", restatedAction(ActionCancelled, false)},
		{"blocking claim", restatedAction(ActionAccepted, true)},
		{"kind", ActionUpdate{ActionID: "action-1", Kind: ActionElicitation, State: ActionAccepted}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			r := newReduction(fullyProven())
			r.openTurn(t)
			require.NoError(t, r.push(ActionEvent(restatedAction(ActionPending, false))))
			require.NoError(t, r.push(ActionEvent(ResolvedAction("action-1", ActionAccepted))))

			require.ErrorIs(t, r.push(ActionEvent(row.carried)),
				&ViolationError{Kind: ViolationPostTerminalMutation})
		})
	}
}

// TestLifecycleValueEqualityComparesNumbersExactly proves the number predicate the
// whole extension compares by: spelling is not content, every zero is one value,
// and two integers beyond double precision that differ in any digit are unequal.
// A huge exponent is decided from the lexeme's normalized form, so the pair below
// costs the digits it is written with rather than the value it names.
func TestLifecycleValueEqualityComparesNumbersExactly(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		left  string
		right string
		equal bool
	}{
		{"1", "1.0", true},
		{"1", "1.00000", true},
		{"100", "1e2", true},
		{"0.001", "1E-3", true},
		{"-0", "0", true},
		{"0.0", "-0.000", true},
		{"1e999999999", "10e999999998", true},
		{"1234567890123456788", "1234567890123456789", false},
		{"1", "-1", false},
		{"1e999999999", "1e999999998", false},
		{"12", "1.2", false},
	} {
		t.Run(row.left+" vs "+row.right, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, row.equal, equalNumber(row.left, row.right))
			require.Equal(t, row.equal, equalNumber(row.right, row.left))
			require.Equal(t, row.equal, equalJSONValue(
				json.Number(row.left), json.Number(row.right)))
		})
	}
}

// TestLifecycleValueEqualityJudgesWholeDecodedValues proves the predicate is deep
// and typed: key order is not a difference, a missing member is, and a number
// never equals the string spelling it.
func TestLifecycleValueEqualityJudgesWholeDecodedValues(t *testing.T) {
	t.Parallel()

	require.True(t, equalRawJSON(
		json.RawMessage(`{"a":1,"b":[1,2.0,{"c":null}]}`),
		json.RawMessage(`{"b":[1.0,2,{"c":null}],"a":1.000}`)))
	require.False(t, equalRawJSON(
		json.RawMessage(`{"a":1,"b":2}`), json.RawMessage(`{"a":1}`)))
	require.False(t, equalRawJSON(json.RawMessage(`{"a":1}`), json.RawMessage(`{"a":"1"}`)))
	require.False(t, equalRawJSON(json.RawMessage(`[1,2]`), json.RawMessage(`[2,1]`)))
	require.True(t, equalRawJSON(nil, nil))
	require.False(t, equalRawJSON(nil, json.RawMessage(`{}`)))
}

// TestLifecycleValueEqualityFallsBackToTheWrittenForm proves the predicate is
// total. Nothing on the wire reaches it undecodable — the decoder refuses a
// malformed member long before a comparison — but a helper that silently called
// two unreadable values equal would be the one way a conflicting duplicate could
// pass, so an unparsable lexeme and an undecodable member are compared as written.
func TestLifecycleValueEqualityFallsBackToTheWrittenForm(t *testing.T) {
	t.Parallel()

	require.True(t, equalNumber("1e", "1e"))
	require.False(t, equalNumber("1e", "1e2"))
	require.True(t, equalNumber(".", "."))
	require.False(t, equalNumber("", "0"))

	require.True(t, equalRawJSON(json.RawMessage(`{`), json.RawMessage(`{`)))
	require.False(t, equalRawJSON(json.RawMessage(`{`), json.RawMessage(`{"a":1}`)))
	require.False(t, equalJSONArray([]any{json.Number("1")}, []any{json.Number("1"), json.Number("2")}))
	require.False(t, equalJSONArray([]any{json.Number("1")}, json.Number("1")))
}

// TestRefusedReplacementSnapshotPreservesTheStandingProjection proves a snapshot
// opens nothing when it is refused, in the replacement position as much as the
// opening one. The reducer validates the whole replacement and swaps only on
// success, so a malformed successor leaves the superseded incarnation's
// projection exactly as it stood — and the refusal latches over that projection
// rather than over an empty one. A reader that terminalizes what it holds needs
// something held, and the superseded incarnation's work is the only truth anyone
// has when its would-be successor turns out to be unreadable.
func TestRefusedReplacementSnapshotPreservesTheStandingProjection(t *testing.T) {
	t.Parallel()

	r := newReduction(fullyProven())
	r.open(t)
	require.NoError(t, r.push(AcceptedEvent(Submission{SubmissionID: "sub-1", ClientNonce: "nonce-1"}, "turn-1")))
	require.NoError(t, r.push(ActionEvent(PendingAction("action-1", ActionPermission, Owner{Type: OwnerTurn, ID: "turn-1"}, true))))

	standing := r.reducer.State()
	require.Equal(t, "stream-1", standing.StreamID)
	require.NotEmpty(t, standing.Turns)
	require.NotEmpty(t, standing.Actions)

	err := r.reducer.Reduce(Delivery{
		StreamID: "stream-2",
		Sequence: 1,
		Carrier:  CarrierSessionInfo,
		// A foreground naming no cycle is refused by the whole-snapshot check.
		Event: SnapshotEvent(Foreground{State: ForegroundIdle}, nil, QuiescenceFact{}),
	})
	require.ErrorIs(t, err, &ViolationError{Kind: ViolationMalformedEnvelope})

	require.Equal(t, standing, r.reducer.State(),
		"the refused replacement retired the standing projection before validating itself")

	latched := r.reducer.Failed()
	require.NotNil(t, latched)
	require.Equal(t, "stream-2", latched.StreamID,
		"the latch named the projection's stream rather than the frame that failed closed")
	require.Equal(t, uint64(1), latched.Sequence)

	// The latch is the reducer's: every later delivery, on either identity,
	// answers with it and the projection never moves again.
	require.ErrorIs(t, r.push(ActionEvent(ResolvedAction("action-1", ActionAccepted))), latched)
	require.Equal(t, standing, r.reducer.State())
}

// TestAcceptedReplacementSnapshotSupersedesTheStandingProjection proves the
// success half of the same swap: a replacement that validates whole takes over,
// adopts nothing from the incarnation it supersedes, and retires that identity so
// it never opens again.
func TestAcceptedReplacementSnapshotSupersedesTheStandingProjection(t *testing.T) {
	t.Parallel()

	r := newReduction(fullyProven())
	r.open(t)
	require.NoError(t, r.push(AcceptedEvent(Submission{SubmissionID: "sub-1", ClientNonce: "nonce-1"}, "turn-1")))

	require.NoError(t, r.reducer.Reduce(Delivery{
		StreamID: "stream-2",
		Sequence: 7,
		Carrier:  CarrierSessionInfo,
		Event:    SnapshotEvent(Foreground{State: ForegroundIdle, CycleID: "cycle-2"}, nil, QuiescenceFact{}),
	}))

	replaced := r.reducer.State()
	require.Equal(t, "stream-2", replaced.StreamID)
	require.Empty(t, replaced.Turns, "the replacement adopted work from the incarnation it superseded")
	require.Equal(t, uint64(7), replaced.ReducedThrough)

	err := r.reducer.Reduce(Delivery{
		StreamID: "stream-1",
		Sequence: 1,
		Carrier:  CarrierSessionInfo,
		Event:    SnapshotEvent(Foreground{State: ForegroundIdle, CycleID: "cycle-1"}, nil, QuiescenceFact{}),
	})
	require.ErrorIs(t, err, &ViolationError{Kind: ViolationStaleStream})
}

// TestReducerJudgesTheEndingIdleItIsGivenDirectly proves the ending-idle rule is
// the reducer's own and not merely the decoder's. The rule holds wherever a
// delivery comes from: the wire decoder refuses a malformed ending transition
// before the reducer sees it, so this drives the reducer directly and pins the
// rule at the layer that projects the turn.
func TestReducerJudgesTheEndingIdleItIsGivenDirectly(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name       string
		stopReason string
		outcome    Outcome
	}{
		{name: "outcome omitted", stopReason: StopReasonEndTurn},
		{name: "failed with a stop reason", stopReason: StopReasonEndTurn, outcome: OutcomeFailed},
		{name: "stop reason omitted", outcome: OutcomeSuccess},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			r := newReduction(promptContained())
			r.open(t)
			require.NoError(t, r.push(AcceptedEvent(
				Submission{SubmissionID: "sub-1", ClientNonce: "nonce-1"}, "turn-1")))

			require.ErrorIs(t,
				r.push(IdleEvent("cycle-1", "turn-1", row.stopReason, row.outcome)),
				&ViolationError{Kind: ViolationMalformedEnvelope})
		})
	}
}
