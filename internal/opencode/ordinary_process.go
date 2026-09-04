package opencode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
)

// superviseOrdinary is the seam the containment tests replace: the platform
// implementation only fails on Windows, where the coverage gate does not run.
var superviseOrdinary = superviseOrdinaryProcess

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

	stdin, stdout, stderr, err := ordinaryProcessPipes(command)
	if err == nil {
		if startErr := command.Start(); startErr != nil {
			err = fmt.Errorf("start native process: %w", startErr)
		}
	}

	if err != nil {
		return ProcessHandle{}, err
	}

	guard, err := superviseOrdinary(command)
	if err != nil {
		// A child the platform will not contain is never handed back, and it
		// is waited on rather than only killed: the wait is what reaps it and
		// closes the three pipes opened above, which nothing else now holds.
		_ = command.Process.Kill()
		_ = command.Wait()

		return ProcessHandle{}, err
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
				waitErr = errors.Join(waitErr, containOrdinaryProcess(command, guard))

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
			won, err := stopOrdinaryProcess(ctx, command, guard)
			if won {
				revoked.Store(true)
			}

			return errors.Join(err, ctx.Err())
		},
	}, nil
}

func ordinaryProcessPipes(command *exec.Cmd) (io.WriteCloser, io.ReadCloser, io.ReadCloser, error) {
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open native stdin: %w", err)
	}

	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()

		return nil, nil, nil, fmt.Errorf("open native stdout: %w", err)
	}

	stderr, err := command.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()

		return nil, nil, nil, fmt.Errorf("open native stderr: %w", err)
	}

	return stdin, stdout, stderr, nil
}

func normalizeOrdinaryWaitError(err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return nil
	}

	return err
}
