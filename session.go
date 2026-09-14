package opencodeacp

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-core/lifecycle"
	"github.com/savid/acp-go-core/wire"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

const (
	sessionAbortTimeout  = 5 * time.Second
	sessionSettleTimeout = 60 * time.Second
)

// session owns one native conversation and one binding to the shared server.
type session struct {
	agent                 *Agent
	id                    acp.SessionId
	cwd                   string
	additionalDirectories []string
	options               OpenCodeOptions
	rawEvents             *wire.RawEvents
	gate                  chan struct{}
	mu                    sync.Mutex
	runtime               *binding
	model                 string
	effort                string
	models                opencode.Catalog
	artifacts             map[string]imageArtifact
	nativeMessages        map[string]opencode.NativeMessageInfo
	completedParents      map[string]bool
	mode                  string
	agents                []opencode.NativeAgent
	commands              []opencode.NativeCommand
	title                 string
	updatedAt             string
	closing               bool
	closeDone             chan struct{}
	closeErr              error
	poison                string
	turn                  *turn
	cycle                 *cycle
	dialogs               map[string]*dialog
	callbacks             sync.WaitGroup
	mirrorMu              sync.Mutex
	lcMu                  sync.Mutex
	lc                    lifecycleState
}

// binding routes the shared server's events to one held conversation.
type binding struct {
	server   *runtime
	client   *opencode.Client
	cancel   context.CancelFunc
	bound    chan struct{}
	bindOnce sync.Once
	done     chan struct{}
	events   chan opencode.Event
	results  chan nativePromptResult
}
type nativePromptResult struct {
	turn    *turn
	message opencode.NativeMessage
	err     error
}

type cycle struct {
	turnID  string
	cycleID string
	origin  lifecycle.Cause
	state   cycleState
	failure error
	done    chan struct{}
}

type turnEnd int

const (
	turnRunning turnEnd = iota
	turnSettled
	turnTransportEnded
)

type turn struct {
	cycle
	submission lifecycle.Submission
	accepted   bool
	cancelled  bool
	timedOut   bool
	ended      turnEnd
	settled    chan struct{}
	settleOnce sync.Once
	finished   chan struct{}
	messageID  string
}

func (t *turn) settle(end turnEnd) {
	t.settleOnce.Do(func() { t.ended = end; close(t.settled) })
}

type dialog struct{ cancel context.CancelCauseFunc }

var errDialogCancelled = errors.New("dialog cancelled by the session")

func (s *session) ensureRuntime(ctx context.Context) (*binding, error) {
	s.mu.Lock()
	rt := s.runtime
	s.mu.Unlock()

	if rt != nil && rt.server.alive() {
		return rt, nil
	}

	if rt != nil {
		s.stopRuntime(context.WithoutCancel(ctx), rt)
	}

	stored, err := s.agent.loadStored(ctx, s.id)
	if err != nil {
		return nil, err
	}

	if !stored.found {
		return nil, wire.RestoreFailed(vendor)
	}

	rt, err = s.launch(ctx)
	if err != nil {
		return nil, err
	}

	if _, err := s.hydrate(ctx, rt, stored); err != nil {
		s.stopRuntime(context.WithoutCancel(ctx), rt)

		return nil, err
	}

	if err := s.configureRuntime(ctx, rt, s.model, string(s.id)); err != nil {
		s.stopRuntime(context.WithoutCancel(ctx), rt)

		return nil, err
	}

	if err := s.publishOpen(ctx); err != nil {
		s.stopRuntime(context.WithoutCancel(ctx), rt)

		return nil, err
	}

	return rt, nil
}

