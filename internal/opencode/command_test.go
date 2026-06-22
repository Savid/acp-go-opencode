package opencode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBuildArgs(t *testing.T) {
	t.Parallel()

	args := BuildArgs(Options{
		Cwd:        "/workspace",
		Pure:       true,
		PrintLogs:  true,
		LogLevel:   "DEBUG",
		Hostname:   "127.0.0.1",
		Port:       intPtr(0),
		MDNS:       true,
		MDNSDomain: "opencode.local",
		CORS:       []string{"https://example.com", " "},
		ExtraArgs:  []string{"--custom"},
	})
	want := []string{
		"acp",
		"--print-logs",
		"--log-level", "DEBUG",
		"--pure",
		"--hostname", "127.0.0.1",
		"--port", "0",
		"--mdns",
		"--mdns-domain", "opencode.local",
		"--cors", "https://example.com",
		"--cwd", "/workspace",
		"--custom",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("BuildArgs() = %#v, want %#v", args, want)
	}
}

func TestBuildEnvMergesSortedOverrides(t *testing.T) {
	oldEnviron := commandEnviron
	commandEnviron = func() []string {
		return []string{"B=old", "A=1", "BROKEN", "=empty"}
	}
	defer func() { commandEnviron = oldEnviron }()

	env := BuildEnv(Options{Env: map[string]string{
		"":  "skip",
		"B": "new",
		"C": "3",
	}})
	want := []string{"B=new", "A=1", "C=3"}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("BuildEnv() = %#v, want %#v", env, want)
	}

	gotMap := envSliceToMap([]string{"BROKEN", "=empty", "A=1"})
	if !reflect.DeepEqual(gotMap, map[string]string{"A": "1"}) {
		t.Fatalf("envSliceToMap() = %#v", gotMap)
	}
}

func TestBuildEnvAddsOpenCodeSettings(t *testing.T) {
	oldEnviron := commandEnviron
	commandEnviron = func() []string {
		return []string{"PATH=/bin"}
	}
	defer func() { commandEnviron = oldEnviron }()

	env := BuildEnvMap(Options{
		QuestionTool: boolPtr(true),
		Telemetry: TelemetryOptions{
			Enabled:            true,
			Endpoint:           "http://otel",
			Protocol:           "http/protobuf",
			MetricsInterval:    "1000",
			ResourceAttributes: "service.name=test",
			DisableLogs:        true,
			DisableTraces:      "session",
			Traceparent:        "00-trace-span-01",
			Tracestate:         "state",
		},
		TempDir: "/tmp/opencode",
	})

	want := map[string]string{
		"PATH":                "/bin",
		envQuestionTool:       "1",
		envEnableTelemetry:    "1",
		envOTLPEndpoint:       "http://otel",
		envOTLPProtocol:       "http/protobuf",
		envOTLPMetrics:        "1000",
		envResourceAttributes: "service.name=test",
		envDisableLogs:        "1",
		envDisableTraces:      "session",
		envTraceparent:        "00-trace-span-01",
		envTracestate:         "state",
		envTempDir:            "/tmp/opencode",
		envTemp:               "/tmp/opencode",
		envTempWindows:        "/tmp/opencode",
	}
	for key, value := range want {
		if env[key] != value {
			t.Fatalf("env[%s] = %q, want %q", key, env[key], value)
		}
	}

	env = BuildEnvMap(Options{QuestionTool: boolPtr(false)})
	if env[envQuestionTool] != "0" {
		t.Fatalf("false question tool env = %q", env[envQuestionTool])
	}
}

