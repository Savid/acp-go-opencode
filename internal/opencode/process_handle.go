package opencode

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

var processContainmentTimeout = 5 * time.Second

type ProcessStarter func(context.Context, string, []string, []string, string) (ProcessHandle, error)

type ProcessHandle struct {
	Input  io.WriteCloser
	Output io.ReadCloser
	Errors io.ReadCloser
	Await  func(context.Context) (ProcessOutcome, error)
	Stop   func(context.Context) error
}

type ProcessOutcome struct {
	ExitCode int
	Signal   int
	Revoked  bool
}

func (p ProcessHandle) valid() bool {
	return p.Input != nil && p.Output != nil && p.Errors != nil && p.Await != nil && p.Stop != nil
}

type processSettlement struct {
	process    ProcessHandle
	mu         sync.Mutex
	started    bool
	waitCancel context.CancelFunc
	done       chan struct{}
	result     ProcessOutcome
	err        error
	observed   bool
	terminal   bool
}

func newProcessSettlement(process ProcessHandle) *processSettlement {
	return &processSettlement{process: process, done: make(chan struct{})}
}

func (s *processSettlement) start() {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()

		return
	}

	waitCtx, cancel := context.WithCancel(context.Background())
	s.started = true
	s.waitCancel = cancel
	s.mu.Unlock()

	go func() {
		result, err := s.process.Await(waitCtx)
		ctxErr := waitCtx.Err()

		s.mu.Lock()
		s.result = result
		s.err = err
		s.terminal = err == nil
		s.observed = err == nil || ctxErr == nil || !errors.Is(err, ctxErr)
		close(s.done)
		s.mu.Unlock()
	}()
}

func (s *processSettlement) wait(ctx context.Context) (ProcessOutcome, error) {
	if s == nil {
		return ProcessOutcome{}, errors.New("native process settlement is unavailable")
	}

	s.start()

	select {
	case <-s.done:
		s.mu.Lock()
		result, err := s.result, s.err
		s.mu.Unlock()

		return result, err
	case <-ctx.Done():
		return ProcessOutcome{}, ctx.Err()
	}
}

func (s *processSettlement) cancel() {
	if s == nil {
		return
	}

	s.start()

	s.mu.Lock()
	cancel := s.waitCancel
	s.mu.Unlock()

	cancel()
}

func (s *processSettlement) observation() (ProcessOutcome, bool, bool, error) {
	if s == nil {
		return ProcessOutcome{}, false, false, errors.New("native process settlement is unavailable")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.result, s.observed, s.terminal, s.err
}

func (s *processSettlement) waitTerminal() (ProcessOutcome, error) {
	if s == nil {
		return ProcessOutcome{}, errors.New("native process settlement is unavailable")
	}

	s.cancel()

	joinCtx, joinCancel := context.WithTimeout(context.Background(), processContainmentTimeout)
	select {
	case <-s.done:
	case <-joinCtx.Done():
		joinCancel()

		return ProcessOutcome{}, errors.Join(ErrProcessContainmentIncomplete, joinCtx.Err())
	}

	joinCancel()

	if result, _, terminal, err := s.observation(); terminal {
		return result, err
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), processContainmentTimeout)
	result, err := s.process.Await(waitCtx)
	ctxErr := waitCtx.Err()

	cancel()

	if err == nil {
		s.mu.Lock()
		s.result = result
		s.err = nil
		s.terminal = true
		s.observed = true
		s.mu.Unlock()

		return result, nil
	}

	if ctxErr != nil && errors.Is(err, ctxErr) {
		return ProcessOutcome{}, errors.Join(ErrProcessContainmentIncomplete, err)
	}

	return ProcessOutcome{}, errors.Join(ErrProcessContainmentIncomplete, err)
}