// pump serializes events and completed HTTP prompts for this binding.
func (s *session) pump(ctx context.Context, rt *binding) {
	defer close(rt.done)
	defer s.runtimeEnded(ctx, rt)

	select {
	case <-rt.bound:
	case <-ctx.Done():
		return
	}

	for {
		select {
		case event := <-rt.events:
			s.handleEvent(ctx, rt, event)
		case result := <-rt.results:
			s.mu.Lock()
			current := s.turn == result.turn
			s.mu.Unlock()

			if !current {
				continue
			}

			t := result.turn
			end := turnSettled

			if result.err != nil {
				t.failure = s.dispatchFailure(ctx, rt, result.err)

				var nativeErr *opencode.HTTPError
				if !errors.As(result.err, &nativeErr) {
					end = turnTransportEnded
				}
			} else {
				s.acceptTurn(ctx, t)

				messages, err := rt.client.Messages(ctx, s.cwd, string(s.id))
				if err != nil {
					t.failure = err
				} else {
					for index := range messages {
						message := &messages[index]
						if message.Info.ParentID == t.messageID {
							if err := s.projectMessage(ctx, &t.cycle, *message); err != nil {
								t.failure = err
							}
						}
					}
				}

				if err := s.projectMessage(ctx, &t.cycle, result.message); err != nil {
					t.failure = err
				}

				if s.completedParents == nil {
					s.completedParents = map[string]bool{}
				}

				s.completedParents[t.messageID] = true
			}

			s.cancelDialogs()
			s.callbacks.Wait()
			t.settle(end)

			select {
			case <-t.finished:
			case <-ctx.Done():
				return
			}
		case <-ctx.Done():
			return
		}
	}
}
func (s *session) runtimeEnded(ctx context.Context, rt *binding) {
	rt.server.mu.Lock()
	if rt.server.bindings[string(s.id)] == rt {
		delete(rt.server.bindings, string(s.id))
	}
	rt.server.mu.Unlock()
	s.mu.Lock()
	if s.runtime != rt {
		s.mu.Unlock()

		return
	}

	s.runtime = nil
	t, c, closing := s.turn, s.cycle, s.closing
	s.cycle = nil
	s.mu.Unlock()
	s.cancelDialogs()
	s.callbacks.Wait()

	if t != nil {
		t.settle(turnTransportEnded)

		return
	}

	if c != nil {
		verdict := cycleVerdict{outcome: lifecycle.OutcomeFailed}
		if closing {
			verdict = cycleVerdict{outcome: lifecycle.OutcomeCancelled, stopReason: lifecycle.StopReasonCancelled}
		}

		_ = s.lcIdle(context.WithoutCancel(ctx), c, verdict)
		close(c.done)
	}

	if !closing {
		s.lcFence()
	}
}
func (s *session) stopRuntime(_ context.Context, rt *binding) { rt.cancel(); <-rt.done }

// abort interrupts the native run under a bounded context detached from the
// caller's cancellation.
func (s *session) abort(ctx context.Context, rt *binding) {
	abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionAbortTimeout)
	defer cancel()

	if err := rt.client.Interrupt(abortCtx, s.cwd, string(s.id)); err != nil {
		s.agent.log.DebugContext(abortCtx, "opencode abort failed", slog.String(nativeSessionIDKey, string(s.id)))
	}
}

// cancel implements session/cancel: it cancels the in-flight turn, resolves
// its pending dialogs, and interrupts opencode. It is a silent no-op with no turn.
func (s *session) cancel(ctx context.Context) {
	s.mu.Lock()
	t := s.turn
	rt := s.runtime

	if t == nil || t.cancelled {
		s.mu.Unlock()

		return
	}

	t.cancelled = true
	s.mu.Unlock()

	s.cancelDialogs()

	if rt != nil {
		s.abort(ctx, rt)
	}
}

// timeout ends a turn that exceeded the configured deadline.
func (s *session) timeout(ctx context.Context, t *turn) {
	s.mu.Lock()
	rt := s.runtime

	if s.turn != t || t.cancelled || t.timedOut {
		s.mu.Unlock()

		return
	}

	t.timedOut = true
	s.mu.Unlock()

	s.cancelDialogs()

	if rt != nil {
		s.abort(ctx, rt)

		select {
		case <-t.settled:
		case <-time.After(sessionAbortTimeout):
			rt.cancel()
		}
	}
}

