package lifecycle

import (
	"bytes"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
)

// Options configures a reducer.
type Options struct {
	// Negotiated are the lifecycle facts the source's configuration proved. The
	// reducer refuses anything the configuration did not advertise.
	Negotiated Negotiated
}

// Reducer reduces one session's lifecycle stream. It validates ordering identity
// before reducing, refuses anything it cannot prove, and latches on the first
// refusal: a stream that failed closed never reduces again.
//
// One reducer follows one session across incarnations. A snapshot bearing a new
// stream identity supersedes the previous incarnation and starts a fresh
// projection; every other event on a foreign or fenced stream is stale.
//
// The one advertised fact it cannot check is `updatesOutsidePrompt`: no fact on
// the ordered stream expresses whether a prompt is in flight, so that obligation
// belongs to the emitter's own configuration and to a transport-facing consumer.
//
// A Reducer is not safe for concurrent use; the owner of the stream serializes
// reduction.
type Reducer struct {
	negotiated Negotiated
	state      State
	base       uint64
	started    bool
	failed     *ViolationError
	// frames holds every decoded notification this incarnation reduced.
	// Wholesale idempotence has no window: an exact retransmission is suppressed
	// however far back its identity was reduced, and the retention ends with the
	// incarnation.
	frames map[uint64]any
	// lastTransition is the highest sequence carrying a transition a quiescence
	// proof must cover before it can certify a boundary.
	lastTransition uint64
	// fence is the highest watermark ever certified on this incarnation. Causal
	// work rooted at or before it is settled and never reopens.
	fence uint64
	// turnSeen and activitySeen record the sequence an identity was first seen
	// at, which is what makes late causal work mechanically detectable.
	turnSeen        map[string]uint64
	activitySeen    map[string]uint64
	turnIndexes     map[string]int
	activityIndexes map[string]int
	actionIndexes   map[string]int
	// liveChildren and liveActions make parent terminalization proportional to
	// the update being reduced rather than to every sibling already projected.
	// They count only direct, currently nonterminal owners; descendant ordering
	// follows inductively because a child cannot terminalize while its own counts
	// remain nonzero.
	liveChildren map[string]int
	liveActions  map[string]int
	// blockedCycle is the cycle owing the accompanying foreground transition a
	// blocking action requires.
	blockedCycle string
	// actionCycle records, per blocking action, the foreground cycle it stopped:
	// a blocker blocks the cycle current at its first sight, and that cycle may
	// not move again until the blocker terminalizes.
	actionCycle     map[string]string
	blockingByCycle map[string]int
	// retired remembers every stream identity a later incarnation superseded.
	// Supersession fences an incarnation the same way close does, so a retired
	// identity never opens again and its projection never resurrects.
	retired map[string]struct{}
}

// NewReducer builds a reducer for one session.
func NewReducer(opts Options) *Reducer {
	reducer := &Reducer{negotiated: opts.Negotiated, retired: map[string]struct{}{}}
	reducer.reset("")

	return reducer
}

// Negotiated reports the configuration the reducer validates against.
func (r *Reducer) Negotiated() Negotiated { return r.negotiated }

// State returns the projection proved so far.
func (r *Reducer) State() State { return r.state.clone() }

// Failed reports the latched refusal, if any.
func (r *Reducer) Failed() *ViolationError { return r.failed }

// Close records that the addressed session's close completed. The incarnation is
// fenced: any later event bearing its identity is stale.
func (r *Reducer) Close() { r.state.Closed = true }

// ReduceSessionUpdate decodes one session/update notification payload and
// reduces the delivery it carries. A frame carrying no envelope reports
// ErrNoEnvelope and changes nothing; every other refusal latches the stream, so
// a caller that stops here holds the projection as it stood at the moment of
// refusal.
func (r *Reducer) ReduceSessionUpdate(params json.RawMessage) error {
	if r.failed != nil {
		return r.failed
	}

	delivery, err := DecodeSessionUpdate(params, r.negotiated)
	if err != nil {
		var refusal *ViolationError
		if errors.As(err, &refusal) {
			r.failed = refusal
			r.nameStream(refusal.StreamID)
		}

		return err
	}

	return r.Reduce(delivery)
}

// nameStream adopts the identity a refused frame named. A stream is identified by
// the envelope naming it, whether or not that envelope reduces, so a refusal
// before the stream ever opened still reports which stream failed closed. An
// already-open stream keeps its own identity: nothing a foreign frame claims
// renames it.
func (r *Reducer) nameStream(streamID string) {
	if !r.started {
		r.state.StreamID = streamID
	}
}

