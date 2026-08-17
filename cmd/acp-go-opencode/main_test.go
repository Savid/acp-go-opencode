package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	opencodeacp "github.com/savid/acp-go-opencode"
)

func TestRunVersionAndFlagError(t *testing.T) {
	restore := replaceGlobals(t)
	defer restore()
	agentVersion = func() string { return "v-test" }

	var stdout bytes.Buffer
	if code := run(context.Background(), []string{"-version"}, strings.NewReader(""), &stdout, io.Discard); code != 0 {
		t.Fatalf("run version code = %d", code)
	}
	if strings.TrimSpace(stdout.String()) != "v-test" {
		t.Fatalf("version stdout = %q", stdout.String())
	}
	if code := run(context.Background(), []string{"-unknown"}, strings.NewReader(""), io.Discard, io.Discard); code != 2 {
		t.Fatalf("flag error code = %d, want 2", code)
	}
}

func TestRunServeSuccessAndError(t *testing.T) {
	stubProcessIsolationConfig(t)
	restore := replaceGlobals(t)
	defer restore()
	agentVersion = func() string { return "v-test" }

	var gotOptions []opencodeacp.Option
	serve = func(ctx context.Context, input io.Reader, output io.Writer, opts ...opencodeacp.Option) error {
		gotOptions = append([]opencodeacp.Option(nil), opts...)
		_, _ = io.Copy(io.Discard, input)
		_, _ = output.Write([]byte{})
		if ctx.Err() != nil {
			return ctx.Err()
		}

		return nil
	}
	seedHost := filepath.Join(t.TempDir(), "opencode.json")
	if err := os.WriteFile(seedHost, []byte(`{"provider":{}}`), 0o600); err != nil {
		t.Fatalf("write seed host: %v", err)
	}
	if code := run(context.Background(), []string{
		"-process-isolation-config", testProcessIsolationConfigPath,
		"-path", "opencode",
		"-home", "/tmp/home",
		"-scratch-dir", "/tmp/scratch",
		"-model", "openai/gpt-test",
		"-debug",
		"-opencode-pure",
		"-opencode-question-tool",
		"-opencode-log-level", "INFO",
		"-opencode-health-timeout", "1s",
		"-seed-file", "opencode.json=" + seedHost,
	}, strings.NewReader(""), io.Discard, io.Discard); code != 0 {
		t.Fatalf("serve success code = %d", code)
	}
	if len(gotOptions) == 0 {
		t.Fatal("serve received no options")
	}
	var configured opencodeacp.Options
	for _, option := range gotOptions {
		option(&configured)
	}
	if configured.ProcessIsolation == nil || configured.ProcessIsolation.UID != 20001 {
		t.Fatalf("process isolation = %#v", configured.ProcessIsolation)
	}

	serve = func(context.Context, io.Reader, io.Writer, ...opencodeacp.Option) error {
		return errors.New("boom")
	}
	var stderr bytes.Buffer
	if code := run(context.Background(), isolatedArgs(), strings.NewReader(""), io.Discard, &stderr); code != 1 {
		t.Fatalf("serve error code = %d", code)
	}
	if !strings.Contains(stderr.String(), "boom") {
		t.Fatalf("stderr = %q", stderr.String())
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	serve = func(context.Context, io.Reader, io.Writer, ...opencodeacp.Option) error {
		return context.Canceled
	}
	if code := run(cancelled, isolatedArgs(), strings.NewReader(""), io.Discard, io.Discard); code != 0 {
		t.Fatalf("cancelled serve code = %d", code)
	}

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
	if code := run(context.Background(), isolatedArgs(), strings.NewReader(""), io.Discard, io.Discard); code != 143 {
		t.Fatalf("signalled serve code = %d", code)
	}
}

func TestSeedFileFlag(t *testing.T) {
	host := filepath.Join(t.TempDir(), "opencode.json")
	if err := os.WriteFile(host, []byte(`{"provider":{}}`), 0o600); err != nil {
		t.Fatalf("write host seed: %v", err)
	}

	var flag seedFileFlag
	if err := flag.Set("opencode.json=" + host); err != nil {
		t.Fatalf("Set valid seed-file: %v", err)
	}
	if flag.files["opencode.json"] != `{"provider":{}}` {
		t.Fatalf("seed contents = %#v", flag.files)
	}
	if flag.String() != "" {
		t.Fatalf("String() = %q", flag.String())
	}

	for _, bad := range []string{"noeq", "=host", "rel=", ""} {
		if err := (&seedFileFlag{}).Set(bad); err == nil {
			t.Fatalf("Set(%q) accepted malformed value", bad)
		}
	}
	if err := (&seedFileFlag{}).Set("rel=/no/such/seed/file"); err == nil {
		t.Fatal("Set accepted unreadable host path")
	}
}

func TestSignals(t *testing.T) {
	signals := forwardedSignals()
	if len(signals) == 0 {
		t.Fatal("no forwarded signals")
	}
	ch := make(chan os.Signal, 1)
	if pendingSignal(ch) != nil {
		t.Fatal("empty channel returned signal")
	}
	ch <- syscall.SIGTERM
	if got := pendingSignal(ch); got != syscall.SIGTERM {
		t.Fatalf("pendingSignal = %v", got)
	}
	if signalCode(syscall.SIGTERM) != 143 {
		t.Fatalf("SIGTERM code = %d", signalCode(syscall.SIGTERM))
	}
	if signalCode(fakeSignal("fake")) != 1 {
		t.Fatalf("fake signal code = %d", signalCode(fakeSignal("fake")))
	}
}

func TestMainAndVersion(t *testing.T) {
	stubProcessIsolationConfig(t)
	restore := replaceGlobals(t)
	defer restore()
	oldArgs := os.Args
	oldBuildVersion := buildVersion
	defer func() {
		os.Args = oldArgs
		buildVersion = oldBuildVersion
	}()

	serve = func(context.Context, io.Reader, io.Writer, ...opencodeacp.Option) error {
		return errors.New("main failed")
	}
	os.Args = []string{"acp-go-opencode", "-process-isolation-config", testProcessIsolationConfigPath}
	exitCode := -1
	exit = func(code int) { exitCode = code }
	main()
	if exitCode != 1 {
		t.Fatalf("main exit code = %d", exitCode)
	}
	buildVersion = ""
	if version() != "dev" {
		t.Fatalf("empty build version = %q", version())
	}
	buildVersion = "v-test"
	if version() != "v-test" {
		t.Fatalf("build version = %q", version())
	}
}

func replaceGlobals(t *testing.T) func() {
	t.Helper()
	oldServe := serve
	oldVersion := agentVersion
	oldExit := exit
	oldShutdown := shutdownOpenTelemetry

	return func() {
		serve = oldServe
		agentVersion = oldVersion
		exit = oldExit
		shutdownOpenTelemetry = oldShutdown
	}
}

type fakeSignal string

func (s fakeSignal) String() string {
	return string(s)
}

func (s fakeSignal) Signal() {}