func TestDiscover(t *testing.T) {
	t.Run("explicit path", func(t *testing.T) {
		executable := testExecutable(t, "opencode")
		path, err := Discover(context.Background(), executable, nil)
		if err != nil {
			t.Fatalf("Discover returned error: %v", err)
		}
		if path != executable {
			t.Fatalf("path = %q", path)
		}
	})

	t.Run("env map", func(t *testing.T) {
		executable := testExecutable(t, "opencode")
		path, err := Discover(context.Background(), "", map[string]string{EnvOpenCodeExecutable: executable})
		if err != nil {
			t.Fatalf("Discover returned error: %v", err)
		}
		if path != executable {
			t.Fatalf("path = %q", path)
		}
	})

	t.Run("process env", func(t *testing.T) {
		executable := testExecutable(t, "opencode")
		t.Setenv(EnvOpenCodeExecutable, executable)
		path, err := Discover(context.Background(), "", nil)
		if err != nil {
			t.Fatalf("Discover returned error: %v", err)
		}
		if path != executable {
			t.Fatalf("path = %q", path)
		}
	})

	t.Run("explicit env map suppresses process env", func(t *testing.T) {
		ambientExecutable := testExecutable(t, "ambient-opencode")
		pathExecutable := testExecutable(t, "opencode")
		t.Setenv(EnvOpenCodeExecutable, ambientExecutable)

		path, err := Discover(context.Background(), "", map[string]string{"PATH": filepath.Dir(pathExecutable)})
		if err != nil {
			t.Fatalf("Discover returned error: %v", err)
		}
		if path != pathExecutable {
			t.Fatalf("path = %q, want %q", path, pathExecutable)
		}
	})

	t.Run("path lookup", func(t *testing.T) {
		t.Setenv(EnvOpenCodeExecutable, "")
		executable := testExecutable(t, "opencode")

		path, err := Discover(context.Background(), "", map[string]string{"PATH": filepath.Dir(executable)})
		if err != nil {
			t.Fatalf("Discover returned error: %v", err)
		}
		if path != executable {
			t.Fatalf("path = %q", path)
		}
	})

	t.Run("canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := Discover(ctx, "/bin/opencode", nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})

	t.Run("missing binary", func(t *testing.T) {
		t.Setenv(EnvOpenCodeExecutable, "")
		_, err := Discover(context.Background(), "", map[string]string{"PATH": t.TempDir()})
		if err == nil || !strings.Contains(err.Error(), "not found in effective PATH") {
			t.Fatalf("err = %v, want effective PATH error", err)
		}
	})
}

func TestRunACP(t *testing.T) {
	t.Run("wires streams", func(t *testing.T) {
		restore := stubCommandContext(t)
		defer restore()

		var stdout bytes.Buffer
		var stderr bytes.Buffer
		err := RunACP(
			context.Background(),
			strings.NewReader("prompt"),
			&stdout,
			&stderr,
			Options{
				CLIPath:        os.Args[0],
				Cwd:            ".",
				Pure:           true,
				PrintLogs:      true,
				Hostname:       "127.0.0.1",
				Port:           intPtr(0),
				IsolateTempDir: true,
				Env: map[string]string{
					"GO_WANT_OPENCODE_HELPER_PROCESS": "1",
					"OPENCODE_HELPER_MODE":            "echo",
				},
			},
		)
		if err != nil {
			t.Fatalf("RunACP returned error: %v", err)
		}
		got := stdout.String()
		if !strings.Contains(got, "args="+os.Args[0]+"|acp|--print-logs|--pure|--hostname|127.0.0.1|--port|0|--cwd|.") ||
			!strings.Contains(got, "stdin=prompt") ||
			!strings.Contains(got, "tmpdir=") {
			t.Fatalf("stdout = %q", got)
		}
		if got := stderr.String(); got != "helper stderr\n" {
			t.Fatalf("stderr = %q", got)
		}
		tempDir := valueAfterPrefix(t, got, "tmpdir=")
		if _, statErr := os.Stat(tempDir); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("temp dir stat error = %v, want not exist", statErr)
		}
	})

	t.Run("wait error", func(t *testing.T) {
		restore := stubCommandContext(t)
		defer restore()

		err := RunACP(context.Background(), strings.NewReader(""), io.Discard, io.Discard, Options{
			CLIPath: os.Args[0],
			Env: map[string]string{
				"GO_WANT_OPENCODE_HELPER_PROCESS": "1",
				"OPENCODE_HELPER_MODE":            "exit3",
			},
		})
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
			t.Fatalf("err = %v, want exit code 3", err)
		}
	})

	t.Run("nil stderr", func(t *testing.T) {
		restore := stubCommandContext(t)
		defer restore()

		err := RunACP(context.Background(), strings.NewReader(""), io.Discard, nil, Options{
			CLIPath: os.Args[0],
			Env: map[string]string{
				"GO_WANT_OPENCODE_HELPER_PROCESS": "1",
				"OPENCODE_HELPER_MODE":            "echo",
			},
		})
		if err != nil {
			t.Fatalf("RunACP returned error: %v", err)
		}
	})

	t.Run("context cancellation", func(t *testing.T) {
		restore := stubCommandContext(t)
		defer restore()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := RunACP(ctx, strings.NewReader(""), io.Discard, io.Discard, Options{
			CLIPath: os.Args[0],
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})

	t.Run("context cancellation after start", func(t *testing.T) {
		restore := stubCommandContext(t)
		defer restore()

		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()

		err := RunACP(ctx, strings.NewReader(""), io.Discard, io.Discard, Options{
			CLIPath: os.Args[0],
			Env: map[string]string{
				"GO_WANT_OPENCODE_HELPER_PROCESS": "1",
				"OPENCODE_HELPER_MODE":            "sleep",
			},
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})

	t.Run("context cancellation with blocking non-file input", func(t *testing.T) {
		restore := stubCommandContext(t)
		defer restore()

		done := make(chan struct{})
		reader := &blockingReader{done: done}
		defer close(done)

		ctx, cancel := context.WithCancel(context.Background())
		errCh := make(chan error, 1)
		go func() {
			errCh <- RunACP(ctx, reader, io.Discard, io.Discard, Options{
				CLIPath: os.Args[0],
				Env: map[string]string{
					"GO_WANT_OPENCODE_HELPER_PROCESS": "1",
					"OPENCODE_HELPER_MODE":            "sleep",
				},
			})
		}()

		time.Sleep(50 * time.Millisecond)
		cancel()

		select {
		case err := <-errCh:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
		case <-time.After(time.Second):
			t.Fatal("RunACP did not return after context cancellation")
		}
	})

	t.Run("temp dir creation error", func(t *testing.T) {
		oldMkdirTemp := mkdirTemp
		mkdirTemp = func(string, string) (string, error) {
			return "", errors.New("mkdir failed")
		}
		defer func() { mkdirTemp = oldMkdirTemp }()

		err := RunACP(context.Background(), strings.NewReader(""), io.Discard, io.Discard, Options{
			CLIPath:        os.Args[0],
			IsolateTempDir: true,
		})
		if err == nil || !strings.Contains(err.Error(), "create opencode temp dir") {
			t.Fatalf("err = %v, want temp dir creation error", err)
		}
	})

	t.Run("temp dir cleanup error", func(t *testing.T) {
		restore := stubCommandContext(t)
		defer restore()

		oldMkdirTemp := mkdirTemp
		oldRemoveAll := removeAll
		mkdirTemp = func(string, string) (string, error) {
			return t.TempDir(), nil
		}
		removeAll = func(string) error {
			return errors.New("cleanup failed")
		}
		defer func() {
			mkdirTemp = oldMkdirTemp
			removeAll = oldRemoveAll
		}()

		err := RunACP(context.Background(), strings.NewReader(""), io.Discard, io.Discard, Options{
			CLIPath:        os.Args[0],
			IsolateTempDir: true,
			Env: map[string]string{
				"GO_WANT_OPENCODE_HELPER_PROCESS": "1",
				"OPENCODE_HELPER_MODE":            "echo",
			},
		})
		if err == nil || !strings.Contains(err.Error(), "cleanup opencode temp dir") {
			t.Fatalf("err = %v, want cleanup error", err)
		}
	})

	t.Run("start error", func(t *testing.T) {
		oldCommandContext := commandContext
		commandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "/path/that/does/not/exist", args...)
		}
		defer func() { commandContext = oldCommandContext }()

		err := RunACP(context.Background(), strings.NewReader(""), io.Discard, io.Discard, Options{
			CLIPath: os.Args[0],
		})
		if err == nil || !strings.Contains(err.Error(), "start opencode acp") {
			t.Fatalf("err = %v, want start error", err)
		}
	})

	t.Run("stdin pipe error", func(t *testing.T) {
		oldCommandContext := commandContext
		commandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperProcess")
			cmd.Stdin = strings.NewReader("")

			return cmd
		}
		defer func() { commandContext = oldCommandContext }()

		err := RunACP(context.Background(), strings.NewReader(""), io.Discard, io.Discard, Options{
			CLIPath: os.Args[0],
		})
		if err == nil || !strings.Contains(err.Error(), "create opencode stdin pipe") {
			t.Fatalf("err = %v, want stdin pipe error", err)
		}
	})

	t.Run("validates streams", func(t *testing.T) {
		var nilContext context.Context
		if err := RunACP(nilContext, strings.NewReader(""), io.Discard, io.Discard, Options{}); err == nil {
			t.Fatal("RunACP accepted nil context")
		}
		if err := RunACP(context.Background(), nil, io.Discard, io.Discard, Options{}); err == nil {
			t.Fatal("RunACP accepted nil input")
		}
		if err := RunACP(context.Background(), strings.NewReader(""), nil, io.Discard, Options{}); err == nil {
			t.Fatal("RunACP accepted nil output")
		}
	})
}

