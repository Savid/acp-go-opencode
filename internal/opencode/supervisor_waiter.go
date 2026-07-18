package opencode

import (
	"context"
	"os/exec"
	"sync"
)

// supervisorWaiter designates one cmd.Wait owner immediately after Start.
// Darwin keeps that owner paused until the original group and child identity
// have been captured; other backends begin it immediately.
type supervisorWaiter struct {
	beginOnce sync.Once
	begin     chan struct{}
	done      chan struct{}
	err       error
}

func newSupervisorWaiter(cmd *exec.Cmd, paused bool) *supervisorWaiter {
	return newSupervisorWaiterFunc(cmd.Wait, paused)
}

func newSupervisorWaiterFunc(wait func() error, paused bool) *supervisorWaiter {
	waiter := &supervisorWaiter{begin: make(chan struct{}), done: make(chan struct{})}

	go func() {
		<-waiter.begin

		if wait != nil {
			waiter.err = wait()
		}

		close(waiter.done)
	}()

	if !paused {
		waiter.start()
	}

	return waiter
}

func (w *supervisorWaiter) start() {
	if w == nil {
		return
	}

	w.beginOnce.Do(func() { close(w.begin) })
}

func (w *supervisorWaiter) result() chan error {
	result := make(chan error, 1)
	if w == nil {
		result <- nil

		return result
	}

	go func() {
		<-w.done

		result <- w.err
	}()

	return result
}

func (w *supervisorWaiter) await(ctx context.Context) (error, bool) {
	if w == nil {
		return nil, true
	}

	select {
	case <-w.done:
		return w.err, true
	case <-ctx.Done():
		return ctx.Err(), false
	}
}
