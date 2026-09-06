package opencodeacp

import (
	"context"
	"errors"
	"fmt"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/lifecycle"
	"github.com/savid/acp-go-opencode/internal/opencode"
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
	// turnNonce is the host-authenticated route of a submission-origin cycle.
	// Agent-origin work carries none, so an event received before a later prompt
	// can never borrow that prompt's route when dispatch holding delays delivery.
	turnNonce string
	// reserved makes prompt admission and agent-origin opening one atomic choice.
	// It is installed before native POST and becomes accepted only when the POST
	// acknowledges admission or a structurally addressed native event proves it.
	reserved         bool
	accepted         bool
	dispatchProven   bool
	dispatchEvidence chan struct{}
	nativeMessageID  string
	// blockers are the actions that stopped this cycle and have not
	// terminalized. The cycle cannot move while one remains.
	blockers map[string]struct{}
	// idle records the native completion signal for this cycle; settled records
	// that the one ending transition has been emitted.
	idle    bool
	settled bool
	// failure is the native turn failure this cycle carries. OpenCode answers one
	// turn with a new assistant message per step, so assistantIDs holds every step
	// this cycle owns and assistantID names the newest of them — the step whose
	// stop reason and usage settle the turn.
	failure      error
	assistantIDs map[string]struct{}
	assistantID  string
	// assistantEvidence closes when the cycle adopts its first step, so a
	// settlement that outran the ordered stream can wait for the identity it
	// needs rather than read a turn the stream has not described yet.
	assistantEvidence chan struct{}
	assistantTerminal bool
	// lost records that the cycle ended with no native terminal signal at all,
	// and incarnationLost records that the reason was the death of the
	// incarnation itself rather than a stop the harness declined to make. The
	// distinction is the close boundary's: a dead incarnation can never send the
	// idle, so its loss is terminal evidence, while a refused interrupt leaves
	// the harness alive with work this adapter never proved stopped.
	lost            error
	incarnationLost bool
	// interrupted records that this cycle's native work was already asked to
	// stop, so a second failure cannot interrupt whatever the session does next.
	interrupted bool
	// runStarted records that the native session began speaking for this cycle —
	// the message it created for the frame, or the busy status of the run it
	// scheduled. Until it does, no run exists to fail.
	runStarted bool
	// refused records that OpenCode rejected the frame before any run existed.
	// Such a cycle is never idled, because there is nothing to idle.
	refused bool
	// signal closes exactly once, when the cycle acquires terminal evidence.
	signal chan struct{}
}

// ownsAssistant reports whether one native assistant message is a step of this
// cycle.
func (c *foregroundCycle) ownsAssistant(id string) bool {
	if id == "" {
		return false
	}

	_, ok := c.assistantIDs[id]

	return ok
}

// claimsRequest reports whether a native request naming this assistant message
// concerns this cycle. OpenCode asks session-level questions with no tool and so
// no message to correlate on; such a request names no other work either, so a
// cycle whose dispatch is already proven is the only thing it can concern.
func (c *foregroundCycle) claimsRequest(messageID string) bool {
	if messageID == "" {
		return c.dispatchProven
	}

	return c.ownsAssistant(messageID)
}

// adoptAssistant records one native assistant message as a step of this cycle.
// assistantID follows the order the steps were created in, so it names the last
// step rather than the first, and a re-published earlier step never displaces it.
func (c *foregroundCycle) adoptAssistant(info opencode.NativeMessageInfo) {
	if info.ID == "" {
		return
	}

	if _, known := c.assistantIDs[info.ID]; !known {
		if c.assistantIDs == nil {
			c.assistantIDs = make(map[string]struct{}, 1)
		}

		c.assistantIDs[info.ID] = struct{}{}
		c.assistantID = info.ID

		if c.assistantEvidence != nil {
			select {
			case <-c.assistantEvidence:
			default:
				close(c.assistantEvidence)
			}
		}
	}

	if info.Finish != "" || info.Time.Completed > 0 {
		c.assistantTerminal = true
	}
}

// terminalEvidence reports whether the cycle has native terminal evidence: the
// native idle signal, the refusal of a frame that never became a run, or the
// loss of the incarnation that would have sent the idle.
func (c *foregroundCycle) terminalEvidence() bool {
	return c.idle || c.refused || c.lost != nil
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
	registry := newActionRegistry()

	var stream *lifecycle.Stream

	if facts.Present() {
		id, err := NewTurnNonce()
		if err != nil {
			s.lifecycleFailed = err
		} else {
			stream = lifecycle.NewStream(id, facts)
		}
	}

	s.incarnation = &nativeIncarnationBinding{
		client: s.client, generation: s.runtimeGeneration, stream: stream, registry: registry,
	}
	s.lifecycleOpened = false
	s.cycle = nil
	s.pendingAgentObservations = 0
}

