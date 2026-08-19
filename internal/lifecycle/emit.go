package lifecycle

import "encoding/json"

// Stream is one incarnation's ordered emitter. It claims a sequence before
// delivery is attempted, so a lost or refused event leaves a detectable gap
// rather than a silently contiguous stream, and it reduces every event through
// the same reducer the fixture battery drives, so a stream this adapter could not
// support fails at the point of emission instead of at its consumers.
//
// A Stream is not safe for concurrent use; one prompt owns its incarnation and
// emits from the goroutine that settles it.
type Stream struct {
	id       string
	reducer  *Reducer
	sequence uint64
}

// NewStream opens an incarnation identified by id. The identity names one native
// lifecycle source lifetime: it never rotates while that source survives, and it
// never outlives it.
func NewStream(id string, negotiated Negotiated) *Stream {
	return &Stream{id: id, reducer: NewReducer(Options{Negotiated: negotiated})}
}

// ID reports the incarnation this stream speaks for.
func (s *Stream) ID() string { return s.id }

// State returns the projection the emitted stream proves.
func (s *Stream) State() State { return s.reducer.State() }

// Negotiated reports the facts this stream is allowed to state.
func (s *Stream) Negotiated() Negotiated { return s.reducer.Negotiated() }

// Close fences this incarnation after process containment. No later event may
// be emitted from the native source.
func (s *Stream) Close() { s.reducer.Close() }

// Emit claims the next sequence, renders the envelope for the notification's
// `_meta`, and validates what it rendered. A refused event is never handed back
// and its sequence stays consumed, which is exactly the detectable gap the
// ordering rule wants.
//
// The self-validation runs on the rendered bytes rather than on the in-process
// value: "the envelopes this adapter emits are well formed" is a claim about
// what goes on the wire, so the notification is marshalled, decoded, and reduced
// by the exact path a consumer takes. Reducing the struct instead proves the
// struct well formed and leaves every encoder infidelity between the two —
// a dropped member, a mistyped one, a patch rendered as a first sight — to be
// discovered by the host.
func (s *Stream) Emit(event Event) (map[string]any, error) {
	// The payload is judged before the sequence claim, so a caller defect
	// neither burns a sequence nor dereferences a payload that is not there.
	// The verdicts mirror the decoder's: an unknown discriminant is the
	// discriminant's violation, a known one without its payload is shape.
	if !event.payloadMatchesType() {
		if !knownEventType(event.Type) {
			return nil, violation(ViolationUnknownEventType, s.id, s.sequence+1,
				"event type "+string(event.Type))
		}

		return nil, violation(ViolationMalformedEnvelope, s.id, s.sequence+1,
			"event payload does not match type "+string(event.Type))
	}

	s.sequence++

	envelope := map[string]any{
		fieldVersion:  Version,
		fieldStreamID: s.id,
		fieldSequence: s.sequence,
		fieldEvent:    encodeEvent(event),
	}

	// A rendered envelope holds only JSON-safe values, and an opaque member this
	// step could not render fails the decode below as a malformed envelope
	// rather than escaping as an untyped error.
	params, _ := json.Marshal(map[string]any{
		metaField:   map[string]any{MetaKey: envelope},
		updateField: map[string]any{sessionUpdateField: string(CarrierSessionInfo)},
	})

	delivery, err := DecodeSessionUpdate(params, s.reducer.Negotiated())
	if err != nil {
		return nil, err
	}

	if err := s.reducer.Reduce(delivery); err != nil {
		return nil, err
	}

	return envelope, nil
}

// knownEventType reports membership of the closed set of six.
func knownEventType(kind EventType) bool {
	switch kind {
	case EventSnapshot, EventPromptAccepted, EventStateUpdate,
		EventActivityUpdate, EventActionUpdate, EventQuiescenceUpdate:
		return true
	default:
		return false
	}
}

// payloadMatchesType reports that this event carries exactly the payload its
// discriminant names. The decoder is the only other producer of an Event and it
// builds the pair together, so this guards the in-process construction paths the
// wire never reaches.
func (e Event) payloadMatchesType() bool {
	switch e.Type {
	case EventSnapshot:
		return e.Snapshot != nil
	case EventPromptAccepted:
		return e.PromptAccepted != nil
	case EventStateUpdate:
		return e.State != nil
	case EventActivityUpdate:
		return e.Activity != nil
	case EventActionUpdate:
		return e.Action != nil
	case EventQuiescenceUpdate:
		return e.Quiescence != nil
	default:
		return false
	}
}

