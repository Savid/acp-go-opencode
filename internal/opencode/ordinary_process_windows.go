//go:build windows

package opencode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ordinaryProcessGuard owns the job object the native process tree lives in.
//
// Windows has no process group that outlives its leader, and the native
// executable is reached through an npm `.cmd` shim, so the direct child is a
// cmd.exe the real harness hangs off. Killing the direct child therefore
// orphans the harness, which goes on holding its loopback port and its
// database open. A job object is the containment Windows does offer: it owns a
// whole tree by membership rather than by parentage, so descendants stay
// contained once their parent is gone.
//
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE makes that containment outlive this
// process as well. When the last handle to the job closes — on teardown, or
// because this process died without tearing anything down — Windows terminates
// every process still inside it. The POSIX process group has no equivalent
// property: a group survives an adapter that exits without signalling it.
//
// The job is entered after the child has started, because Go's exec offers no
// way to resume a suspended process. A descendant created in the window
// between start and assignment would not be a member, since membership is
// inherited at creation. The window is the shim's own startup and no native
// descendant has been observed to escape it; a descendant that did would be
// left exactly where every descendant was before the job existed.
type ordinaryProcessGuard struct {
	mu     sync.Mutex
	job    windows.Handle
	closed bool
}

func configureOrdinaryProcess(*exec.Cmd) {}

// superviseOrdinaryProcess places the freshly started process, and every
// descendant it goes on to create, in a kill-on-close job object.
func superviseOrdinaryProcess(command *exec.Cmd) (*ordinaryProcessGuard, error) {
	if command == nil || command.Process == nil {
		return &ordinaryProcessGuard{}, nil
	}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create native job object: %w", err)
	}

	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		_ = windows.CloseHandle(job)

		return nil, fmt.Errorf("limit native job object: %w", err)
	}

	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false,
		uint32(command.Process.Pid))
	if err != nil {
		_ = windows.CloseHandle(job)

		return nil, fmt.Errorf("open native process: %w", err)
	}

	defer func() { _ = windows.CloseHandle(process) }()

	if err = windows.AssignProcessToJobObject(job, process); err != nil {
		_ = windows.CloseHandle(job)

		return nil, fmt.Errorf("assign native process to job object: %w", err)
	}

	return &ordinaryProcessGuard{job: job}, nil
}

// terminate kills every process in the job. It reports whether it was this
// call that did so, and answers a job already closed the way the POSIX group
// kill answers a group already gone: nothing to revoke, and no error. The
// handle is read under the lock because teardown races the wait goroutine's
// close, and a terminate against a closed handle would at best fail and at
// worst address whatever object Windows had since given that handle value to.
func (g *ordinaryProcessGuard) terminate() (bool, error) {
	if g == nil {
		return false, nil
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if g.job == 0 {
		return false, nil
	}

	if err := windows.TerminateJobObject(g.job, 1); err != nil {
		return false, fmt.Errorf("terminate native job object: %w", err)
	}

	return true, nil
}

// close releases the job, which terminates anything still inside it. It is
// idempotent, so the wait goroutine may close a job an earlier teardown has
// already terminated.
func (g *ordinaryProcessGuard) close() error {
	if g == nil {
		return nil
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if g.job == 0 || g.closed {
		return nil
	}

	handle := g.job
	g.job = 0
	g.closed = true

	if err := windows.CloseHandle(handle); err != nil {
		return fmt.Errorf("close native job object: %w", err)
	}

	return nil
}

func stopOrdinaryProcess(_ context.Context, command *exec.Cmd, guard *ordinaryProcessGuard) (bool, error) {
	if stopped, err := guard.terminate(); err != nil || stopped {
		return stopped, err
	}

	if command == nil || command.Process == nil {
		return false, nil
	}

	err := command.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return false, nil
	}

	return err == nil, err
}

// containOrdinaryProcess closes the job once the direct child has been waited
// on, mirroring the POSIX group kill that follows the same wait.
func containOrdinaryProcess(_ *exec.Cmd, guard *ordinaryProcessGuard) error {
	return guard.close()
}

func ordinaryProcessOutcome(command *exec.Cmd, _ error) ProcessOutcome {
	result := ProcessOutcome{ExitCode: -1}
	if command == nil || command.ProcessState == nil {
		return result
	}

	result.ExitCode = command.ProcessState.ExitCode()

	return result
}
