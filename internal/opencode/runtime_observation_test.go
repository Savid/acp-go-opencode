package opencode

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestProcessRuntimeObservation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	var nilObservation *runtimeProcessObservation
	nilObservation.markSupervisorsReady(ctx)
	nilObservation.markExited()
	(&runtimeProcessObservation{}).markSupervisorsReady(ctx)

	var deltas []int64
	observed := &runtimeProcessObservation{observe: func(_ context.Context, kind string, delta int64) {
		if kind != "home_lock_supervisor" {
			t.Fatalf("kind = %q", kind)
		}
		deltas = append(deltas, delta)
	}}
	observed.markSupervisorsReady(ctx)
	observed.markSupervisorsReady(ctx)
	observed.markExited()
	observed.markExited()
	if len(deltas) != 2 || deltas[0] != 2 || deltas[1] != -2 {
		t.Fatalf("deltas = %v", deltas)
	}

	exited := &runtimeProcessObservation{observe: observed.observe}
	exited.markExited()
	exited.markSupervisorsReady(ctx)
}

func TestObserveOpenCodeStartupStage(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	observeOpenCodeStartupStage(ctx, StartOptions{}, "runtime", "spawn", time.Now(), nil)

	wantErr := errors.New("spawn failed")
	called := false
	observeOpenCodeStartupStage(ctx, StartOptions{ObserveStartupStage: func(gotCtx context.Context, lifecycle, stage string, elapsed time.Duration, err error) {
		called = true
		if gotCtx != ctx || lifecycle != "runtime" || stage != "spawn" || elapsed < 0 || !errors.Is(err, wantErr) {
			t.Fatalf("observation = (%v, %q, %q, %v, %v)", gotCtx, lifecycle, stage, elapsed, err)
		}
	}}, "runtime", "spawn", time.Now(), wantErr)
	if !called {
		t.Fatal("startup-stage callback was not called")
	}
}