// SnapshotEvent opens a stream from the whole state this adapter can state
// truthfully: the foreground cycle, the complete nonterminal action set the
// action registry holds, and whatever quiescence fact the configuration's proof
// class actually established.
func SnapshotEvent(foreground Foreground, actions []ActionUpdate, quiescence QuiescenceFact) Event {
	return Event{Type: EventSnapshot, Snapshot: &Snapshot{
		Foreground: foreground,
		Actions:    actions,
		Quiescence: quiescence,
	}}
}

// AcceptedEvent records that the native dispatcher took durable ownership of a
// submitted frame. The submission identity is echoed verbatim from the prompt's
// correlation value.
func AcceptedEvent(submission Submission, turnID string) Event {
	return Event{Type: EventPromptAccepted, PromptAccepted: &PromptAccepted{
		SubmissionID: submission.SubmissionID,
		ClientNonce:  submission.ClientNonce,
		TurnID:       turnID,
		RunID:        submission.RunID,
	}}
}

// TransitionEvent reports one live foreground transition — running or
// requires_action — for the named cycle and turn. The cause is stated rather
// than assumed: an activity-caused running transition naming a turn the stream
// has not introduced is the only event other than acceptance that opens one.
func TransitionEvent(state ForegroundState, cycleID, turnID string, cause Cause) Event {
	return Event{Type: EventStateUpdate, State: &StateTransition{
		State:   state,
		CycleID: cycleID,
		TurnID:  turnID,
		Cause:   cause,
	}}
}

// IdleEvent ends one cycle, carrying the turn's truthful stop reason and recorded
// outcome. A failed outcome carries no stop reason: no ACP v1 stop reason names a
// failure and the v1 error carries it instead.
func IdleEvent(cycleID, turnID, stopReason string, outcome Outcome) Event {
	return Event{Type: EventStateUpdate, State: &StateTransition{
		State:      ForegroundIdle,
		CycleID:    cycleID,
		TurnID:     turnID,
		Cause:      CauseSubmission,
		StopReason: stopReason,
		Outcome:    outcome,
	}}
}

// ActionEvent reports one action's first sight or one later state patch. A first
// sight carries every member that fixes what the action is; a patch carries the
// identity and the state it reached.
func ActionEvent(action ActionUpdate) Event {
	return Event{Type: EventActionUpdate, Action: &action}
}

// ActivityUpdateEvent reports one activity. This adapter proves no activity kind,
// so nothing here emits one; the constructor exists because the same package
// validates streams it reads.
func ActivityUpdateEvent(activity ActivityUpdate) Event {
	return Event{Type: EventActivityUpdate, Activity: &activity}
}

// QuiescenceEvent states the authoritative quiescence fact a completed proof
// produced. It carries the proof class and the watermark that proof covers, never
// a guess, a heuristic, or a confidence.
func QuiescenceEvent(fact QuiescenceFact) Event {
	return Event{Type: EventQuiescenceUpdate, Quiescence: &fact}
}

// PendingAction builds one action's first sight for the registry's snapshot and
// announcement paths. blocksForeground is stated rather than defaulted, because
// an omitted member would silently demote a blocking request to a background one.
func PendingAction(actionID string, kind ActionKind, owner Owner, blocksForeground bool) ActionUpdate {
	blocks := blocksForeground

	return ActionUpdate{
		ActionID:         actionID,
		Kind:             kind,
		State:            ActionPending,
		Owner:            owner,
		BlocksForeground: &blocks,
	}
}

// ResolvedAction builds one action's terminal patch.
func ResolvedAction(actionID string, state ActionState) ActionUpdate {
	return ActionUpdate{ActionID: actionID, State: state}
}

// encodeEvent renders every event in the closed set. The reducer reduces all six
// because it is also the validator for this adapter's own emitted stream, and the
// encoder covers all six for the same reason.
func encodeEvent(event Event) map[string]any {
	switch event.Type {
	case EventSnapshot:
		return encodeSnapshot(*event.Snapshot)
	case EventPromptAccepted:
		return withOptional(map[string]any{
			fieldType:         string(EventPromptAccepted),
			fieldSubmissionID: event.PromptAccepted.SubmissionID,
			fieldClientNonce:  event.PromptAccepted.ClientNonce,
			fieldTurnID:       event.PromptAccepted.TurnID,
		}, fieldRunID, event.PromptAccepted.RunID)
	case EventActivityUpdate:
		return map[string]any{
			fieldType:     string(EventActivityUpdate),
			fieldActivity: encodeActivity(*event.Activity),
		}
	case EventActionUpdate:
		return map[string]any{
			fieldType:   string(EventActionUpdate),
			fieldAction: encodeAction(*event.Action),
		}
	case EventQuiescenceUpdate:
		fact := encodeQuiescence(*event.Quiescence)
		fact[fieldType] = string(EventQuiescenceUpdate)

		return fact
	default:
		return encodeTransition(*event.State)
	}
}

