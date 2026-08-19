package opencodeacp

import (
	"context"
	"errors"
	"fmt"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/lifecycle"
)

// foregroundCycle is one foreground epoch of the session's lifecycle stream: a
// prompt-origin turn opened by an accepted submission, or an agent-origin turn
// opened by native work this adapter did not submit.
//
// Every member is read and written under the session's lifecycle mutex, which is
// what keeps the pump and a foreground prompt from reporting two orders for the
// same cycle.
type foregroundCycle struct {
	id     string
	turnID string
	origin lifecycle.Cause
	// blockers are the actions that stopped this cycle and have not
	// terminalized. The cycle cannot move while one remains.
	blockers map[string]struct{}
	// idle records the native completion signal for this cycle; settled records
	// that the one ending transition has been emitted.
	idle    bool
	settled bool
	// failure is the native turn failure this cycle carries, and assistantID the
	// native assistant message it produced.
	failure     error
	assistantID string
	// lost records an incarnation loss that ended the cycle with no native
	// terminal signal at all.
	lost error
	// interrupted records that this cycle's native work was already asked to
	// stop, so a second failure cannot interrupt whatever the session does next.
	interrupted bool
	// signal closes exactly once, when the cycle acquires terminal evidence.
	signal chan struct{}
}

// terminalEvidence reports whether the cycle has native terminal evidence: the
// native idle signal, or the loss of the incarnation that would have sent it.
func (c *foregroundCycle) terminalEvidence() bool {
	return c.idle || c.lost != nil
}

// wake publishes terminal evidence to whoever waits on the cycle. It fires once:
// the first evidence ends the cycle and later evidence changes nothing. Every
// cycle is created with its signal, so there is no unsignalled cycle to guard.
func (c *foregroundCycle) wake() {
	select {
	case <-c.signal:
	default:
		close(c.signal)
	}
}

// openLifecycleStream creates the stream for one native session incarnation. It
// emits nothing: the opening snapshot is published once the establishing session
// response is on its way, and every later envelope is ordered behind it.
//
// The stream is deliberately session-owned rather than prompt-owned. Prompt
// completion never rotates it; native generation loss and session close fence it.
func (s *session) openLifecycleStream() {
	s.installLifecycleStream(s.agent.lifecycleNegotiated())
}

// installLifecycleStream fits a fresh stream to one native incarnation. The
// caller owns the lifecycle mutex, or owns the session outright because it has not
// been published yet.
func (s *session) installLifecycleStream(facts lifecycle.Negotiated) {
	if !facts.Present() {
		return
	}

	id, err := NewTurnNonce()
	if err != nil {
		s.lifecycleFailed = err

		return
	}

	s.lifecycleStream = lifecycle.NewStream(id, facts)
	s.lifecycleOpened = false
	s.cycle = nil
}

// reopenLifecycleStream replaces a fenced incarnation's stream with the recovered
// incarnation's. The fenced stream can carry nothing further, and the recovered
// native session's events belong to their own ordering, so the identity changes
// with the incarnation rather than outliving it.
func (s *session) reopenLifecycleStream() {
	facts := s.agent.lifecycleNegotiated()

	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	s.lifecycleFailed = nil
	s.installLifecycleStream(facts)
}

// publishLifecycleStream emits the opening snapshot. It states the whole truth
// this incarnation can state: an idle foreground with no turn, no activity kind
// this adapter proves, and no quiescence class this configuration completes.
//
// A recovered incarnation opens its own stream with its own snapshot, so a host
// never reduces a new native session's events against the fenced one's ordering.
func (s *session) publishLifecycleStream(ctx context.Context) error {
	s.lifecycleMu.Lock()

	if s.lifecycleStream == nil || s.lifecycleOpened {
		err := s.lifecycleFailed
		s.lifecycleMu.Unlock()

		return err
	}

	s.lifecycleOpened = true
	s.cycleCounter++
	cycleID := fmt.Sprintf("cycle-%d", s.cycleCounter)
	s.lifecycleMu.Unlock()

	return s.emitLifecycle(ctx, lifecycle.SnapshotEvent(
		lifecycle.Foreground{State: lifecycle.ForegroundIdle, CycleID: cycleID},
		nil,
		lifecycle.QuiescenceFact{Quiescent: false},
	))
}

// emitLifecycle claims the next sequence, reduces the event against the same
// rules a consumer applies, and delivers it on the one legal carrier. A refused
// or undelivered event latches the stream: the sequence is already spent, so
// pretending the stream is healthy would hide the gap behind an apparently
// contiguous stream.
func (s *session) emitLifecycle(ctx context.Context, event lifecycle.Event) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	return s.emitLifecycleLocked(ctx, event)
}

