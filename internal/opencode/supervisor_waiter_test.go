package opencode

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestSupervisorWaiterPausedImmediateAndNilBranches(t *testing.T) {
	(*supervisorWaiter)(nil).start()
	if err := <-(*supervisorWaiter)(nil).result(); err != nil {
		t.Fatal(err)
	}
	if err, completed := (*supervisorWaiter)(nil).await(context.Background()); err != nil || !completed {
		t.Fatalf("nil await = %v, %v", err, completed)
	}

	pausedCommand := exec.Command("/usr/bin/true")
	if err := pausedCommand.Start(); err != nil {
		t.Fatal(err)
	}
	paused := newSupervisorWaiter(pausedCommand, true)
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err, completed := paused.await(ctx); err == nil || completed {
		t.Fatalf("paused await = %v, %v", err, completed)
	}
	paused.start()
	paused.start()
	if err := <-paused.result(); err != nil {
		t.Fatal(err)
	}
	if err, completed := paused.await(context.Background()); err != nil || !completed {
		t.Fatalf("completed await = %v, %v", err, completed)
	}

	immediateCommand := exec.Command("/usr/bin/true")
	if err := immediateCommand.Start(); err != nil {
		t.Fatal(err)
	}
	if err := <-newSupervisorWaiter(immediateCommand, false).result(); err != nil {
		t.Fatal(err)
	}
}
