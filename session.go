package opencodeacp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/lifecycle"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

const sessionUpdateAvailableCommands = "available_commands_update"

// sessionRecoveryGate is the context-aware admission boundary shared by
// runtime recovery, MCP refresh, and close. Its zero value is ready to use so
// narrowly constructed test sessions remain valid. A timed-out waiter owns no
// goroutine and no future admission: it simply leaves the permit with the
// current owner, which is essential for predecessor-first replacement.
type sessionRecoveryGate struct {
	initMu sync.Mutex
	permit chan struct{}
}

func (g *sessionRecoveryGate) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	permit := g.permitChannel()

	select {
	case <-permit:
		if err := ctx.Err(); err != nil {
			permit <- struct{}{}

			return err
		}

		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *sessionRecoveryGate) unlock() {
	select {
	case g.permitChannel() <- struct{}{}:
	default:
		panic("unlock unlocked session recovery gate")
	}
}

func (g *sessionRecoveryGate) permitChannel() chan struct{} {
	g.initMu.Lock()
	defer g.initMu.Unlock()

	if g.permit == nil {
		g.permit = make(chan struct{}, 1)
		g.permit <- struct{}{}
	}

	return g.permit
}

type session struct {
	agent                 *Agent
	id                    acp.SessionId
	cwd                   string
	additionalDirectories []string
	idmap                 idmapRecord
	title                 string
	updatedAt             string
	providerID            string
	modelID               string
	mode                  string
	permission            string
	secretNeedles         []string
	outputSchema          map[string]any
	rawMessages           rawMessageConfig
	// carrier is the environment and search-path prefix this session was
	// admitted under. Recovery rebinds the native session to these values
	// rather than to whatever the runtime last carried.
	carrier sessionCarrier

	client            opencode.Client
	delivery          *sessionDelivery
	directoryRelease  func()
	mcpServers        []opencode.MCPServerConfig
	mcpRefreshPending bool
	recoveryMu        sessionRecoveryGate
	// establishMu serializes the short state transitions around establishment.
	// The opening host notification runs without it held; establishing keeps a
	// second caller waiting on the one immutable result.
	establishMu              sync.Mutex
	establishing             bool
	establishmentAttempt     chan struct{}
	establishmentAttemptErr  error
	established              bool
	establishmentWritten     bool
	establishmentFailed      error
	establishmentReady       chan struct{}
	establishmentReadyClosed bool

	turn chan struct{}
	mu   sync.Mutex
	// pump is the session-owned consumer of the native event stream for the
	// current runtime binding.
	pump *sessionPump
	// dispatchGate holds native event routing across a dispatch acknowledgement,
	// so no event caused by a frame reaches a host before the acceptance that
	// explains it.
	dispatchGate sync.Mutex
	// lifecycleMu guards the lifecycle stream and the foreground cycle. Both the
	// pump and a foreground prompt emit, so one mutex fixes one order.
	lifecycleMu              sync.Mutex
	incarnation              *nativeIncarnationBinding
	lifecycleFailed          error
	lifecycleOpened          bool
	cycle                    *foregroundCycle
	pendingAgentObservations uint64
	cycleCounter             uint64
	turnCounter              uint64
	updateMu                 sync.Mutex
	cancel                   context.CancelFunc
	turnDone                 <-chan struct{}
	cancelled                bool
	// pendingDispatchFailure carries a native failure that arrived while a
	// frame was still awaiting acceptance, where no cycle exists to carry it.
	pendingDispatchFailure  error
	emittedPartText         map[string]string
	emittedTools            map[string]emittedToolState
	emittedUsage            map[string]emittedUsageState
	turnEpoch               uint64
	turnNonce               string
	submission              lifecycle.Submission
	imageArtifacts          map[string]imageArtifactRecord
	imageArtifactIdentities map[string]string
	emittedToolContent      map[string][]imageOutputItem
	emittedFileParts        map[string]struct{}
	activeMessageIDs        map[string]struct{}
	publishedToolCalls      map[string]struct{}
	exclusiveTurn           bool
	commandsByName          map[string]opencode.NativeCommand
	availableCommands       []acp.AvailableCommand
	commandCatalogPublished bool
	contextWindows          map[string]int
	messageRoles            map[string]string
	poisonCause             string
	runtimeLostCause        string
	runtimeGeneration       uint64
	closed                  bool
	// retainedCapture holds the generation a close boundary read before it
	// contained the native scope. The capture needs the loopback API and the
	// containment proof takes it away, so a boundary that fails after the
	// capture could never read it again: the material a still-owed commit needs
	// is retained rather than destroyed with the scope, and the retry commits
	// exactly what this capture read.
	retainedCapture *capturedStateSnapshot
	// boundarySettled records that a close boundary ran to completion: the
	// containment proof passed, the durable rung it owed committed, and the
	// terminal transition was made. It is deliberately not `closed`, which
	// latches at ladder step 1 so no further prompt is admitted. A close that
	// failed a rung has stopped admitting work but has proved nothing, so it
	// leaves this false and the next close re-runs the whole boundary.
	boundarySettled bool
}

type sessionSnapshot struct {
	id                    acp.SessionId
	cwd                   string
	additionalDirectories []string
	idmap                 idmapRecord
	title                 string
	updatedAt             string
	providerID            string
	modelID               string
	mode                  string
	permission            string
	rawMessages           rawMessageConfig
	carrier               sessionCarrier
	client                opencode.Client
}

