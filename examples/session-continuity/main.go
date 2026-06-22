package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/acp-go-sdk"
	opencodeacp "github.com/savid/acp-go-opencode"
)

const defaultSessionFile = "session.json"

type sessionConnection interface {
	Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error)
	NewSession(context.Context, acp.NewSessionRequest) (acp.NewSessionResponse, error)
	SetSessionConfigOption(context.Context, acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error)
	ListSessions(context.Context, acp.ListSessionsRequest) (acp.ListSessionsResponse, error)
	LoadSession(context.Context, acp.LoadSessionRequest) (acp.LoadSessionResponse, error)
	ResumeSession(context.Context, acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error)
	UnstableForkSession(context.Context, acp.UnstableForkSessionRequest) (acp.UnstableForkSessionResponse, error)
	Prompt(context.Context, acp.PromptRequest) (acp.PromptResponse, error)
	CloseSession(context.Context, acp.CloseSessionRequest) (acp.CloseSessionResponse, error)
}

type startedAgent struct {
	conn  sessionConnection
	close func()
	wait  func() error
}

type lifecycleResult struct {
	sessionID acp.SessionId
	forkID    acp.SessionId
}

type config struct {
	cwd         string
	prompt      string
	sessionFile string
}

var getwd = os.Getwd
var exit = os.Exit
var input io.Reader = os.Stdin
var args = os.Args[1:]
var readFile = os.ReadFile
var absPath = filepath.Abs
var runtimeCaller = runtime.Caller
var nextID = newOpenCodeID
var startAgent = startEmbeddedAgent
var serveAgent = opencodeacp.Serve
var exportSession = opencodeacp.ExportSession
var importSessionFile = opencodeacp.ImportSessionFile
var importSaved = importSavedSession
var deleteSession = opencodeacp.DeleteSession
var mkdirTemp = os.MkdirTemp
var removeAll = os.RemoveAll
var writeFile = os.WriteFile

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	exit(run(ctx, os.Stdout, os.Stderr))
}

func defaultSessionFilePath() string {
	_, file, _, ok := runtimeCaller(0)
	if !ok {
		return defaultSessionFile
	}

	return filepath.Join(filepath.Dir(file), defaultSessionFile)
}

func run(ctx context.Context, stdout io.Writer, stderr io.Writer) int {
	cfg, err := parseConfig(args, stderr)
	if err != nil {
		printError(stderr, err)

		return 1
	}

	agent, err := startAgent(ctx, cfg.cwd, stdout, stderr)
	if err != nil {
		printError(stderr, err)

		return 1
	}

	result, err := runSessionContinuity(ctx, agent.conn, cfg.cwd, stdout)
	stopAgent(agent)
	if err != nil {
		printError(stderr, err)

		return 1
	}

	opts := sessionCLIOptions(cfg.cwd, stderr)
	if result.forkID != "" {
		defer func() {
			_ = deleteSession(context.Background(), result.forkID, opts...)
		}()
	}
	if err := runSessionFileContinuity(ctx, result.sessionID, stdout, opts...); err != nil {
		printError(stderr, err)

		return 1
	}
	defer func() {
		_ = deleteSession(context.Background(), result.sessionID, opts...)
	}()

	savedSessionID, err := importSaved(ctx, cfg.sessionFile, cfg.cwd, stdout, opts...)
	if err != nil {
		printError(stderr, err)

		return 1
	}
	defer func() {
		_ = deleteSession(context.Background(), savedSessionID, opts...)
	}()

	agent, err = startAgent(ctx, cfg.cwd, stdout, stderr)
	if err != nil {
		printError(stderr, err)

		return 1
	}
	defer stopAgent(agent)

	if err := runImportedSessionContinuity(ctx, agent.conn, cfg.cwd, savedSessionID, cfg.prompt, input, stdout); err != nil {
		if ctx.Err() != nil {
			printError(stderr, ctx.Err())

			return 130
		}

		printError(stderr, err)

		return 1
	}

	return 0
}

