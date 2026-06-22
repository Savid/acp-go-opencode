package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	opencodeacp "github.com/savid/acp-go-opencode"
)

func TestRun(t *testing.T) {
	t.Run("parse error", func(t *testing.T) {
		var stderr bytes.Buffer
		code := run(context.Background(), []string{"-unknown"}, strings.NewReader(""), io.Discard, &stderr)
		if code != 2 {
			t.Fatalf("code = %d, want 2", code)
		}
	})

	t.Run("version", func(t *testing.T) {
		oldVersion := agentVersion
		agentVersion = func() string { return "test-version" }
		defer func() { agentVersion = oldVersion }()

		var stdout bytes.Buffer
		code := run(context.Background(), []string{"-version"}, strings.NewReader(""), &stdout, io.Discard)
		if code != 0 {
			t.Fatalf("code = %d, want 0", code)
		}
		if stdout.String() != "test-version\n" {
			t.Fatalf("stdout = %q", stdout.String())
		}
	})

	t.Run("serve success", func(t *testing.T) {
		oldServe := serve
		serve = func(
			ctx context.Context,
			input io.Reader,
			output io.Writer,
			opts ...opencodeacp.Option,
		) error {
			options := opencodeacp.Options{}
			for _, opt := range opts {
				opt(&options)
			}
			if options.Hostname != "127.0.0.1" ||
				options.Port == nil ||
				*options.Port != 0 ||
				!options.MDNS ||
				options.MDNSDomain != "opencode.local" ||
				len(options.CORS) != 1 ||
				options.CORS[0] != "https://example.com" ||
				options.QuestionTool == nil ||
				!*options.QuestionTool ||
				!options.IsolateTempDir ||
				!options.Telemetry.Enabled ||
				options.Telemetry.Endpoint != "http://otel" {
				return errors.New("unexpected options")
			}
			_, _ = io.Copy(output, input)
			return nil
		}
		defer func() { serve = oldServe }()

		var stdout bytes.Buffer
		code := run(context.Background(), []string{
			"-debug",
			"-hostname", "127.0.0.1",
			"-port", "0",
			"-mdns",
			"-mdns-domain", "opencode.local",
			"-cors", "https://example.com",
			"-question-tool",
			"-enable-telemetry",
			"-otlp-endpoint", "http://otel",
			"--", "--custom",
		}, strings.NewReader("abc"), &stdout, io.Discard)
		if code != 0 {
			t.Fatalf("code = %d, want 0", code)
		}
		if stdout.String() != "abc" {
			t.Fatalf("stdout = %q", stdout.String())
		}
	})

	t.Run("serve error", func(t *testing.T) {
		oldServe := serve
		serve = func(context.Context, io.Reader, io.Writer, ...opencodeacp.Option) error {
			return errors.New("boom")
		}
		defer func() { serve = oldServe }()

		var stderr bytes.Buffer
		code := run(context.Background(), nil, strings.NewReader(""), io.Discard, &stderr)
		if code != 1 {
			t.Fatalf("code = %d, want 1", code)
		}
		if !strings.Contains(stderr.String(), "boom") {
			t.Fatalf("stderr = %q", stderr.String())
		}
	})

	t.Run("signal exit", func(t *testing.T) {
		oldServe := serve
		serve = func(ctx context.Context, _ io.Reader, _ io.Writer, _ ...opencodeacp.Option) error {
			process, err := os.FindProcess(os.Getpid())
			if err != nil {
				return err
			}
			if err := process.Signal(syscall.SIGHUP); err != nil {
				return err
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second):
				return errors.New("signal did not cancel context")
			}
		}
		defer func() { serve = oldServe }()

		code := run(context.Background(), nil, strings.NewReader(""), io.Discard, io.Discard)
		if code != 128+int(syscall.SIGHUP) {
			t.Fatalf("signal code = %d", code)
		}
	})
}