// stampIncarnationGeneration completes the immutable binding assembled before a
// newly created session is published. No action can exist yet; retaining the
// fresh stream and registry while fixing the runtime identity avoids four
// independently mutable authority fields.
func (s *session) stampIncarnationGeneration(client opencode.Client, generation uint64) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if s.incarnation == nil {
		s.incarnation = &nativeIncarnationBinding{
			client: client, generation: generation, registry: newActionRegistry(),
		}

		return
	}

	s.incarnation = &nativeIncarnationBinding{
		client: client, generation: generation,
		stream: s.incarnation.stream, registry: s.incarnation.registry,
	}
}

// publishLifecycleStream emits the opening snapshot. It states the whole truth
// this incarnation can state: an idle foreground with no turn, no activity kind
// this adapter proves, and no quiescence class this configuration completes.
//
// A recovered incarnation opens its own stream with its own snapshot, so a host
// never reduces a new native session's events against the fenced one's ordering.
func (s *session) publishLifecycleStream(ctx context.Context) error {
	s.lifecycleMu.Lock()

	stream := s.lifecycleStreamLocked()
	if stream == nil || s.lifecycleOpened {
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
	binding := s.currentIncarnation()
	s.lifecycleMu.Lock()
	receipt, err := s.emitLifecycleLocked(ctx, event)
	s.lifecycleMu.Unlock()

	if err == nil {
		err = waitDelivery(ctx, receipt)
	}

	if err != nil {
		s.failLifecycleDelivery(binding, err)
	}

	return err
}

func (s *session) emitLifecycleLocked(ctx context.Context, event lifecycle.Event) (deliveryReceipt, error) {
	return s.emitLifecycleLockedWithFailure(ctx, event, true)
}

// emitLifecycleLockedWithFailure allows action admission to defer incarnation
// retirement until it has rejected the exact native request that the failed
// announcement would otherwise leave blocked. All other authoritative delivery
// retains the worker's immediate fail-closed callback.
func (s *session) emitLifecycleLockedWithFailure(
	ctx context.Context,
	event lifecycle.Event,
	containOnFailure bool,
) (deliveryReceipt, error) {
	stream := s.lifecycleStreamLocked()
	if stream == nil {
		return nil, s.lifecycleFailed
	}

	if s.lifecycleFailed != nil {
		return nil, s.lifecycleFailed
	}

	envelope, err := stream.Emit(event) // claims before delivery
	if err != nil {
		s.lifecycleFailed = err

		return nil, err
	}

	binding := s.incarnation

	onFailures := []func(error){}
	if containOnFailure {
		onFailures = append(onFailures, func(err error) { s.failLifecycleDelivery(binding, err) })
	}

	receipt, err := s.delivery.enqueueUpdate(ctx, acp.SessionNotification{
		SessionId: s.id,
		Meta:      map[string]any{lifecycle.MetaKey: envelope},
		Update: acp.SessionUpdate{SessionInfoUpdate: &acp.SessionSessionInfoUpdate{
			SessionUpdate: string(lifecycle.CarrierSessionInfo),
		}},
	}, onFailures...)
	if err != nil {
		s.lifecycleFailed = errors.New("lifecycle delivery failed")

		return nil, s.lifecycleFailed
	}

	return receipt, nil
}

func (s *session) failLifecycleDelivery(binding *nativeIncarnationBinding, err error) {
	if err == nil || binding == nil {
		return
	}

	s.lifecycleMu.Lock()
	if s.incarnation != binding {
		s.lifecycleMu.Unlock()

		return
	}

	if s.lifecycleFailed == nil {
		s.lifecycleFailed = errors.New("lifecycle delivery failed")
	}

	failure := s.lifecycleFailed
	s.lifecycleMu.Unlock()

	s.failNativeIncarnation(binding, failure)
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
func (s *session) reservePromptCycle(
	ctx context.Context,
	nativeMessageID string,
	submission lifecycle.Submission,
) (*foregroundCycle, context.Context, error) {
	s.lifecycleMu.Lock()
	binding := s.incarnation

	if s.cycle != nil || s.pendingAgentObservations > 0 {
		s.lifecycleMu.Unlock()

		return nil, nil, acp.NewInvalidRequest(map[string]any{
			jsonFieldError: valBackpressure,
			jsonFieldLimit: limitSessionPrompt,
		})
	}

	nextCycle := s.cycleCounter + 1
	nextTurn := s.turnCounter + 1

	cycle := &foregroundCycle{
		id:                fmt.Sprintf("cycle-%d", nextCycle),
		turnID:            fmt.Sprintf("turn-%d", nextTurn),
		origin:            lifecycle.CauseSubmission,
		turnNonce:         turnNonceFromContext(ctx),
		reserved:          true,
		dispatchEvidence:  make(chan struct{}),
		nativeMessageID:   nativeMessageID,
		blockers:          map[string]struct{}{},
		signal:            make(chan struct{}),
		assistantEvidence: make(chan struct{}),
	}
	if stream := s.lifecycleStreamLocked(); stream != nil {
		if err := stream.Preflight(
			lifecycle.AcceptedEvent(submission, cycle.turnID),
			lifecycle.TransitionEvent(
				lifecycle.ForegroundRunning, cycle.id, cycle.turnID, lifecycle.CauseSubmission,
			),
		); err != nil {
			s.lifecycleFailed = err
			s.lifecycleMu.Unlock()
			s.failNativeIncarnation(binding, err)

			return nil, nil, err
		}
	}

	turnCtx, cancel := context.WithCancel(withNativeIncarnationBinding(ctx, binding))
	turnCtx = withTurnRoute(turnCtx, cycle.turnNonce)
	s.cycle = cycle
	s.cancel = cancel
	s.cancelled = false
	s.pendingDispatchFailure = nil
	s.turnNonce = cycle.turnNonce
	s.submission = submission
	s.lifecycleMu.Unlock()

	return cycle, turnCtx, nil
}

func (s *session) acceptPromptCycle(
	ctx context.Context,
	cycle *foregroundCycle,
	submission lifecycle.Submission,
) error {
	binding := s.nativeIncarnationForContext(ctx)
	s.lifecycleMu.Lock()

	if s.cycle != cycle || cycle == nil || !cycle.reserved {
		s.lifecycleMu.Unlock()

		return errors.New("OpenCode prompt reservation is no longer current")
	}

	cycle.reserved = false
	cycle.accepted = true

	receipts := make([]deliveryReceipt, 0, 2)

	receipt, err := s.emitLifecycleLocked(ctx, lifecycle.AcceptedEvent(submission, cycle.turnID))
	if err == nil {
		receipts = append(receipts, receipt)
	}

	if err == nil {
		receipt, err = s.emitLifecycleLocked(ctx, lifecycle.TransitionEvent(
			lifecycle.ForegroundRunning, cycle.id, cycle.turnID, lifecycle.CauseSubmission,
		))
		if err == nil {
			receipts = append(receipts, receipt)
		}
	}

	if err != nil {
		s.cycle = nil
		s.lifecycleMu.Unlock()
		s.failNativeIncarnation(binding, err)

		return err
	}

	s.cycleCounter++
	s.turnCounter++

	s.lifecycleMu.Unlock()

	for _, receipt := range receipts {
		if err := waitDelivery(ctx, receipt); err != nil {
			s.failLifecycleDelivery(binding, err)

			return err
		}
	}

	return nil
}

func (s *session) abandonPromptCycle(cycle *foregroundCycle) {
	s.lifecycleMu.Lock()

	if cycle == nil || s.cycle != cycle || !cycle.reserved {
		s.lifecycleMu.Unlock()

		return
	}

	cycle.settled = true
	s.cycle = nil
	cancel := s.cancel
	s.cancel = nil
	s.pendingDispatchFailure = nil
	s.turnNonce = ""
	s.submission = lifecycle.Submission{}
	s.lifecycleMu.Unlock()

	if cancel != nil {
		cancel()
	}
}

// openAgentCycle opens an agent-origin turn for native work no submission
// caused. Its turn id is minted from the ordered stream rather than from a route
// nonce, because no client turn authenticated it.
func (s *session) openAgentCycleLocked(ctx context.Context) (*foregroundCycle, error) {
	return s.openAgentCycleLockedWithFailure(ctx, true)
}

// openActionCycleLocked defers containment of an opening-delivery failure until
// action admission has answered the exact native request. The action path owns
// that ordering; every other agent-origin cycle retains immediate containment.
func (s *session) openActionCycleLocked(ctx context.Context) (*foregroundCycle, error) {
	return s.openAgentCycleLockedWithFailure(ctx, false)
}

func (s *session) openAgentCycleLockedWithFailure(
	ctx context.Context,
	containOnFailure bool,
) (*foregroundCycle, error) {
	if s.lifecycleStreamLocked() == nil {
		return nil, s.lifecycleFailed
	}

	if s.cycle != nil {
		return s.cycle, nil
	}

	nextCycle := s.cycleCounter + 1
	nextTurn := s.turnCounter + 1
	cycle := &foregroundCycle{
		id:       fmt.Sprintf("cycle-%d", nextCycle),
		turnID:   fmt.Sprintf("agent-turn-%d", nextTurn),
		origin:   lifecycle.CauseActivity,
		blockers: map[string]struct{}{},
		signal:   make(chan struct{}),
	}

	event := lifecycle.TransitionEvent(
		lifecycle.ForegroundRunning, cycle.id, cycle.turnID, lifecycle.CauseActivity,
	)
	if err := s.lifecycleStreamLocked().Preflight(event); err != nil {
		s.lifecycleFailed = err

		return nil, err
	}

	if _, err := s.emitLifecycleLockedWithFailure(ctx, event, containOnFailure); err != nil {
		return nil, err
	}

	s.cycleCounter++
	s.turnCounter++
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

// cycleSettlement reports how one cycle's ending transition fared, and with it
// whether the turn is still free to answer anything other than what that
// transition said.
//
// The distinction is not cosmetic. A notification that failed on its way out was
// still handed to the carrier, and an error from a JSON-RPC notification never
// proves the peer did not receive it: the host may hold that ending transition
// for good. So an affirmative end that reached the carrier binds the answer, and
// an end that never got that far leaves the turn free to report the failure.
type cycleSettlement struct {
	err error
	// affirmed records that the ending transition claimed the turn succeeded and
	// was handed to the transport, so a host may already hold success evidence for
	// it. Nothing may contradict such a settlement afterwards: durable success
	// evidence for a turn answered with an error is the one settlement pair a
	// reducer can never repair, because both halves are terminal and each denies
	// the other. A failed or cancelled end binds nothing — it and an error answer
	// say the same thing about the turn — so an undeliverable one still fails it.
	//
	// It is the hand-off that affirms, never the enqueue. An ending transition the
	// delivery lane drained without ever sending — the worker stopped, or a
	// preceding write failed first — provably reached nobody, and answering such a
	// turn with success would leave a host holding no end at all for a turn it was
	// told had finished: a consumer waiting on the idle would wait for good.
	affirmed bool
}

// failsTurn reports that the ending transition failed without leaving any host
// able to hold an affirmative end, so the turn answers with that failure rather
// than with a completion the stream never carried.
func (settlement cycleSettlement) failsTurn() bool {
	return settlement.err != nil && !settlement.affirmed
}

// settleCycle emits the one ending transition for the open cycle. Every blocking
// action must already be terminal, the native-safe prefix must already be
// committed, and the turn's own answer must already be decided: this is the last
// lifecycle event of the turn, nothing after it may change what the turn answers,
// and the ACP prompt response follows it.
func (s *session) settleCycle(
	ctx context.Context,
	cycle *foregroundCycle,
	outcome lifecycle.Outcome,
	stopReason string,
) cycleSettlement {
	binding := s.nativeIncarnationForContext(ctx)
	s.lifecycleMu.Lock()

	if !s.retireCycleLocked(cycle) {
		s.lifecycleMu.Unlock()

		return cycleSettlement{}
	}

	receipt, err := s.emitLifecycleLocked(ctx, lifecycle.IdleEvent(cycle.id, cycle.turnID, stopReason, outcome))
	s.lifecycleMu.Unlock()

	if err != nil {
		s.failNativeIncarnation(binding, err)

		return cycleSettlement{err: err}
	}

	delivered := waitDeliveryOutcome(ctx, receipt)
	if delivered.err != nil {
		s.failNativeIncarnation(binding, delivered.err)

		return cycleSettlement{
			err:      delivered.err,
			affirmed: delivered.handed && outcome == lifecycle.OutcomeSuccess,
		}
	}

	return cycleSettlement{}
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
	binding := s.nativeIncarnationForContext(ctx)
	s.lifecycleMu.Lock()

	if !s.retireCycleLocked(cycle) {
		s.lifecycleMu.Unlock()

		return nil
	}

	if s.lifecycleStreamLocked() == nil || s.incarnationFencedLocked() {
		s.lifecycleMu.Unlock()

		return nil
	}

	receipt, err := s.emitLifecycleLocked(ctx, lifecycle.IdleEvent(
		cycle.id, cycle.turnID, string(acp.StopReasonCancelled), lifecycle.OutcomeCancelled))
	s.lifecycleMu.Unlock()

	if err == nil {
		err = waitDelivery(ctx, receipt)
	}

	if err != nil {
		s.failNativeIncarnation(binding, err)
	}

	return err
}

// incarnationFencedLocked reports that this session's incarnation was fenced: the
// stream it spoke for is closed, so nothing may be emitted on it again and
// whatever fenced it already reported the end.
//
// It is deliberately the stream's own fence rather than the session's latched
// failure. A latch records that one event did not reach the host; a fence records
// that the stream is over. Reading the latch as a fence would let a live
// incarnation close in silence on a stream that had merely dropped an event.
//
// It answers only the emission question. A fence is not terminal evidence of
// anything the native harness did — a close boundary installs one whether or not
// it proved the stop — so no settlement decision is made from it.
func (s *session) incarnationFencedLocked() bool {
	stream := s.lifecycleStreamLocked()

	return stream != nil && stream.State().Closed
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
//
// The woken cycle is marked as ended by the loss of its incarnation, which is
// what makes it terminal evidence for a close boundary: the source that would
// have sent the native idle is gone, so no later boundary can obtain one. That
// mark travels on the cycle rather than on the stream, because the fence alone
// says only that the stream is over — a close boundary installs one too.
func (s *session) fenceLifecycle(cause string) {
	s.latchLifecycleIncarnationLoss(cause, nil)
}

func (s *session) latchLifecycleIncarnationLoss(cause string, failure error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	s.latchLifecycleIncarnationLossLocked(s.incarnation, cause, failure)
}

// latchLifecycleIncarnationLossLocked terminalizes only the exact incarnation
// pointer the caller claimed while holding lifecycleMu.
func (s *session) latchLifecycleIncarnationLossLocked(
	binding *nativeIncarnationBinding,
	cause string,
	failure error,
) bool {
	if binding == nil || s.incarnation != binding {
		return false
	}

	if binding.stream != nil {
		binding.stream.Close()
	}

	loss := fmt.Errorf("active lifecycle incarnation lost: %s", cause)

	if s.cycle != nil && !s.cycle.settled {
		if failure != nil && s.cycle.failure == nil {
			s.cycle.failure = failure
		}

		if s.cycle.lost == nil {
			s.cycle.lost = loss
		}

		s.cycle.incarnationLost = true

		s.cycle.wake()
	}

	if binding.stream != nil && s.lifecycleFailed == nil {
		s.lifecycleFailed = loss
	}

	return true
}

// fenceBoundary installs the close boundary's end-of-emissions mark. The
// boundary has said everything this incarnation will ever say — whether or not
// it proved what it set out to prove — so the stream carries nothing further and
// a conforming reducer refuses anything that arrives after it.
//
// It is deliberately not an incarnation loss. It records no failure on the open
// cycle and wakes no waiter, because a boundary that failed to prove the stop
// must present the next boundary with that same unproven stop rather than with
// terminal evidence this one invented. The fence marks where the stream ended;
// only the completed-boundary latch says a close settled.
func (s *session) fenceBoundary() {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	stream := s.lifecycleStreamLocked()
	if stream == nil {
		return
	}

	stream.Close()
}

// lifecycleActionMeta stamps one outbound permission or elicitation request with
// its action correlation. The value names the emitting stream and the action, and
// it never replaces the routing envelope a callback is authenticated by.
func (s *session) lifecycleActionMeta(action *pendingAction) map[string]any {
	if action == nil || action.binding == nil || action.binding.stream == nil {
		return nil
	}

	return map[string]any{lifecycle.MetaKey: lifecycle.ActionCorrelation{
		StreamID: action.binding.stream.ID(), ActionID: action.id, Owner: action.owner, RunID: action.runID,
	}.Value()}
}

func (s *session) lifecycleStreamLocked() *lifecycle.Stream {
	if s.incarnation == nil {
		return nil
	}

	return s.incarnation.stream
}
