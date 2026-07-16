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
	"github.com/savid/acp-go-opencode/internal/opencode"
)

const sessionUpdateAvailableCommands = "available_commands_update"

var observeCancellationWait = func() {}

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

	client            opencode.Client
	directoryRelease  func()
	mcpServers        []opencode.MCPServerConfig
	mcpRefreshPending bool
	recoveryMu        sync.Mutex

	turn                      chan struct{}
	mu                        sync.Mutex
	updateMu                  sync.Mutex
	rawEventMu                sync.Mutex
	cancel                    context.CancelFunc
	turnDone                  <-chan struct{}
	cancelled                 bool
	cancelling                bool
	cancellationEpoch         uint64
	cancellationResolvedEpoch uint64
	cancellationDone          chan struct{}
	cancellationErr           error
	rawSeq                    int64
	emittedPartText           map[string]string
	emittedTools              map[string]emittedToolState
	emittedUsage              map[string]emittedUsageState
	pending                   map[string]opencode.PermissionRequest
	questions                 map[string]opencode.QuestionRequest
	processedPermission       map[string]struct{}
	processedQuestion         map[string]struct{}
	turnEpoch                 uint64
	turnNonce                 string
	activeMessageIDs          map[string]struct{}
	activeToolCallIDs         map[string]struct{}
	failedStreamEpochs        map[uint64]struct{}
	failedMessageIDs          map[string]struct{}
	exclusiveTurn             bool
	commandsByName            map[string]opencode.NativeCommand
	availableCommands         []acp.AvailableCommand
	contextWindows            map[string]int
	messageRoles              map[string]string
	poisonCause               string
	runtimeLostCause          string
	runtimeGeneration         uint64
	closed                    bool
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

	return &session{
		agent:                 agent,
		id:                    id,
		cwd:                   cwd,
		additionalDirectories: append([]string(nil), additionalDirectories...),
		idmap:                 idmap,
		title:                 title,
		updatedAt:             updatedAt,
		providerID:            providerID,
		modelID:               modelID,
		mode:                  firstNonEmpty(meta.Mode, native.Agent, "build"),
		permission:            normalizeOpenCodePermission(meta.Permission),
		outputSchema:          cloneAnyMap(meta.OutputSchema),
		rawMessages:           meta.RawMessages,
		client:                client,
		emittedPartText:       map[string]string{},
		emittedTools:          map[string]emittedToolState{},
		emittedUsage:          map[string]emittedUsageState{},
		pending:               map[string]opencode.PermissionRequest{},
		questions:             map[string]opencode.QuestionRequest{},
		processedPermission:   map[string]struct{}{},
		processedQuestion:     map[string]struct{}{},
		activeMessageIDs:      map[string]struct{}{},
		failedStreamEpochs:    map[uint64]struct{}{},
		failedMessageIDs:      map[string]struct{}{},
	}
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

	if s.cancelling {
		return nil, acp.NewInvalidRequest(map[string]any{jsonFieldError: "session_cancelling"})
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

	turnNonce := "unit-test-turn"
	if len(turnNonces) > 0 {
		turnNonce = turnNonces[0]
	}

	turnCtx, cancel := context.WithCancel(ctx)
	turnCtx = withTurnRoute(turnCtx, turnNonce)
	s.cancel = cancel
	s.turnDone = turnCtx.Done()
	s.cancelled = false
	s.turnEpoch++
	s.turnNonce = turnNonce
	s.activeMessageIDs = map[string]struct{}{}
	s.activeToolCallIDs = map[string]struct{}{}

	return turnCtx
}

func (s *session) finishTurn() {
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil

	s.turnDone = nil
	if !s.cancelling {
		s.cancelled = false
	}

	s.turnNonce = ""
	s.updatedAt = time.Now().UTC().Format(time.RFC3339)
	s.pending = map[string]opencode.PermissionRequest{}
	s.questions = map[string]opencode.QuestionRequest{}
	s.activeMessageIDs = map[string]struct{}{}
	s.activeToolCallIDs = map[string]struct{}{}
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
}

func (s *session) currentTurnNonce() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.turnNonce
}

func (s *session) cancelTurn() {
	s.signalTurnCancellation(true)
}

