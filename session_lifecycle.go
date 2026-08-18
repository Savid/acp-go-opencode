package opencodeacp

import (
	"context"
	"errors"
	"fmt"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/lifecycle"
)

// openLifecycleStream creates one stream for the native session incarnation.
// It is deliberately session-owned: prompt completion never rotates it, while
// native generation loss and close fence it permanently.
func (s *session) openLifecycleStream() {
	if s.agent == nil {
		return
	}

	facts := s.agent.lifecycleNegotiated()
	if !facts.Present() {
		return
	}

	id, err := NewTurnNonce()
	if err != nil {
		s.lifecycleFailed = err

		return
	}

	s.lifecycleStream = lifecycle.NewStream(id, facts)
	s.lifecycleCycle = "idle-0"
	s.lifecycleSettled = true
}

func (s *session) emitLifecycle(ctx context.Context, event lifecycle.Event) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

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
		// The sequence is already consumed. Latching prevents later delivery
		// from hiding the gap behind an apparently contiguous stream.
		s.lifecycleFailed = fmt.Errorf("deliver lifecycle sequence: %w", err)

		return s.lifecycleFailed
	}

	return nil
}

func (s *session) beginLifecycleTurn(ctx context.Context) error {
	if s.lifecycleStream == nil {
		return s.lifecycleFailed
	}

	s.lifecycleMu.Lock()
	state := s.lifecycleStream.State()
	first := state.ReducedThrough == 0
	s.lifecycleMu.Unlock()

	if first {
		if err := s.emitLifecycle(ctx, lifecycle.SnapshotEvent(
			lifecycle.Foreground{State: lifecycle.ForegroundIdle, CycleID: s.lifecycleCycle}, nil,
			lifecycle.QuiescenceFact{Quiescent: false},
		)); err != nil {
			return err
		}
	}

	s.mu.Lock()
	s.lifecycleTurn = s.turnNonce
	s.lifecycleCycle = fmt.Sprintf("cycle-%d", s.turnEpoch)
	s.lifecycleSettled = false
	submission := s.submission
	turn, cycle := s.lifecycleTurn, s.lifecycleCycle
	s.mu.Unlock()

	if err := s.emitLifecycle(ctx, lifecycle.AcceptedEvent(submission, turn)); err != nil {
		return err
	}

	return s.emitLifecycle(ctx, lifecycle.TransitionEvent(lifecycle.ForegroundRunning, cycle, turn))
}

func (s *session) settleLifecycleTurn(ctx context.Context, stopReason string, outcome lifecycle.Outcome) error {
	s.mu.Lock()
	if s.lifecycleSettled {
		s.mu.Unlock()

		return nil
	}

	s.lifecycleSettled = true
	cycle, turn := s.lifecycleCycle, s.lifecycleTurn
	s.mu.Unlock()

	return s.emitLifecycle(ctx, lifecycle.IdleEvent(cycle, turn, stopReason, outcome))
}

func (s *session) lifecycleAction(ctx context.Context, action lifecycle.ActionUpdate) error {
	if s.lifecycleStream == nil {
		return s.lifecycleFailed
	}

	return s.emitLifecycle(ctx, lifecycle.ActionEvent(action))
}

func (s *session) lifecycleActionMeta(actionID string, owner lifecycle.Owner) map[string]any {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if s.lifecycleStream == nil {
		return nil
	}

	return map[string]any{lifecycle.MetaKey: lifecycle.ActionCorrelation{
		StreamID: s.lifecycleStream.ID(), ActionID: actionID, Owner: owner,
		RunID: s.currentSubmission().RunID,
	}.Value()}
}

func (s *session) currentLifecycleOwner() lifecycle.Owner {
	return lifecycle.Owner{Type: lifecycle.OwnerTurn, ID: s.currentTurnNonce()}
}

func (s *session) lifecycleActionPending(ctx context.Context, action lifecycle.ActionUpdate) error {
	if err := s.lifecycleAction(ctx, action); err != nil {
		return err
	}

	return s.emitLifecycle(ctx, lifecycle.TransitionEvent(
		lifecycle.ForegroundRequiresAction, s.lifecycleCycle, s.lifecycleTurn,
	))
}

func (s *session) lifecycleActionResolved(ctx context.Context, actionID string, state lifecycle.ActionState) error {
	if err := s.lifecycleAction(ctx, lifecycle.ResolvedAction(actionID, state)); err != nil {
		return err
	}

	return s.emitLifecycle(ctx, lifecycle.TransitionEvent(
		lifecycle.ForegroundRunning, s.lifecycleCycle, s.lifecycleTurn,
	))
}

func (s *session) fenceLifecycle(cause string) {
	s.mu.Lock()
	settled := s.lifecycleSettled
	s.mu.Unlock()
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if s.lifecycleStream == nil {
		return
	}

	s.lifecycleStream.Close()

	if !settled && s.lifecycleFailed == nil {
		s.lifecycleFailed = fmt.Errorf("active lifecycle incarnation lost: %s", cause)
	}
}
