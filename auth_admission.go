package opencodeacp

import (
	"context"
	"sync"

	"github.com/coder/acp-go-sdk"
)

// authGate serializes the legs that address one named resource. A leg holds its
// key's gate across a whole check-then-set — the read that decides, the native
// call, and the write that records it — because releasing anywhere in between
// leaves a window the length of that native call in which another leg decides
// against state this one is about to replace.
type authGate[K comparable] struct {
	mu    sync.Mutex
	gates map[K]chan struct{}
}

func newAuthGate[K comparable]() *authGate[K] {
	return &authGate[K]{gates: make(map[K]chan struct{})}
}

// admit waits for the key's gate and returns the release its caller defers. It
// reports false only when ctx ended first, which is the one way a leg leaves
// without the gate and therefore without the right to mutate the resource.
func (g *authGate[K]) admit(ctx context.Context, key K) (func(), bool) {
	g.mu.Lock()

	gate, ok := g.gates[key]
	if !ok {
		gate = make(chan struct{}, 1)
		g.gates[key] = gate
	}

	g.mu.Unlock()

	select {
	case gate <- struct{}{}:
		return func() { <-gate }, true
	case <-ctx.Done():
		return nil, false
	}
}

// forget drops the gates whose keys match. A gate a leg still holds is dropped
// from the map but not from that leg: the release closure captured the channel,
// so it drains the gate it took rather than a successor's.
func (g *authGate[K]) forget(match func(K) bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	for key := range g.gates {
		if match(key) {
			delete(g.gates, key)
		}
	}
}

// publishFlow registers the record every later leg addresses. Publication is
// the authoritative session-admission check: an authorize that passed session
// lookup before session/close took its cleanup set has to be refused here,
// because a record published after that set was taken is one close can no
// longer see, and the broker process and home this leg would go on to create
// would outlive the session that owns them. The refusal is the same answer the
// session lookup gives for an id nobody knows, so a host cannot tell the two
// apart and does not have to.
func (p *providerAuth) publishFlow(key authFlowKey, flow *authFlow) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, closed := p.closedSessions[key.sessionID]; closed {
		return authSessionUnknown()
	}

	p.flows[key] = flow
	p.byID[flow.id] = flow

	return nil
}

// sessionAdmitted reports whether the session still accepts legs. The caller
// holds the mutex.
func (p *providerAuth) sessionAdmitted(sessionID acp.SessionId) bool {
	_, closed := p.closedSessions[sessionID]

	return !closed
}

// reopenSession drops the closed mark when the agent registers a session under
// the id again. A session/load after a session/close builds a new record under
// the same id, and refusing that one would be refusing a session the host can
// see and prompt through.
func (p *providerAuth) reopenSession(sessionID acp.SessionId) {
	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.closedSessions, sessionID)
}

func authSessionUnknown() error {
	return acp.NewInvalidParams(map[string]any{
		jsonFieldError: errValueSessionUnknown,
		jsonFieldField: jsonFieldSessionID,
	})
}

// claimFlow gives one leg the exclusive right to drive a flow's native
// completion. The terminal read and the claim happen in one critical section
// because they are one decision: a check-then-set that releases the mutex in
// between hands two legs the same live flow with no data race for -race to
// find, since every individual field access is itself locked. Each of them then
// applies its own value against the store, each reports the flow saved, and the
// one the operator authorized is whichever landed last.
func (p *providerAuth) claimFlow(flow *authFlow) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.claimFlowLocked(flow)
}

// claimFlowLocked takes the claim. The caller holds the mutex.
func (p *providerAuth) claimFlowLocked(flow *authFlow) error {
	if authTerminal(flow.state) || flow.claimed {
		return authFailed(authCauseFlowState, flow.providerID, flow.method.ID, flow.id)
	}

	flow.claimed = true

	return nil
}

// releaseFlow drops the claim. A flow that terminalized under the claim rejects
// a later one anyway, so releasing after success costs nothing and keeps every
// claimant's shape the same.
func (p *providerAuth) releaseFlow(flow *authFlow) {
	p.mu.Lock()
	defer p.mu.Unlock()

	flow.claimed = false
}

// requestRetired reports whether the key names a request a later authorize
// already replaced. Only the newest record can be replayed verbatim, so an
// older key is unanswerable — and minting in its place would destroy the live
// flow the key never named, which is the one thing an idempotency key exists to
// prevent.
func (p *providerAuth) requestRetired(key authFlowKey, requestID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	_, retired := p.retired[key][requestID]

	return retired
}

// retire records a request id the broker can no longer answer. The caller holds
// the mutex.
func (p *providerAuth) retire(key authFlowKey, requestID string) {
	requests, ok := p.retired[key]
	if !ok {
		requests = make(map[string]struct{})
		p.retired[key] = requests
	}

	requests[requestID] = struct{}{}
}
