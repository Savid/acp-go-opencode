package opencodeacp

import (
	"encoding/json"

	"github.com/savid/acp-go-opencode/internal/lifecycle"
)

// metaMember is the ACP request member every reserved value rides under.
const metaMember = "_meta"

// provenFacts is the lifecycle answer this adapter gives, and it is one answer
// rather than a table: no field below turns on how the native process boundary
// is enforced, so keying them on the containment mode would state a dependency
// the values do not have. Negotiating version 1 obligates the complete ordered
// foreground stream whatever the fields say.
//
// Each field states what this adapter proves, and nothing more:
//
//   - `updatesOutsidePrompt` is true on every configuration. The native event
//     stream is consumed by a session-owned pump that runs from session start to
//     session close, so a transcript part, a plan change, a permission, or a
//     foreground transition arriving with no prompt in flight is routed to the
//     host rather than queued or refused. The pump drains the native channel
//     unconditionally and holds nothing but the events of one dispatch
//     acknowledgement, which it then routes in arrival order.
//   - `authoritativeQuiescence` is false on every platform because no boundary
//     here proves whole-tree vacancy for the addressed session. The native idle
//     event proves the session's own agent loop stopped, which settles a
//     foreground turn but says nothing about a subsession, a shell, or a pty the
//     turn started; the live descendant inventory reports nothing on any
//     platform; and one session cannot contain the shared runtime's process tree
//     without ending every peer session's work.
//   - `activityKinds` is empty because OpenCode publishes no owned-work state
//     machine. Plan entries, session status, and the agent catalog are
//     presentation and configuration, and none carries an instance identity with
//     a lifecycle of its own.
//
// A field is upgraded only when a deterministic fixture in this repository proves
// both the native source it reads and the ordering it claims.
func provenFacts() lifecycle.Negotiated {
	return lifecycle.Negotiated{
		UpdatesOutsidePrompt:    true,
		AuthoritativeQuiescence: false,
		ActivityKinds:           []lifecycle.ActivityKind{},
	}
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
		answer, _ = offer.Answer(provenFacts())
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