func (s *session) signalTurnCancellation(markCancelled bool) {
	s.mu.Lock()

	cancel := s.cancel
	if cancel != nil && markCancelled {
		s.cancelled = true
	}

	pending := make([]opencode.PermissionRequest, 0, len(s.pending))
	for id := range s.pending {
		pending = append(pending, s.pending[id])
	}

	s.pending = map[string]opencode.PermissionRequest{}

	questions := make([]opencode.QuestionRequest, 0, len(s.questions))
	for _, req := range s.questions {
		questions = append(questions, req)
	}

	s.questions = map[string]opencode.QuestionRequest{}
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	ctx, done := context.WithTimeout(context.Background(), closeTimeout)
	defer done()

	for i := range pending {
		_ = s.client.ReplyPermission(ctx, pending[i], permissionReplyReject, reasonCancelled)
	}

	for _, req := range questions {
		_ = s.client.RejectQuestion(ctx, req)
	}
}

// beginCancellation closes turn admission before cancelling the active turn.
// The returned epoch is the native cancellation fence that must resolve before
// another turn may start on this session.
func (s *session) beginCancellation(turnNonce string, requireMatch bool, markCancelled bool) (uint64, error) {
	s.mu.Lock()
	if requireMatch && (s.cancel == nil || turnNonce == "" || s.turnNonce != turnNonce) {
		s.mu.Unlock()

		return 0, invalidRoute("cancel route is missing, stale, or does not target the active turn")
	}

	if s.cancellationResolvedEpoch == s.turnEpoch {
		s.cancelled = s.cancelled || markCancelled
		epoch := s.turnEpoch
		cancel := s.cancel
		s.mu.Unlock()

		if cancel != nil {
			cancel()
		}

		return epoch, nil
	}

	if s.cancelling {
		s.cancelled = s.cancelled || markCancelled
		epoch := s.cancellationEpoch
		s.mu.Unlock()

		return epoch, nil
	}

	if s.cancel == nil {
		s.mu.Unlock()

		return 0, nil
	}

	s.cancelling = true
	s.cancelled = s.cancelled || markCancelled
	s.cancellationEpoch = s.turnEpoch
	s.cancellationErr = nil
	epoch := s.cancellationEpoch
	s.mu.Unlock()

	s.signalTurnCancellation(markCancelled)

	return epoch, nil
}