func parseConfig(arguments []string, stderr io.Writer) (config, error) {
	cfg := config{sessionFile: defaultSessionFilePath()}
	flags := flag.NewFlagSet("session-continuity", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&cfg.cwd, "cwd", "", "absolute working directory for the OpenCode session")
	flags.StringVar(&cfg.prompt, "prompt", "", "optional prompt to send after imported session/resume")
	flags.StringVar(&cfg.sessionFile, "session-file", cfg.sessionFile, "OpenCode session JSON fixture to import and resume")

	if err := flags.Parse(arguments); err != nil {
		return config{}, err
	}

	if positionalPrompt := strings.TrimSpace(strings.Join(flags.Args(), " ")); positionalPrompt != "" {
		cfg.prompt = positionalPrompt
	}

	if cfg.cwd == "" {
		cwd, err := getwd()
		if err != nil {
			return config{}, err
		}

		cfg.cwd = cwd
	}

	if !filepath.IsAbs(cfg.cwd) {
		return config{}, fmt.Errorf("cwd must be absolute: %s", cfg.cwd)
	}

	if !filepath.IsAbs(cfg.sessionFile) {
		sessionFile, err := absPath(cfg.sessionFile)
		if err != nil {
			return config{}, err
		}

		cfg.sessionFile = sessionFile
	}

	return cfg, nil
}

func sessionCLIOptions(cwd string, _ io.Writer) []opencodeacp.Option {
	return []opencodeacp.Option{
		opencodeacp.WithCwd(cwd),
		opencodeacp.WithPure(true),
		opencodeacp.WithStderr(io.Discard),
	}
}

func stopAgent(agent *startedAgent) {
	if agent.close != nil {
		agent.close()
	}
	if agent.wait != nil {
		_ = agent.wait()
	}
}

func runSessionContinuity(
	ctx context.Context,
	conn sessionConnection,
	cwd string,
	stdout io.Writer,
) (lifecycleResult, error) {
	if _, err := conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientInfo: &acp.Implementation{
			Name:    "acp-go-opencode-session-continuity",
			Version: "example",
		},
	}); err != nil {
		return lifecycleResult{}, err
	}

	session, err := conn.NewSession(ctx, opencodeacp.NewSessionRequest(cwd))
	if err != nil {
		return lifecycleResult{}, err
	}

	if _, err := conn.SetSessionConfigOption(
		ctx,
		opencodeacp.SetModeConfigRequest(session.SessionId, opencodeacp.OpenCodeModeBuild),
	); err != nil {
		_, _ = conn.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: session.SessionId})

		return lifecycleResult{}, err
	}

	list, err := conn.ListSessions(ctx, opencodeacp.ListSessionsRequest(opencodeacp.WithListSessionsCwd(cwd)))
	if err != nil {
		_, _ = conn.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: session.SessionId})

		return lifecycleResult{}, err
	}
	if _, err := conn.LoadSession(ctx, opencodeacp.LoadSessionRequest(session.SessionId, cwd)); err != nil {
		_, _ = conn.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: session.SessionId})

		return lifecycleResult{}, err
	}
	if _, err := conn.ResumeSession(ctx, opencodeacp.ResumeSessionRequest(session.SessionId, cwd)); err != nil {
		_, _ = conn.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: session.SessionId})

		return lifecycleResult{}, err
	}
	fork, err := conn.UnstableForkSession(ctx, opencodeacp.ForkSessionRequest(session.SessionId, cwd))
	if err != nil {
		_, _ = conn.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: session.SessionId})

		return lifecycleResult{}, err
	}
	_, _ = conn.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: fork.SessionId})
	_, _ = conn.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: session.SessionId})

	fmt.Fprintf(stdout, "session: %s\n", session.SessionId)
	fmt.Fprintf(stdout, "listed sessions: %d\n", len(list.Sessions))
	fmt.Fprintf(stdout, "fork: %s\n", fork.SessionId)

	return lifecycleResult{
		sessionID: session.SessionId,
		forkID:    fork.SessionId,
	}, nil
}

func runSessionFileContinuity(
	ctx context.Context,
	sessionID acp.SessionId,
	stdout io.Writer,
	opts ...opencodeacp.Option,
) error {
	exported, err := exportSession(ctx, sessionID, opts...)
	if err != nil {
		return err
	}
	if exportedID, err := opencodeacp.SessionIDFromExport(exported); err != nil {
		return err
	} else if exportedID != sessionID {
		return fmt.Errorf("exported session id %s did not match %s", exportedID, sessionID)
	}

	tempDir, err := mkdirTemp("", "acp-go-opencode-session-continuity-")
	if err != nil {
		return err
	}
	defer func() {
		_ = removeAll(tempDir)
	}()

	exportPath := filepath.Join(tempDir, "session.json")
	if err := writeFile(exportPath, exported, 0o600); err != nil {
		return err
	}
	if err := deleteSession(ctx, sessionID, opts...); err != nil {
		return err
	}
	if _, err := importSessionFile(ctx, exportPath, opts...); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "exported bytes: %d\n", len(exported))
	fmt.Fprintf(stdout, "reimported session: %s\n", sessionID)

	return nil
}