func newSession(agent *Agent, id acp.SessionId, cwd string, additionalDirectories []string, native opencode.NativeSession, client opencode.Client, meta sessionMeta, idmap idmapRecord) *session {
	title := native.Title
	if title == "" {
		title = "OpenCode session"
	}

	updatedAt := time.Now().UTC().Format(time.RFC3339)
	if native.Time.Updated > 0 {
		updatedAt = time.UnixMilli(native.Time.Updated).UTC().Format(time.RFC3339)
	}

	providerID := native.Model.ProviderID

	modelID := firstNonEmpty(native.Model.ModelID, native.Model.ID)
	if meta.Model != "" {
		providerID, modelID = splitModelValue(meta.Model, providerID, modelID)
	}

	if idmap.NativeSessionID == "" {
		idmap.NativeSessionID = native.ID
	}

	now := time.Now().UnixMilli()
	if idmap.CreatedAtUnixMilli == 0 {
		idmap.CreatedAtUnixMilli = now
	}

	idmap.UpdatedAtUnixMilli = now

	session := &session{
		agent:                   agent,
		id:                      id,
		cwd:                     cwd,
		additionalDirectories:   append([]string(nil), additionalDirectories...),
		idmap:                   idmap,
		title:                   title,
		updatedAt:               updatedAt,
		providerID:              providerID,
		modelID:                 modelID,
		mode:                    firstNonEmpty(meta.Mode, native.Agent, defaultMode),
		permission:              normalizeOpenCodePermission(meta.Permission),
		outputSchema:            cloneAnyMap(meta.OutputSchema),
		rawMessages:             meta.RawMessages,
		carrier:                 newSessionCarrier(meta.Env, meta.ExtraPathDirs),
		client:                  client,
		delivery:                newSessionDelivery(agent, id),
		emittedPartText:         map[string]string{},
		emittedTools:            map[string]emittedToolState{},
		emittedUsage:            map[string]emittedUsageState{},
		imageArtifacts:          map[string]imageArtifactRecord{},
		imageArtifactIdentities: map[string]string{},
		emittedToolContent:      map[string][]imageOutputItem{},
		emittedFileParts:        map[string]struct{}{},
		activeMessageIDs:        map[string]struct{}{},
		publishedToolCalls:      map[string]struct{}{},
		establishmentReady:      make(chan struct{}),
	}

	session.openLifecycleStream()

	return session
}

// establish opens this incarnation's lifecycle stream to the host and starts
// routing native events. The opening snapshot goes out first, so it is the first
// lifecycle envelope a host sees for the incarnation and no native event is
// routed before it. Events the harness published in the meantime wait in the
// native stream's own buffer and are routed in arrival order as soon as the pump
// starts, so establishment holds no separate spool.
//
// A recovered incarnation calls this directly: it has just reopened its stream
// and owes a snapshot of its own whatever the previous one did.
func (s *session) establish(ctx context.Context) error {
	return s.establishCurrentIncarnation(ctx, true)
}

// ensureEstablished establishes the session once after its complete response has
// been written. The transport-owned post-response hook is the only production
// caller; prompt admission merely observes the resulting establishment state.
func (s *session) ensureEstablished(ctx context.Context) error {
	return s.establishCurrentIncarnation(ctx, false)
}

func (s *session) establishCurrentIncarnation(ctx context.Context, replace bool) error {
	s.establishMu.Lock()
	if s.established && !replace {
		s.establishMu.Unlock()

		return nil
	}

	if s.establishmentFailed != nil {
		failure := s.establishmentFailed
		s.establishMu.Unlock()

		return failure
	}

	if s.establishing {
		attempt := s.establishmentAttempt
		s.establishMu.Unlock()

		select {
		case <-attempt:
		case <-ctx.Done():
			return ctx.Err()
		}

		s.establishMu.Lock()
		attemptErr := s.establishmentAttemptErr
		s.establishMu.Unlock()

		return attemptErr
	}

	s.establishing = true

	s.establishmentAttempt = make(chan struct{})
	s.establishmentAttemptErr = nil

	if replace {
		s.established = false
	}
	s.establishMu.Unlock()

	binding := s.currentIncarnation()
	if err := s.publishLifecycleStream(ctx); err != nil {
		failure := errors.New("session establishment failed")
		s.failNativeIncarnationCause(binding, "session establishment failed", failure)

		s.establishMu.Lock()

		s.establishing = false
		if s.establishmentFailed == nil {
			s.establishmentFailed = failure
		}

		s.closeEstablishmentReadyLocked()
		s.establishmentAttemptErr = s.establishmentFailed
		close(s.establishmentAttempt)
		s.establishMu.Unlock()

		return err
	}

	s.startPump()

	s.establishMu.Lock()

	s.establishing = false
	s.established = true
	s.closeEstablishmentReadyLocked()
	close(s.establishmentAttempt)
	s.establishMu.Unlock()

	return nil
}

func (s *session) markEstablishmentResponseWritten() {
	s.establishMu.Lock()
	s.establishmentWritten = true
	s.establishMu.Unlock()
}

func (s *session) failEstablishment(_ error) {
	s.establishMu.Lock()
	if s.established || s.establishmentFailed != nil {
		s.establishMu.Unlock()

		return
	}

	failure := errors.New("session establishment response was not fully written")
	s.establishmentFailed = failure
	s.closeEstablishmentReadyLocked()
	s.establishMu.Unlock()

	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.latchLifecycleIncarnationLoss("establishing response was not delivered", failure)

	go func() {
		defer handleAgentGoroutinePanicRecover(context.Background(), agentLogger(s.agent), "failed session establishment close", nil)

		closeCtx, closeCancel := context.WithTimeout(context.Background(), closeTimeout)
		_ = s.Close(closeCtx)

		closeCancel()
	}()
}

func (s *session) closeEstablishmentReadyLocked() {
	if !s.establishmentReadyClosed {
		close(s.establishmentReady)
		s.establishmentReadyClosed = true
	}
}

func (s *session) requireEstablished(ctx context.Context) error {
	s.establishMu.Lock()
	written := s.establishmentWritten
	ready := s.establishmentReady
	s.establishMu.Unlock()

	if !written {
		return acp.NewInvalidRequest(map[string]any{jsonFieldError: "session establishment response not written"})
	}

	select {
	case <-ready:
	case <-ctx.Done():
		return ctx.Err()
	}

	s.establishMu.Lock()
	defer s.establishMu.Unlock()

	if s.establishmentFailed != nil || !s.established {
		return acp.NewInvalidRequest(map[string]any{jsonFieldError: "session establishment failed"})
	}

	return nil
}

func (s *session) acquireTurn(ctx context.Context) (func(), error) {
	return s.acquireTurnSlot(ctx, false)
}

func (s *session) acquireCommandTurn(ctx context.Context) (func(), error) {
	return s.acquireTurnSlot(ctx, true)
}

func (s *session) acquireTurnSlot(ctx context.Context, exclusive bool) (func(), error) {
	turn := s.turnQueue()
	s.mu.Lock()
	defer s.mu.Unlock()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	if err := s.poisonedErrorLocked(); err != nil {
		return nil, err
	}

	if exclusive {
		if s.exclusiveTurn || len(turn) > 0 {
			return nil, acp.NewInvalidRequest(map[string]any{jsonFieldError: valBackpressure, jsonFieldLimit: limitSessionPrompt})
		}

		s.exclusiveTurn = true

		turn <- struct{}{}

		return func() {
			s.mu.Lock()
			<-turn

			s.exclusiveTurn = false
			s.mu.Unlock()
		}, nil
	}

	if s.exclusiveTurn || len(turn) >= cap(turn) {
		return nil, acp.NewInvalidRequest(map[string]any{jsonFieldError: valBackpressure, jsonFieldLimit: limitSessionPrompt})
	}

	turn <- struct{}{}

	return func() {
		s.mu.Lock()
		<-turn
		s.mu.Unlock()
	}, nil
}

