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
	gates map[K]*authGateEntry
}

// authGateEntry is one key's gate and the number of legs that hold it or are
// waiting for it. The count is what decides when the entry may be dropped:
// dropping it while a leg still holds the channel would not free anything the
// holder needs, but the next leg for that key would mint a second channel and
// run beside the leg it was supposed to queue behind, which is the gate
// silently ceasing to serialize. Nothing outside admit may remove an entry, and
// the keys are unbounded over the life of an agent that outlives its sessions,
// so the count is also the only thing keeping the map from growing forever.
type authGateEntry struct {
	gate    chan struct{}
	pending int
}

func newAuthGate[K comparable]() *authGate[K] {
	return &authGate[K]{gates: make(map[K]*authGateEntry)}
}

// admit waits for the key's gate and returns the release its caller defers. It
// reports false only when ctx ended first, which is the one way a leg leaves
// without the gate and therefore without the right to mutate the resource.
func (g *authGate[K]) admit(ctx context.Context, key K) (func(), bool) {
	g.mu.Lock()

	entry, ok := g.gates[key]
	if !ok {
		entry = &authGateEntry{gate: make(chan struct{}, 1)}
		g.gates[key] = entry
	}

	entry.pending++
	g.mu.Unlock()

	select {
	case entry.gate <- struct{}{}:
		return func() {
			<-entry.gate

			g.depart(key)
		}, true
	case <-ctx.Done():
		g.depart(key)

		return nil, false
	}
}

// depart drops the caller from the key's entry and removes the entry once
// nobody holds it or waits for it.
func (g *authGate[K]) depart(key K) {
	g.mu.Lock()
	defer g.mu.Unlock()

	entry := g.gates[key]

	entry.pending--
	if entry.pending == 0 {
		delete(g.gates, key)
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