func (s *session) resolveCancellation(ctx context.Context, epoch uint64) error {
	if epoch == 0 {
		return nil
	}

	for {
		s.mu.Lock()
		if !s.cancelling || s.cancellationEpoch != epoch {
			err := s.cancellationErr
			s.mu.Unlock()

			return err
		}

		if done := s.cancellationDone; done != nil {
			s.mu.Unlock()
			observeCancellationWait()

			select {
			case <-done:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		done := make(chan struct{})
		s.cancellationDone = done
		client := s.client
		nativeID := s.idmap.NativeSessionID
		generation := s.runtimeGeneration
		agent := s.agent
		s.mu.Unlock()

		if client != nil && nativeID != "" {
			// Native abort is advisory. The exact-generation runtime retirement
			// below is the containment certificate and supersedes an abort error.
			_ = client.Abort(ctx, nativeID)
		}

		var err error
		if agent == nil {
			err = errors.Join(opencode.ErrProcessTreeUnproven, errors.New("native runtime owner is unavailable"))
		} else {
			err = agent.retireSharedRuntime(generation, "shared OpenCode runtime retired after turn cancellation", s)
		}

		s.mu.Lock()
		if s.cancellationEpoch == epoch {
			s.cancellationErr = err
			s.cancelling = false

			s.cancellationDone = nil

			if err != nil && s.poisonCause == "" {
				s.poisonCause = fmt.Sprintf("native cancellation fence failed: %v", err)
			} else if err == nil {
				s.cancellationResolvedEpoch = epoch
			}
		}

		close(done)
		s.mu.Unlock()

		if err != nil {
			return acp.NewInternalError(map[string]any{
				jsonFieldError: "opencode_cancellation_fence_failed",
				jsonFieldCause: err.Error(),
			})
		}

		return nil
	}
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

func (s *session) markActiveToolCallID(toolCallID string) {
	if toolCallID == "" {
		return
	}

	s.mu.Lock()
	if s.activeToolCallIDs == nil {
		s.activeToolCallIDs = map[string]struct{}{}
	}

	s.activeToolCallIDs[toolCallID] = struct{}{}
	s.mu.Unlock()
}

func (s *session) ownsCurrentToolCall(toolCallID string) bool {
	if toolCallID == "" {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cancel == nil || s.cancelling {
		return false
	}

	_, ok := s.activeToolCallIDs[toolCallID]

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

func (s *session) addPendingPermission(req opencode.PermissionRequest) {
	s.mu.Lock()
	if s.pending == nil {
		s.pending = map[string]opencode.PermissionRequest{}
	}

	s.pending[req.ID] = req
	s.mu.Unlock()
}

func (s *session) claimPermissionRequest(id string) bool {
	if id == "" {
		return true
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.processedPermission == nil {
		s.processedPermission = map[string]struct{}{}
	}

	if _, ok := s.processedPermission[id]; ok {
		return false
	}

	s.processedPermission[id] = struct{}{}

	return true
}

func (s *session) takePendingPermission(id string) (opencode.PermissionRequest, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	req, ok := s.pending[id]
	if ok {
		delete(s.pending, id)
	}

	return req, ok, s.cancelled
}

func (s *session) addPendingQuestion(req opencode.QuestionRequest) {
	s.mu.Lock()
	if s.questions == nil {
		s.questions = map[string]opencode.QuestionRequest{}
	}

	s.questions[req.ID] = req
	s.mu.Unlock()
}

func (s *session) claimQuestionRequest(id string) bool {
	if id == "" {
		return true
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.processedQuestion == nil {
		s.processedQuestion = map[string]struct{}{}
	}

	if _, ok := s.processedQuestion[id]; ok {
		return false
	}

	s.processedQuestion[id] = struct{}{}

	return true
}

func (s *session) takePendingQuestion(id string) (opencode.QuestionRequest, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	req, ok := s.questions[id]
	if ok {
		delete(s.questions, id)
	}

	return req, ok, s.cancelled
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

	changed := !sameAvailableCommands(s.availableCommands, available)
	if changed {
		s.availableCommands = cloneAvailableCommands(available)
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

// Close shuts down the native OpenCode process under bounded background
// contexts so a cancelled or expired caller context can never skip the
// graceful abort/close ladder.
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
		epoch, err := s.beginCancellation("", false, true)

		cancelErr = err
		if cancelErr == nil && epoch != 0 {
			cancelCtx, cancel := context.WithTimeout(context.Background(), settlementTimeout)
			cancelErr = s.resolveCancellation(cancelCtx, epoch)

			cancel()
		}
	}

	s.mu.Lock()
	client := s.client
	nativeID := s.idmap.NativeSessionID
	release := s.directoryRelease
	s.mu.Unlock()

	var err = cancelErr

	if client != nil && nativeID != "" {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), closeTimeout)
		err = errors.Join(err, client.Close(closeCtx))

		closeCancel()
	}

	if err == nil && release != nil {
		release()

		s.mu.Lock()
		s.directoryRelease = nil
		s.mu.Unlock()
	}

	return err
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
	s.pending = make(map[string]opencode.PermissionRequest)
	s.questions = make(map[string]opencode.QuestionRequest)
	release := s.directoryRelease
	s.directoryRelease = nil
	client := s.client
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}

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
	s.mu.Unlock()

	storeCtx, cancel := s.agent.sessionStoreContext(ctx)
	idmap, snapshot, ok, err := hydrateStateFromStore(storeCtx, s.agent.sessionStore(), string(id))

	cancel()

	if err != nil {
		return fmt.Errorf("load committed OpenCode recovery generation: %w", err)
	}

	if !ok {
		return acp.NewInvalidRequest(map[string]any{jsonFieldError: "opencode_recovery_generation_missing"})
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		client, releaseDirectory, generation, err := s.agent.newOpenCodeClient(ctx, id, cwd, mcpServers)
		if err != nil {
			return err
		}

		releaseCandidate := func() error {
			return s.agent.closeDirectoryScope(client, releaseDirectory, generation)
		}

		if validateErr := validateModel(ctx, client, model, modelFieldSessionMeta); validateErr != nil {
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
			return nil
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
	s.processedPermission = map[string]struct{}{}
	s.processedQuestion = map[string]struct{}{}
	s.cancelling = false
	s.cancellationErr = nil
	s.cancellationDone = nil

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
	epoch, err := s.beginCancellation("", false, true)
	if err == nil && epoch != 0 {
		cancelCtx, cancel := context.WithTimeout(context.Background(), settlementTimeout)
		err = s.resolveCancellation(cancelCtx, epoch)

		cancel()
	}

	s.mu.Lock()
	client := s.client
	nativeID := s.idmap.NativeSessionID
	s.mu.Unlock()

	if client != nil && nativeID != "" {
		deleteCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		if deleteErr := client.DeleteSession(deleteCtx, nativeID); deleteErr != nil && s.agent != nil && s.agent.log != nil {
			s.agent.log.DebugContext(deleteCtx, "delete native OpenCode session failed", slog.String("error", deleteErr.Error()))
		}

		cancel()
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
