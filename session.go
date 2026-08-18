package opencodeacp

import (
	"context"
	"errors"
	"fmt"
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

// internalSeamTurnNonce authenticates a turn driven through the internal session
// seam, which deterministic unit tests use in place of the wire. The public Agent
// path never reaches it: both Prompt and Cancel hard-fail on a missing route.
const internalSeamTurnNonce = "internal-seam-turn"

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
	directoryRelease  func()
	mcpServers        []opencode.MCPServerConfig
	mcpRefreshPending bool
	recoveryMu        sync.Mutex

	turn chan struct{}
	mu   sync.Mutex
	// pump is the session-owned consumer of the native event stream for the
	// current runtime binding.
	pump *sessionPump
	// dispatchGate holds native event routing across a dispatch acknowledgement,
	// so no event caused by a frame reaches a host before the acceptance that
	// explains it.
	dispatchGate sync.Mutex
	// dispatchEvidence is armed while a completion-reporting native route waits
	// to learn that the harness admitted its frame.
	dispatchEvidence chan struct{}
	// lifecycleMu guards the lifecycle stream and the foreground cycle. Both the
	// pump and a foreground prompt emit, so one mutex fixes one order.
	lifecycleMu     sync.Mutex
	lifecycleStream *lifecycle.Stream
	lifecycleFailed error
	lifecycleOpened bool
	cycle           *foregroundCycle
	cycleCounter    uint64
	turnCounter     uint64
	actions         *actionRegistry
	updateMu        sync.Mutex
	rawEventMu      sync.Mutex
	cancel          context.CancelFunc
	turnDone        <-chan struct{}
	cancelled       bool
	// pendingDispatchFailure carries a native failure that arrived while a
	// frame was still awaiting acceptance, where no cycle exists to carry it.
	pendingDispatchFailure  error
	rawSeq                  int64
	emittedPartText         map[string]string
	emittedTools            map[string]emittedToolState
	emittedUsage            map[string]emittedUsageState
	turnEpoch               uint64
	turnNonce               string
	submission              lifecycle.Submission
	seamSubmissions         uint64
	imageArtifacts          map[string]imageArtifactRecord
	imageArtifactIdentities map[string]string
	emittedToolContent      map[string][]imageOutputItem
	emittedFileParts        map[string]struct{}
	activeMessageIDs        map[string]struct{}
	publishedToolCalls      map[string]struct{}
	failedStreamEpochs      map[uint64]struct{}
	failedMessageIDs        map[string]struct{}
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

	if idmap.SessionID == "" {
		idmap.SessionID = string(id)
	}

	if idmap.NativeSessionID == "" {
		idmap.NativeSessionID = native.ID
	}

	if idmap.Format == "" {
		idmap.Format = SessionStoreFormat
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
		mode:                    firstNonEmpty(meta.Mode, native.Agent, "build"),
		permission:              normalizeOpenCodePermission(meta.Permission),
		outputSchema:            cloneAnyMap(meta.OutputSchema),
		rawMessages:             meta.RawMessages,
		carrier:                 newSessionCarrier(meta.Env, meta.ExtraPathDirs),
		client:                  client,
		emittedPartText:         map[string]string{},
		emittedTools:            map[string]emittedToolState{},
		emittedUsage:            map[string]emittedUsageState{},
		imageArtifacts:          map[string]imageArtifactRecord{},
		imageArtifactIdentities: map[string]string{},
		emittedToolContent:      map[string][]imageOutputItem{},
		emittedFileParts:        map[string]struct{}{},
		activeMessageIDs:        map[string]struct{}{},
		publishedToolCalls:      map[string]struct{}{},
		failedStreamEpochs:      map[uint64]struct{}{},
		failedMessageIDs:        map[string]struct{}{},
		actions:                 newActionRegistry(),
	}

	session.openLifecycleStream()

	return session
}