func TestMain(t *testing.T) {
	oldExit := exit
	oldArgs := os.Args
	t.Cleanup(func() {
		exit = oldExit
		os.Args = oldArgs
	})

	exitCode := -1
	exit = func(code int) { exitCode = code }
	os.Args = []string{"acp-go-opencode", "-unknown"}
	main()
	if exitCode != 2 {
		t.Fatalf("main exit = %d, want 2", exitCode)
	}
}

func TestFlagWasSet(t *testing.T) {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	value := flags.Bool("enabled", false, "")
	if err := flags.Parse([]string{"-enabled"}); err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if !*value {
		t.Fatal("flag value was false")
	}
	if !flagWasSet(flags, "enabled") {
		t.Fatal("flagWasSet returned false")
	}
	if flagWasSet(flags, "missing") {
		t.Fatal("flagWasSet returned true for missing flag")
	}
}

func TestStringListFlag(t *testing.T) {
	var nilValues *stringListFlag
	if got := nilValues.String(); got != "" {
		t.Fatalf("nil String() = %q", got)
	}

	var values stringListFlag
	if got := values.String(); got != "" {
		t.Fatalf("String() = %q", got)
	}
	if err := values.Set("a"); err != nil {
		t.Fatalf("Set returned error: %v", err)
	}
	if err := values.Set("b"); err != nil {
		t.Fatalf("Set returned error: %v", err)
	}
	if got := values.String(); got != "a,b" {
		t.Fatalf("String() = %q", got)
	}
}

func TestVersion(t *testing.T) {
	old := buildVersion
	buildVersion = ""
	if got := version(); got != "dev" {
		t.Fatalf("version() = %q", got)
	}
	buildVersion = "v1"
	if got := version(); got != "v1" {
		t.Fatalf("version() = %q", got)
	}
	buildVersion = old
}

func TestPendingSignal(t *testing.T) {
	signals := make(chan os.Signal, 1)
	if sig := pendingSignal(signals); sig != nil {
		t.Fatalf("pendingSignal empty = %v", sig)
	}
	signals <- syscall.SIGTERM
	if sig := pendingSignal(signals); sig != syscall.SIGTERM {
		t.Fatalf("pendingSignal = %v", sig)
	}
}

func TestCommandExitCode(t *testing.T) {
	if code := commandExitCode(errors.New("plain")); code != 1 {
		t.Fatalf("plain code = %d", code)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestExitHelperProcess")
	cmd.Env = append(os.Environ(), "GO_WANT_EXIT_HELPER_PROCESS=1")
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("err = %v, want ExitError", err)
	}
	if code := signalExitCode(exitErr); code != 0 {
		t.Fatalf("non-signal exit code = %d, want 0", code)
	}
	if code := commandExitCode(exitErr); code != 7 {
		t.Fatalf("exit code = %d, want 7", code)
	}

	cmd = exec.Command(os.Args[0], "-test.run=TestSignalHelperProcess")
	cmd.Env = append(os.Environ(), "GO_WANT_SIGNAL_HELPER_PROCESS=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("Signal returned error: %v", err)
	}
	err = cmd.Wait()
	if !errors.As(err, &exitErr) {
		t.Fatalf("signal err = %v, want ExitError", err)
	}
	if code := commandExitCode(exitErr); code != 128+int(syscall.SIGTERM) {
		t.Fatalf("signal exit code = %d", code)
	}
}

func TestSignalCode(t *testing.T) {
	if code := signalCode(syscall.SIGTERM); code != 128+int(syscall.SIGTERM) {
		t.Fatalf("signalCode(SIGTERM) = %d", code)
	}
	if code := signalCode(fakeSignal("custom")); code != 1 {
		t.Fatalf("signalCode(custom) = %d", code)
	}
}

func TestExitHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_EXIT_HELPER_PROCESS") != "1" {
		return
	}
	os.Exit(7)
}

func TestSignalHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_SIGNAL_HELPER_PROCESS") != "1" {
		return
	}

	time.Sleep(time.Minute)
}

type fakeSignal string

func (s fakeSignal) Signal() {}

func (s fakeSignal) String() string {
	return string(s)
}
