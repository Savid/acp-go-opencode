package main

import (
	"bufio"
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
	"golang.org/x/term"
)

type agentConnection interface {
	Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error)
	NewSession(context.Context, acp.NewSessionRequest) (acp.NewSessionResponse, error)
	Prompt(context.Context, acp.PromptRequest) (acp.PromptResponse, error)
	Cancel(context.Context, acp.CancelNotification) error
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
var stdin = io.Reader(os.Stdin)
var isTerminal = term.IsTerminal
var makeTerminalRaw = term.MakeRaw
var restoreTerminal = term.Restore
var rawTerminalInput bool

const interruptExitCode = 130

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	restore, err := configureTerminal(stdin)
	if err != nil {
		printError(os.Stderr, err)
		exit(1)

		return
	}

	code := run(ctx, stdin, os.Stdout, os.Stderr)
	restore()
	exit(code)
}

func run(ctx context.Context, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
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

	if err := runChat(ctx, agent.conn, stdin, cwd, stdout); err != nil {
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

	ui := newChatUI(output)
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

	conn := acp.NewClientSideConnection(&chatClient{ui: ui}, clientToAgentWriter, agentToClientReader)

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

func runChat(ctx context.Context, conn agentConnection, input io.Reader, cwd string, stdout io.Writer) error {
	if _, err := conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientInfo: &acp.Implementation{
			Name:    "acp-go-opencode-interactive-chat",
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

	ui := newChatUI(stdout)
	ui.writeHeader(cwd)

	lines := readInputLines(input, stdout, rawTerminalInput)
	for {
		ui.writePrompt()

		event, ok, err := nextInputLine(ctx, lines)
		if err != nil {
			ui.endThought()

			return err
		}
		if !ok {
			ui.endThought()

			return nil
		}

		line := strings.TrimSpace(event.text)
		switch line {
		case "":
			continue
		case "/exit", "/quit":
			return nil
		case "/cancel":
			if err := conn.Cancel(ctx, acp.CancelNotification{SessionId: session.SessionId}); err != nil {
				return err
			}
			ui.writeNotice("cancel", "sent")
		default:
			ui.writeUserPrompt(line)
			resp, err := conn.Prompt(ctx, opencodeacp.TextPromptRequest(session.SessionId, line))
			if err != nil {
				return err
			}
			ui.writeStopReason(resp.StopReason)
		}
	}
}

type inputLine struct {
	text      string
	err       error
	interrupt bool
}

func configureTerminal(input io.Reader) (func(), error) {
	file, ok := input.(*os.File)
	if !ok {
		return func() {}, nil
	}

	fd := int(file.Fd())
	if !isTerminal(fd) {
		return func() {}, nil
	}

	state, err := makeTerminalRaw(fd)
	if err != nil {
		return nil, fmt.Errorf("configure terminal: %w", err)
	}
	rawTerminalInput = true

	return func() {
		rawTerminalInput = false
		_ = restoreTerminal(fd, state)
	}, nil
}

func readInputLines(input io.Reader, echo io.Writer, echoInput bool) <-chan inputLine {
	lines := make(chan inputLine, 1)

	go func() {
		defer close(lines)

		reader := bufio.NewReader(input)
		var line []rune
		emitLine := func() {
			lines <- inputLine{text: string(line)}
			line = line[:0]
		}
		echoString := func(text string) {
			if echoInput && echo != nil {
				_, _ = io.WriteString(echo, text)
			}
		}

		for {
			value, _, err := reader.ReadRune()
			if err != nil {
				if errors.Is(err, io.EOF) {
					if len(line) > 0 {
						emitLine()
					}

					return
				}

				lines <- inputLine{err: err}

				return
			}

			switch value {
			case '\x03':
				echoString("^C\r\n")
				lines <- inputLine{interrupt: true}

				return
			case '\n':
				echoString("\r\n")
				emitLine()
			case '\r':
				echoString("\r\n")
				emitLine()
			case '\b', '\x7f':
				if len(line) > 0 {
					line = line[:len(line)-1]
					echoString("\b \b")
				}
			default:
				line = append(line, value)
				echoString(string(value))
			}
		}
	}()

	return lines
}

func nextInputLine(ctx context.Context, lines <-chan inputLine) (inputLine, bool, error) {
	select {
	case <-ctx.Done():
		return inputLine{}, false, ctx.Err()
	case event, ok := <-lines:
		if !ok {
			return inputLine{}, false, nil
		}
		if event.err != nil {
			return inputLine{}, false, event.err
		}
		if event.interrupt {
			return inputLine{}, false, context.Canceled
		}

		return event, true, nil
	}
}

func printError(stderr io.Writer, err error) {
	_, _ = fmt.Fprintf(stderr, "interactive-chat: %v\n", err)
}

type chatClient struct {
	ui *chatUI
}

var _ acp.Client = (*chatClient)(nil)
var _ acp.ExtensionMethodHandler = (*chatClient)(nil)

func (*chatClient) ReadTextFile(_ context.Context, params acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	if !filepath.IsAbs(params.Path) {
		return acp.ReadTextFileResponse{}, fmt.Errorf("path must be absolute: %s", params.Path)
	}

	data, err := os.ReadFile(params.Path)
	if err != nil {
		return acp.ReadTextFileResponse{}, err
	}

	return acp.ReadTextFileResponse{Content: string(data)}, nil
}

func (*chatClient) WriteTextFile(_ context.Context, params acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	if !filepath.IsAbs(params.Path) {
		return acp.WriteTextFileResponse{}, fmt.Errorf("path must be absolute: %s", params.Path)
	}

	if err := os.MkdirAll(filepath.Dir(params.Path), 0o755); err != nil {
		return acp.WriteTextFileResponse{}, err
	}

	return acp.WriteTextFileResponse{}, os.WriteFile(params.Path, []byte(params.Content), 0o600)
}

func (c *chatClient) RequestPermission(
	_ context.Context,
	params acp.RequestPermissionRequest,
) (acp.RequestPermissionResponse, error) {
	c.ui.writeNotice("permission", permissionTitle(params))

	for _, option := range params.Options {
		if option.Kind == acp.PermissionOptionKindAllowOnce || option.Kind == acp.PermissionOptionKindAllowAlways {
			return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected(option.OptionId)}, nil
		}
	}

	return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
}

func (c *chatClient) SessionUpdate(_ context.Context, params acp.SessionNotification) error {
	update := params.Update

	switch {
	case update.UserMessageChunk != nil && update.UserMessageChunk.Content.Text != nil:
		c.ui.writeUserPrompt(update.UserMessageChunk.Content.Text.Text)
	case update.AgentMessageChunk != nil && update.AgentMessageChunk.Content.Text != nil:
		chunk := update.AgentMessageChunk
		c.ui.writeAgentText(chunk.MessageId, chunk.Content.Text.Text)
	case update.AgentThoughtChunk != nil && update.AgentThoughtChunk.Content.Text != nil:
		c.ui.writeThought(update.AgentThoughtChunk.Content.Text.Text)
	case update.ToolCall != nil:
		c.ui.writeToolCall(update.ToolCall)
	case update.ToolCallUpdate != nil:
		c.ui.writeToolCallUpdate(update.ToolCallUpdate)
	case update.UsageUpdate != nil:
		c.ui.writeNotice("usage", fmt.Sprintf("%d/%d tokens", update.UsageUpdate.Used, update.UsageUpdate.Size))
	}

	return nil
}

func (*chatClient) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{TerminalId: "terminal-1"}, nil
}

func (*chatClient) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, nil
}

