package opencodeacp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

const helperEnvKey = "ACP_GO_OPENCODE_HELPER_PROCESS"

func TestProbeCLI(t *testing.T) {
	restore := stubOpenCodeCommand(t)
	defer restore()

	probe, err := ProbeCLI(context.Background(), helperOptions()...)
	if err != nil {
		t.Fatalf("ProbeCLI returned error: %v", err)
	}
	if probe.Path != os.Args[0] {
		t.Fatalf("path = %q, want %q", probe.Path, os.Args[0])
	}
	if probe.Version != "opencode 1.2.3" {
		t.Fatalf("version = %q", probe.Version)
	}
	if !probe.SupportsACP || probe.ACPHelp != "ACP help" {
		t.Fatalf("ACP support = %v help=%q", probe.SupportsACP, probe.ACPHelp)
	}
	if !probe.SupportsModels || probe.ModelsHelp != "models help" {
		t.Fatalf("models support = %v help=%q", probe.SupportsModels, probe.ModelsHelp)
	}
	if len(probe.Diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v", probe.Diagnostics)
	}
}

func TestProbeCLIDiagnostics(t *testing.T) {
	restore := stubOpenCodeCommand(t)
	defer restore()

	opts := append(helperOptions(), WithEnv(map[string]string{
		helperEnvKey:              "1",
		"OPENCODEACP_HELPER_MODE": "fail-acp-help",
	}))

	probe, err := ProbeCLI(context.Background(), opts...)
	if err != nil {
		t.Fatalf("ProbeCLI returned error: %v", err)
	}
	if probe.SupportsACP {
		t.Fatal("expected ACP support to be false")
	}
	if len(probe.Diagnostics) != 1 || probe.Diagnostics[0].Command != "acp --help" ||
		!strings.Contains(probe.Diagnostics[0].Error, "acp unavailable") {
		t.Fatalf("diagnostics = %#v", probe.Diagnostics)
	}
}

func TestOpenCodeVersionAndRunCommand(t *testing.T) {
	restore := stubOpenCodeCommand(t)
	defer restore()

	version, err := OpenCodeVersion(context.Background(), helperOptions()...)
	if err != nil {
		t.Fatalf("OpenCodeVersion returned error: %v", err)
	}
	if version != "opencode 1.2.3" {
		t.Fatalf("version = %q", version)
	}

	out, err := RunOpenCodeCommand(context.Background(), []string{"acp", "--help"}, helperOptions()...)
	if err != nil {
		t.Fatalf("RunOpenCodeCommand returned error: %v", err)
	}
	if strings.TrimSpace(string(out)) != "ACP help" {
		t.Fatalf("output = %q", out)
	}

	path, err := ResolveOpenCodePath(context.Background(),
		WithOpenCodePath(os.Args[0]),
		WithEnv(map[string]string{helperEnvKey: "1"}),
	)
	if err != nil {
		t.Fatalf("ResolveOpenCodePath returned error: %v", err)
	}
	if path != os.Args[0] {
		t.Fatalf("resolved path = %q, want %q", path, os.Args[0])
	}
}

