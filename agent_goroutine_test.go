package opencodeacp

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestRecoverAgentGoroutineLogsOnlyClosedPanicClassification(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	func() {
		defer recoverAgentGoroutine(context.Background(), logger, "test goroutine")
		panic("SECRET_SENTINEL")
	}()

	if !strings.Contains(buf.String(), "test goroutine") || strings.Contains(buf.String(), "SECRET_SENTINEL") {
		t.Fatalf("panic log = %q", buf.String())
	}
}

func TestHandleAgentGoroutinePanicBranches(t *testing.T) {
	handleAgentGoroutinePanic(context.Background(), nil, "none", nil, nil)

	var recovered any
	handleAgentGoroutinePanic(context.Background(), nil, "with shutdown", func(value any) {
		recovered = value
	}, "panic value")
	if recovered != "agent goroutine panicked" {
		t.Fatalf("shutdown recovered = %#v", recovered)
	}
	if agentLogger(nil) != nil {
		t.Fatal("nil agent logger returned non-nil")
	}
	agent := NewAgent()
	if agentLogger(agent) != agent.log {
		t.Fatal("agent logger mismatch")
	}
}