func TestConfigureCommandStdin(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "stdin-*")
	if err != nil {
		t.Fatalf("create temp stdin: %v", err)
	}
	defer func() { _ = file.Close() }()

	cmd := exec.Command("cat")
	closeStdin, err := configureCommandStdin(cmd, file)
	if err != nil {
		t.Fatalf("file stdin returned error: %v", err)
	}
	if cmd.Stdin != file {
		t.Fatalf("cmd.Stdin = %#v, want file", cmd.Stdin)
	}
	closeStdin()

	cmd = exec.Command("cat")
	closeStdin, err = configureCommandStdin(cmd, strings.NewReader(""))
	if err != nil {
		t.Fatalf("pipe stdin returned error: %v", err)
	}
	closeStdin()

	cmd = exec.Command("cat")
	cmd.Stdin = strings.NewReader("")
	if _, err := configureCommandStdin(cmd, strings.NewReader("")); err == nil ||
		!strings.Contains(err.Error(), "create opencode stdin pipe") {
		t.Fatalf("stdin pipe error = %v", err)
	}
}

func TestShutdownProcess(t *testing.T) {
	oldTerminate := processTerminate
	oldKill := processKill
	oldDelay := processShutdownWaitDelay
	t.Cleanup(func() {
		processTerminate = oldTerminate
		processKill = oldKill
		processShutdownWaitDelay = oldDelay
	})
	processShutdownWaitDelay = time.Millisecond

	terminated := false
	killed := false
	processTerminate = func(*exec.Cmd) error {
		terminated = true

		return nil
	}
	processKill = func(*exec.Cmd) error {
		killed = true

		return nil
	}
	waitErr := make(chan error, 1)
	waitErr <- nil
	if err := shutdownProcess(&exec.Cmd{}, waitErr); err != nil {
		t.Fatalf("shutdownProcess returned error: %v", err)
	}
	if !terminated || killed {
		t.Fatalf("terminated=%v killed=%v", terminated, killed)
	}

	terminated = false
	killed = false
	waitErr = make(chan error, 1)
	processKill = func(*exec.Cmd) error {
		killed = true
		waitErr <- nil

		return nil
	}
	if err := shutdownProcess(&exec.Cmd{}, waitErr); err != nil {
		t.Fatalf("shutdownProcess after kill returned error: %v", err)
	}
	if !terminated || !killed {
		t.Fatalf("terminated=%v killed=%v", terminated, killed)
	}

	processTerminate = func(*exec.Cmd) error {
		return errors.New("terminate failed")
	}
	processKill = func(*exec.Cmd) error {
		return errors.New("kill failed")
	}
	waitErr = make(chan error)
	err := shutdownProcess(&exec.Cmd{}, waitErr)
	if err == nil ||
		!strings.Contains(err.Error(), "terminate failed") ||
		!strings.Contains(err.Error(), "kill failed") ||
		!errors.Is(err, errProcessWaitTimeout) {
		t.Fatalf("shutdownProcess timeout error = %v", err)
	}
}

