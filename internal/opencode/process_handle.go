package opencode

import (
	"context"
	"errors"
	"io"
	"sync"
)

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
	process ProcessHandle
	once    sync.Once
	done    chan struct{}
	result  ProcessOutcome
	err     error
}

func newProcessSettlement(process ProcessHandle) *processSettlement {
	return &processSettlement{process: process, done: make(chan struct{})}
}

func (s *processSettlement) start() {
	s.once.Do(func() {
		go func() {
			s.result, s.err = s.process.Await(context.Background())
			close(s.done)
		}()
	})
}

func (s *processSettlement) wait(ctx context.Context) (ProcessOutcome, error) {
	if s == nil {
		return ProcessOutcome{}, errors.New("native process settlement is unavailable")
	}

	s.start()

	select {
	case <-s.done:
		return s.result, s.err
	case <-ctx.Done():
		return ProcessOutcome{}, ctx.Err()
	}
}