// contextWindow returns the model's true context-window size in tokens for the
// given native provider/model, caching the lookup per model. It returns 0 when
// the size is genuinely unavailable, never a fabricated value.
func (s *session) contextWindow(ctx context.Context, providerID string, modelID string) int {
	value := joinModelValue(providerID, modelID)
	if value == "" {
		return 0
	}

	s.mu.Lock()
	if s.contextWindows == nil {
		s.contextWindows = make(map[string]int, 1)
	}

	if size, ok := s.contextWindows[value]; ok {
		s.mu.Unlock()

		return size
	}

	client := s.client
	s.mu.Unlock()

	if client == nil {
		return 0
	}

	providers, err := client.ConfigProviders(ctx)
	if err != nil {
		return 0
	}

	size, _ := providers.ModelContextWindow(value)

	s.mu.Lock()
	s.contextWindows[value] = size
	s.mu.Unlock()

	return size
}

func (s *session) turnQueue() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.turn == nil {
		s.turn = make(chan struct{}, sessionTurnCapacity)
	}

	return s.turn
}

// failPendingDispatch fails a frame still awaiting acceptance. The failure is
// recorded for the dispatch classification and the turn context is released, so
// the blocked route answers instead of waiting out a run that already died.
// With no turn in flight there is nothing to fail and the event is the pump's to
// judge.
func (s *session) failPendingDispatch(err error) {
	s.lifecycleMu.Lock()

	if s.cancel == nil {
		s.lifecycleMu.Unlock()

		return
	}

	if s.pendingDispatchFailure == nil {
		s.pendingDispatchFailure = err
	}

	cancel := s.cancel
	s.lifecycleMu.Unlock()

	cancel()
}

// takePendingDispatchFailure reports the failure recorded against the frame the
// session was dispatching, if any.
func (s *session) takePendingDispatchFailure() error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	return s.pendingDispatchFailure
}

func (s *session) finishTurn() {
	s.lifecycleMu.Lock()
	cancel := s.cancel
	s.cancel = nil

	s.turnDone = nil
	s.cancelled = false
	s.turnNonce = ""
	s.submission = lifecycle.Submission{}
	s.lifecycleMu.Unlock()

	s.mu.Lock()
	s.updatedAt = time.Now().UTC().Format(time.RFC3339)
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
}

// currentSubmission reports the submission identity bound to the active turn.
func (s *session) currentSubmission() lifecycle.Submission {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	return s.submission
}

func (s *session) currentTurnNonce() string {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	return s.turnNonce
}

// cancelTurn is the session-scoped native interrupt. It marks the active turn
// cancelled, releases its context so the waiting prompt arbitrates, and asks
// OpenCode to abort this session's work. No peer session and no shared runtime is
// touched: routine cancellation is one session putting its own work down, not a
// containment event.
func (s *session) cancelTurn(ctx context.Context) error {
	s.lifecycleMu.Lock()
	cancel := s.cancel
	alreadyCancelled := s.cancelled
	s.cancelled = true
	binding := s.incarnation

	var client opencode.Client

	if binding != nil {
		client = binding.client
	}

	cycle := s.cycle
	interrupt := cycle != nil && !cycle.interrupted && !cycle.settled

	if cycle != nil {
		cycle.interrupted = true
	} else {
		interrupt = !alreadyCancelled
	}
	s.lifecycleMu.Unlock()

	s.mu.Lock()
	nativeID := s.idmap.NativeSessionID
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	// One turn is interrupted once, whichever mechanism gets there first: a
	// cancel notification, the cancelled turn's own settlement, and a delivery
	// failure all share the open cycle's interrupt claim, and a second native
	// abort would interrupt whatever the session does next. Before acceptance
	// no cycle exists to carry the claim, so the cancelled flag itself is the
	// once-guard there.
	// A request can be blocked in its registration handshake while the pump is
	// therefore unable to route the native idle produced by Abort. Cancel those
	// exact request contexts first; their admission handshakes will release
	// without ever announcing an unanswerable action.
	s.cancelActionAdmissions()

	if !interrupt || client == nil || nativeID == "" {
		return nil
	}

	if err := client.Abort(ctx, nativeID); err != nil {
		s.recordInterruptFailure(err)

		return fmt.Errorf("interrupt native OpenCode session: %w", err)
	}

	return nil
}

// interruptNativeWork asks OpenCode to stop this session's current work without
// marking the session cancelled: the caller is ending a turn on a failure, and
// cancellation state belongs only to a cancel the host asked for.
func (s *session) interruptNativeWork(ctx context.Context) {
	s.mu.Lock()
	client := s.client
	nativeID := s.idmap.NativeSessionID
	s.mu.Unlock()

	if client == nil || nativeID == "" {
		return
	}

	if err := client.Abort(ctx, nativeID); err != nil {
		s.recordInterruptFailure(err)
	}
}

// requireActiveTurn refuses a cancel that does not address the session's current
// turn. A stale or absent route is never applied to a later turn.
func (s *session) requireActiveTurn(turnNonce string) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if s.cancel == nil || turnNonce == "" || s.turnNonce != turnNonce {
		return invalidRoute("cancel route is missing, stale, or does not target the active turn")
	}

	return nil
}

// recordInterruptFailure records that the native interrupt itself failed. The
// open cycle escalates immediately rather than waiting out the settlement budget:
// a harness that refused the interrupt is not going to report the idle the
// cancellation needs.
func (s *session) recordInterruptFailure(err error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if s.cycle == nil {
		return
	}

	if s.cycle.lost == nil {
		s.cycle.lost = err
	}

	s.cycle.wake()
}

// awaitNativeSettlement waits for the native terminal evidence of one cycle. It
// is the cancellation and close boundary: OpenCode has been asked to stop, and
// this is the acknowledgement that it did.
func (s *session) awaitNativeSettlement(ctx context.Context, cycle *foregroundCycle) error {
	if cycle == nil {
		return nil
	}

	select {
	case <-cycle.signal:
		s.lifecycleMu.Lock()
		lost := cycle.lost
		s.lifecycleMu.Unlock()

		return lost
	case <-ctx.Done():
		return fmt.Errorf("OpenCode session did not report idle after the interrupt: %w", ctx.Err())
	}
}