// establish opens the session's lifecycle stream to the host and starts routing
// native events. It runs at the end of the establishing session request, so the
// opening snapshot is the first lifecycle envelope a host sees for this session
// and no native event is routed before it. Events the harness published in the
// meantime wait in the native stream's own buffer and are routed in arrival order
// as soon as the pump starts, so establishment holds no separate spool.
func (s *session) establish(ctx context.Context) error {
	if err := s.publishLifecycleStream(ctx); err != nil {
		return err
	}

	s.startPump()

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
			return nil, acp.NewInvalidRequest(map[string]any{jsonFieldError: errValueBackpressure, jsonFieldLimit: limitSessionPrompt})
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
		return nil, acp.NewInvalidRequest(map[string]any{jsonFieldError: errValueBackpressure, jsonFieldLimit: limitSessionPrompt})
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

func (s *session) beginTurn(ctx context.Context, turnNonces ...string) context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()

	turnNonce := internalSeamTurnNonce
	if len(turnNonces) > 0 {
		turnNonce = turnNonces[0]
	}

	turnCtx, cancel := context.WithCancel(ctx)
	turnCtx = withTurnRoute(turnCtx, turnNonce)
	s.cancel = cancel
	s.turnDone = turnCtx.Done()
	s.cancelled = false
	s.pendingDispatchFailure = nil
	s.turnEpoch++
	s.turnNonce = turnNonce

	return turnCtx
}

// failPendingDispatch fails a frame still awaiting acceptance. The failure is
// recorded for the dispatch classification and the turn context is released, so
// the blocked route answers instead of waiting out a run that already died.
// With no turn in flight there is nothing to fail and the event is the pump's to
// judge.
func (s *session) failPendingDispatch(err error) {
	s.mu.Lock()

	if s.cancel == nil {
		s.mu.Unlock()

		return
	}

	if s.pendingDispatchFailure == nil {
		s.pendingDispatchFailure = err
	}

	cancel := s.cancel
	s.mu.Unlock()

	cancel()
}

// takePendingDispatchFailure reports the failure recorded against the frame the
// session was dispatching, if any.
func (s *session) takePendingDispatchFailure() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.pendingDispatchFailure
}

func (s *session) finishTurn() {
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil

	s.turnDone = nil
	s.cancelled = false
	s.turnNonce = ""
	s.submission = lifecycle.Submission{}
	s.updatedAt = time.Now().UTC().Format(time.RFC3339)
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
}

// recordSubmission binds the prompt's submission identity to the turn the route
// nonce authorized. Both envelopes name the same turn, and neither value is
// derived from the other.
func (s *session) recordSubmission(submission lifecycle.Submission) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.submission = submission
}

// currentSubmission reports the submission identity bound to the active turn.
func (s *session) currentSubmission() lifecycle.Submission {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.submission
}

func (s *session) currentTurnNonce() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.turnNonce
}

// cancelTurn is the session-scoped native interrupt. It marks the active turn
// cancelled, releases its context so the waiting prompt arbitrates, and asks
// OpenCode to abort this session's work. No peer session and no shared runtime is
// touched: routine cancellation is one session putting its own work down, not a
// containment event.
func (s *session) cancelTurn(ctx context.Context) error {
	s.mu.Lock()
	cancel := s.cancel
	alreadyCancelled := s.cancelled
	s.cancelled = true
	client := s.client
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
	s.lifecycleMu.Lock()
	cycle := s.cycle
	interrupt := cycle != nil && !cycle.interrupted && !cycle.settled

	if cycle != nil {
		cycle.interrupted = true
	} else {
		interrupt = !alreadyCancelled
	}
	s.lifecycleMu.Unlock()

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
	s.mu.Lock()
	defer s.mu.Unlock()

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

// currentClient reports the runtime client this session is bound to.
func (s *session) currentClient() opencode.Client {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.client
}

// lifecycleStreamAbsent reports that this session carries no lifecycle stream, so
// nothing may be emitted or correlated on one.
func (s *session) lifecycleStreamAbsent() bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	return s.lifecycleStream == nil
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
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.cancelled
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

	return acp.NewInvalidRequest(map[string]any{
		jsonFieldError: "session_poisoned",
		"cause":        s.poisonCause,
	})
}