// Reduce validates and reduces one delivery. Carrier legality is structural, so
// it is judged before ordering: an envelope on a carrier a conformant client may
// coalesce is no evidence the sequence it claims was ever delivered. An exact
// retransmission of an already-reduced identity is suppressed wholesale and
// returns nil without changing the projection.
func (r *Reducer) Reduce(delivery Delivery) error {
	switch {
	case r.failed != nil:
		return r.failed
	case delivery.Carrier != CarrierSessionInfo:
		return r.fail(delivery, ViolationIllegalCarrier, "carrier "+string(delivery.Carrier))
	case r.state.Closed:
		return r.fail(delivery, ViolationStaleStream, "the session's close containment completed")
	case !r.started:
		return r.reduceFirst(delivery)
	case delivery.StreamID != r.state.StreamID:
		return r.reduceForeign(delivery)
	case delivery.Sequence < r.base:
		return r.fail(delivery, ViolationSequenceRegression, "below the stream's snapshot boundary")
	case delivery.Sequence <= r.state.ReducedThrough:
		return r.reduceDuplicate(delivery)
	case delivery.Sequence > r.state.ReducedThrough+1:
		return r.fail(delivery, ViolationSequenceGap, "expected the next contiguous sequence")
	case delivery.Event.Type == EventSnapshot:
		return r.fail(delivery, ViolationStreamCycle, "a snapshot opens a stream and never appears inside one")
	}

	if err := r.apply(delivery); err != nil {
		return err
	}

	r.commit(delivery)

	return nil
}

// reduceForeign admits the next incarnation. Only its opening snapshot may arrive
// on a stream identity this reducer has not seen; a projection is per incarnation
// and adopts nothing from the one it supersedes. A closed session admits no
// incarnation at all, which is why the fence is judged before this.
//
// The replacement is validated whole and swapped in only on success. A refused
// snapshot opens nothing and leaves no half-built projection behind, and that
// governs the replacement position too: the standing projection stays exactly as
// it stood and the refusal latches over it. Retiring the old incarnation first
// would latch over an empty projection instead — a reader that terminalizes what
// it holds needs something held, and the superseded incarnation's work is the
// only truth anyone has when its would-be successor turns out to be malformed.
func (r *Reducer) reduceForeign(delivery Delivery) error {
	if delivery.Event.Type != EventSnapshot {
		return r.fail(delivery, ViolationStaleStream, "stream is "+r.state.StreamID)
	}

	if _, superseded := r.retired[delivery.StreamID]; superseded {
		return r.fail(delivery, ViolationStaleStream, "stream "+delivery.StreamID+" was superseded")
	}

	next := &Reducer{negotiated: r.negotiated}
	next.reset(delivery.StreamID)

	if err := next.reduceFirst(delivery); err != nil {
		r.failed = next.failed

		return err
	}

	next.state.Closed = r.state.Closed
	next.retired = r.retired
	next.retired[r.state.StreamID] = struct{}{}
	*r = *next

	return nil
}

func (r *Reducer) reset(streamID string) {
	r.state = State{StreamID: streamID}
	r.base = 0
	r.started = false
	r.frames = make(map[uint64]any)
	r.lastTransition = 0
	r.fence = 0
	r.turnSeen = make(map[string]uint64)
	r.activitySeen = make(map[string]uint64)
	r.turnIndexes = make(map[string]int)
	r.activityIndexes = make(map[string]int)
	r.actionIndexes = make(map[string]int)
	r.liveChildren = make(map[string]int)
	r.liveActions = make(map[string]int)
	r.blockedCycle = ""
	r.actionCycle = make(map[string]string)
	r.blockingByCycle = make(map[string]int)
}

func (r *Reducer) reduceFirst(delivery Delivery) error {
	r.state.StreamID = delivery.StreamID

	if delivery.Event.Type != EventSnapshot {
		return r.fail(delivery, ViolationDeltaBeforeSnapshot, "first event was "+string(delivery.Event.Type))
	}

	r.started = true
	r.base = delivery.Sequence

	if err := r.applySnapshot(delivery); err != nil {
		r.started = false

		return err
	}

	r.commit(delivery)

	return nil
}

func (r *Reducer) reduceDuplicate(delivery Delivery) error {
	if recorded, known := r.frames[delivery.Sequence]; known && equalJSONValue(recorded, delivery.Frame) {
		r.state.SuppressedRetransmissions++

		return nil
	}

	return r.fail(delivery, ViolationConflictingDuplicate, "the identity already delivered different content")
}

// equalJSONValue compares two decoded JSON values under lifecycle value
// equality: key order and insignificant whitespace are not differences, and
// numbers compare as exact mathematical values rather than as lexemes or as
// float64. Opaque carrier members and progress objects may carry integers beyond
// IEEE-754 double precision, and collapsing those through a double would suppress
// a conflicting duplicate that changed its content.
func equalJSONValue(left, right any) bool {
	switch value := left.(type) {
	case map[string]any:
		return equalJSONObject(value, right)
	case []any:
		return equalJSONArray(value, right)
	case json.Number:
		other, ok := right.(json.Number)

		return ok && equalNumber(string(value), string(other))
	default:
		return value == right
	}
}