func TestOpenCodeCommandProcessErrors(t *testing.T) {
	badPath := filepath.Join(t.TempDir(), "missing-opencode")
	badOpts := []Option{WithOpenCodePath(badPath), WithIsolatedTempDir(false)}

	if _, err := ProbeCLI(context.Background(), badOpts...); err == nil {
		t.Fatal("ProbeCLI missing binary succeeded")
	}
	if _, err := ResolveOpenCodePath(context.Background(), badOpts...); err == nil {
		t.Fatal("ResolveOpenCodePath missing binary succeeded")
	}
	if _, err := OpenCodeVersion(context.Background(), badOpts...); err == nil {
		t.Fatal("OpenCodeVersion missing binary succeeded")
	}

	oldMkdirTemp := probeMkdirTemp
	probeMkdirTemp = func(string, string) (string, error) {
		return "", errors.New("mkdir failed")
	}
	if _, err := ProbeCLI(context.Background(), WithOpenCodePath(os.Args[0])); err == nil ||
		!strings.Contains(err.Error(), "create opencode temp dir") {
		t.Fatalf("ProbeCLI mkdir error = %v", err)
	}
	probeMkdirTemp = oldMkdirTemp

	removed := false
	tempDir := t.TempDir()
	oldRemoveAll := probeRemoveAll
	probeMkdirTemp = func(string, string) (string, error) {
		return tempDir, nil
	}
	probeRemoveAll = func(path string) error {
		if path == tempDir {
			removed = true
		}

		return nil
	}
	_, err := ResolveOpenCodePath(context.Background(), WithOpenCodePath(badPath))
	probeMkdirTemp = oldMkdirTemp
	probeRemoveAll = oldRemoveAll
	if err == nil {
		t.Fatal("ResolveOpenCodePath missing isolated binary succeeded")
	}
	if !removed {
		t.Fatal("temporary directory cleanup was not called")
	}
}

func TestCommandProcessRunErrors(t *testing.T) {
	originalExec := execCommandContext
	execCommandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "exit 6")
	}
	process := &openCodeCommandProcess{
		path: os.Args[0],
		env: map[string]string{
			helperEnvKey:              "1",
			"OPENCODEACP_HELPER_MODE": "fail-version-silent",
		},
		cwd:    ".",
		stderr: io.Discard,
	}
	if _, err := process.run(context.Background(), "--version"); err == nil ||
		!strings.Contains(err.Error(), "opencode --version") {
		t.Fatalf("silent failure error = %v", err)
	}
	execCommandContext = originalExec

	restore := stubOpenCodeCommand(t)
	defer restore()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	process.env["OPENCODEACP_HELPER_MODE"] = ""
	if _, err := process.run(ctx, "--version"); !errors.Is(err, context.Canceled) {
		t.Fatalf("context error = %v, want context.Canceled", err)
	}
}

func TestParseModelsVerbose(t *testing.T) {
	t.Parallel()

	data := []byte(`noise
{"id":"beta","providerID":"anthropic","name":"Beta","capabilities":{"reasoning":true,"toolcall":true,"attachment":true,"input":{"image":true,"pdf":true}},"limit":{"context":200000,"output":8192},"variants":{"high":{"reasoningEffort":"high"},"low":{"reasoningEffort":"low"}}}
{"id":"alpha","providerID":"openai","name":"Alpha","capabilities":{"reasoning":false,"toolcall":true,"input":{"audio":true,"video":true}},"limit":{"context":128000,"output":4096},"variants":{"medium":{}}}
`)

	models, err := ParseModelsVerbose(data)
	if err != nil {
		t.Fatalf("ParseModelsVerbose returned error: %v", err)
	}
	if got := modelIDs(models); !reflect.DeepEqual(got, []string{"anthropic/beta", "openai/alpha"}) {
		t.Fatalf("ids = %#v", got)
	}
	beta := models[0]
	if beta.Name != "Beta" || beta.ContextWindow != 200000 || beta.MaxOutputTokens != 8192 {
		t.Fatalf("beta = %#v", beta)
	}
	if !reflect.DeepEqual(beta.Efforts, []string{"low", "high"}) {
		t.Fatalf("efforts = %#v", beta.Efforts)
	}
	if !reflect.DeepEqual(beta.Capabilities, []string{"attachment", "image", "pdf", "reasoning", "tools"}) {
		t.Fatalf("capabilities = %#v", beta.Capabilities)
	}
}