func (s *session) emitLifecycleLocked(ctx context.Context, event lifecycle.Event) error {
	if s.lifecycleStream == nil {
		return s.lifecycleFailed
	}

	if s.lifecycleFailed != nil {
		return s.lifecycleFailed
	}

	envelope, err := s.lifecycleStream.Emit(event) // claims before delivery
	if err != nil {
		s.lifecycleFailed = err

		return err
	}

	conn := s.agent.connection()
	if conn == nil {
		err = errors.New("lifecycle delivery has no ACP connection")
		s.lifecycleFailed = err

		return err
	}

	err = conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: s.id,
		Meta:      map[string]any{lifecycle.MetaKey: envelope},
		Update: acp.SessionUpdate{SessionInfoUpdate: &acp.SessionSessionInfoUpdate{
			SessionUpdate: string(lifecycle.CarrierSessionInfo),
		}},
	})
	if err != nil {
		s.lifecycleFailed = fmt.Errorf("deliver lifecycle sequence: %w", err)

		return s.lifecycleFailed
	}

	return nil
}

// acceptPromptCycle opens the prompt-origin turn for a frame the native
// dispatcher has already accepted. It is the dispatch linearization point: the
// caller holds the event gate across it, so no event caused by the accepted frame
// reaches a host before the acceptance that explains it.
//
// The cycle exists whether or not this connection carries the lifecycle
// extension: it is the turn's completion mechanism, not a stream artifact. The
// turn id is minted from the stream's own counter rather than from the route
// nonce, because the nonce is the host's anti-stale authenticator, not turn
// identity — a host that reuses a nonce must never collide with a turn the
// stream already introduced.
func (s *session) acceptPromptCycle(ctx context.Context, submission lifecycle.Submission) (*foregroundCycle, error) {
	s.lifecycleMu.Lock()

	if s.cycle != nil {
		cycle := s.cycle
		s.lifecycleMu.Unlock()

		return nil, fmt.Errorf("OpenCode session already holds foreground cycle %s", cycle.id)
	}

	s.cycleCounter++
	s.turnCounter++
	cycle := &foregroundCycle{
		id:       fmt.Sprintf("cycle-%d", s.cycleCounter),
		turnID:   fmt.Sprintf("turn-%d", s.turnCounter),
		origin:   lifecycle.CauseSubmission,
		blockers: map[string]struct{}{},
		signal:   make(chan struct{}),
	}
	s.cycle = cycle

	err := s.emitLifecycleLocked(ctx, lifecycle.AcceptedEvent(submission, cycle.turnID))
	if err == nil {
		err = s.emitLifecycleLocked(ctx, lifecycle.TransitionEvent(
			lifecycle.ForegroundRunning, cycle.id, cycle.turnID, lifecycle.CauseSubmission,
		))
	}

	if err != nil {
		s.cycle = nil
		s.lifecycleMu.Unlock()

		return nil, err
	}

	s.lifecycleMu.Unlock()

	return cycle, nil
}

// openAgentCycle opens an agent-origin turn for native work no submission
// caused. Its turn id is minted from the ordered stream rather than from a route
// nonce, because no client turn authenticated it.
func (s *session) openAgentCycleLocked(ctx context.Context) (*foregroundCycle, error) {
	if s.lifecycleStream == nil {
		return nil, s.lifecycleFailed
	}

	if s.cycle != nil {
		return s.cycle, nil
	}

	s.cycleCounter++
	s.turnCounter++
	cycle := &foregroundCycle{
		id:       fmt.Sprintf("cycle-%d", s.cycleCounter),
		turnID:   fmt.Sprintf("agent-turn-%d", s.turnCounter),
		origin:   lifecycle.CauseActivity,
		blockers: map[string]struct{}{},
		signal:   make(chan struct{}),
	}

	if err := s.emitLifecycleLocked(ctx, lifecycle.TransitionEvent(
		lifecycle.ForegroundRunning, cycle.id, cycle.turnID, lifecycle.CauseActivity,
	)); err != nil {
		return nil, err
	}

	s.cycle = cycle

	return cycle, nil
}

// currentCycle reports the open foreground cycle, or nil while the session is
// idle.
func (s *session) currentCycle() *foregroundCycle {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	return s.cycle
}

// settleCycle emits the one ending transition for the open cycle. Every blocking
// action must already be terminal, and the native-safe prefix must already be
// committed: this is the last lifecycle event of the turn, and the ACP prompt
// response follows it.
func (s *session) settleCycle(ctx context.Context, cycle *foregroundCycle, outcome lifecycle.Outcome, stopReason string) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if !s.retireCycleLocked(cycle) {
		return nil
	}

	return s.emitLifecycleLocked(ctx, lifecycle.IdleEvent(cycle.id, cycle.turnID, stopReason, outcome))
}

