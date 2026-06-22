package opencodeacp

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/savid/acp-go-opencode/internal/opencode"
)

const (
	// DefaultProbeTimeout bounds individual OpenCode compatibility probe commands.
	DefaultProbeTimeout = 15 * time.Second
	// DefaultModelCatalogTTL is the default cache lifetime for ModelCatalog.
	DefaultModelCatalogTTL = 5 * time.Minute
)

var (
	execCommandContext = exec.CommandContext
	probeMkdirTemp     = os.MkdirTemp
	probeRemoveAll     = os.RemoveAll
)

// ProbeDiagnostic describes a non-fatal CLI probe failure.
type ProbeDiagnostic struct {
	// Command is the OpenCode subcommand that failed.
	Command string
	// Error is the command error, including stderr when available.
	Error string
}

// CLIProbe summarizes what the installed OpenCode CLI appears to support.
type CLIProbe struct {
	// Path is the resolved OpenCode executable path.
	Path string
	// Version is trimmed output from `opencode --version`, when available.
	Version string
	// ACPHelp is trimmed output from `opencode acp --help`, when available.
	ACPHelp string
	// ModelsHelp is trimmed output from `opencode models --help`, when available.
	ModelsHelp string
	// SupportsACP is true when `opencode acp --help` exits successfully.
	SupportsACP bool
	// SupportsModels is true when `opencode models --help` exits successfully.
	SupportsModels bool
	// Diagnostics contains non-fatal probe failures.
	Diagnostics []ProbeDiagnostic
}

// ProbeCLI resolves OpenCode and runs small no-token commands to inspect the
// installed CLI. Only executable resolution is fatal; command failures are
// returned as Diagnostics so embedding hosts can degrade gracefully.
func ProbeCLI(ctx context.Context, opts ...Option) (CLIProbe, error) {
	process, err := newOpenCodeCommandProcess(ctx, opts...)
	if err != nil {
		return CLIProbe{}, err
	}
	defer func() {
		_ = process.cleanup()
	}()

	probe := CLIProbe{Path: process.path}
	run := func(command string, args ...string) (string, bool) {
		out, runErr := process.run(ctx, args...)
		if runErr != nil {
			probe.Diagnostics = append(probe.Diagnostics, ProbeDiagnostic{
				Command: command,
				Error:   runErr.Error(),
			})

			return "", false
		}

		return strings.TrimSpace(string(out)), true
	}

	probe.Version, _ = run("--version", "--version")
	probe.ACPHelp, probe.SupportsACP = run("acp --help", "acp", "--help")
	probe.ModelsHelp, probe.SupportsModels = run("models --help", "models", "--help")

	return probe, nil
}

// ResolveOpenCodePath resolves the OpenCode executable using the same
// path/env rules as Serve.
func ResolveOpenCodePath(ctx context.Context, opts ...Option) (string, error) {
	process, err := newOpenCodeCommandProcess(ctx, opts...)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = process.cleanup()
	}()

	return process.path, nil
}

// OpenCodeVersion returns trimmed output from `opencode --version`.
func OpenCodeVersion(ctx context.Context, opts ...Option) (string, error) {
	out, err := RunOpenCodeCommand(ctx, []string{"--version"}, opts...)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(out)), nil
}

// RunOpenCodeCommand runs a short OpenCode CLI command using the same
// executable, environment, cwd, and temp-dir isolation rules as Serve.
func RunOpenCodeCommand(ctx context.Context, args []string, opts ...Option) ([]byte, error) {
	process, err := newOpenCodeCommandProcess(ctx, opts...)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = process.cleanup()
	}()

	return process.run(ctx, args...)
}

type openCodeCommandProcess struct {
	path    string
	env     map[string]string
	cwd     string
	stderr  io.Writer
	cleanup func() error
}

func newOpenCodeCommandProcess(ctx context.Context, opts ...Option) (*openCodeCommandProcess, error) {
	options := applyOptions(opts)
	processOptions := opencodeOptions(options)

	cleanup := func() error { return nil }
	if processOptions.IsolateTempDir {
		tempDir, err := probeMkdirTemp("", "acp-go-opencode-probe-")
		if err != nil {
			return nil, fmt.Errorf("create opencode temp dir: %w", err)
		}
		processOptions.TempDir = tempDir
		cleanup = func() error {
			return probeRemoveAll(tempDir)
		}
	}

	envMap := opencode.BuildEnvMap(processOptions)
	path, err := opencode.Discover(ctx, processOptions.CLIPath, envMap)
	if err != nil {
		_ = cleanup()

		return nil, err
	}

	stderr := options.Stderr
	if stderr == nil {
		stderr = io.Discard
	}

	return &openCodeCommandProcess{
		path:    path,
		env:     envMap,
		cwd:     options.Cwd,
		stderr:  stderr,
		cleanup: cleanup,
	}, nil
}

