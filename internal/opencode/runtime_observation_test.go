package opencode

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestProcessRuntimeObservation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	var nilObservation *runtimeProcessObservation
	nilObservation.markSupervisorsReady(ctx)
	nilObservation.markDescendantsReady(ctx, func() (int, bool) { return 1, true })
	nilObservation.markDescendantsQuiesced(ctx)
	nilObservation.markExited()
	(&runtimeProcessObservation{}).markSupervisorsReady(ctx)
	(&runtimeProcessObservation{}).markDescendantsReady(ctx, func() (int, bool) { return 1, true })
	(&runtimeProcessObservation{}).markDescendantsQuiesced(ctx)

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

func TestDescendantRuntimeObservationRequiresProofAndRetainsUnprovenCount(t *testing.T) {
	ctx := context.Background()
	var snapshots []int
	observed := &runtimeProcessObservation{observeSnapshot: func(_ context.Context, kind string, count int) {
		if kind != "provider_descendant" {
			t.Fatalf("kind = %q", kind)
		}
		snapshots = append(snapshots, count)
	}}

	observed.markDescendantsReady(ctx, nil)
	observed.markDescendantsReady(ctx, func() (int, bool) { return 4, false })
	observed.markDescendantsReady(ctx, func() (int, bool) { return -1, true })
	if len(snapshots) != 0 {
		t.Fatalf("unavailable inventory emitted snapshots %v", snapshots)
	}

	count := 4
	inventory := func() (int, bool) { return count, true }
	count = 5
	observed.markDescendantsReady(ctx, inventory)
	observed.markDescendantsReady(ctx, func() (int, bool) { return 6, true })
	observed.markExited()
	if len(snapshots) != 1 || snapshots[0] != 5 {
		t.Fatalf("live snapshots = %v", snapshots)
	}

	observed.markDescendantsQuiesced(ctx)
	observed.markDescendantsQuiesced(ctx)
	observed.markDescendantsReady(ctx, func() (int, bool) { return 1, true })
	if len(snapshots) != 2 || snapshots[1] != 0 {
		t.Fatalf("lifecycle snapshots = %v", snapshots)
	}
}

func TestDescendantRuntimeObservationSerializesReadyAndQuiescence(t *testing.T) {
	ctx := context.Background()
	var snapshots []int
	observed := &runtimeProcessObservation{observeSnapshot: func(_ context.Context, _ string, count int) {
		snapshots = append(snapshots, count)
	}}

	var group sync.WaitGroup
	group.Go(func() { observed.markDescendantsReady(ctx, func() (int, bool) { return 2, true }) })
	group.Go(func() { observed.markDescendantsQuiesced(ctx) })
	group.Wait()

	if len(snapshots) == 0 || snapshots[len(snapshots)-1] != 0 {
		t.Fatalf("concurrent lifecycle snapshots = %v", snapshots)
	}
}

func TestRuntimeObservationAllowsReentrantLifecycleCallbacks(t *testing.T) {
	ctx := context.Background()
	var (
		deltas    []int64
		snapshots []int
		observed  *runtimeProcessObservation
	)
	observed = &runtimeProcessObservation{
		observe: func(_ context.Context, _ string, delta int64) {
			deltas = append(deltas, delta)
			if delta == 2 {
				observed.markExited()
			}
		},
		observeSnapshot: func(callbackCtx context.Context, _ string, count int) {
			snapshots = append(snapshots, count)
			if count == 3 {
				observed.markDescendantsQuiesced(callbackCtx)
			}
		},
	}

	observed.markDescendantsReady(ctx, func() (int, bool) { return 3, true })
	observed.markSupervisorsReady(ctx)

	if len(deltas) != 2 || deltas[0] != 2 || deltas[1] != -2 {
		t.Fatalf("reentrant supervisor deltas = %v", deltas)
	}
	if len(snapshots) != 2 || snapshots[0] != 3 || snapshots[1] != 0 {
		t.Fatalf("reentrant descendant snapshots = %v", snapshots)
	}
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