func (*chatClient) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{Output: "", Truncated: false}, nil
}

func (*chatClient) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, nil
}

func (*chatClient) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, nil
}

func (*chatClient) HandleExtensionMethod(_ context.Context, method string, _ json.RawMessage) (any, error) {
	if strings.HasPrefix(method, "_") {
		return map[string]any{}, nil
	}

	return nil, fmt.Errorf("unexpected extension method %q", method)
}

type chatUI struct {
	output    io.Writer
	rawOutput bool
	mu        sync.Mutex
	messages  map[string]*messageDisplay
	fallback  messageDisplay
	thoughts  thoughtDisplay
}

type messageDisplay struct {
	text string
}

type thoughtDisplay struct {
	text string
	open bool
}

func newChatUI(output io.Writer) *chatUI {
	if output == nil {
		output = os.Stdout
	}

	return &chatUI{output: output, rawOutput: rawTerminalInput}
}

func (ui *chatUI) writeHeader(cwd string) {
	ui.endThought()
	ui.writeLine("ACP interactive chat example")
	ui.writeLine(fmt.Sprintf("cwd: %s", cwd))
	ui.writeLine("type /exit or /quit to leave")
}

func (ui *chatUI) writePrompt() {
	ui.endThought()
	ui.writeString("\nmessage> ")
}

