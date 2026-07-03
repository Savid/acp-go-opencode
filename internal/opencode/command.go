package opencode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// EnvOpenCodeExecutable overrides the executable used when Options.CLIPath is empty.
const EnvOpenCodeExecutable = "OPENCODE_EXECUTABLE"

const (
	envQuestionTool       = "OPENCODE_ENABLE_QUESTION_TOOL"
	envEnableTelemetry    = "OPENCODE_ENABLE_TELEMETRY"
	envOTLPEndpoint       = "OPENCODE_OTLP_ENDPOINT"
	envOTLPProtocol       = "OPENCODE_OTLP_PROTOCOL"
	envOTLPMetrics        = "OPENCODE_OTLP_METRICS_INTERVAL"
	envResourceAttributes = "OPENCODE_RESOURCE_ATTRIBUTES"
	envDisableLogs        = "OPENCODE_DISABLE_LOGS"
	envDisableTraces      = "OPENCODE_DISABLE_TRACES"
	envTraceparent        = "OPENCODE_TRACEPARENT"
	envTracestate         = "OPENCODE_TRACESTATE"
	envTempDir            = "TMPDIR"
	envTemp               = "TMP"
	envTempWindows        = "TEMP"
	processTempPrefix     = "acp-go-opencode-"
)

var (
	errProcessWaitTimeout = errors.New("opencode process did not exit after shutdown")

	commandContext = exec.CommandContext
	commandEnviron = os.Environ
	fileStat       = os.Stat
	mkdirTemp      = os.MkdirTemp
	removeAll      = os.RemoveAll
	runtimeGOOS    = runtime.GOOS

	processShutdownWaitDelay = 5 * time.Second
	processExitGracePeriod   = 2 * time.Second
	processTerminate         = terminateProcess
	processKill              = killProcess
)

// BuildArgs returns the OpenCode CLI arguments for the ACP server.
func BuildArgs(options Options) []string {
	args := []string{"acp"}
	if options.PrintLogs {
		args = append(args, "--print-logs")
	}
	if options.LogLevel != "" {
		args = append(args, "--log-level", options.LogLevel)
	}
	if options.Pure {
		args = append(args, "--pure")
	}
	if options.Hostname != "" {
		args = append(args, "--hostname", options.Hostname)
	}
	if options.Port != nil {
		args = append(args, "--port", fmt.Sprint(*options.Port))
	}
	if options.MDNS {
		args = append(args, "--mdns")
	}
	if options.MDNSDomain != "" {
		args = append(args, "--mdns-domain", options.MDNSDomain)
	}
	for _, domain := range options.CORS {
		if strings.TrimSpace(domain) != "" {
			args = append(args, "--cors", domain)
		}
	}
	if options.Cwd != "" {
		args = append(args, "--cwd", options.Cwd)
	}
	args = append(args, options.ExtraArgs...)

	return args
}

// BuildEnv returns the environment for an OpenCode process.
func BuildEnv(options Options) []string {
	values := make(map[string]string)
	keys := make([]string, 0, len(commandEnviron())+len(options.Env))

	set := func(key string, value string) {
		if key == "" {
			return
		}
		if _, ok := values[key]; !ok {
			keys = append(keys, key)
		}
		values[key] = value
	}

	for _, entry := range commandEnviron() {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		set(key, value)
	}

	optionKeys := make([]string, 0, len(options.Env))
	for key := range options.Env {
		optionKeys = append(optionKeys, key)
	}
	slices.Sort(optionKeys)
	for _, key := range optionKeys {
		set(key, options.Env[key])
	}

	for key, value := range processEnvAdditions(options) {
		set(key, value)
	}

	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}

	return env
}

// BuildEnvMap returns the final child process environment keyed by variable name.
func BuildEnvMap(options Options) map[string]string {
	return envSliceToMap(BuildEnv(options))
}

func envSliceToMap(env []string) map[string]string {
	out := make(map[string]string)
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		out[key] = value
	}

	return out
}

func processEnvAdditions(options Options) map[string]string {
	out := make(map[string]string)
	if options.QuestionTool != nil {
		out[envQuestionTool] = boolEnv(*options.QuestionTool)
	}
	if options.Telemetry.Enabled {
		out[envEnableTelemetry] = "1"
	}
	if options.Telemetry.Endpoint != "" {
		out[envOTLPEndpoint] = options.Telemetry.Endpoint
	}
	if options.Telemetry.Protocol != "" {
		out[envOTLPProtocol] = options.Telemetry.Protocol
	}
	if options.Telemetry.MetricsInterval != "" {
		out[envOTLPMetrics] = options.Telemetry.MetricsInterval
	}
	if options.Telemetry.ResourceAttributes != "" {
		out[envResourceAttributes] = options.Telemetry.ResourceAttributes
	}
	if options.Telemetry.DisableLogs {
		out[envDisableLogs] = "1"
	}
	if options.Telemetry.DisableTraces != "" {
		out[envDisableTraces] = options.Telemetry.DisableTraces
	}
	if options.Telemetry.Traceparent != "" {
		out[envTraceparent] = options.Telemetry.Traceparent
	}
	if options.Telemetry.Tracestate != "" {
		out[envTracestate] = options.Telemetry.Tracestate
	}
	if options.TempDir != "" {
		out[envTempDir] = options.TempDir
		out[envTemp] = options.TempDir
		out[envTempWindows] = options.TempDir
	}

	return out
}

func boolEnv(value bool) string {
	if value {
		return "1"
	}

	return "0"
}

