package opencodeacp

import (
	"context"

	"github.com/coder/acp-go-sdk"
)

type sessionLifecycleFlight struct {
	done chan struct{}
}

func (a *Agent) acquireSessionLifecycle(ctx context.Context, id acp.SessionId) (func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		a.lifecycleAdmissionMu.Lock()
		if a.lifecycleFence == nil {
			a.lifecycleFence = make(chan struct{})
		}

		if a.lifecycleFenced {
			a.lifecycleAdmissionMu.Unlock()

			return nil, acp.NewInvalidRequest(map[string]any{jsonFieldError: errValueAgentClosed})
		}

		if a.lifecycleFlights == nil {
			a.lifecycleFlights = make(map[acp.SessionId]*sessionLifecycleFlight)
		}

		flight := a.lifecycleFlights[id]
		if flight == nil {
			flight = &sessionLifecycleFlight{done: make(chan struct{})}
			a.lifecycleFlights[id] = flight
			a.lifecycleAdmissionMu.Unlock()

			return func() { a.releaseSessionLifecycle(id, flight) }, nil
		}

		done := flight.done
		fence := a.lifecycleFence
		a.lifecycleAdmissionMu.Unlock()

		select {
		case <-done:
		case <-fence:
			return nil, acp.NewInvalidRequest(map[string]any{jsonFieldError: errValueAgentClosed})
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (a *Agent) releaseSessionLifecycle(id acp.SessionId, flight *sessionLifecycleFlight) {
	a.lifecycleAdmissionMu.Lock()
	defer a.lifecycleAdmissionMu.Unlock()

	if a.lifecycleFlights[id] != flight {
		return
	}

	delete(a.lifecycleFlights, id)
	close(flight.done)
}

func (a *Agent) fenceSessionLifecycle() {
	a.lifecycleAdmissionMu.Lock()
	defer a.lifecycleAdmissionMu.Unlock()

	if a.lifecycleFenced {
		return
	}

	a.lifecycleFenced = true
	if a.lifecycleFence == nil {
		a.lifecycleFence = make(chan struct{})
	}

	a.lifecycleFlights = nil
	close(a.lifecycleFence)
}