// settleCloseCycle ends the close boundary's foreground cycle. The emission rungs
// of the close ladder apply only to a live incarnation: when this session's stream
// is already fenced — a cancel or an incarnation loss ended it — or none ever
// opened, the boundary emits nothing on it, because an event bearing a fenced
// stream identity is exactly what a conforming reducer refuses as stale and the
// terminal state was already reported by whatever fenced the stream. The cycle the
// loss ended keeps the end it was given: close never restates it as cancelled. The
// chain of preconditions stays intact across the skipped rung, so the settlement
// response follows the last durable commit directly.
//
// The fence is the only thing that skips the rung. A stream that merely latched —
// one refused or undelivered event on an incarnation still live — is not fenced,
// so this boundary still owes it the terminal transition, and the latch is what
// refuses to let the boundary claim one: the close fails with the latched error
// rather than reporting a turn over on a stream that has already lost a sequence.
func (s *session) settleCloseCycle(ctx context.Context, cycle *foregroundCycle) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if !s.retireCycleLocked(cycle) {
		return nil
	}

	if s.lifecycleStream == nil || s.incarnationFencedLocked() {
		return nil
	}

	return s.emitLifecycleLocked(ctx, lifecycle.IdleEvent(
		cycle.id, cycle.turnID, string(acp.StopReasonCancelled), lifecycle.OutcomeCancelled))
}

// incarnationFencedLocked reports that this session's incarnation was fenced: the
// stream it spoke for is closed, so nothing may be emitted on it again and
// whatever fenced it already reported the end.
//
// It is deliberately the stream's own fence rather than the session's latched
// failure. A latch records that one event did not reach the host; a fence records
// that the incarnation behind the stream is over. Reading the latch as a fence
// would let a live incarnation close in silence on a stream that already dropped
// an event, and reading the latch as terminal evidence would let a close accept a
// stop no fence ever proved.
func (s *session) incarnationFencedLocked() bool {
	return s.lifecycleStream != nil && s.lifecycleStream.State().Closed
}

// retireCycleLocked ends the cycle locally and reports whether this caller is the
// one that ended it. Retirement is what makes the ending transition emit once: a
// cycle already retired says nothing further, whichever path reaches it.
func (s *session) retireCycleLocked(cycle *foregroundCycle) bool {
	if cycle == nil || cycle.settled {
		return false
	}

	cycle.settled = true

	if s.cycle == cycle {
		s.cycle = nil
	}

	cycle.wake()

	return true
}

// blockCycleLocked records that an action stopped the cycle that owes the
// accompanying foreground transition. Every cycle is created with its blocker
// set, so there is no unopened cycle to guard.
func (s *session) blockCycleLocked(actionID string, cycle *foregroundCycle) {
	cycle.blockers[actionID] = struct{}{}
}

// fenceLifecycle ends this incarnation. Nothing may be emitted on the stream
// afterwards, and an unsettled cycle is woken with the loss so its waiter reports
// a failed turn instead of blocking on a signal the dead source cannot send.
func (s *session) fenceLifecycle(cause string) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if s.lifecycleStream == nil {
		return
	}

	s.lifecycleStream.Close()

	loss := fmt.Errorf("active lifecycle incarnation lost: %s", cause)

	if s.cycle != nil && !s.cycle.settled {
		if s.cycle.lost == nil {
			s.cycle.lost = loss
		}

		s.cycle.wake()
	}

	if s.lifecycleFailed == nil {
		s.lifecycleFailed = loss
	}
}

// lifecycleActionMeta stamps one outbound permission or elicitation request with
// its action correlation. The value names the emitting stream and the action, and
// it never replaces the routing envelope a callback is authenticated by.
func (s *session) lifecycleActionMeta(actionID string, owner lifecycle.Owner) map[string]any {
	runID := s.currentSubmission().RunID

	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if s.lifecycleStream == nil {
		return nil
	}

	return map[string]any{lifecycle.MetaKey: lifecycle.ActionCorrelation{
		StreamID: s.lifecycleStream.ID(), ActionID: actionID, Owner: owner, RunID: runID,
	}.Value()}
}

// lifecycleFailure reports the latched stream failure, if any. A latched stream
// can carry nothing further: the sequence it consumed is spent, and a later
// delivery would hide that gap behind an apparently contiguous stream.
func (s *session) lifecycleFailure() error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	return s.lifecycleFailed
}

// lifecycleStreamID reports the incarnation this session's stream speaks for.
func (s *session) lifecycleStreamID() string {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if s.lifecycleStream == nil {
		return ""
	}

	return s.lifecycleStream.ID()
}
