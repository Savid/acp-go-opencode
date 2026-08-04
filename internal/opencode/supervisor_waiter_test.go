package opencode

import (
	"context"
	"errors"
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

func TestSupervisorWaiterResultStaysPausedUntilRelease(t *testing.T) {
	waitErr := errors.New("wait result")
	source := make(chan error, 1)
	released := make(chan struct{})
	waiter := newSupervisorWaiterResult(source, func() { close(released) }, true)
	result := waiter.result()
	source <- waitErr

	select {
	case <-released:
		t.Fatal("paused result released its creator-thread wait")
	case err := <-result:
		t.Fatalf("paused result completed before release: %v", err)
	default:
	}

	waiter.start()
	waiter.start()
	<-released
	if err := <-result; !errors.Is(err, waitErr) {
		t.Fatalf("result = %v, want %v", err, waitErr)
	}
}