func (s *session) poisonNativeSessionDrift(ctx context.Context, field string, actual string) error {
	expected := s.idmap.NativeSessionID
	cause := fmt.Sprintf("%s native session id drift: expected %q, got %q", field, expected, actual)

	return s.poison(ctx, cause)
}

func (s *session) poison(ctx context.Context, cause string) error {
	err := acp.NewInternalError(map[string]any{
		jsonFieldError: "opencode_native_session_id_drift",
		"cause":        cause,
	})

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

func (s *session) markStreamFailed(epoch uint64) {
	s.mu.Lock()
	if s.failedMessageIDs == nil {
		s.failedMessageIDs = map[string]struct{}{}
	}

	for messageID := range s.activeMessageIDs {
		s.failedMessageIDs[messageID] = struct{}{}
	}

	if epoch > 0 {
		if s.failedStreamEpochs == nil {
			s.failedStreamEpochs = map[uint64]struct{}{}
		}

		s.failedStreamEpochs[epoch] = struct{}{}
	}

	s.mu.Unlock()
}

func (s *session) shouldSuppressEvent(event opencode.Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if event.StreamEpoch > 0 {
		if _, ok := s.failedStreamEpochs[event.StreamEpoch]; ok {
			return true
		}
	}

	if part, ok := eventPart(event.Properties); ok && part.MessageID != "" {
		_, ok := s.failedMessageIDs[part.MessageID]

		return ok
	}

	return false
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

func (s *session) validatedModelSelector(ctx context.Context, field string) (opencode.ModelSelector, bool, error) {
	snapshot := s.snapshot()

	modelValue := snapshot.modelValue()
	if err := validateModel(ctx, snapshot.client, modelValue, field); err != nil {
		return opencode.ModelSelector{}, false, err
	}

	if snapshot.providerID == "" || snapshot.modelID == "" {
		return opencode.ModelSelector{}, false, nil
	}

	return opencode.ModelSelector{ProviderID: snapshot.providerID, ModelID: snapshot.modelID}, true, nil
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

// Close ends this session. It interrupts the session's own native work, waits
// for OpenCode to acknowledge that the work stopped, commits the native-safe
// prefix, and only then releases the session's resources and fences its lifecycle
// stream. Every step runs under a bounded background context, so a cancelled or
// expired caller context can never skip the ladder, and no step touches a peer
// session or the shared runtime.
func (s *session) Close(_ context.Context) error {
	s.recoveryMu.Lock()
	defer s.recoveryMu.Unlock()

	s.mu.Lock()

	firstClose := !s.closed
	if !firstClose && s.directoryRelease == nil {
		s.mu.Unlock()

		return nil
	}

	s.closed = true
	s.mu.Unlock()

	var cancelErr error

	if firstClose {
		cancelCtx, cancel := context.WithTimeout(context.Background(), settlementTimeout)
		cancelErr = s.settleForShutdown(cancelCtx)

		cancel()
	}

	// Pending provider-auth flows are cancelled after pending elicitation is
	// resolved and before the native interrupt, so a flow is never abandoned to
	// a process that is already being torn down.
	if s.agent != nil && s.agent.providerAuth != nil {
		flowCtx, flowCancel := context.WithTimeout(context.Background(), closeTimeout)
		s.agent.providerAuth.closeSession(flowCtx, s.id)

		flowCancel()
	}

	s.mu.Lock()
	client := s.client
	nativeID := s.idmap.NativeSessionID
	release := s.directoryRelease
	s.mu.Unlock()

	err := cancelErr
	if err == nil {
		s.stopPump()
	}

	if err == nil && client != nil && nativeID != "" {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), closeTimeout)
		err = client.Close(closeCtx)

		closeCancel()
	}

	if err == nil && release != nil {
		release()

		s.mu.Lock()
		s.directoryRelease = nil
		s.mu.Unlock()
	}

	if err == nil {
		s.fenceLifecycle("session closed")
	}

	return err
}

// settleForShutdown interrupts this session's native work and waits for the
// harness to acknowledge that it stopped, then commits the native-safe prefix and
// terminalizes the open cycle. It is the close and delete boundary: a session is
// not released until the work it owned is over and durable.
func (s *session) settleForShutdown(ctx context.Context) error {
	cycle := s.currentCycle()

	if err := s.cancelTurn(ctx); err != nil {
		return err
	}

	if cycle == nil {
		return nil
	}

	if err := s.awaitNativeSettlement(ctx, cycle); err != nil {
		return err
	}

	s.cancelActions(ctx)

	if err := s.commitForegroundPrefix(ctx); err != nil {
		return err
	}

	return s.settleCycle(ctx, cycle, lifecycle.OutcomeCancelled, string(acp.StopReasonCancelled))
}

func (s *session) detachRuntime(generation uint64, cause string) {
	s.mu.Lock()
	if s.closed || s.runtimeGeneration != generation {
		s.mu.Unlock()

		return
	}

	s.runtimeGeneration = 0
	s.runtimeLostCause = cause

	cancel := s.cancel
	s.cancel = nil
	s.turnDone = nil
	release := s.directoryRelease
	s.directoryRelease = nil
	client := s.client
	s.mu.Unlock()

	// The runtime that would have reported the open cycle's idle is gone, so
	// that evidence can never arrive: the loss itself is the terminal evidence,
	// recorded as the cycle's failure before its waiter is woken. The cancelled
	// flag stays untouched — a lost runtime is not a cancellation.
	s.lifecycleMu.Lock()
	if s.cycle != nil && s.cycle.failure == nil && !s.cycle.settled {
		s.cycle.failure = acp.NewInternalError(turnFailedData(causeTransport, cause, 0, ""))
		s.cycle.wake()
	}
	s.lifecycleMu.Unlock()

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

	s.fenceLifecycle(cause)
}

func (s *session) ensureRuntime(ctx context.Context) error {
	s.recoveryMu.Lock()
	defer s.recoveryMu.Unlock()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()

		return acp.NewInvalidRequest(map[string]any{jsonFieldError: errValueSessionUnknown})
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
		s.detachRuntime(generation, "shared OpenCode runtime exited")
		s.mu.Lock()
	}

	id := s.id
	cwd := s.cwd
	model := joinModelValue(s.providerID, s.modelID)
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

		client, releaseDirectory, generation, err := s.agent.newOpenCodeClient(ctx, id, cwd, mcpServers, carrier)
		if err != nil {
			return err
		}

		releaseCandidate := func() error {
			return s.agent.closeDirectoryScope(client, releaseDirectory, generation)
		}

		if validateErr := validateStartupModel(ctx, client, model, modelFieldSessionMeta); validateErr != nil {
			current := s.agent.runtimeGenerationIsCurrent(generation)

			closeErr := releaseCandidate()

			if !current {
				continue
			}

			return errors.Join(validateErr, closeErr)
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

			// The recovered native session is a new incarnation: it gets its own
			// stream identity, its own opening snapshot, and its own pump. A host
			// therefore never reduces the new incarnation's events against the
			// fenced one's ordering.
			s.reopenLifecycleStream()

			return s.establish(ctx)
		}

		_ = releaseCandidate()

		if closed {
			return acp.NewInvalidRequest(map[string]any{jsonFieldError: errValueSessionUnknown})
		}
	}
}

func (s *session) installRecoveredRuntime(client opencode.Client, releaseDirectory func(), idmap idmapRecord, generation uint64) (bool, bool) {
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
	s.actions = newActionRegistry()
	s.commandCatalogPublished = false

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

func (s *session) DeleteNativeAndClose(ctx context.Context) error {
	settleCtx, settleCancel := context.WithTimeout(context.Background(), settlementTimeout)
	err := s.settleForShutdown(settleCtx)

	settleCancel()

	s.mu.Lock()
	client := s.client
	nativeID := s.idmap.NativeSessionID
	s.mu.Unlock()

	if client != nil && nativeID != "" {
		deleteCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		deleteErr := client.DeleteSession(deleteCtx, nativeID)

		cancel()

		if deleteErr != nil {
			return errors.Join(err, fmt.Errorf("delete native OpenCode session: %w", deleteErr))
		}
	}

	return errors.Join(err, s.Close(ctx))
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
