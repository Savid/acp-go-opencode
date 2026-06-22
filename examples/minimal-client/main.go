package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"

	"github.com/coder/acp-go-sdk"
	opencodeacp "github.com/savid/acp-go-opencode"
)

type agentConnection interface {
	Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error)
	NewSession(context.Context, acp.NewSessionRequest) (acp.NewSessionResponse, error)
	Prompt(context.Context, acp.PromptRequest) (acp.PromptResponse, error)
	CloseSession(context.Context, acp.CloseSessionRequest) (acp.CloseSessionResponse, error)
}

type startedAgent struct {
	conn  agentConnection
	close func()
	wait  func() error
}

var startAgent = startEmbeddedAgent
var getwd = os.Getwd
var exit = os.Exit
var serveAgent = opencodeacp.Serve

const interruptExitCode = 130

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	prompt := strings.TrimSpace(strings.Join(args, " "))
	if prompt == "" {
		prompt = "Reply with a short hello from ACP."
	}

	cwd, err := getwd()
	if err != nil {
		printError(stderr, err)

		return 1
	}

	agent, err := startAgent(ctx, cwd, stdout, stderr)
	if err != nil {
		printError(stderr, err)

		return 1
	}
	defer func() {
		if agent.close != nil {
			agent.close()
		}
		if agent.wait != nil {
			_ = agent.wait()
		}
	}()

	if err := runConversation(ctx, agent.conn, prompt, cwd, stdout); err != nil {
		if errors.Is(err, context.Canceled) {
			return interruptExitCode
		}
		printError(stderr, err)

		return 1
	}

	return 0
}

func startEmbeddedAgent(ctx context.Context, cwd string, output io.Writer, stderr io.Writer) (*startedAgent, error) {
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

	conn := acp.NewClientSideConnection(&client{output: output}, clientToAgentWriter, agentToClientReader)

	return &startedAgent{
		conn: conn,
		close: func() {
			_ = clientToAgentWriter.Close()
			cancel()
		},
		wait: func() error {
			err := <-done
			_ = clientToAgentReader.Close()
			_ = agentToClientReader.Close()
			if errors.Is(err, context.Canceled) {
				return nil
			}

			return err
		},
	}, nil
}

func runConversation(ctx context.Context, conn agentConnection, prompt string, cwd string, stdout io.Writer) error {
	if _, err := conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientInfo: &acp.Implementation{
			Name:    "acp-go-opencode-minimal-client",
			Version: "example",
		},
	}); err != nil {
		return err
	}

	session, err := conn.NewSession(ctx, opencodeacp.NewSessionRequest(cwd))
	if err != nil {
		return err
	}
	defer func() {
		_, _ = conn.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: session.SessionId})
	}()

	resp, err := conn.Prompt(ctx, opencodeacp.TextPromptRequest(session.SessionId, prompt))
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "\n\nstop reason: %s\n", resp.StopReason)

	return nil
}

func printError(stderr io.Writer, err error) {
	_, _ = fmt.Fprintf(stderr, "minimal-client: %v\n", err)
}

type client struct {
	output   io.Writer
	mu       sync.Mutex
	messages map[string]*messageDisplay
	fallback messageDisplay
	thoughts thoughtDisplay
}

var _ acp.Client = (*client)(nil)
var _ acp.ExtensionMethodHandler = (*client)(nil)

func (c *client) writer() io.Writer {
	if c.output != nil {
		return c.output
	}

	return os.Stdout
}

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

func (c *client) SessionUpdate(_ context.Context, params acp.SessionNotification) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	update := params.Update
	output := c.writer()

	switch {
	case update.AgentMessageChunk != nil && update.AgentMessageChunk.Content.Text != nil:
		chunk := update.AgentMessageChunk
		c.thoughts.endLine(output)
		c.messageDisplay(chunk.MessageId).writeText(output, chunk.Content.Text.Text)
	case update.AgentThoughtChunk != nil && update.AgentThoughtChunk.Content.Text != nil:
		c.thoughts.writeChunk(output, "\n[thought] ", update.AgentThoughtChunk.Content.Text.Text)
	case update.ToolCall != nil:
		c.thoughts.endLine(output)
		fmt.Fprintf(output, "\n[tool] %s %s\n", update.ToolCall.ToolCallId, update.ToolCall.Title)
	case update.ToolCallUpdate != nil:
		c.thoughts.endLine(output)
		status := any(nil)
		if update.ToolCallUpdate.Status != nil {
			status = *update.ToolCallUpdate.Status
		}
		fmt.Fprintf(output, "\n[tool] %s %v\n", update.ToolCallUpdate.ToolCallId, status)
	}

	return nil
}

