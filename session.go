package opencodeacp

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/coder/acp-go-sdk"
)

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
	env                   map[string]string
	rawMessages           rawMessageConfig

	client openCodeClient

	turn      chan struct{}
	mu        sync.Mutex
	cancel    context.CancelFunc
	turnDone  <-chan struct{}
	cancelled bool
	rawSeq    int64
	seenParts map[string]string
	pending   map[string]permissionRequest
	questions map[string]questionRequest
	closed    bool
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
	env                   map[string]string
	rawMessages           rawMessageConfig
	client                openCodeClient
}

func newSession(agent *Agent, id acp.SessionId, cwd string, additionalDirectories []string, native nativeSession, client openCodeClient, meta sessionMeta, idmap idmapRecord) *session {
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
		env:                   cloneStringMap(meta.Env),
		rawMessages:           meta.RawMessages,
		client:                client,
		seenParts:             map[string]string{},
		pending:               map[string]permissionRequest{},
		questions:             map[string]questionRequest{},
	}
}

func (s *session) acquireTurn(ctx context.Context) (func(), error) {
	turn := s.turnQueue()
	select {
	case turn <- struct{}{}:
		return func() { <-turn }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return nil, acp.NewInvalidRequest(map[string]any{jsonFieldError: "backpressure", "limit": "session_prompt"})
	}
}

func (s *session) turnQueue() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turn == nil {
		limit := defaultMaxConcurrentPrompts
		if s.agent != nil && s.agent.options.ConcurrencyLimits.MaxConcurrentPrompts > 0 {
			limit = s.agent.options.ConcurrencyLimits.MaxConcurrentPrompts
		}
		s.turn = make(chan struct{}, limit)
	}
	return s.turn
}

func (s *session) beginTurn(ctx context.Context) context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	turnCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.turnDone = turnCtx.Done()
	s.cancelled = false
	return turnCtx
}

func (s *session) finishTurn() {
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.turnDone = nil
	s.cancelled = false
	s.updatedAt = time.Now().UTC().Format(time.RFC3339)
	s.pending = map[string]permissionRequest{}
	s.questions = map[string]questionRequest{}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *session) cancelTurn() {
	s.mu.Lock()
	cancel := s.cancel
	if cancel != nil {
		s.cancelled = true
	}
	pending := make([]permissionRequest, 0, len(s.pending))
	for _, req := range s.pending {
		pending = append(pending, req)
	}
	questions := make([]questionRequest, 0, len(s.questions))
	for _, req := range s.questions {
		questions = append(questions, req)
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	ctx, done := context.WithTimeout(context.Background(), closeTimeout)
	defer done()
	for _, req := range pending {
		_ = s.client.ReplyPermission(ctx, req.SessionID, req.ID, "reject", "cancelled")
	}
	for _, req := range questions {
		_ = s.client.RejectQuestion(ctx, req.SessionID, req.ID)
	}
}

func (s *session) wasCancelled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancelled
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
		env:                   cloneStringMap(s.env),
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

func (s *session) modelSelector() *openCodeModelSelector {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.providerID == "" || s.modelID == "" {
		return nil
	}
	return &openCodeModelSelector{ProviderID: s.providerID, ModelID: s.modelID}
}

func (s *session) currentMode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode
}

func (s *session) nextRawEventSequence() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rawSeq++
	return s.rawSeq
}

func (s *session) markPart(part nativePart) bool {
	if part.ID == "" {
		return true
	}
	encoded := string(part.Raw)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seenParts[part.ID] == encoded {
		return false
	}
	s.seenParts[part.ID] = encoded
	return true
}

func (s *session) Close(ctx context.Context) error {
	s.cancelTurn()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	client := s.client
	nativeID := s.idmap.NativeSessionID
	s.mu.Unlock()
	var err error
	if client != nil && nativeID != "" {
		abortCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		_ = client.Abort(abortCtx, nativeID)
		cancel()
		err = errors.Join(err, client.Close(ctx))
	}
	return err
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