// awaitCloseSettlement waits for the close boundary's cycle to acquire terminal
// evidence. An incarnation loss is terminal evidence rather than an unproven
// interrupt: the source that would have reported the idle is gone, so no boundary
// will ever obtain one, and the rungs below this one — the containment proof and
// both durable commits — run whether or not the incarnation survived to say
// anything.
//
// What it reads is the cycle's own record of that loss, never the fence on the
// stream. A close boundary fences on its way out whatever it proved, so a fence
// says only that this incarnation has stopped speaking; reading it as evidence
// would let a failed boundary's own mark answer for the stop the harness never
// made, and the retry — or the delete behind it — would contain and delete over
// native work still running.
//
// A cycle woken by a refused native interrupt carries its failure in the same
// member and carries no loss of incarnation: the harness is still there, it
// declined to stop, and the work this boundary asked it to put down was never
// proved stopped. That does not settle the boundary — it stops the ladder exactly
// where a cycle that never reported at all does, rather than letting close report
// the turn cancelled and the session idle over native work still running.
func (s *session) awaitCloseSettlement(ctx context.Context, cycle *foregroundCycle) error {
	err := s.awaitNativeSettlement(ctx, cycle)
	if err == nil {
		return nil
	}

	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if cycle != nil && cycle.incarnationLost {
		return nil
	}

	return err
}

// currentClient reports the runtime client this session is bound to.
func (s *session) currentClient() opencode.Client {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.client
}

type nativeIncarnationContextKey struct{}

// nativeIncarnationBinding is the immutable authority for one native session
// incarnation. Actions retain this pointer, and every operation that can remove,
// answer, fail, or terminalize one requires pointer identity with the session's
// current binding. Recovery replaces the complete tuple in one lifecycle-locked
// write; no action can observe a mixed client, generation, stream, and registry.
type nativeIncarnationBinding struct {
	client     opencode.Client
	generation uint64
	stream     *lifecycle.Stream
	registry   *actionRegistry
}

func withNativeIncarnationBinding(ctx context.Context, binding *nativeIncarnationBinding) context.Context {
	if binding == nil {
		return ctx
	}

	return context.WithValue(ctx, nativeIncarnationContextKey{}, binding)
}

func (s *session) currentIncarnation() *nativeIncarnationBinding {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	return s.incarnation
}

func (s *session) incarnationIsCurrent(binding *nativeIncarnationBinding) bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	return binding != nil && s.incarnation == binding
}

func (s *session) nativeIncarnationForContext(ctx context.Context) *nativeIncarnationBinding {
	if binding, ok := ctx.Value(nativeIncarnationContextKey{}).(*nativeIncarnationBinding); ok && binding != nil {
		return binding
	}

	return s.currentIncarnation()
}

// failNativeIncarnation latches a delivery or transport gap against only the
// binding that produced it. The shared generation is removed from admission
// before the caller returns; containment then runs asynchronously so a pump may
// exit before retirement joins it. A stale failure from an older binding cannot
// fence a replacement.
func (s *session) failNativeIncarnation(binding *nativeIncarnationBinding, err error) {
	if err == nil {
		return
	}

	s.failNativeIncarnationCause(binding, "native lifecycle delivery failed", err)
}

func (s *session) failNativeIncarnationCause(
	binding *nativeIncarnationBinding,
	cause string,
	err error,
) {
	if err == nil || binding == nil {
		return
	}

	s.lifecycleMu.Lock()
	if s.incarnation != binding {
		s.lifecycleMu.Unlock()

		return
	}

	// Claim the failure, cancellation authority, stream fence, and terminal
	// cycle evidence in the same synchronization domain as the incarnation
	// pointer. A recovery swap can therefore happen wholly before or wholly after
	// this failure, never between its identity check and its mutation.
	if s.cancel != nil && s.pendingDispatchFailure == nil {
		s.pendingDispatchFailure = err
	}

	cancel := s.cancel

	if binding.stream != nil {
		binding.stream.Close()
	}

	loss := fmt.Errorf("active lifecycle incarnation lost: %s", cause)
	cycle := s.cycle

	if cycle != nil && !cycle.settled {
		if cycle.failure == nil {
			cycle.failure = err
		}

		if cycle.lost == nil {
			cycle.lost = loss
		}

		cycle.incarnationLost = true
		cycle.wake()
	}

	if binding.stream != nil && s.lifecycleFailed == nil {
		s.lifecycleFailed = loss
	}

	agentOrigin := cycle != nil && cycle.origin != lifecycle.CauseSubmission
	s.lifecycleMu.Unlock()

	if cancel != nil {
		cancel()
	}

	if s.agent != nil && binding.generation != 0 {
		s.agent.containSharedRuntimeGeneration(binding.generation, cause)
	}

	if agentOrigin {
		go func() {
			ctx := context.Background()
			defer recoverAgentGoroutine(ctx, agentLogger(s.agent), "failed OpenCode agent cycle settlement")

			_ = s.settleAgentCycle(ctx, cycle)
		}()
	}
}

// lifecycleStreamAbsent reports that this session carries no lifecycle stream, so
// nothing may be emitted or correlated on one.
func (s *session) lifecycleStreamAbsent() bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	return s.lifecycleStreamLocked() == nil
}

// commitForegroundPrefix commits the native-safe prefix of the session's current
// foreground work. Every terminal path — success, failure, and cancellation —
// runs it before the ending lifecycle transition, so a reload can never restore a
// generation older than the one the stream already reported as finished.
func (s *session) commitForegroundPrefix(ctx context.Context) error {
	s.mu.Lock()
	s.activeMessageIDs = map[string]struct{}{}
	s.mu.Unlock()

	captured, err := s.captureStateSnapshot(ctx, true)
	if err != nil {
		return err
	}

	return s.commitStateSnapshot(ctx, captured)
}
func (s *session) wasCancelled() bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	return s.cancelled
}

// parentSessionID names this session's ACP fork parent, or the empty id for a
// lineage root. It is durable identity: it is set when the session is created or
// hydrated and survives every runtime recovery.
func (s *session) parentSessionID() acp.SessionId {
	s.mu.Lock()
	defer s.mu.Unlock()

	return acp.SessionId(s.idmap.ParentSessionID)
}