func TestParseModelsVerboseEdges(t *testing.T) {
	t.Parallel()

	data := []byte(`}{"id":"escaped","name":"A \"quoted\" \\ slash","variants":null}`)
	models, err := ParseModelsVerbose(data)
	if err != nil {
		t.Fatalf("ParseModelsVerbose escaped returned error: %v", err)
	}
	if len(models) != 1 || models[0].ID != "escaped" || models[0].Efforts != nil {
		t.Fatalf("models = %#v", models)
	}

	if _, err := ParseModelsVerbose([]byte(`{bad}`)); err == nil {
		t.Fatal("invalid JSON parse succeeded")
	}

	efforts := verboseEfforts(map[string]verboseVariant{
		"":       {},
		"custom": {},
		"high":   {ReasoningEffort: "high"},
		"copy":   {ReasoningEffort: "high"},
	})
	if !reflect.DeepEqual(efforts, []string{"high", "custom"}) {
		t.Fatalf("efforts = %#v", efforts)
	}

	values := []string{"zeta", "low", "alpha", "medium"}
	sortEfforts(values)
	if !reflect.DeepEqual(values, []string{"low", "medium", "alpha", "zeta"}) {
		t.Fatalf("sorted efforts = %#v", values)
	}
	if cloneModelMetadataSlice(nil) != nil {
		t.Fatal("cloneModelMetadataSlice(nil) returned non-nil")
	}
}