func (ui *chatUI) writeUserPrompt(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}

	ui.endThought()
	ui.writeLine(fmt.Sprintf("\nyou> %s", text))
}

func (ui *chatUI) writeAgentText(messageID *string, text string) {
	ui.mu.Lock()
	defer ui.mu.Unlock()

	ui.thoughts.endLine(ui.output)
	ui.messageDisplayLocked(messageID).writeText(ui.output, text)
}

func (ui *chatUI) writeThought(text string) {
	ui.mu.Lock()
	defer ui.mu.Unlock()

	ui.thoughts.writeChunk(ui.output, "\nthinking> ", text)
}

func (ui *chatUI) writeNotice(kind string, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}

	ui.endThought()
	ui.writeLine(fmt.Sprintf("\n%s> %s", kind, text))
}

func (ui *chatUI) writeToolCall(tool *acp.SessionUpdateToolCall) {
	if tool == nil {
		return
	}

	ui.endThought()
	ui.writeLine(fmt.Sprintf("\ntool> %s %s", tool.ToolCallId, tool.Title))
}

func (ui *chatUI) writeToolCallUpdate(tool *acp.SessionToolCallUpdate) {
	if tool == nil {
		return
	}

	status := any(nil)
	if tool.Status != nil {
		status = *tool.Status
	}
	ui.endThought()
	ui.writeLine(fmt.Sprintf("\ntool> %s %v", tool.ToolCallId, status))
}

func (ui *chatUI) writeStopReason(reason acp.StopReason) {
	ui.endThought()
	ui.writeLine(fmt.Sprintf("\nstop reason: %s", reason))
}

func (ui *chatUI) writeLine(line string) {
	ui.writeString(line + "\n")
}

func (ui *chatUI) writeString(text string) {
	ui.mu.Lock()
	defer ui.mu.Unlock()

	if ui.rawOutput {
		text = strings.ReplaceAll(text, "\r\n", "\n")
		text = strings.ReplaceAll(text, "\n", "\r\n")
	}

	_, _ = io.WriteString(ui.output, text)
}

func (ui *chatUI) endThought() {
	ui.mu.Lock()
	defer ui.mu.Unlock()

	ui.thoughts.endLine(ui.output)
}

func (ui *chatUI) messageDisplayLocked(messageID *string) *messageDisplay {
	if messageID == nil || *messageID == "" {
		return &ui.fallback
	}

	if ui.messages == nil {
		ui.messages = make(map[string]*messageDisplay)
	}

	display := ui.messages[*messageID]
	if display == nil {
		display = &messageDisplay{}
		ui.messages[*messageID] = display
	}

	return display
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

func permissionTitle(params acp.RequestPermissionRequest) string {
	if params.ToolCall.Title != nil && strings.TrimSpace(*params.ToolCall.Title) != "" {
		return *params.ToolCall.Title
	}

	return "auto-allowing request"
}