func TestConfigureProcessCommand(t *testing.T) {
	cmd := exec.Command("cat")
	configureProcessCommand(cmd)
	if cmd.WaitDelay != processShutdownWaitDelay {
		t.Fatalf("WaitDelay = %v, want %v", cmd.WaitDelay, processShutdownWaitDelay)
	}
}

func TestResolveBinaryEdges(t *testing.T) {
	if _, err := resolveBinary(" ", nil); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty resolve error = %v", err)
	}

	dir := t.TempDir()
	if _, err := resolveBinary(dir, nil); err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("directory resolve error = %v", err)
	}

	nonExecutable := filepath.Join(dir, "plain")
	if err := os.WriteFile(nonExecutable, []byte("plain"), 0o600); err != nil {
		t.Fatalf("write non-executable: %v", err)
	}
	if err := executableFile(nonExecutable); err == nil || !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("non-executable error = %v", err)
	}

	executable := testExecutable(t, "opencode")
	pathWithEmptyEntry := string(os.PathListSeparator) + filepath.Dir(executable)
	path, err := resolveBinary("opencode", map[string]string{"PATH": pathWithEmptyEntry})
	if err != nil {
		t.Fatalf("resolve with empty PATH entry returned error: %v", err)
	}
	if path != executable {
		t.Fatalf("path = %q, want %q", path, executable)
	}

	oldGOOS := runtimeGOOS
	runtimeGOOS = "windows"
	defer func() { runtimeGOOS = oldGOOS }()

	windowsDir := t.TempDir()
	windowsExecutable := filepath.Join(windowsDir, "opencode.exe")
	if err := os.WriteFile(windowsExecutable, []byte("exe"), 0o600); err != nil {
		t.Fatalf("write windows executable: %v", err)
	}
	path, err = resolveBinary("opencode", map[string]string{"PATH": windowsDir})
	if err != nil {
		t.Fatalf("windows resolve returned error: %v", err)
	}
	if path != windowsExecutable {
		t.Fatalf("windows path = %q, want %q", path, windowsExecutable)
	}
}