func importSavedSession(
	ctx context.Context,
	sessionFile string,
	cwd string,
	stdout io.Writer,
	opts ...opencodeacp.Option,
) (acp.SessionId, error) {
	exported, err := readFile(sessionFile)
	if err != nil {
		return "", fmt.Errorf("open saved session file %s: %w", sessionFile, err)
	}

	generated, sessionID, err := rewriteSavedSessionExport(exported, cwd)
	if err != nil {
		return "", err
	}

	tempDir, err := mkdirTemp("", "acp-go-opencode-saved-session-")
	if err != nil {
		return "", err
	}
	defer func() {
		_ = removeAll(tempDir)
	}()

	importPath := filepath.Join(tempDir, "session.json")
	if err := writeFile(importPath, generated, 0o600); err != nil {
		return "", err
	}

	if _, err := importSessionFile(ctx, importPath, opts...); err != nil {
		return "", err
	}

	fmt.Fprintf(stdout, "saved session file: %s\n", sessionFile)
	fmt.Fprintf(stdout, "imported saved session: %s\n", sessionID)

	return sessionID, nil
}

type savedSessionExport struct {
	Info     map[string]any        `json:"info"`
	Messages []savedSessionMessage `json:"messages"`
}

type savedSessionMessage struct {
	Info  map[string]any   `json:"info"`
	Parts []map[string]any `json:"parts"`
}

func rewriteSavedSessionExport(data []byte, cwd string) ([]byte, acp.SessionId, error) {
	var exported savedSessionExport
	if err := json.Unmarshal(data, &exported); err != nil {
		return nil, "", fmt.Errorf("parse saved session JSON: %w", err)
	}
	if exported.Info == nil {
		return nil, "", errors.New("saved session JSON missing info")
	}

	oldSessionID, ok := exported.Info["id"].(string)
	if !ok || strings.TrimSpace(oldSessionID) == "" {
		return nil, "", errors.New("saved session JSON missing info.id")
	}

	newSessionID := acp.SessionId(nextID("ses_"))
	replacements := map[string]string{oldSessionID: string(newSessionID)}

	for _, message := range exported.Messages {
		if message.Info != nil {
			oldMessageID, ok := message.Info["id"].(string)
			if ok && strings.TrimSpace(oldMessageID) != "" {
				replacements[oldMessageID] = nextID("msg_")
			}
		}
		for _, part := range message.Parts {
			oldPartID, ok := part["id"].(string)
			if ok && strings.TrimSpace(oldPartID) != "" {
				replacements[oldPartID] = nextID("prt_")
			}
		}
	}

	replaceIDs(exported.Info, replacements)
	exported.Info["id"] = string(newSessionID)
	exported.Info["directory"] = cwd
	exported.Info["path"] = openCodePath(cwd)

	for index := range exported.Messages {
		message := &exported.Messages[index]
		if message.Info == nil {
			message.Info = map[string]any{}
		}
		replaceIDs(message.Info, replacements)
		message.Info["sessionID"] = string(newSessionID)
		if path, ok := message.Info["path"].(map[string]any); ok {
			path["cwd"] = cwd
		}
		for partIndex := range message.Parts {
			part := message.Parts[partIndex]
			replaceIDs(part, replacements)
			part["sessionID"] = string(newSessionID)
		}
	}

	rewritten, _ := json.MarshalIndent(exported, "", "  ")

	return append(rewritten, '\n'), newSessionID, nil
}

func replaceIDs(value any, replacements map[string]string) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if text, ok := child.(string); ok {
				if replacement, exists := replacements[text]; exists {
					typed[key] = replacement
				}

				continue
			}

			replaceIDs(child, replacements)
		}
	case []any:
		for _, child := range typed {
			replaceIDs(child, replacements)
		}
	}
}

var idCounter uint64

func newOpenCodeID(prefix string) string {
	count := atomic.AddUint64(&idCounter, 1)

	return fmt.Sprintf("%sacpgo%x%x", prefix, time.Now().UnixNano(), count)
}

func openCodePath(cwd string) string {
	return strings.TrimPrefix(filepath.ToSlash(filepath.Clean(cwd)), "/")
}