// requireMCPRefresh marks this session's directory MCP registration as needing
// re-registration before its next prompt. A co-holder of the same canonical
// directory unregistered the shared servers when it left, and the registration
// is per directory rather than per scope, so the survivors reinstate it.
func (s *session) requireMCPRefresh() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.mcpServers) > 0 {
		s.mcpRefreshPending = true
	}
}

func (s *session) ensureNotPoisoned() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.poisonedErrorLocked()
}

func (s *session) poisonedErrorLocked() error {
	if s.poisonCause == "" {
		return nil
	}

	return poisonedSessionError(s.poisonCause)
}

// poisonCauseNativeSessionDrift is the one poison cause this adapter can reach:
// the native harness attributed a message or part to a session id the logical
// session does not own. The vocabulary is closed, so the drifting field and the
// two ids stay in the debug stream rather than on the wire.
const poisonCauseNativeSessionDrift = "native_session_id_drift"

// poisonedSessionError is the refusal a poisoned session answers every operation
// but close and delete with. It is an internal error, never invalid request: the
// caller's params are fine and the session's own state is what refuses.
func poisonedSessionError(cause string) error {
	return acp.NewInternalError(map[string]any{
		jsonFieldError: valSessionPoisoned,
		jsonFieldCause: cause,
	})
}

func (s *session) poisonNativeSessionDrift(ctx context.Context, field string, actual string) error {
	if s.agent != nil && s.agent.log != nil {
		s.agent.log.DebugContext(ctx, "OpenCode native session id drift",
			slog.String("session_id", string(s.id)),
			slog.String("field", field),
			slog.String("expected_native_session_id", s.idmap.NativeSessionID),
			slog.String("actual_native_session_id", actual))
	}

	return s.poison(ctx, poisonCauseNativeSessionDrift)
}

func (s *session) poison(ctx context.Context, cause string) error {
	err := poisonedSessionError(cause)

	s.mu.Lock()
	if s.poisonCause != "" {
		existing := s.poisonedErrorLocked()
		s.mu.Unlock()

		return existing
	}

	s.poisonCause = cause

	shouldClear := len(s.availableCommands) > 0
	if shouldClear {
		s.availableCommands = []acp.AvailableCommand{}
		s.commandsByName = map[string]opencode.NativeCommand{}
	}
	s.mu.Unlock()

	if !shouldClear {
		return err
	}

	clearErr := s.emitUpdate(ctx, acp.SessionUpdate{
		AvailableCommandsUpdate: &acp.SessionAvailableCommandsUpdate{
			SessionUpdate:     sessionUpdateAvailableCommands,
			AvailableCommands: []acp.AvailableCommand{},
		},
	})

	return errors.Join(err, clearErr)
}

func (s *session) markActiveMessageID(messageID string) {
	if messageID == "" {
		return
	}

	s.mu.Lock()
	if s.activeMessageIDs == nil {
		s.activeMessageIDs = map[string]struct{}{}
	}

	s.activeMessageIDs[messageID] = struct{}{}
	s.mu.Unlock()
}

// markPublishedToolCall records a tool call this session has published to the
// host. The set is session-scoped rather than turn-scoped: a native permission
// can arrive for a call published before the current foreground cycle, and it
// stays answerable, while a call this session never published is refused.
func (s *session) markPublishedToolCall(toolCallID string) {
	if toolCallID == "" {
		return
	}

	s.mu.Lock()
	if s.publishedToolCalls == nil {
		s.publishedToolCalls = map[string]struct{}{}
	}

	s.publishedToolCalls[toolCallID] = struct{}{}
	s.mu.Unlock()
}

// publishedToolCall reports whether this session published the named tool call.
func (s *session) publishedToolCall(toolCallID string) bool {
	if toolCallID == "" {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	_, ok := s.publishedToolCalls[toolCallID]

	return ok
}

// recordMessageRole remembers the native role declared for a message so live
// part events can be attributed. OpenCode emits `message.updated` (carrying
// the role) before any `message.part.*` event for that message.
func (s *session) recordMessageRole(info opencode.NativeMessageInfo) {
	if info.ID == "" || info.Role == "" {
		return
	}

	s.mu.Lock()
	if s.messageRoles == nil {
		s.messageRoles = map[string]string{}
	}

	s.messageRoles[info.ID] = info.Role
	s.mu.Unlock()
}

func (s *session) messageRole(messageID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.messageRoles[messageID]
}

func (s *session) snapshot() sessionSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	return sessionSnapshot{
		id:                    s.id,
		cwd:                   s.cwd,
		additionalDirectories: append([]string(nil), s.additionalDirectories...),
		idmap:                 s.idmap,
		title:                 s.title,
		updatedAt:             s.updatedAt,
		providerID:            s.providerID,
		modelID:               s.modelID,
		mode:                  s.mode,
		permission:            s.permission,
		rawMessages:           s.rawMessages,
		carrier:               s.carrier.clone(),
		client:                s.client,
	}
}

func (s *session) setModel(value string) {
	provider, model := splitModelValue(value, "", "")

	s.mu.Lock()
	s.providerID = provider
	s.modelID = model
	s.mu.Unlock()
}

func (s *session) setMode(value string) {
	s.mu.Lock()
	s.mode = value
	s.mu.Unlock()
}

// modelSelector reports the model this session addresses its native frames
// with. A half-named model addresses nothing, so the frame carries no selector
// at all and OpenCode picks its own default; a named one is passed through
// unjudged, because the runtime that runs it is the authority on whether it
// resolves.
func (s *session) modelSelector() (opencode.ModelSelector, bool) {
	snapshot := s.snapshot()

	if snapshot.providerID == "" || snapshot.modelID == "" {
		return opencode.ModelSelector{}, false
	}

	return opencode.ModelSelector{ProviderID: snapshot.providerID, ModelID: snapshot.modelID}, true
}

func (s *session) currentMode() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.mode
}

func (s *session) currentModel() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return joinModelValue(s.providerID, s.modelID)
}

func (s *session) commandContext() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.mode, joinModelValue(s.providerID, s.modelID)
}

