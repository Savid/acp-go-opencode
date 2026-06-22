package opencodeacp

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/savid/acp-go-opencode/internal/opencode"
)

func TestServeValidatesInputs(t *testing.T) {
	t.Parallel()

	var nilContext context.Context
	if err := Serve(nilContext, strings.NewReader(""), &strings.Builder{}); err == nil {
		t.Fatal("Serve accepted nil context")
	}
	if err := Serve(context.Background(), nil, &strings.Builder{}); err == nil {
		t.Fatal("Serve accepted nil input")
	}
	if err := Serve(context.Background(), strings.NewReader(""), nil); err == nil {
		t.Fatal("Serve accepted nil output")
	}
}

func TestServeRunsACP(t *testing.T) {
	originalRunACP := runACP
	t.Cleanup(func() { runACP = originalRunACP })

	var gotStderr io.Writer
	runACP = func(
		ctx context.Context,
		input io.Reader,
		output io.Writer,
		stderr io.Writer,
		options opencode.Options,
	) error {
		if ctx == nil || input == nil || output == nil {
			return errors.New("missing stream")
		}
		gotStderr = stderr
		if options.CLIPath != "/bin/opencode" || options.Port == nil || *options.Port != 0 {
			return errors.New("unexpected options")
		}

		return nil
	}

	if err := Serve(
		context.Background(),
		strings.NewReader(""),
		&strings.Builder{},
		WithOpenCodePath("/bin/opencode"),
		WithPort(0),
	); err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}
	if gotStderr == nil {
		t.Fatal("stderr was nil")
	}
}
