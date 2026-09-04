//go:build !unix

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"

	opencodeacp "github.com/savid/acp-go-opencode"
)

// TestRunReportsASignalThisPlatformCannotRaise states the platform half of the
// forwarding path. os.Process.Signal refuses everything but Kill on Windows, so
// no process can raise SIGTERM on itself and run's forwarded exit code is not
// reachable from inside the process; the code the handler would return is
// proved directly by TestSignals. What run owes here is that the refusal
// reaches the operator instead of being swallowed into a success.
func TestRunReportsASignalThisPlatformCannotRaise(t *testing.T) {
	restore := replaceGlobals(t)
	defer restore()

	serve = func(context.Context, io.Reader, io.Writer, ...opencodeacp.Option) error {
		proc, err := os.FindProcess(os.Getpid())
		if err != nil {
			return err
		}

		return proc.Signal(syscall.SIGTERM)
	}

	var stderr bytes.Buffer

	if code := run(context.Background(), nil, strings.NewReader(""), io.Discard, &stderr); code != 1 {
		t.Fatalf("refused signal code = %d", code)
	}

	if stderr.Len() == 0 {
		t.Fatal("the refused signal was not reported")
	}
}