func TestOpenCodeModelsAndCatalog(t *testing.T) {
	var calls atomic.Int64
	restore := stubOpenCodeCommandWithCount(t, &calls)
	defer restore()

	catalog := NewModelCatalog(helperOptions()...)
	models, err := catalog.Load(context.Background())
	if err != nil {
		t.Fatalf("catalog first load returned error: %v", err)
	}
	if len(models) != 1 || models[0].ID != "openai/gpt-test" {
		t.Fatalf("models = %#v", models)
	}
	models[0].Capabilities[0] = "mutated"

	cached, err := catalog.Load(context.Background())
	if err != nil {
		t.Fatalf("catalog second load returned error: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("command calls = %d, want 1", calls.Load())
	}
	if cached[0].Capabilities[0] == "mutated" {
		t.Fatalf("cached result was not cloned: %#v", cached)
	}
}

func TestOpenCodeModelCatalogEdges(t *testing.T) {
	restore := stubOpenCodeCommand(t)
	defer restore()

	t.Setenv("OPENCODE_EXECUTABLE", os.Args[0])
	t.Setenv(helperEnvKey, "1")

	var nilCatalog *ModelCatalog
	models, err := nilCatalog.Load(context.Background())
	if err != nil {
		t.Fatalf("nil catalog Load returned error: %v", err)
	}
	if len(models) != 1 || models[0].ID != "openai/gpt-test" {
		t.Fatalf("nil catalog models = %#v", models)
	}

	zeroTTLCatalog := &ModelCatalog{opts: helperOptions()}
	models, err = zeroTTLCatalog.Load(context.Background())
	if err != nil {
		t.Fatalf("zero TTL catalog Load returned error: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("zero TTL models = %#v", models)
	}

	errorCatalog := NewModelCatalog(append(helperOptions(), WithEnv(map[string]string{
		helperEnvKey:              "1",
		"OPENCODEACP_HELPER_MODE": "fail-models-verbose",
	}))...)
	if _, err := errorCatalog.Load(context.Background()); err == nil {
		t.Fatal("error catalog Load succeeded")
	}
}

func TestOpenCodeModelsErrors(t *testing.T) {
	if _, err := ParseModelsVerbose([]byte("no json")); err == nil {
		t.Fatal("expected parse error")
	}
	if _, err := ParseModelsVerbose([]byte(`{"name":"missing id"}`)); err == nil {
		t.Fatal("expected usable metadata error")
	}

	restore := stubOpenCodeCommand(t)
	defer restore()

	opts := append(helperOptions(), WithEnv(map[string]string{
		helperEnvKey:              "1",
		"OPENCODEACP_HELPER_MODE": "fail-models-verbose",
	}))
	_, err := OpenCodeModels(context.Background(), opts...)
	if err == nil || !strings.Contains(err.Error(), "models failed") {
		t.Fatalf("OpenCodeModels error = %v", err)
	}
}

func modelIDs(models []ModelMetadata) []string {
	out := make([]string, len(models))
	for i, model := range models {
		out[i] = model.ID
	}

	return out
}

func helperOptions() []Option {
	return []Option{
		WithOpenCodePath(os.Args[0]),
		WithIsolatedTempDir(false),
		WithEnv(map[string]string{
			helperEnvKey: "1",
		}),
	}
}

func stubOpenCodeCommand(t *testing.T) func() {
	t.Helper()

	return stubOpenCodeCommandWithCount(t, nil)
}

func stubOpenCodeCommandWithCount(t *testing.T, count *atomic.Int64) func() {
	t.Helper()

	original := execCommandContext
	execCommandContext = func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		if len(args) >= 2 && args[0] == "models" && args[1] == "--verbose" && count != nil {
			count.Add(1)
		}

		helperArgs := append([]string{"-test.run=TestOpenCodeHelperProcess", "--"}, args...)

		return exec.CommandContext(ctx, os.Args[0], helperArgs...) // #nosec G204 -- test helper command.
	}

	return func() {
		execCommandContext = original
	}
}

func TestOpenCodeHelperProcess(t *testing.T) {
	if os.Getenv(helperEnvKey) != "1" {
		return
	}

	args := helperArgs(os.Args)
	mode := os.Getenv("OPENCODEACP_HELPER_MODE")
	if len(args) > 0 {
		switch args[0] {
		case "export":
			if mode == "fail-export" {
				fmt.Fprintln(os.Stderr, "export failed")
				os.Exit(7)
			}
			if mode == "export-no-json" {
				fmt.Println("Exporting session: ses_test")
				os.Exit(0)
			}
			if mode == "export-invalid-json" {
				fmt.Println("Exporting session: ses_test")
				fmt.Println(`{"info":`)
				os.Exit(0)
			}
			sessionID := "ses_latest"
			if len(args) > 1 {
				sessionID = args[1]
			}
			fmt.Printf("Exporting session: %s\n", sessionID)
			fmt.Printf("{\"info\":{\"id\":%q},\"messages\":[]}\n", sessionID)
			os.Exit(0)
		case "import":
			if mode == "fail-import" {
				fmt.Fprintln(os.Stderr, "import failed")
				os.Exit(8)
			}
			if mode == "import-output-error" {
				fmt.Println("Error: Missing key")
				os.Exit(0)
			}
			if len(args) < 2 {
				fmt.Fprintln(os.Stderr, "missing import path")
				os.Exit(2)
			}
			fmt.Printf("Imported %s\n", args[1])
			os.Exit(0)
		case "session":
			if mode == "fail-delete" {
				fmt.Fprintln(os.Stderr, "delete failed")
				os.Exit(9)
			}
			if mode == "delete-output-error" {
				fmt.Println("Error: Session not found")
				os.Exit(0)
			}
			if len(args) == 3 && args[1] == "delete" {
				fmt.Printf("Deleted %s\n", args[2])
				os.Exit(0)
			}
		}
	}
	switch strings.Join(args, " ") {
	case "--version":
		if mode == "fail-version-silent" {
			os.Exit(6)
		}
		fmt.Println("opencode 1.2.3")
	case "acp --help":
		if mode == "fail-acp-help" {
			fmt.Fprintln(os.Stderr, "acp unavailable")
			os.Exit(4)
		}
		fmt.Println("ACP help")
	case "models --help":
		fmt.Println("models help")
	case "models --verbose":
		if mode == "fail-models-verbose" {
			fmt.Fprintln(os.Stderr, "models failed")
			os.Exit(5)
		}
		fmt.Println(`{"id":"gpt-test","providerID":"openai","name":"GPT Test","capabilities":{"toolcall":true},"limit":{"context":100,"output":10},"variants":{"low":{"reasoningEffort":"low"}}}`)
	default:
		fmt.Fprintf(os.Stderr, "unexpected args %q\n", strings.Join(args, " "))
		os.Exit(2)
	}

	os.Exit(0)
}

func helperArgs(args []string) []string {
	for i, arg := range args {
		if arg == "--" {
			return args[i+1:]
		}
	}

	panic(errors.New("missing helper arg separator"))
}
