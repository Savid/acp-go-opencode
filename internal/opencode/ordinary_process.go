package opencode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
)

// superviseOrdinary is the seam the containment tests replace: the platform
// implementation only fails on Windows, where the coverage gate does not run.
var superviseOrdinary = superviseOrdinaryProcess

// newProcessPipe is the seam the pipe refusal branches are proven through.
var newProcessPipe = os.Pipe

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

	pipes, err := ordinaryProcessPipes(command)
	if err != nil {
		return ProcessHandle{}, err
	}

	startErr := command.Start()

	// From Start onward the child holds its own ends of all three pipes, and
	// this process must drop its copies: while a copy lives here the child
	// never reads EOF on stdin, and neither drain ever reaches one on stdout
	// or stderr.
	pipes.closeChildEnds()

	if startErr != nil {
		pipes.closeParentEnds()

		return ProcessHandle{}, fmt.Errorf("start native process: %w", startErr)
	}

	guard, err := superviseOrdinary(command)
	if err != nil {
		// A child the platform will not contain is never handed back, and it
		// is waited on rather than only killed: the wait is what reaps it. The
		// parent ends are this backend's own property, so they are released
		// here as well — nothing else now holds them.
		_ = command.Process.Kill()
		_ = command.Wait()

		pipes.closeParentEnds()

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
		Input: pipes.input, Output: pipes.output, Errors: pipes.errors,
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

// ordinaryPipes holds both ends of the child's three standard streams while the
// process is being started, so a refusal at any point releases every
// descriptor already claimed.
type ordinaryPipes struct {
	input  *os.File
	output *os.File
	errors *os.File

	childInput  *os.File
	childOutput *os.File
	childErrors *os.File
}

func (p *ordinaryPipes) closeChildEnds() {
	_ = p.childInput.Close()
	_ = p.childOutput.Close()
	_ = p.childErrors.Close()
}

func (p *ordinaryPipes) closeParentEnds() {
	_ = p.input.Close()
	_ = p.output.Close()
	_ = p.errors.Close()
}

// ordinaryProcessPipes wires the child's three standard streams as ordinary OS
// pipes this backend owns outright.
//
// exec.Cmd's own StdinPipe/StdoutPipe/StderrPipe hand their parent ends to
// Cmd.Wait, which closes them the moment the child exits — a close that races
// whoever is still draining what the child already wrote. This package runs
// that wait concurrently with its readers: the settlement waits on its own
// goroutine while the stdout and stderr drains are still reading, so a runtime
// that logs its reason for leaving and then exits loses exactly the tail that
// explains the exit. Owning both ends here keeps every parent end open until
// its reader sees EOF, so the child's last bytes survive whatever the
// scheduler does with the exit.
func ordinaryProcessPipes(command *exec.Cmd) (_ *ordinaryPipes, err error) {
	pipes := &ordinaryPipes{}

	defer func() {
		if err != nil {
			pipes.closeChildEnds()
			pipes.closeParentEnds()
		}
	}()

	if command.Stdin != nil {
		return nil, errors.New("open native stdin: stdin already set")
	}

	pipes.childInput, pipes.input, err = newProcessPipe()
	if err != nil {
		return nil, fmt.Errorf("open native stdin: %w", err)
	}

	if command.Stdout != nil {
		return nil, errors.New("open native stdout: stdout already set")
	}

	pipes.output, pipes.childOutput, err = newProcessPipe()
	if err != nil {
		return nil, fmt.Errorf("open native stdout: %w", err)
	}

	if command.Stderr != nil {
		return nil, errors.New("open native stderr: stderr already set")
	}

	pipes.errors, pipes.childErrors, err = newProcessPipe()
	if err != nil {
		return nil, fmt.Errorf("open native stderr: %w", err)
	}

	command.Stdin = pipes.childInput
	command.Stdout = pipes.childOutput
	command.Stderr = pipes.childErrors

	return pipes, nil
}

func normalizeOrdinaryWaitError(err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return nil
	}

	return err
}