func (s *session) cachedCommand(name string) (opencode.NativeCommand, bool) {
	if !validSlashCommandName(name) {
		return opencode.NativeCommand{}, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	cmd, ok := s.commandsByName[name]

	return cmd, ok
}

func (s *session) refreshCommands(ctx context.Context) error {
	if err := s.ensureNotPoisoned(); err != nil {
		return err
	}

	commands, err := s.client.Commands(ctx)
	if err != nil {
		return err
	}

	commandsByName := make(map[string]opencode.NativeCommand, len(commands))

	available := make([]acp.AvailableCommand, 0, len(commands))
	for i := range commands {
		command := &commands[i]
		if !validSlashCommandName(command.Name) {
			continue
		}

		commandsByName[command.Name] = *command
		available = append(available, availableCommandFromNative(*command))
	}

	s.mu.Lock()

	changed := !s.commandCatalogPublished || !sameAvailableCommands(s.availableCommands, available)
	if changed {
		s.availableCommands = cloneAvailableCommands(available)
		s.commandCatalogPublished = true
	}

	s.commandsByName = commandsByName
	emit := cloneAvailableCommands(s.availableCommands)
	s.mu.Unlock()

	if !changed {
		return nil
	}

	return s.emitUpdate(ctx, acp.SessionUpdate{
		AvailableCommandsUpdate: &acp.SessionAvailableCommandsUpdate{
			SessionUpdate:     sessionUpdateAvailableCommands,
			AvailableCommands: emit,
		},
	})
}

func availableCommandFromNative(command opencode.NativeCommand) acp.AvailableCommand {
	description := command.Description
	if description == "" {
		source := strings.TrimSpace(command.Source)
		if source == "" {
			description = "OpenCode command"
		} else {
			description = "OpenCode " + source + " command"
		}
	}

	available := acp.AvailableCommand{
		Name:        command.Name,
		Description: description,
	}
	if len(command.Hints) > 0 {
		available.Input = &acp.AvailableCommandInput{
			Unstructured: &acp.UnstructuredCommandInput{Hint: strings.Join(command.Hints, " ")},
		}
	}

	return available
}

func cloneAvailableCommands(in []acp.AvailableCommand) []acp.AvailableCommand {
	if in == nil {
		return nil
	}

	out := make([]acp.AvailableCommand, len(in))
	for i := range in {
		out[i] = in[i]
		if in[i].Meta != nil {
			out[i].Meta = cloneAnyMap(in[i].Meta)
		}
	}

	return out
}

func sameAvailableCommands(left []acp.AvailableCommand, right []acp.AvailableCommand) bool {
	if len(left) == 0 && len(right) == 0 {
		return true
	}

	return reflect.DeepEqual(left, right)
}

func validSlashCommandName(name string) bool {
	if name == "" || strings.Contains(name, "/") || !utf8.ValidString(name) {
		return false
	}

	for _, r := range name {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return false
		}
	}

	return true
}

// Close ends this session at the containment-proving boundary. Every step runs
// under a bounded background context, so a cancelled or expired caller context
// can never skip the ladder, and no step touches a peer session or the shared
// runtime.
//
// Both branches of the boundary fence the stream. The fence is the boundary's
// end-of-emissions mark rather than a proof of containment: this incarnation has
// said everything it will ever say, and a failed boundary — which terminalizes
// nothing, commits nothing new, and emits no quiescence fact — has said the last
// of it too. What the fence never becomes is evidence: settlement is judged from
// the cycle's own loss, and a completed close from the boundary latch.
// ctx bounds waiting to enter the recovery/close gate. Once admitted, the
// boundary runs its detached, internally bounded ladder to completion.
func (s *session) Close(ctx context.Context) error {
	return s.closeSession(ctx, false)
}

// CloseAndCommit is the close boundary that owes a resumable generation. It runs
// the same ladder and, on a containment that completed, additionally commits the
// state a later load or resume restores this conversation from. Agent.Close runs
// it too: the durable rung travels with the ladder, and an embedded shutdown
// dropping state a wire close would have committed is the same lost generation
// however the process ended. Close without the commit belongs to the two callers
// that owe a host no generation at all: a rollback of a session that never
// finished starting, and a delete that has already tombstoned the id. Its
// context has the same admission-only role described by Close.
func (s *session) CloseAndCommit(ctx context.Context) error {
	return s.closeSession(ctx, true)
}

// closeSession runs the boundary once and answers for it. The retry contract is
// the whole point of the two flags it reads: `closed` latches at ladder step 1,
// so a failed close still admits no further prompt, while `boundarySettled`
// latches only after every rung completed. A close that failed a rung therefore
// leaves the session retryable, and the retry re-runs the ladder from the top —
// capture, containment, durable commit — because a boundary that proved nothing
// owes all of it. The completed-boundary latch is the only thing that answers
// "this session was closed"; the fence never is.
//
// Every boundary fences on its way out, because the fence marks the end of what
// this incarnation says rather than the success of what it did. The retry
// therefore runs against a stream already dead, which costs it only the emission
// rungs: the terminal transition is skipped exactly as it is for any other fenced
// incarnation, while capture, containment, and the durable commit all still run.
// It answers silently on success because the failure was already reported to the
// caller who ran the boundary that failed.
func (s *session) closeSession(ctx context.Context, commitResumable bool) error {
	if err := s.recoveryMu.lock(ctx); err != nil {
		return err
	}

	defer s.recoveryMu.unlock()

	s.mu.Lock()

	if s.boundarySettled {
		s.mu.Unlock()

		return nil
	}

	s.closed = true
	s.mu.Unlock()

	err := s.closeBoundary(commitResumable)

	s.fenceBoundary()
	s.delivery.close()

	if err != nil {
		return err
	}

	s.mu.Lock()
	s.boundarySettled = true
	s.mu.Unlock()

	return nil
}