func (s *session) registerDialog(id string, cancel context.CancelCauseFunc) (func(), bool) {
	s.mu.Lock()
	if s.dialogs[id] != nil || s.closing || s.runtime == nil || (s.turn != nil && (s.turn.cancelled || s.turn.timedOut)) {
		s.mu.Unlock()
		cancel(errDialogCancelled)

		return func() {}, false
	}

	if s.dialogs == nil {
		s.dialogs = make(map[string]*dialog)
	}

	s.callbacks.Add(1)

	entry := &dialog{cancel: cancel}
	s.dialogs[id] = entry
	s.mu.Unlock()

	return sync.OnceFunc(func() {
		defer s.callbacks.Done()

		s.mu.Lock()
		if s.dialogs[id] == entry {
			delete(s.dialogs, id)
		}
		s.mu.Unlock()
	}), true
}

func (s *session) cancelDialogs() {
	s.mu.Lock()

	dialogs := make([]*dialog, 0, len(s.dialogs))
	for _, d := range s.dialogs {
		dialogs = append(dialogs, d)
	}
	s.mu.Unlock()

	for _, d := range dialogs {
		d.cancel(errDialogCancelled)
	}
}

// admissionError reports why a session admits no further work: it is closing
// or poisoned.
func (s *session) admissionError() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case s.poison != "":
		return wire.SessionPoisoned(vendor, s.poison)
	case s.closing:
		return wire.UnknownSession()
	default:
		return nil
	}
}

// poisonSession fences every operation but close and delete.
func (s *session) poisonSession(ctx context.Context, cause string) {
	s.mu.Lock()

	first := s.poison == ""
	if first {
		s.poison = cause
	}
	s.mu.Unlock()

	if !first {
		return
	}

	_ = s.emit(ctx, acp.SessionUpdate{AvailableCommandsUpdate: &acp.SessionAvailableCommandsUpdate{AvailableCommands: []acp.AvailableCommand{}}})
	s.agent.log.ErrorContext(ctx, "opencode session poisoned",
		slog.String(nativeSessionIDKey, string(s.id)), slog.String("cause", cause))
}

// acquireGate admits one foreground operation. limit names the backpressure
// token a refusal carries.
func (s *session) acquireGate(limit string) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closing {
		return nil, wire.UnknownSession()
	}

	select {
	case s.gate <- struct{}{}:
		return func() { <-s.gate }, nil
	default:
		return nil, wire.Backpressure(limit)
	}
}

// close interrupts and joins session work, captures native state, and releases its binding.
func (s *session) close(ctx context.Context) error {
	s.mu.Lock()
	if s.closing {
		done := s.closeDone
		s.mu.Unlock()
		<-done

		return s.closeErr
	}

	s.closing = true
	s.closeDone = make(chan struct{})

	t, rt, closingCycle := s.turn, s.runtime, s.cycle
	if t != nil {
		t.cancelled = true
	}
	s.mu.Unlock()
	s.cancelDialogs()
	s.callbacks.Wait()

	if t != nil {
		if rt != nil {
			s.abort(ctx, rt)
		}

		select {
		case <-t.finished:
		case <-time.After(sessionSettleTimeout):
		}
	}

	if closingCycle != nil && rt != nil {
		s.abort(ctx, rt)

		select {
		case <-closingCycle.done:
		case <-time.After(sessionAbortTimeout):
			s.stopRuntime(ctx, rt)
		}
	}

	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionSettleTimeout)
	defer cancel()

	var errs []error

	if rt != nil {
		if err := s.commitMirror(commitCtx); err != nil {
			errs = append(errs, err)
		}

		s.stopRuntime(commitCtx, rt)
	}

	s.lcFence()
	s.mu.Lock()
	s.closeErr = errors.Join(errs...)
	close(s.closeDone)
	s.mu.Unlock()

	return s.closeErr
}