func (process *openCodeCommandProcess) run(ctx context.Context, args ...string) ([]byte, error) {
	runCtx, cancel := context.WithTimeout(ctx, DefaultProbeTimeout)
	defer cancel()

	cmd := execCommandContext(runCtx, process.path, args...) // #nosec G204 -- caller selects OpenCode path/args.
	cmd.Env = envMapToSlice(process.env)
	if process.cwd != "" {
		cmd.Dir = process.cwd
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.MultiWriter(&stderr, process.stderr)

	err := cmd.Run()
	if err == nil {
		return stdout.Bytes(), nil
	}
	if ctxErr := runCtx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if text := strings.TrimSpace(stderr.String()); text != "" {
		return nil, fmt.Errorf("opencode %s: %w: %s", strings.Join(args, " "), err, text)
	}

	return nil, fmt.Errorf("opencode %s: %w", strings.Join(args, " "), err)
}

// ModelMetadata is normalized metadata from `opencode models --verbose`.
type ModelMetadata struct {
	// ID is provider/model when ProviderID is present, otherwise ModelID.
	ID string
	// ProviderID is the OpenCode provider identifier.
	ProviderID string
	// ModelID is the model identifier within the provider.
	ModelID string
	// Name is the human-readable model name.
	Name string
	// Capabilities contains normalized capability names such as tools,
	// reasoning, attachment, image, audio, pdf, and video.
	Capabilities []string
	// Efforts contains sorted reasoning-effort variants when OpenCode reports them.
	Efforts []string
	// ContextWindow is the reported context window in tokens.
	ContextWindow int
	// MaxOutputTokens is the reported maximum output size in tokens.
	MaxOutputTokens int
}

// OpenCodeModels returns normalized metadata from `opencode models --verbose`.
func OpenCodeModels(ctx context.Context, opts ...Option) ([]ModelMetadata, error) {
	out, err := RunOpenCodeCommand(ctx, []string{"models", "--verbose"}, opts...)
	if err != nil {
		return nil, err
	}

	return ParseModelsVerbose(out)
}

// ModelCatalog caches `opencode models --verbose` metadata for embedding hosts
// that need repeated model discovery.
type ModelCatalog struct {
	// TTL controls how long successful model metadata is cached. When zero,
	// DefaultModelCatalogTTL is used.
	TTL time.Duration

	opts []Option
	mu   sync.Mutex
	// expires and models are guarded by mu.
	expires time.Time
	models  []ModelMetadata
}

// NewModelCatalog constructs a cached OpenCode model catalog.
func NewModelCatalog(opts ...Option) *ModelCatalog {
	return &ModelCatalog{
		TTL:  DefaultModelCatalogTTL,
		opts: append([]Option(nil), opts...),
	}
}

// Load returns cached model metadata, refreshing via OpenCode when the cache is
// empty or expired.
func (catalog *ModelCatalog) Load(ctx context.Context) ([]ModelMetadata, error) {
	if catalog == nil {
		return OpenCodeModels(ctx)
	}

	now := time.Now()
	ttl := catalog.TTL
	if ttl == 0 {
		ttl = DefaultModelCatalogTTL
	}

	catalog.mu.Lock()
	if now.Before(catalog.expires) && catalog.models != nil {
		models := cloneModelMetadataSlice(catalog.models)
		catalog.mu.Unlock()

		return models, nil
	}
	catalog.mu.Unlock()

	models, err := OpenCodeModels(ctx, catalog.opts...)
	if err != nil {
		return nil, err
	}

	catalog.mu.Lock()
	catalog.models = cloneModelMetadataSlice(models)
	catalog.expires = now.Add(ttl)
	catalog.mu.Unlock()

	return cloneModelMetadataSlice(models), nil
}

type verboseModel struct {
	ID           string                    `json:"id"`
	ProviderID   string                    `json:"providerID"` //nolint:tagliatelle // OpenCode emits providerID.
	Name         string                    `json:"name"`
	Capabilities verboseModelCapabilities  `json:"capabilities"`
	Limit        verboseModelLimit         `json:"limit"`
	Variants     map[string]verboseVariant `json:"variants"`
}

type verboseModelCapabilities struct {
	Reasoning  bool `json:"reasoning"`
	ToolCall   bool `json:"toolcall"`
	Attachment bool `json:"attachment"`
	Input      struct {
		Image bool `json:"image"`
		Audio bool `json:"audio"`
		PDF   bool `json:"pdf"`
		Video bool `json:"video"`
	} `json:"input"`
}

type verboseModelLimit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

type verboseVariant struct {
	ReasoningEffort string `json:"reasoningEffort"`
}

// ParseModelsVerbose parses `opencode models --verbose` output. The parser
// scans JSON objects out of mixed output so logs or progress lines do not make
// the helper brittle.
func ParseModelsVerbose(data []byte) ([]ModelMetadata, error) {
	objects := extractJSONObjects(data)
	if len(objects) == 0 {
		return nil, errors.New("opencode verbose model catalog did not contain JSON model objects")
	}

	models := make([]ModelMetadata, 0, len(objects))
	for _, raw := range objects {
		var model verboseModel
		if err := json.Unmarshal(raw, &model); err != nil {
			return nil, fmt.Errorf("parse opencode verbose model metadata: %w", err)
		}
		if model.ID == "" {
			continue
		}

		id := model.ID
		if model.ProviderID != "" {
			id = model.ProviderID + "/" + model.ID
		}
		models = append(models, ModelMetadata{
			ID:              id,
			ProviderID:      model.ProviderID,
			ModelID:         model.ID,
			Name:            model.Name,
			Capabilities:    verboseCapabilities(model.Capabilities),
			Efforts:         verboseEfforts(model.Variants),
			ContextWindow:   model.Limit.Context,
			MaxOutputTokens: model.Limit.Output,
		})
	}
	if len(models) == 0 {
		return nil, errors.New("opencode verbose model catalog did not contain usable model metadata")
	}

	slices.SortStableFunc(models, func(a ModelMetadata, b ModelMetadata) int {
		return cmp.Compare(a.ID, b.ID)
	})

	return models, nil
}

func extractJSONObjects(data []byte) [][]byte {
	var objects [][]byte
	depth := 0
	start := -1
	inString := false
	escaped := false

	for index, value := range data {
		if inString {
			inString, escaped = scanJSONStringByte(value, escaped)

			continue
		}

		switch value {
		case '"':
			inString = true
		case '{':
			if depth == 0 {
				start = index
			}
			depth++
		case '}':
			if depth == 0 {
				continue
			}
			depth--
			if depth == 0 && start >= 0 {
				objects = append(objects, append([]byte(nil), data[start:index+1]...))
				start = -1
			}
		}
	}

	return objects
}

func scanJSONStringByte(value byte, escaped bool) (bool, bool) {
	if escaped {
		return true, false
	}

	switch value {
	case '\\':
		return true, true
	case '"':
		return false, false
	default:
		return true, false
	}
}

func verboseCapabilities(capabilities verboseModelCapabilities) []string {
	var out []string
	if capabilities.ToolCall {
		out = append(out, "tools")
	}
	if capabilities.Reasoning {
		out = append(out, "reasoning")
	}
	if capabilities.Attachment {
		out = append(out, "attachment")
	}
	if capabilities.Input.Image {
		out = append(out, "image")
	}
	if capabilities.Input.Audio {
		out = append(out, "audio")
	}
	if capabilities.Input.PDF {
		out = append(out, "pdf")
	}
	if capabilities.Input.Video {
		out = append(out, "video")
	}
	slices.Sort(out)

	return out
}

func verboseEfforts(variants map[string]verboseVariant) []string {
	if variants == nil {
		return nil
	}

	efforts := make([]string, 0, len(variants))
	seen := make(map[string]struct{}, len(variants))
	for key, variant := range variants {
		effort := strings.TrimSpace(variant.ReasoningEffort)
		if effort == "" {
			effort = strings.TrimSpace(key)
		}
		if effort == "" {
			continue
		}
		if _, ok := seen[effort]; ok {
			continue
		}
		seen[effort] = struct{}{}
		efforts = append(efforts, effort)
	}
	sortEfforts(efforts)

	return efforts
}

func sortEfforts(efforts []string) {
	rank := map[string]int{
		"none":   0,
		"low":    1,
		"medium": 2,
		"high":   3,
		"xhigh":  4,
		"max":    5,
	}
	slices.SortStableFunc(efforts, func(a string, b string) int {
		left, leftOK := rank[a]
		right, rightOK := rank[b]
		if leftOK && rightOK {
			return cmp.Compare(left, right)
		}
		if leftOK {
			return -1
		}
		if rightOK {
			return 1
		}

		return cmp.Compare(a, b)
	})
}

func cloneModelMetadataSlice(models []ModelMetadata) []ModelMetadata {
	if models == nil {
		return nil
	}

	out := make([]ModelMetadata, len(models))
	for i, model := range models {
		model.Capabilities = append([]string(nil), model.Capabilities...)
		model.Efforts = append([]string(nil), model.Efforts...)
		out[i] = model
	}

	return out
}

func envMapToSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for key, value := range env {
		out = append(out, key+"="+value)
	}
	slices.Sort(out)

	return out
}