// closeBoundary runs the close ladder in the one order it has: this session's
// native work stops, the state a reload restores from is captured while the
// loopback API can still answer it, the native scope is contained — and only a
// containment that completed earns the terminal transition and the durable
// commit that follow it. A boundary that does not complete terminalizes nothing,
// commits nothing new, and answers with the containment error, because terminal
// is immutable and this session has just failed to prove what it would be
// declaring over.
//
// Terminalization precedes the durable commit, and each rung is a precondition of
// the next: the still-open turn is told the boundary ended it before the boundary
// writes the generation a reload restores, and a commit the store refuses fails
// the close with the stream fenced behind terminalizations that already stand.
//
// The containment proof and the durable commit are unconditional; only the
// terminal transition is a stream rung, and it is emitted only while this session
// still speaks for a live incarnation. Every rung is re-run by a retry, because a
// boundary that answered with an error proved none of them — and the retry loses
// only the emission, which the fence its predecessor left behind skips for the
// same reason any fenced incarnation skips it.
func (s *session) closeBoundary(commitResumable bool) error {
	settleCtx, settleCancel := context.WithTimeout(context.Background(), settlementTimeout)
	cycle, captured, err := s.settleBeforeContainment(settleCtx, commitResumable)

	settleCancel()

	if err != nil {
		return err
	}

	// Pending provider-auth flows are cancelled after pending elicitation is
	// resolved and before the native scope is closed, so a flow is never
	// abandoned to a process that is already being torn down.
	if s.agent != nil && s.agent.providerAuth != nil {
		flowCtx, flowCancel := context.WithTimeout(context.Background(), closeTimeout)
		s.agent.providerAuth.closeSession(flowCtx, s.id)

		flowCancel()
	}

	if err := s.containNativeScope(); err != nil {
		return err
	}

	closeCtx, closeCancel := context.WithTimeout(context.Background(), closeTimeout)
	defer closeCancel()

	if err := s.settleCloseCycle(closeCtx, cycle); err != nil {
		return err
	}

	if err := s.commitStateSnapshot(closeCtx, captured); err != nil {
		return err
	}

	// The generation is durable, so the boundary owes it nothing further and the
	// retained copy is released.
	s.mu.Lock()
	s.retainedCapture = nil
	s.mu.Unlock()

	return nil
}

// settleBeforeContainment stops this session's native work and reads the state a
// reload restores from. Both halves need the loopback API, so both run ahead of
// the containment boundary, and neither writes anything durable: a commit before
// the boundary completes would publish a generation this session has not proved
// contained.
//
// There is state to read whenever a turn was open — that turn's native-safe
// prefix is owed to the stream before its terminal transition — or whenever this
// close commits a resumable generation. A tombstoned session owns no durable row
// at all, so it captures nothing whatever it was closed for.
func (s *session) settleBeforeContainment(
	ctx context.Context,
	commitResumable bool,
) (*foregroundCycle, capturedStateSnapshot, error) {
	cycle := s.currentCycle()

	if err := s.cancelTurn(ctx); err != nil {
		return nil, capturedStateSnapshot{}, containmentFailure(err)
	}

	if err := s.awaitCloseSettlement(ctx, cycle); err != nil {
		return nil, capturedStateSnapshot{}, containmentFailure(err)
	}

	// Every pending action is answered while OpenCode can still receive the
	// answer, and ahead of the capture, which refuses to read a graph still
	// blocked on one.
	s.cancelActions(ctx)

	if (cycle == nil && !commitResumable) || s.tombstoned() {
		return cycle, capturedStateSnapshot{}, nil
	}

	s.mu.Lock()
	retained := s.retainedCapture
	s.mu.Unlock()

	if retained != nil {
		return cycle, *retained, nil
	}

	s.mu.Lock()
	s.activeMessageIDs = map[string]struct{}{}
	s.mu.Unlock()

	captured, err := s.captureStateSnapshot(ctx, true)
	if err != nil {
		return cycle, capturedStateSnapshot{}, err
	}

	s.mu.Lock()
	s.retainedCapture = &captured
	s.mu.Unlock()

	return cycle, captured, nil
}

var errRuntimeConfigurationIncomplete = errors.New("OpenCode runtime configuration incomplete")

// containmentFailure preserves a genuine process-containment classification,
// while assigning loopback session and directory-scope failures to the private
// runtime-configuration boundary they actually belong to.
func containmentFailure(err error) error {
	if err == nil || errors.Is(err, ErrContainmentIncomplete) || errors.Is(err, errRuntimeConfigurationIncomplete) {
		return err
	}

	return errors.Join(errRuntimeConfigurationIncomplete, err)
}

// containNativeScope proves this session's native scope gone: the pump stops
// routing, the directory-scoped client is closed, and the directory binding is
// released. This is the containment boundary the whole close ladder is ordered
// around.
func (s *session) containNativeScope() error {
	s.mu.Lock()
	client := s.client
	nativeID := s.idmap.NativeSessionID
	release := s.directoryRelease
	s.mu.Unlock()

	s.stopPump()

	if client != nil && nativeID != "" {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), closeTimeout)
		err := client.Close(closeCtx)

		closeCancel()

		if err != nil {
			return containmentFailure(err)
		}
	}

	if release != nil {
		release()

		s.mu.Lock()
		s.directoryRelease = nil
		s.mu.Unlock()
	}

	return nil
}

// tombstoned reports that this session's id is durably deleted. A tombstoned
// session owns no store row: writing one would unlist a tombstone this session
// did not create and resurrect a session already reported gone.
func (s *session) tombstoned() bool {
	return s.agent != nil && s.agent.isDeleted(s.id)
}

func (s *session) detachRuntime(generation uint64, cause string) {
	s.mu.Lock()
	if s.closed || s.runtimeGeneration != generation {
		s.mu.Unlock()

		return
	}

	s.lifecycleMu.Lock()

	binding := s.incarnation
	if binding == nil || binding.generation != generation {
		s.lifecycleMu.Unlock()
		s.mu.Unlock()

		return
	}

	s.runtimeGeneration = 0
	s.runtimeLostCause = cause
	cancel := s.cancel
	s.cancel = nil
	s.turnDone = nil
	s.latchLifecycleIncarnationLossLocked(
		binding,
		cause,
		acp.NewInternalError(turnFailedData(causeTransport, cause, 0, "")),
	)
	s.incarnation = nil
	release := s.directoryRelease
	s.directoryRelease = nil
	client := binding.client
	s.lifecycleMu.Unlock()
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	// The pump belongs to the lost binding. Stopping it before the client is
	// closed keeps a routed event from being attributed to a session that no
	// longer holds the runtime that produced it.
	s.stopPump()

	if client != nil {
		ctx, closeCancel := context.WithTimeout(context.Background(), closeTimeout)
		_ = client.Close(ctx)

		closeCancel()
	}

	if release != nil {
		release()
	}
}