func (*client) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{TerminalId: "terminal-1"}, nil
}

func (*client) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, nil
}

func (*client) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{Output: "", Truncated: false}, nil
}

func (*client) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, nil
}

func (*client) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, nil
}

func (*client) HandleExtensionMethod(_ context.Context, method string, _ json.RawMessage) (any, error) {
	if strings.HasPrefix(method, "_") {
		return map[string]any{}, nil
	}

	return nil, fmt.Errorf("unexpected extension method %q", method)
}

type messageDisplay struct {
	text string
}

type thoughtDisplay struct {
	text string
	open bool
}

func (m *messageDisplay) writeText(output io.Writer, text string) {
	switch {
	case text == "":
		return
	case m.text == text:
		return
	case strings.HasPrefix(text, m.text):
		fmt.Fprint(output, text[len(m.text):])
		m.text = text
	default:
		fmt.Fprint(output, text)
		m.text += text
	}
}

func (t *thoughtDisplay) writeChunk(output io.Writer, prefix string, chunk string) {
	chunk = normalizeThoughtChunk(chunk)
	if chunk == "" {
		return
	}

	next := t.nextText(chunk)
	if next == t.text {
		return
	}

	delta := next
	if strings.HasPrefix(next, t.text) {
		delta = next[len(t.text):]
	}
	if !t.open {
		fmt.Fprint(output, prefix)
		t.open = true
		delta = next
	}

	fmt.Fprint(output, delta)
	t.text = next
}

func (t *thoughtDisplay) endLine(output io.Writer) {
	if !t.open {
		return
	}

	fmt.Fprintln(output)
	t.text = ""
	t.open = false
}

func (t *thoughtDisplay) nextText(chunk string) string {
	switch {
	case t.text == "":
		return chunk
	case strings.HasPrefix(chunk, t.text):
		return chunk
	case strings.HasPrefix(t.text, chunk):
		return t.text
	default:
		return t.text + thoughtSeparator(t.text, chunk) + chunk
	}
}

func normalizeThoughtChunk(chunk string) string {
	return strings.Join(strings.Fields(chunk), " ")
}

func thoughtSeparator(current string, next string) string {
	if current == "" || next == "" || strings.HasSuffix(current, " ") || strings.HasPrefix(next, " ") {
		return ""
	}
	if shouldJoinThoughtChunk(current, next) {
		return ""
	}

	switch next[0] {
	case '.', ',', '!', '?', ';', ':', '%', ')', ']', '}', '\'', '"':
		return ""
	default:
		return " "
	}
}

func shouldJoinThoughtChunk(current string, next string) bool {
	word := trailingThoughtWord(current)
	if word == "" {
		return false
	}

	word = strings.ToLower(word)
	next = strings.ToLower(next)
	if word == "conc" && next == "is" {
		return true
	}
	if strings.HasSuffix(word, "is") && next == "ely" {
		return true
	}

	if len(word) < 4 {
		return false
	}

	for _, suffix := range []string{
		"s", "ed", "er", "est", "ing", "ly", "ely", "tion", "ions",
		"ment", "ness", "able", "ible", "ally", "ive", "ous", "less", "ful",
	} {
		if next == suffix {
			return true
		}
	}

	return false
}

func trailingThoughtWord(text string) string {
	end := len(text)
	for end > 0 && !isASCIIAlpha(text[end-1]) {
		end--
	}

	start := end
	for start > 0 && isASCIIAlpha(text[start-1]) {
		start--
	}

	return text[start:end]
}

func isASCIIAlpha(value byte) bool {
	return (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z')
}

func (c *client) messageDisplay(messageID *string) *messageDisplay {
	if messageID == nil || *messageID == "" {
		return &c.fallback
	}

	if c.messages == nil {
		c.messages = make(map[string]*messageDisplay)
	}

	display := c.messages[*messageID]
	if display == nil {
		display = &messageDisplay{}
		c.messages[*messageID] = display
	}

	return display
}