type blockingReader struct {
	done <-chan struct{}
}

func (r *blockingReader) Read([]byte) (int, error) {
	<-r.done

	return 0, io.EOF
}

func stubCommandContext(t *testing.T) func() {
	t.Helper()

	oldCommandContext := commandContext
	commandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		helperArgs := make([]string, 0, 3+len(args))
		helperArgs = append(helperArgs, "-test.run=TestHelperProcess", "--", name)
		helperArgs = append(helperArgs, args...)

		return exec.CommandContext(ctx, os.Args[0], helperArgs...)
	}

	return func() { commandContext = oldCommandContext }
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_OPENCODE_HELPER_PROCESS") != "1" {
		return
	}

	mode := os.Getenv("OPENCODE_HELPER_MODE")
	switch mode {
	case "echo":
		index := slicesIndex(os.Args, "--")
		if index < 0 {
			fmt.Fprintln(os.Stderr, "missing --")
			os.Exit(2)
		}
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read stdin: %v\n", err)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stdout, "args=%s\nstdin=%s\n", strings.Join(os.Args[index+1:], "|"), string(data))
		fmt.Fprintf(os.Stdout, "tmpdir=%s\n", os.Getenv(envTempDir))
		fmt.Fprintln(os.Stderr, "helper stderr")
		os.Exit(0)
	case "exit3":
		os.Exit(3)
	case "sleep":
		time.Sleep(time.Minute)
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
		os.Exit(2)
	}
}

func slicesIndex(values []string, target string) int {
	for index, value := range values {
		if value == target {
			return index
		}
	}

	return -1
}

func testExecutable(t *testing.T, name string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write executable: %v", err)
	}

	return path
}

func intPtr(value int) *int {
	return &value
}

func boolPtr(value bool) *bool {
	return &value
}

func valueAfterPrefix(t *testing.T, text string, prefix string) string {
	t.Helper()

	for _, line := range strings.Split(text, "\n") {
		if value, ok := strings.CutPrefix(line, prefix); ok {
			return value
		}
	}
	t.Fatalf("missing prefix %q in %q", prefix, text)

	return ""
}