func runImportedSessionContinuity(
	ctx context.Context,
	conn sessionConnection,
	cwd string,
	sessionID acp.SessionId,
	prompt string,
	stdin io.Reader,
	stdout io.Writer,
) error {
	if _, err := conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientInfo: &acp.Implementation{
			Name:    "acp-go-opencode-session-continuity-imported",
			Version: "example",
		},
	}); err != nil {
		return err
	}

	list, err := conn.ListSessions(ctx, opencodeacp.ListSessionsRequest(opencodeacp.WithListSessionsCwd(cwd)))
	if err != nil {
		return err
	}
	if !sessionListed(list, sessionID) {
		return fmt.Errorf("imported session %s was not listed", sessionID)
	}
	if _, err := conn.LoadSession(ctx, opencodeacp.LoadSessionRequest(sessionID, cwd)); err != nil {
		return err
	}
	if _, err := conn.ResumeSession(ctx, opencodeacp.ResumeSessionRequest(sessionID, cwd)); err != nil {
		return err
	}
	defer func() {
		_, _ = conn.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: sessionID})
	}()

	fmt.Fprintf(stdout, "\nloaded imported session: %s\n", sessionID)
	fmt.Fprintf(stdout, "resumed imported session: %s\n", sessionID)

	if strings.TrimSpace(prompt) == "" {
		typedPrompt, err := readTypedPrompt(ctx, stdin, stdout)
		if errors.Is(err, errPromptInterrupted) {
			fmt.Fprintln(stdout, "\ninterrupted; closing session")

			return nil
		}
		if err != nil {
			return err
		}
		if typedPrompt == "" {
			fmt.Fprintln(stdout, "\nno typed prompt entered; closing session")

			return nil
		}

		prompt = typedPrompt
		fmt.Fprintln(stdout, "\n== typed prompt ==")
	} else {
		fmt.Fprintln(stdout, "== resumed prompt ==")
	}

	resp, err := promptSession(ctx, conn, sessionID, prompt)
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "\n\nprompt stop reason: %s\n", resp.StopReason)

	return nil
}

func promptSession(
	ctx context.Context,
	conn sessionConnection,
	sessionID acp.SessionId,
	prompt string,
) (acp.PromptResponse, error) {
	return conn.Prompt(ctx, opencodeacp.TextPromptRequest(sessionID, prompt))
}

var errPromptInterrupted = errors.New("typed prompt interrupted")

type typedPromptResult struct {
	text string
	err  error
}

func readTypedPrompt(ctx context.Context, stdin io.Reader, stdout io.Writer) (string, error) {
	fmt.Fprint(stdout, "\nenter one prompt (blank or Ctrl-C to exit): ")

	select {
	case <-ctx.Done():
		return "", errPromptInterrupted
	default:
	}

	result := make(chan typedPromptResult, 1)

	go func() {
		text, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			result <- typedPromptResult{err: err}

			return
		}

		result <- typedPromptResult{text: strings.TrimSpace(text)}
	}()

	select {
	case <-ctx.Done():
		return "", errPromptInterrupted
	case got := <-result:
		return got.text, got.err
	}
}

func sessionListed(list acp.ListSessionsResponse, sessionID acp.SessionId) bool {
	for _, session := range list.Sessions {
		if session.SessionId == sessionID {
			return true
		}
	}

	return false
}

func startEmbeddedAgent(
	ctx context.Context,
	cwd string,
	stdout io.Writer,
	stderr io.Writer,
) (*startedAgent, error) {
	clientToAgentReader, clientToAgentWriter := io.Pipe()
	agentToClientReader, agentToClientWriter := io.Pipe()
	serveCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)

	go func() {
		defer func() {
			_ = agentToClientWriter.Close()
		}()

		done <- serveAgent(serveCtx, clientToAgentReader, agentToClientWriter,
			opencodeacp.WithCwd(cwd),
			opencodeacp.WithPure(true),
			opencodeacp.WithHostname("127.0.0.1"),
			opencodeacp.WithPort(0),
			opencodeacp.WithStderr(stderr),
		)
	}()

	conn := acp.NewClientSideConnection(&client{output: stdout}, clientToAgentWriter, agentToClientReader)

	closeAgent := func() {
		_ = clientToAgentWriter.Close()
		cancel()
	}
	waitAgent := func() error {
		err := <-done
		_ = clientToAgentReader.Close()
		_ = agentToClientReader.Close()
		if errors.Is(err, context.Canceled) {
			return nil
		}

		return err
	}

	return &startedAgent{conn: conn, close: closeAgent, wait: waitAgent}, nil
}

