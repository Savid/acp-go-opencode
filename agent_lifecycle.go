package opencodeacp

import (
	"encoding/json"

	"github.com/savid/acp-go-opencode/internal/lifecycle"
)

// metaMember is the ACP request member every reserved value rides under.
const metaMember = "_meta"

// lifecycleFactsByContainment is the per-configuration lifecycle truth table.
// The answer describes the active configuration, so it is keyed on the same
// containment mode that enforces the native process boundary rather than
// compiled in once for the package.
//
// Every row is degenerate today, and each field is degenerate for its own
// reason:
//
//   - `updatesOutsidePrompt` is false because this adapter holds no channel to a
//     host between prompts. A native event arriving out of turn is answered and
//     terminalized against the runtime rather than held to be announced later, so
//     one incarnation opens and fences inside each prompt.
//   - `authoritativeQuiescence` is false on every platform because no boundary
//     here proves whole-tree vacancy for the addressed session. The one
//     containment proof this adapter completes is agent-wide runtime generation
//     retirement, which is not a session's own boundary; the live descendant
//     inventory reports nothing on any platform; and the native completion signal
//     is a message and status poll, which the closed proof-class set excludes by
//     construction.
//   - `activityKinds` is empty because OpenCode publishes no owned-work state
//     machine. Plan entries, the status poll, and the agent catalog are
//     presentation and configuration, and none carries an instance identity with
//     a lifecycle of its own.
//
// A row is upgraded only when a deterministic fixture in this repository proves
// both the native source the fact reads and the ordering it claims.
var lifecycleFactsByContainment = map[RuntimeContainmentMode]lifecycle.Negotiated{
	RuntimeContainmentAuthoritative:  degenerateLifecycleFacts(),
	RuntimeContainmentBestEffort:     degenerateLifecycleFacts(),
	RuntimeContainmentSharedIdentity: degenerateLifecycleFacts(),
	RuntimeContainmentUnavailable:    degenerateLifecycleFacts(),
}

// degenerateLifecycleFacts is the answer a configuration gives when it proves no
// out-of-prompt delivery, no quiescence class, and no activity kind. It is a
// truthful answer rather than a gap: negotiating version 1 still obligates the
// complete ordered foreground stream.
func degenerateLifecycleFacts() lifecycle.Negotiated {
	return lifecycle.Negotiated{
		UpdatesOutsidePrompt:    false,
		AuthoritativeQuiescence: false,
		ActivityKinds:           []lifecycle.ActivityKind{},
	}
}

// provenLifecycleFacts reads one configuration's row. A mode with no row proves
// nothing, which is the same answer the unavailable boundary gives.
func provenLifecycleFacts(mode RuntimeContainmentMode) lifecycle.Negotiated {
	facts, known := lifecycleFactsByContainment[mode]
	if !known {
		return degenerateLifecycleFacts()
	}

	return facts
}

// negotiateLifecycle reads the host's `initialize` offer and records the answer
// this connection speaks under. An absent offer is the host asking for nothing:
// the returned answer is then not present, so the response omits the key and
// every envelope, correlation read, and lifecycle fact stays illegal for the whole
// connection.
func (a *Agent) negotiateLifecycle(meta map[string]any) (lifecycle.Negotiated, error) {
	offer, present, refusal := lifecycle.DecodeOffer(meta)
	if refusal != nil {
		return lifecycle.Negotiated{}, unsupportedField(refusal.Field)
	}

	var answer lifecycle.Negotiated
	if present {
		answer, _ = offer.Answer(provenLifecycleFacts(a.containmentMode))
	}

	a.mu.Lock()
	a.lifecycle = answer
	a.mu.Unlock()

	return answer, nil
}

// lifecycleNegotiated reports the answer this connection carries. It is the only
// gate on emitting an envelope, reading a prompt correlation, or stamping an
// action correlation.
func (a *Agent) lifecycleNegotiated() lifecycle.Negotiated {
	if a == nil {
		return lifecycle.Negotiated{}
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	return a.lifecycle
}

// refuseLifecycleMeta rejects the reserved family literal on a surface that
// carries no lifecycle value. It runs before the surface's own side effects and
// before its own refusal: "the key is not read here" and "the request is wrong
// for another reason" are different answers, and a host that got the key wrong is
// owed the first one.
func refuseLifecycleMeta(meta map[string]any) error {
	if refusal := lifecycle.RefuseKey(meta); refusal != nil {
		return unsupportedField(refusal.Field)
	}

	return nil
}

// refuseLifecycleRawMeta applies the same rule to an extension method, whose
// params arrive as raw JSON. A params body this cannot read carries no reserved
// key to refuse, and the method's own decoder reports what is wrong with it.
func refuseLifecycleRawMeta(params json.RawMessage) error {
	var members map[string]json.RawMessage
	if json.Unmarshal(params, &members) == nil {
		var meta map[string]any
		if json.Unmarshal(members[metaMember], &meta) == nil {
			return refuseLifecycleMeta(meta)
		}
	}

	return nil
}

// promptSubmission reads the prompt's lifecycle correlation. While the extension
// is negotiated the value is required; while it is not, a present key is
// rejected. Either way the verdict is reached before the prompt is dispatched, so
// no frame is written to the harness.
func (a *Agent) promptSubmission(meta map[string]any) (lifecycle.Submission, error) {
	submission, refusal := lifecycle.DecodePromptCorrelation(meta, a.lifecycleNegotiated())
	if refusal != nil {
		return lifecycle.Submission{}, unsupportedField(refusal.Field)
	}

	return submission, nil
}

// lifecycleAnswerMeta places the negotiated answer on the initialize response's
// top-level `_meta`. The answer never rides `agentCapabilities._meta`, which later
// protocol work relocates, and never rides the vendor namespace. An answer that is
// not present omits the key entirely rather than stating an empty one.
func lifecycleAnswerMeta(answer lifecycle.Negotiated) map[string]any {
	if !answer.Present() {
		return nil
	}

	return map[string]any{lifecycle.MetaKey: answer.Advertisement()}
}