func equalJSONObject(left map[string]any, right any) bool {
	other, ok := right.(map[string]any)
	if !ok || len(left) != len(other) {
		return false
	}

	for key, member := range left {
		counterpart, present := other[key]
		if !present || !equalJSONValue(member, counterpart) {
			return false
		}
	}

	return true
}

func equalJSONArray(left []any, right any) bool {
	other, ok := right.([]any)
	if !ok || len(left) != len(other) {
		return false
	}

	for index := range left {
		if !equalJSONValue(left[index], other[index]) {
			return false
		}
	}

	return true
}

// equalRawJSON compares two encoded members under the same predicate. A member
// neither side can decode is compared as written: a byte comparison never calls
// two different values equal, and nothing undecodable reaches a comparison here
// because the decoder refuses it first.
func equalRawJSON(left, right json.RawMessage) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}

	leftValue, leftOK := decodeJSONValue(left)

	rightValue, rightOK := decodeJSONValue(right)
	if !leftOK || !rightOK {
		return bytes.Equal(left, right)
	}

	return equalJSONValue(leftValue, rightValue)
}

// decodeJSONValue decodes one member without materializing any number it carries.
func decodeJSONValue(raw json.RawMessage) (any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}

	return value, true
}

// normalizedNumber is one JSON number lexeme's normalized decimal form: a sign, a
// coefficient stripped of leading and trailing zeros, and the power of ten that
// coefficient scales by. Every zero normalizes to an empty coefficient, which is
// what makes -0 equal to 0.
type normalizedNumber struct {
	negative bool
	digits   string
	exponent *big.Int
}

// equalNumber decides two number lexemes without expanding either. The decision
// costs the digits a literal is written with rather than the value it names, so a
// twelve-byte literal carrying a nine-digit exponent allocates nothing beyond its
// own exponent.
func equalNumber(left, right string) bool {
	leftParts, leftOK := normalizeNumber(left)

	rightParts, rightOK := normalizeNumber(right)
	if !leftOK || !rightOK {
		return left == right
	}

	if leftParts.digits == "" || rightParts.digits == "" {
		return leftParts.digits == rightParts.digits
	}

	return leftParts.negative == rightParts.negative &&
		leftParts.digits == rightParts.digits &&
		leftParts.exponent.Cmp(rightParts.exponent) == 0
}

func normalizeNumber(lexeme string) (normalizedNumber, bool) {
	negative := strings.HasPrefix(lexeme, "-")
	mantissa := strings.TrimPrefix(lexeme, "-")
	exponent := new(big.Int)

	if index := strings.IndexAny(mantissa, "eE"); index >= 0 {
		if _, ok := exponent.SetString(strings.TrimPrefix(mantissa[index+1:], "+"), 10); !ok {
			return normalizedNumber{}, false
		}

		mantissa = mantissa[:index]
	}

	whole, fraction := mantissa, ""
	if index := strings.IndexByte(mantissa, '.'); index >= 0 {
		whole, fraction = mantissa[:index], mantissa[index+1:]
	}

	digits := whole + fraction
	if digits == "" || strings.TrimLeft(digits, "0123456789") != "" {
		return normalizedNumber{}, false
	}

	exponent.Sub(exponent, big.NewInt(int64(len(fraction))))

	digits = strings.TrimLeft(digits, "0")
	trimmed := strings.TrimRight(digits, "0")
	exponent.Add(exponent, big.NewInt(int64(len(digits)-len(trimmed))))

	return normalizedNumber{negative: negative, digits: trimmed, exponent: exponent}, true
}

func (r *Reducer) commit(delivery Delivery) {
	r.state.ReducedThrough = delivery.Sequence
	r.frames[delivery.Sequence] = delivery.Frame
}

func (r *Reducer) fail(delivery Delivery, kind ViolationKind, detail string) error {
	r.failed = violation(kind, delivery.StreamID, delivery.Sequence, detail)

	return r.failed
}

func (r *Reducer) apply(delivery Delivery) error {
	switch delivery.Event.Type {
	case EventPromptAccepted:
		return r.applyPromptAccepted(delivery)
	case EventStateUpdate:
		return r.applyStateUpdate(delivery)
	case EventActivityUpdate:
		return r.applyActivityUpdate(delivery)
	case EventActionUpdate:
		return r.applyActionUpdate(delivery)
	default:
		return r.applyQuiescence(delivery)
	}
}

// invalidateQuiescence revokes the certified fact. Acceptance, a live foreground
// transition, a new activity, and a pending action all invalidate it; the
// revoking sequence is the first one that did, so a stale fact can never be
// re-read as fresh.
func (r *Reducer) invalidateQuiescence(sequence uint64) {
	if !r.state.Quiescence.Certified {
		return
	}

	r.state.Quiescence = QuiescenceState{InvalidatedAt: sequence}
}
