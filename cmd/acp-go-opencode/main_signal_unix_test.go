//go:build unix

package main

import (
	"context"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"

	opencodeacp "github.com/savid/acp-go-opencode"
)

// TestRunForwardsATerminationSignal drives the whole forwarding path: the
// process raises SIGTERM on itself, run's handler cancels the serve context,
// and the exit code is the signal's. Raising a signal on this process is the
// part only a POSIX platform can do.
func TestRunForwardsATerminationSignal(t *testing.T) {
	restore := replaceGlobals(t)
	defer restore()

	serve = func(ctx context.Context, _ io.Reader, _ io.Writer, _ ...opencodeacp.Option) error {
		proc, err := os.FindProcess(os.Getpid())
		if err != nil {
			return err
		}

		if err := proc.Signal(syscall.SIGTERM); err != nil {
			return err
		}

		<-ctx.Done()

		return ctx.Err()
	}

	if code := run(context.Background(), nil, strings.NewReader(""), io.Discard, io.Discard); code != 143 {
		t.Fatalf("signalled serve code = %d", code)
	}
}
