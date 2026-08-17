//go:build unix

package opencode

import (
	"errors"
	"syscall"
	"testing"
)

// TestSignalLeasedProcessGroupReportsARefusedSignal pins that a signal the
// kernel refused is reported rather than read as the orphan having left.
func TestSignalLeasedProcessGroupReportsARefusedSignal(t *testing.T) {
	original := openCodeSyscallKill
	openCodeSyscallKill = func(int, syscall.Signal) error { return errors.New("signal") }

	t.Cleanup(func() { openCodeSyscallKill = original })

	if err := signalLeasedProcessGroup(4242, false); err == nil {
		t.Fatal("a refused signal reported success")
	}
}