func (s *session) ensureRuntime(ctx context.Context) error {
	if err := s.recoveryMu.lock(ctx); err != nil {
		return err
	}

	defer s.recoveryMu.unlock()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()

		return acp.NewInvalidRequest(map[string]any{jsonFieldError: valSessionUnknown})
	}

	if s.runtimeLostCause == "" {
		generation := s.runtimeGeneration
		s.mu.Unlock()

		if generation == 0 || s.agent.runtimeGenerationIsCurrent(generation) {
			return nil
		}

		// The runtime-exit watcher clears the Agent generation before it
		// detaches each session. A prompt entering in that narrow interval
		// performs the same idempotent detach itself rather than touching the
		// already-fenced client.
		s.detachRuntime(generation, valSharedRuntimeExited)
		s.mu.Lock()
	}

	id := s.id
	cwd := s.cwd
	mcpServers := cloneNativeMCPServerConfigs(s.mcpServers)
	carrier := s.carrier.clone()
	s.mu.Unlock()

	storeCtx, cancel := s.agent.sessionStoreContext(ctx)
	idmap, snapshot, ok, err := hydrateStateFromStore(storeCtx, s.agent.sessionStore(), string(id))

	cancel()

	if err != nil {
		return fmt.Errorf("load committed OpenCode recovery generation: %w", err)
	}

	if !ok {
		if failure := s.runtimeFailure(); failure != nil {
			return failure
		}

		return acp.NewInvalidRequest(map[string]any{jsonFieldError: "opencode_recovery_generation_missing"})
	}

	artifacts, err := s.agent.loadAndRehydrateArtifacts(ctx, string(id), snapshot.Events)
	if err != nil {
		return err
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		client, releaseDirectory, generation, err := s.agent.newOpenCodeClient(ctx, id, s.parentSessionID(), cwd, mcpServers, carrier)
		if err != nil {
			return err
		}

		releaseCandidate := func() error {
			return s.agent.closeDirectoryScope(client, releaseDirectory, generation)
		}

		s.agent.restoreMu.Lock()
		native, err := restoreSyncState(ctx, client, snapshot, idmap.NativeSessionID, cwd)
		s.agent.restoreMu.Unlock()

		if err != nil {
			current := s.agent.runtimeGenerationIsCurrent(generation)

			closeErr := releaseCandidate()

			if !current {
				continue
			}

			return errors.Join(fmt.Errorf("restore committed OpenCode recovery generation: %w", err), closeErr)
		}

		if native.ID != idmap.NativeSessionID {
			closeErr := releaseCandidate()

			return errors.Join(
				fmt.Errorf("restored OpenCode native session id drift: expected %q, got %q", idmap.NativeSessionID, native.ID),
				closeErr,
			)
		}

		installed, closed := s.installRecoveredRuntime(client, releaseDirectory, idmap, generation)
		if installed {
			s.setImageArtifacts(artifacts)

			return s.establish(ctx)
		}

		_ = releaseCandidate()

		if closed {
			return acp.NewInvalidRequest(map[string]any{jsonFieldError: valSessionUnknown})
		}
	}
}

func (s *session) installRecoveredRuntime(client opencode.Client, releaseDirectory func(), idmap idmapRecord, generation uint64) (bool, bool) {
	facts := s.agent.lifecycleNegotiated()
	s.agent.mu.Lock()
	defer s.agent.mu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || s.agent.closed {
		return false, true
	}

	if s.agent.runtime == nil || s.agent.runtimeGeneration != generation {
		return false, false
	}

	if exited := s.agent.runtime.RuntimeExited(); exited != nil {
		select {
		case <-exited:
			return false, false
		default:
		}
	}

	s.client = client
	s.directoryRelease = releaseDirectory
	s.idmap = idmap
	s.runtimeGeneration = generation
	s.runtimeLostCause = ""
	s.commandsByName = nil
	s.availableCommands = nil
	s.contextWindows = nil
	s.messageRoles = nil
	s.publishedToolCalls = map[string]struct{}{}
	s.commandCatalogPublished = false

	// Recovery publishes the complete action authority as one immutable tuple.
	// The session is still held out of admission by recoveryMu, and both runtime
	// identity and lifecycle identity become visible before that fence opens.
	s.lifecycleMu.Lock()
	s.lifecycleFailed = nil
	s.cycleCounter = 0
	s.turnCounter = 0
	s.installLifecycleStream(facts)
	s.lifecycleMu.Unlock()

	return true, false
}

func (s *session) runtimeFailure() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.runtimeLostCause == "" {
		return nil
	}

	return acp.NewInternalError(turnFailedData(causeTransport, s.runtimeLostCause, 0, ""))
}

// DeleteNativeAndClose removes the native session and then closes this one. The
// native deletion runs while the loopback API is still online and after this
// session's own work has stopped; the close boundary that follows contains the
// scope whether or not the deletion succeeded, so a refused deletion never
// leaves a native process nobody owns.
//
// It waits on the same settlement the close boundary does, because it is the same
// boundary with a deletion in front of it: the tombstone is already durable by the
// time this runs, so a fenced incarnation that close accepts as terminal evidence
// cannot be the thing that makes delete report failure for a session already
// answered for.
func (s *session) DeleteNativeAndClose(ctx context.Context) error {
	cycle := s.currentCycle()

	settleCtx, settleCancel := context.WithTimeout(context.Background(), settlementTimeout)

	err := s.cancelTurn(settleCtx)
	if err == nil {
		err = s.awaitCloseSettlement(settleCtx, cycle)
	}

	settleCancel()

	if err == nil {
		err = s.deleteNativeSession()
	}

	return errors.Join(err, s.Close(ctx))
}

func (s *session) deleteNativeSession() error {
	s.mu.Lock()
	client := s.client
	nativeID := s.idmap.NativeSessionID
	s.mu.Unlock()

	if client == nil || nativeID == "" {
		return nil
	}

	deleteCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()

	if err := client.DeleteSession(deleteCtx, nativeID); err != nil {
		return fmt.Errorf("delete native OpenCode session: %w", err)
	}

	return nil
}

func (s *session) info() acp.SessionInfo {
	snapshot := s.snapshot()
	title := snapshot.title
	updatedAt := snapshot.updatedAt

	return acp.SessionInfo{
		SessionId:             snapshot.id,
		Cwd:                   snapshot.cwd,
		AdditionalDirectories: snapshot.additionalDirectories,
		Title:                 &title,
		UpdatedAt:             &updatedAt,
		Meta:                  sessionInfoMeta(snapshot),
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return ""
}

func splitModelValue(value string, fallbackProvider string, fallbackModel string) (string, string) {
	if value == "" {
		return fallbackProvider, fallbackModel
	}

	provider, model, ok := strings.Cut(value, "/")
	if !ok || provider == "" || model == "" {
		return fallbackProvider, value
	}

	return provider, model
}

func joinModelValue(provider string, model string) string {
	if provider == "" {
		return model
	}

	if model == "" {
		return provider
	}

	return provider + "/" + model
}