type client struct {
	output io.Writer
	mu     sync.Mutex
	stream string
}

var _ acp.Client = (*client)(nil)
var _ acp.ExtensionMethodHandler = (*client)(nil)

func (*client) ReadTextFile(_ context.Context, params acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	if !filepath.IsAbs(params.Path) {
		return acp.ReadTextFileResponse{}, fmt.Errorf("path must be absolute: %s", params.Path)
	}

	data, err := os.ReadFile(params.Path)
	if err != nil {
		return acp.ReadTextFileResponse{}, err
	}

	return acp.ReadTextFileResponse{Content: string(data)}, nil
}

func (*client) WriteTextFile(_ context.Context, params acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	if !filepath.IsAbs(params.Path) {
		return acp.WriteTextFileResponse{}, fmt.Errorf("path must be absolute: %s", params.Path)
	}
	if err := os.MkdirAll(filepath.Dir(params.Path), 0o755); err != nil {
		return acp.WriteTextFileResponse{}, err
	}

	return acp.WriteTextFileResponse{}, os.WriteFile(params.Path, []byte(params.Content), 0o600)
}

func (*client) RequestPermission(
	_ context.Context,
	params acp.RequestPermissionRequest,
) (acp.RequestPermissionResponse, error) {
	for _, option := range params.Options {
		if option.Kind == acp.PermissionOptionKindAllowOnce || option.Kind == acp.PermissionOptionKindAllowAlways {
			return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected(option.OptionId)}, nil
		}
	}

	return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
}

func (c *client) writer() io.Writer {
	if c.output != nil {
		return c.output
	}

	return os.Stdout
}

func (c *client) SessionUpdate(_ context.Context, params acp.SessionNotification) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	update := params.Update
	output := c.writer()

	switch {
	case update.UserMessageChunk != nil && update.UserMessageChunk.Content.Text != nil:
		text := update.UserMessageChunk.Content.Text.Text
		if text != "" {
			c.endStream(output)
			fmt.Fprintf(output, "\n[user] %s\n", text)
		}
	case update.AgentMessageChunk != nil && update.AgentMessageChunk.Content.Text != nil:
		text := update.AgentMessageChunk.Content.Text.Text
		if text != "" {
			c.startStream(output, "agent", "\n")
			fmt.Fprint(output, text)
		}
	case update.AgentThoughtChunk != nil && update.AgentThoughtChunk.Content.Text != nil:
		text := update.AgentThoughtChunk.Content.Text.Text
		if text != "" {
			c.startStream(output, "thought", "\n[thought] ")
			fmt.Fprint(output, text)
		}
	case update.ToolCall != nil:
		c.endStream(output)
		fmt.Fprintf(output, "\n[tool] %s %s\n", update.ToolCall.ToolCallId, update.ToolCall.Title)
	case update.ToolCallUpdate != nil && update.ToolCallUpdate.Status != nil:
		c.endStream(output)
		fmt.Fprintf(output, "\n[tool] %s %s\n", update.ToolCallUpdate.ToolCallId, *update.ToolCallUpdate.Status)
	}

	return nil
}

func (c *client) startStream(output io.Writer, stream string, prefix string) {
	if c.stream == stream {
		return
	}
	if c.stream != "" {
		fmt.Fprintln(output)
	}
	fmt.Fprint(output, prefix)
	c.stream = stream
}

func (c *client) endStream(output io.Writer) {
	if c.stream == "" {
		return
	}
	fmt.Fprintln(output)
	c.stream = ""
}

func (*client) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{TerminalId: "terminal-1"}, nil
}

func (*client) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, nil
}

func (*client) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{Output: ""}, nil
}

func (*client) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, nil
}

func (*client) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, nil
}

func (*client) HandleExtensionMethod(_ context.Context, method string, params json.RawMessage) (any, error) {
	if !strings.HasPrefix(method, "_") {
		return nil, fmt.Errorf("unexpected extension method %q", method)
	}
	if len(params) == 0 {
		return map[string]any{}, nil
	}

	var decoded any
	if err := json.Unmarshal(params, &decoded); err != nil {
		return nil, err
	}

	return map[string]any{}, nil
}

func printError(stderr io.Writer, err error) {
	_, _ = fmt.Fprintf(stderr, "session-continuity: %v\n", err)
}