// Discover finds the OpenCode executable.
func Discover(ctx context.Context, cliPath string, env map[string]string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if strings.TrimSpace(cliPath) != "" {
		return resolveBinary(cliPath, env)
	}
	if env != nil {
		if path := strings.TrimSpace(env[EnvOpenCodeExecutable]); path != "" {
			return resolveBinary(path, env)
		}
	} else if path := strings.TrimSpace(os.Getenv(EnvOpenCodeExecutable)); path != "" {
		return resolveBinary(path, env)
	}

	return resolveBinary("opencode", env)
}

// RunACP launches `opencode acp` and wires the provided ACP streams to it.
func RunACP(ctx context.Context, input io.Reader, output io.Writer, stderr io.Writer, options Options) (err error) {
	if ctx == nil {
		return errors.New("context is nil")
	}
	if input == nil {
		return errors.New("input is nil")
	}
	if output == nil {
		return errors.New("output is nil")
	}
	if stderr == nil {
		stderr = io.Discard
	}

	processOptions := options
	if processOptions.IsolateTempDir {
		tempDir, tempErr := mkdirTemp("", processTempPrefix)
		if tempErr != nil {
			return fmt.Errorf("create opencode temp dir: %w", tempErr)
		}
		processOptions.TempDir = tempDir
		defer func() {
			if cleanupErr := removeAll(tempDir); cleanupErr != nil && err == nil {
				err = fmt.Errorf("cleanup opencode temp dir %s: %w", tempDir, cleanupErr)
			}
		}()
	}

	envMap := BuildEnvMap(processOptions)
	path, err := Discover(ctx, processOptions.CLIPath, envMap)
	if err != nil {
		return err
	}

	// Detach exec's context so cancellation uses shutdownProcess's TERM-to-KILL path.
	cmd := commandContext(context.Background(), path, BuildArgs(processOptions)...)
	cmd.Stdout = output
	cmd.Stderr = stderr
	cmd.Env = envMapToSlice(envMap)
	if processOptions.Cwd != "" {
		cmd.Dir = processOptions.Cwd
	}
	configureProcessCommand(cmd)

	closeStdin, stdinEOF, err := configureCommandStdin(cmd, input)
	if err != nil {
		return err
	}
	defer closeStdin()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start opencode acp: %w", err)
	}

	waitErr := make(chan error, 1)
	go func() {
		waitErr <- cmd.Wait()
	}()

	select {
	case err = <-waitErr:
	case <-ctx.Done():
		eofClose := closeStdin
		if !stdinEOF {
			eofClose = nil
		}
		err = errors.Join(ctx.Err(), shutdownProcess(cmd, waitErr, eofClose))
	}

	return err
}

func configureProcessCommand(cmd *exec.Cmd) {
	cmd.WaitDelay = processShutdownWaitDelay
	configureProcessCommandPlatform(cmd)
}

// configureCommandStdin wires input to the child's stdin. The returned bool
// reports whether the returned close function delivers EOF to the child: a
// direct *os.File stdin is inherited by the child, so closing the parent's
// handle has no such effect.
func configureCommandStdin(cmd *exec.Cmd, input io.Reader) (func(), bool, error) {
	if file, ok := input.(*os.File); ok {
		cmd.Stdin = file

		return func() {}, false, nil
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, false, fmt.Errorf("create opencode stdin pipe: %w", err)
	}

	go func() {
		_, _ = io.Copy(stdin, input)
		_ = stdin.Close()
	}()

	return func() {
		_ = stdin.Close()
	}, true, nil
}

// shutdownProcess escalates: stdin EOF → SIGTERM → SIGKILL. closeStdin, when
// non-nil, closes the child's stdin pipe; the grace window after EOF lets the
// process exit on its own so in-flight cleanup (e.g. MCP session termination)
// completes instead of being cut short by a signal.
func shutdownProcess(cmd *exec.Cmd, waitErr <-chan error, closeStdin func()) error {
	if closeStdin != nil && processExitGracePeriod > 0 {
		closeStdin()

		timer := time.NewTimer(processExitGracePeriod)
		defer timer.Stop()

		select {
		case <-waitErr:
			return nil
		case <-timer.C:
		}
	}

	var shutdownErr error
	if err := processTerminate(cmd); err != nil {
		shutdownErr = errors.Join(shutdownErr, err)
	}

	timer := time.NewTimer(processShutdownWaitDelay)
	defer timer.Stop()

	select {
	case <-waitErr:
		return shutdownErr
	case <-timer.C:
		if err := processKill(cmd); err != nil {
			shutdownErr = errors.Join(shutdownErr, err)
		}

		return waitForProcessExit(waitErr, shutdownErr, processShutdownWaitDelay)
	}
}

func waitForProcessExit(waitErr <-chan error, shutdownErr error, timeout time.Duration) error {
	select {
	case <-waitErr:
		return shutdownErr
	case <-time.After(timeout):
		return errors.Join(shutdownErr, errProcessWaitTimeout)
	}
}

func resolveBinary(command string, env map[string]string) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", errors.New("opencode binary path is empty")
	}
	if strings.ContainsRune(command, os.PathSeparator) {
		if err := executableFile(command); err != nil {
			return "", fmt.Errorf("opencode binary %q: %w", command, err)
		}

		return command, nil
	}

	for _, dir := range filepath.SplitList(env["PATH"]) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, command)
		if runtimeGOOS == "windows" && filepath.Ext(candidate) == "" {
			for _, ext := range []string{".exe", ".bat", ".cmd"} {
				if err := executableFile(candidate + ext); err == nil {
					return candidate + ext, nil
				}
			}
		}
		if err := executableFile(candidate); err == nil {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("opencode binary %q not found in effective PATH", command)
}

func executableFile(path string) error {
	info, err := fileStat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return errors.New("is a directory")
	}
	if runtimeGOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return errors.New("is not executable")
	}

	return nil
}

func envMapToSlice(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+env[key])
	}

	return out
}