// encodeSnapshot renders the whole-state assertion. The two sets are always
// present as arrays, and the foreground names its turn and that turn's origin
// exactly while one is open.
func encodeSnapshot(snapshot Snapshot) map[string]any {
	foreground := map[string]any{
		fieldState:   string(snapshot.Foreground.State),
		fieldCycleID: snapshot.Foreground.CycleID,
	}
	withOptional(foreground, fieldTurnID, snapshot.Foreground.TurnID)
	withOptional(foreground, fieldOrigin, string(snapshot.Foreground.Origin))

	activities := make([]any, 0, len(snapshot.Activities))
	for index := range snapshot.Activities {
		activities = append(activities, encodeActivity(snapshot.Activities[index]))
	}

	actions := make([]any, 0, len(snapshot.Actions))
	for _, action := range snapshot.Actions {
		actions = append(actions, encodeAction(action))
	}

	return map[string]any{
		fieldType:       string(EventSnapshot),
		fieldForeground: foreground,
		fieldActivities: activities,
		fieldActions:    actions,
		fieldQuiescence: encodeQuiescence(snapshot.Quiescence),
	}
}

func encodeActivity(activity ActivityUpdate) map[string]any {
	encoded := map[string]any{
		fieldActivityID: activity.ActivityID,
		fieldState:      string(activity.State),
	}
	withOptional(encoded, fieldKind, string(activity.Kind))
	withOptional(encoded, fieldParentID, activity.ParentID)
	withOptional(encoded, fieldToolCallID, activity.ToolCallID)
	withOptional(encoded, fieldCause, string(activity.Cause))
	withOptional(encoded, fieldOriginTurnID, activity.OriginTurnID)
	withOptional(encoded, fieldRunID, activity.RunID)

	if activity.Progress != nil {
		encoded[fieldProgress] = activity.Progress
	}

	return encoded
}

// encodeAction renders one action. blocksForeground is emitted only when the
// update states it, so a patch that restates nothing stays a patch.
func encodeAction(action ActionUpdate) map[string]any {
	encoded := map[string]any{
		fieldActionID: action.ActionID,
		fieldState:    string(action.State),
	}
	withOptional(encoded, fieldKind, string(action.Kind))
	withOptional(encoded, fieldRunID, action.RunID)

	if action.Owner.ID != "" {
		encoded[fieldOwner] = map[string]any{
			fieldType: string(action.Owner.Type),
			fieldID:   action.Owner.ID,
		}
	}

	if action.BlocksForeground != nil {
		encoded[fieldBlocksForeground] = *action.BlocksForeground
	}

	return encoded
}

func encodeTransition(transition StateTransition) map[string]any {
	encoded := map[string]any{
		fieldType:    string(EventStateUpdate),
		fieldState:   string(transition.State),
		fieldCycleID: transition.CycleID,
		fieldTurnID:  transition.TurnID,
		fieldCause:   string(transition.Cause),
	}
	withOptional(encoded, fieldStopReason, transition.StopReason)
	withOptional(encoded, fieldOutcome, string(transition.Outcome))

	return encoded
}

// encodeQuiescence renders a fact's members. A negative fact carries no proof at
// all: `source` is present if and only if the fact is positive, and it is never a
// `none` sentinel.
func encodeQuiescence(fact QuiescenceFact) map[string]any {
	if !fact.Quiescent {
		return map[string]any{fieldQuiescent: false}
	}

	encoded := map[string]any{
		fieldQuiescent: true,
		fieldSource:    string(fact.Source),
		fieldWatermark: fact.Watermark,
	}

	return withOptional(encoded, fieldBarrier, fact.Barrier)
}

// withOptional adds a member only when it has a value. An optional member is
// omitted rather than emitted empty, because an empty opaque identifier fails
// closed on the reading side.
func withOptional(encoded map[string]any, key, value string) map[string]any {
	if value != "" {
		encoded[key] = value
	}

	return encoded
}
