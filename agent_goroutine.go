package opencodeacp

import (
	"context"
	"log/slog"
)

// recoverAgentGoroutine is deferred at the top of agent-owned goroutines so a
// panic in background work is logged instead of crashing the host process.
func recoverAgentGoroutine(ctx context.Context, log *slog.Logger, name string) {
	handleAgentGoroutinePanic(ctx, log, name, nil, recover())
}

// handleAgentGoroutinePanicRecover is deferred directly: it recovers, logs, and
// hands the panic to a caller that has to publish a result in place of the work
// that died.
func handleAgentGoroutinePanicRecover(ctx context.Context, log *slog.Logger, name string, shutdown func(any)) {
	handleAgentGoroutinePanic(ctx, log, name, shutdown, recover())
}

func handleAgentGoroutinePanic(ctx context.Context, log *slog.Logger, name string, shutdown func(any), recovered any) {
	if recovered == nil {
		return
	}

	if log == nil {
		log = slog.Default()
	}

	log.ErrorContext(ctx, "agent goroutine panic", slog.String("goroutine", name))

	if shutdown != nil {
		shutdown("agent goroutine panicked")
	}
}

func agentLogger(agent *Agent) *slog.Logger {
	if agent == nil {
		return nil
	}

	return agent.log
}
