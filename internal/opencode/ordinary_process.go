package opencode

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"sync/atomic"
)

func startOrdinaryProcess(
	_ context.Context,
	executable string,
	arguments []string,
	environment []string,
	workingDirectory string,
) (ProcessHandle, error) {
	resolved, err := resolveOrdinaryProcessExecutable(executable, environment)
	if err != nil {
		return ProcessHandle{}, err
	}

	command := exec.Command(resolved, arguments...)

	command.Env = append([]string(nil), environment...)
	command.Dir = workingDirectory
	configureOrdinaryProcess(command)

	stdin, err := command.StdinPipe()
	if err != nil {
		return ProcessHandle{}, fmt.Errorf("open native stdin: %w", err)
	}

	stdout, err := command.StdoutPipe()
	if err != nil {
		return ProcessHandle{}, fmt.Errorf("open native stdout: %w", err)
	}

	stderr, err := command.StderrPipe()
	if err != nil {
		return ProcessHandle{}, fmt.Errorf("open native stderr: %w", err)
	}

	if err := command.Start(); err != nil {
		return ProcessHandle{}, fmt.Errorf("start native process: %w", err)
	}

	var (
		waitOnce sync.Once
		waitDone = make(chan struct{})
		outcome  ProcessOutcome
		waitErr  error
		revoked  atomic.Bool
	)

	await := func(ctx context.Context) (ProcessOutcome, error) {
		waitOnce.Do(func() {
			go func() {
				waitErr = command.Wait()
				outcome = ordinaryProcessOutcome(command, waitErr)
				outcome.Revoked = revoked.Load()
				waitErr = normalizeOrdinaryWaitError(waitErr)
				waitErr = errors.Join(waitErr, containOrdinaryProcess(command))

				close(waitDone)
			}()
		})

		select {
		case <-waitDone:
			return outcome, waitErr
		case <-ctx.Done():
			return ProcessOutcome{}, ctx.Err()
		}
	}

	return ProcessHandle{
		Input: stdin, Output: stdout, Errors: stderr,
		Await: await,
		Stop: func(ctx context.Context) error {
			won, err := stopOrdinaryProcess(ctx, command)
			if won {
				revoked.Store(true)
			}

			return err
		},
	}, nil
}

func normalizeOrdinaryWaitError(err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return nil
	}

	return err
}
