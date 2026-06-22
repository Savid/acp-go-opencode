package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	opencodeacp "github.com/savid/acp-go-opencode"
	"golang.org/x/term"
)

func TestRunChat(t *testing.T) {
	conn := &fakeAgentConnection{}
	var output bytes.Buffer

	err := runChat(context.Background(), conn, strings.NewReader("hello\n/cancel\n/quit\n"), "/repo", &output)
	if err != nil {
		t.Fatalf("runChat returned error: %v", err)
	}
	if !conn.initializedSnapshot() || conn.cwdSnapshot() != "/repo" || !conn.closedSnapshot() {
		t.Fatalf("conn state = %#v", conn)
	}
	if prompts := conn.promptsSnapshot(); len(prompts) != 1 || prompts[0] != "hello" {
		t.Fatalf("prompts = %#v", prompts)
	}
	if cancelled := conn.cancelledSnapshot(); len(cancelled) != 1 || cancelled[0] != "s" {
		t.Fatalf("cancelled = %#v", cancelled)
	}
	if got := output.String(); !strings.Contains(got, "ACP interactive chat example") ||
		!strings.Contains(got, "stop reason: end_turn") {
		t.Fatalf("output = %q", got)
	}

	if err := runChat(context.Background(), &fakeAgentConnection{}, strings.NewReader(""), "/repo", io.Discard); err != nil {
		t.Fatalf("runChat EOF returned error: %v", err)
	}
	if err := runChat(context.Background(), &fakeAgentConnection{}, strings.NewReader("\n/quit\n"), "/repo", io.Discard); err != nil {
		t.Fatalf("runChat empty line returned error: %v", err)
	}
}

func TestRunChatErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		conn  *fakeAgentConnection
		input string
	}{
		"initialize": {conn: &fakeAgentConnection{initErr: errors.New("init failed")}},
		"new":        {conn: &fakeAgentConnection{newErr: errors.New("new failed")}},
		"prompt":     {conn: &fakeAgentConnection{promptErr: errors.New("prompt failed")}, input: "hello\n"},
		"cancel":     {conn: &fakeAgentConnection{cancelErr: errors.New("cancel failed")}, input: "/cancel\n"},
	} {
		input := tc.input
		if input == "" {
			input = "/quit\n"
		}
		if err := runChat(context.Background(), tc.conn, strings.NewReader(input), "/repo", io.Discard); err == nil {
			t.Fatalf("%s error path succeeded", name)
		}
	}

	if err := runChat(context.Background(), &fakeAgentConnection{}, errReader{}, "/repo", io.Discard); err == nil {
		t.Fatal("scanner error path succeeded")
	}
}

func TestRunChatReturnsOnContextCancellationWhileWaitingForInput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	inputReader, inputWriter := io.Pipe()
	t.Cleanup(func() {
		_ = inputReader.Close()
		_ = inputWriter.Close()
	})

	done := make(chan error, 1)
	go func() {
		done <- runChat(ctx, &fakeAgentConnection{}, inputReader, "/repo", io.Discard)
	}()

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runChat error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runChat did not return after context cancellation")
	}
}

func TestRunChatReturnsOnControlCByte(t *testing.T) {
	err := runChat(context.Background(), &fakeAgentConnection{}, strings.NewReader("\x03"), "/repo", io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runChat error = %v, want context canceled", err)
	}
}

func TestRunUsesInjectedAgent(t *testing.T) {
	originalStart := startAgent
	originalGetwd := getwd
	originalExit := exit
	originalStdin := stdin
	t.Cleanup(func() {
		startAgent = originalStart
		getwd = originalGetwd
		exit = originalExit
		stdin = originalStdin
	})

	conn := &fakeAgentConnection{}
	startAgent = func(_ context.Context, cwd string, _ io.Writer, _ io.Writer) (*startedAgent, error) {
		conn.startedCwd = cwd

		return &startedAgent{
			conn:  conn,
			close: func() { conn.closedStarter = true },
			wait:  func() error { conn.waited = true; return nil },
		}, nil
	}
	getwd = func() (string, error) { return "/repo", nil }

	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), strings.NewReader("hello\n/quit\n"), &stdout, &stderr); code != 0 {
		t.Fatalf("run code = %d stderr=%q", code, stderr.String())
	}
	if conn.startedCwd != "/repo" || !conn.closedStarter || !conn.waited {
		t.Fatalf("conn state = %#v", conn)
	}

	exitCode := -1
	exit = func(code int) { exitCode = code }
	if code := run(context.Background(), strings.NewReader("/quit\n"), &stdout, &stderr); code != 0 {
		t.Fatalf("second run code = %d stderr=%q", code, stderr.String())
	}
	stdin = strings.NewReader("/quit\n")
	main()
	if exitCode != 0 {
		t.Fatalf("main exit=%d", exitCode)
	}
}

func TestMainConfigureTerminalError(t *testing.T) {
	originalExit := exit
	originalStdin := stdin
	originalIsTerminal := isTerminal
	originalMakeRaw := makeTerminalRaw
	t.Cleanup(func() {
		exit = originalExit
		stdin = originalStdin
		isTerminal = originalIsTerminal
		makeTerminalRaw = originalMakeRaw
	})

	file, err := os.CreateTemp(t.TempDir(), "stdin-*")
	if err != nil {
		t.Fatalf("create temp stdin: %v", err)
	}
	defer func() { _ = file.Close() }()

	exitCode := -1
	exit = func(code int) { exitCode = code }
	stdin = file
	isTerminal = func(int) bool { return true }
	makeTerminalRaw = func(int) (*term.State, error) {
		return nil, errors.New("raw failed")
	}

	main()
	if exitCode != 1 {
		t.Fatalf("main exit = %d, want 1", exitCode)
	}
}

func TestRunErrors(t *testing.T) {
	originalStart := startAgent
	originalGetwd := getwd
	t.Cleanup(func() {
		startAgent = originalStart
		getwd = originalGetwd
	})

	getwd = func() (string, error) { return "", errors.New("cwd failed") }
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), strings.NewReader(""), &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "cwd failed") {
		t.Fatalf("cwd failure code=%d stderr=%q", code, stderr.String())
	}

	getwd = func() (string, error) { return "/repo", nil }
	startAgent = func(context.Context, string, io.Writer, io.Writer) (*startedAgent, error) {
		return nil, errors.New("start failed")
	}
	stderr.Reset()
	if code := run(context.Background(), strings.NewReader(""), &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "start failed") {
		t.Fatalf("start failure code=%d stderr=%q", code, stderr.String())
	}

	startAgent = func(context.Context, string, io.Writer, io.Writer) (*startedAgent, error) {
		return &startedAgent{conn: &fakeAgentConnection{promptErr: errors.New("prompt failed")}}, nil
	}
	stderr.Reset()
	if code := run(context.Background(), strings.NewReader("hello\n"), &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "prompt failed") {
		t.Fatalf("prompt failure code=%d stderr=%q", code, stderr.String())
	}

	startAgent = func(context.Context, string, io.Writer, io.Writer) (*startedAgent, error) {
		return &startedAgent{conn: &fakeAgentConnection{promptErr: context.Canceled}}, nil
	}
	stderr.Reset()
	if code := run(context.Background(), strings.NewReader("hello\n"), &stdout, &stderr); code != interruptExitCode ||
		stderr.String() != "" {
		t.Fatalf("cancel failure code=%d stderr=%q", code, stderr.String())
	}

	startAgent = func(context.Context, string, io.Writer, io.Writer) (*startedAgent, error) {
		return &startedAgent{conn: &fakeAgentConnection{}}, nil
	}
	stderr.Reset()
	if code := run(context.Background(), strings.NewReader("\x03"), &stdout, &stderr); code != interruptExitCode ||
		stderr.String() != "" {
		t.Fatalf("ctrl-c byte code=%d stderr=%q", code, stderr.String())
	}
}

func TestChatClientAndUI(t *testing.T) {
	var output bytes.Buffer
	ui := newChatUI(&output)
	c := &chatClient{ui: ui}

	if newChatUI(nil).output == nil {
		t.Fatal("nil output UI returned nil writer")
	}

	dir := t.TempDir()
	file := filepath.Join(dir, "nested", "file.txt")
	if _, err := c.WriteTextFile(context.Background(), acp.WriteTextFileRequest{Path: file, Content: "body"}); err != nil {
		t.Fatalf("WriteTextFile returned error: %v", err)
	}
	read, err := c.ReadTextFile(context.Background(), acp.ReadTextFileRequest{Path: file})
	if err != nil || read.Content != "body" {
		t.Fatalf("ReadTextFile = %#v err=%v", read, err)
	}
	if _, err := c.ReadTextFile(context.Background(), acp.ReadTextFileRequest{Path: "relative"}); err == nil {
		t.Fatal("ReadTextFile accepted relative path")
	}
	if _, err := c.ReadTextFile(context.Background(), acp.ReadTextFileRequest{Path: filepath.Join(dir, "missing.txt")}); err == nil {
		t.Fatal("ReadTextFile missing file succeeded")
	}
	if _, err := c.WriteTextFile(context.Background(), acp.WriteTextFileRequest{Path: "relative"}); err == nil {
		t.Fatal("WriteTextFile accepted relative path")
	}
	notDir := filepath.Join(dir, "not-dir")
	if err := os.WriteFile(notDir, []byte("x"), 0o600); err != nil {
		t.Fatalf("write not-dir: %v", err)
	}
	if _, err := c.WriteTextFile(context.Background(), acp.WriteTextFileRequest{
		Path:    filepath.Join(notDir, "child.txt"),
		Content: "body",
	}); err == nil {
		t.Fatal("WriteTextFile under file path succeeded")
	}

	title := "Run tool"
	resp, err := c.RequestPermission(context.Background(), acp.RequestPermissionRequest{
		ToolCall: acp.ToolCallUpdate{Title: &title},
		Options:  []acp.PermissionOption{{OptionId: "allow", Kind: acp.PermissionOptionKindAllowAlways}},
	})
	if err != nil || resp.Outcome.Selected == nil || resp.Outcome.Selected.OptionId != "allow" {
		t.Fatalf("permission resp=%#v err=%v", resp, err)
	}
	cancelResp, err := c.RequestPermission(context.Background(), acp.RequestPermissionRequest{})
	if err != nil || cancelResp.Outcome.Cancelled == nil {
		t.Fatalf("cancel permission resp=%#v err=%v", cancelResp, err)
	}

	messageID := "33333333-3333-4333-8333-333333333333"
	status := acp.ToolCallStatusCompleted
	if err := c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.UpdateUserMessageText("user text"),
	}); err != nil {
		t.Fatalf("user update returned error: %v", err)
	}
	if err := c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
			MessageId: &messageID,
			Content:   acp.TextBlock("Hello"),
		}},
	}); err != nil {
		t.Fatalf("agent update returned error: %v", err)
	}
	if err := c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
			MessageId: &messageID,
			Content:   acp.TextBlock("Hello world"),
		}},
	}); err != nil {
		t.Fatalf("agent update returned error: %v", err)
	}
	if err := c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.UpdateAgentThoughtText("thinking"),
	}); err != nil {
		t.Fatalf("thought update returned error: %v", err)
	}
	if err := c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.StartToolCall("tool", "Read file"),
	}); err != nil {
		t.Fatalf("tool update returned error: %v", err)
	}
	if err := c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.UpdateToolCall("tool", acp.WithUpdateStatus(status)),
	}); err != nil {
		t.Fatalf("tool status update returned error: %v", err)
	}
	if err := c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{UsageUpdate: &acp.SessionUsageUpdate{Used: 1, Size: 2}},
	}); err != nil {
		t.Fatalf("usage update returned error: %v", err)
	}
	if got := output.String(); !strings.Contains(got, "permission> Run tool") ||
		!strings.Contains(got, "Hello world") ||
		!strings.Contains(got, "usage> 1/2 tokens") {
		t.Fatalf("output = %q", got)
	}

	if terminal, err := c.CreateTerminal(context.Background(), acp.CreateTerminalRequest{}); err != nil ||
		terminal.TerminalId == "" {
		t.Fatalf("CreateTerminal = %#v err=%v", terminal, err)
	}
	if _, err := c.KillTerminal(context.Background(), acp.KillTerminalRequest{}); err != nil {
		t.Fatalf("KillTerminal returned error: %v", err)
	}
	if terminalOutput, err := c.TerminalOutput(context.Background(), acp.TerminalOutputRequest{}); err != nil ||
		terminalOutput.Truncated {
		t.Fatalf("TerminalOutput = %#v err=%v", terminalOutput, err)
	}
	if _, err := c.ReleaseTerminal(context.Background(), acp.ReleaseTerminalRequest{}); err != nil {
		t.Fatalf("ReleaseTerminal returned error: %v", err)
	}
	if _, err := c.WaitForTerminalExit(context.Background(), acp.WaitForTerminalExitRequest{}); err != nil {
		t.Fatalf("WaitForTerminalExit returned error: %v", err)
	}
	if result, err := c.HandleExtensionMethod(context.Background(), "_opencode/example", json.RawMessage(`{}`)); err != nil ||
		result == nil {
		t.Fatalf("HandleExtensionMethod underscore = %#v err=%v", result, err)
	}
	if _, err := c.HandleExtensionMethod(context.Background(), "bad/method", nil); err == nil {
		t.Fatal("HandleExtensionMethod accepted non-extension method")
	}

	ui.writeUserPrompt(" ")
	ui.writeNotice("empty", " ")
	ui.writeToolCall(nil)
	ui.writeToolCallUpdate(nil)
	var display messageDisplay
	display.writeText(&output, "")
	display.writeText(&output, "x")
	display.writeText(&output, "x")
	display.writeText(&output, "xy")
	display.writeText(&output, "z")
	ui.writeAgentText(nil, "fallback")
	emptyID := ""
	ui.writeAgentText(&emptyID, "fallback again")
}

func TestConfigureTerminal(t *testing.T) {
	originalRaw := rawTerminalInput
	originalIsTerminal := isTerminal
	originalMakeRaw := makeTerminalRaw
	originalRestore := restoreTerminal
	t.Cleanup(func() {
		rawTerminalInput = originalRaw
		isTerminal = originalIsTerminal
		makeTerminalRaw = originalMakeRaw
		restoreTerminal = originalRestore
	})

	restore, err := configureTerminal(strings.NewReader(""))
	if err != nil {
		t.Fatalf("configureTerminal non-file returned error: %v", err)
	}
	restore()

	file, err := os.CreateTemp(t.TempDir(), "stdin-*")
	if err != nil {
		t.Fatalf("create temp stdin: %v", err)
	}
	defer func() { _ = file.Close() }()

	isTerminal = func(int) bool { return false }
	restore, err = configureTerminal(file)
	if err != nil {
		t.Fatalf("configureTerminal non-terminal returned error: %v", err)
	}
	restore()

	isTerminal = func(int) bool { return true }
	makeTerminalRaw = func(int) (*term.State, error) {
		return nil, errors.New("raw failed")
	}
	if _, err := configureTerminal(file); err == nil || !strings.Contains(err.Error(), "configure terminal") {
		t.Fatalf("raw error = %v", err)
	}

	restored := false
	makeTerminalRaw = func(int) (*term.State, error) {
		return &term.State{}, nil
	}
	restoreTerminal = func(int, *term.State) error {
		restored = true

		return nil
	}
	restore, err = configureTerminal(file)
	if err != nil {
		t.Fatalf("configureTerminal raw returned error: %v", err)
	}
	if !rawTerminalInput {
		t.Fatal("rawTerminalInput was not enabled")
	}
	restore()
	if rawTerminalInput || !restored {
		t.Fatalf("rawTerminalInput=%v restored=%v", rawTerminalInput, restored)
	}
}

func TestReadInputLinesEdges(t *testing.T) {
	var echo bytes.Buffer
	lines := readInputLines(strings.NewReader("ab\x7fc\rtail"), &echo, true)
	first, ok, err := nextInputLine(context.Background(), lines)
	if err != nil || !ok || first.text != "ac" {
		t.Fatalf("first line = %#v ok=%v err=%v", first, ok, err)
	}
	second, ok, err := nextInputLine(context.Background(), lines)
	if err != nil || !ok || second.text != "tail" {
		t.Fatalf("second line = %#v ok=%v err=%v", second, ok, err)
	}
	if !strings.Contains(echo.String(), "\b \b") || !strings.Contains(echo.String(), "\r\n") {
		t.Fatalf("echo = %q", echo.String())
	}

	closed := make(chan inputLine)
	close(closed)
	if _, ok, err := nextInputLine(context.Background(), closed); err != nil || ok {
		t.Fatalf("closed nextInputLine ok=%v err=%v", ok, err)
	}
}

func TestChatUIUsesCRLFInRawTerminalMode(t *testing.T) {
	originalRaw := rawTerminalInput
	t.Cleanup(func() { rawTerminalInput = originalRaw })

	rawTerminalInput = true

	var output bytes.Buffer
	ui := newChatUI(&output)
	ui.writeHeader("/repo")

	got := output.String()
	if strings.Contains(got, "example\ncwd") {
		t.Fatalf("raw output used bare newline: %q", got)
	}
	if !strings.Contains(got, "example\r\ncwd") {
		t.Fatalf("raw output missing CRLF: %q", got)
	}
}

func TestChatThoughtDisplayCombinesChunks(t *testing.T) {
	var output bytes.Buffer
	c := &chatClient{ui: newChatUI(&output)}

	for _, chunk := range []string{
		"The", "user", "is", "saying", "hello", ".", "I", "should", "respond", "conc", "is", "ely", ".",
	} {
		if err := c.SessionUpdate(context.Background(), acp.SessionNotification{
			Update: acp.UpdateAgentThoughtText(chunk),
		}); err != nil {
			t.Fatalf("thought update %q returned error: %v", chunk, err)
		}
	}
	if err := c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.UpdateAgentMessageText("Hello"),
	}); err != nil {
		t.Fatalf("agent update returned error: %v", err)
	}

	got := output.String()
	if count := strings.Count(got, "thinking>"); count != 1 {
		t.Fatalf("thinking prefix count = %d, output = %q", count, got)
	}
	if !strings.Contains(got, "thinking> The user is saying hello. I should respond concisely.\nHello") {
		t.Fatalf("output = %q", got)
	}
}

func TestStartEmbeddedAgent(t *testing.T) {
	originalServe := serveAgent
	t.Cleanup(func() { serveAgent = originalServe })

	started := make(chan struct{})
	serveAgent = func(
		_ context.Context,
		input io.Reader,
		_ io.Writer,
		_ ...opencodeacp.Option,
	) error {
		close(started)
		_, _ = io.Copy(io.Discard, input)

		return nil
	}

	agent, err := startEmbeddedAgent(context.Background(), "/repo", io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("startEmbeddedAgent returned error: %v", err)
	}
	<-started
	agent.close()
	if err := agent.wait(); err != nil {
		t.Fatalf("wait returned error: %v", err)
	}

	wantErr := errors.New("serve failed")
	serveAgent = func(context.Context, io.Reader, io.Writer, ...opencodeacp.Option) error {
		return wantErr
	}

	agent, err = startEmbeddedAgent(context.Background(), "/repo", io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("startEmbeddedAgent returned error: %v", err)
	}
	if err := agent.wait(); !errors.Is(err, wantErr) {
		t.Fatalf("wait error = %v, want %v", err, wantErr)
	}

	serveAgent = func(context.Context, io.Reader, io.Writer, ...opencodeacp.Option) error {
		return context.Canceled
	}
	agent, err = startEmbeddedAgent(context.Background(), "/repo", io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("startEmbeddedAgent returned error: %v", err)
	}
	if err := agent.wait(); err != nil {
		t.Fatalf("context canceled wait returned error: %v", err)
	}
}

func TestThoughtDisplayEdges(t *testing.T) {
	var out bytes.Buffer
	var thoughts thoughtDisplay

	thoughts.writeChunk(&out, "thinking> ", "   ")
	if out.Len() != 0 {
		t.Fatalf("empty thought chunk wrote output: %q", out.String())
	}

	thoughts.writeChunk(&out, "thinking> ", "same")
	first := out.String()
	thoughts.writeChunk(&out, "thinking> ", "same")
	if out.String() != first {
		t.Fatalf("duplicate chunk changed output: %q -> %q", first, out.String())
	}
	thoughts.writeChunk(&out, "thinking> ", "same plus")
	thoughts.writeChunk(&out, "thinking> ", "same")
	thoughts.endLine(&out)

	if got := (&thoughtDisplay{}).nextText("first"); got != "first" {
		t.Fatalf("empty nextText = %q", got)
	}
	for _, tc := range []struct {
		current string
		next    string
		want    string
	}{
		{current: "", next: "word", want: ""},
		{current: "word ", next: "next", want: ""},
		{current: "word", next: " next", want: ""},
		{current: "word", next: ".", want: ""},
		{current: "word", next: "next", want: " "},
	} {
		if got := thoughtSeparator(tc.current, tc.next); got != tc.want {
			t.Fatalf("thoughtSeparator(%q, %q) = %q, want %q", tc.current, tc.next, got, tc.want)
		}
	}
	if shouldJoinThoughtChunk("123", "ing") {
		t.Fatal("joined chunk without trailing word")
	}
	if !shouldJoinThoughtChunk("build", "ing") {
		t.Fatal("did not join suffix chunk")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) {
	return 0, errors.New("read failed")
}

type fakeAgentConnection struct {
	mu            sync.Mutex
	initialized   bool
	closed        bool
	closedStarter bool
	waited        bool
	cwd           string
	startedCwd    string
	prompts       []string
	cancelled     []acp.SessionId
	initErr       error
	newErr        error
	promptErr     error
	cancelErr     error
}

func (f *fakeAgentConnection) Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.initErr != nil {
		return acp.InitializeResponse{}, f.initErr
	}

	f.initialized = true

	return acp.InitializeResponse{}, nil
}

func (f *fakeAgentConnection) NewSession(_ context.Context, params acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.newErr != nil {
		return acp.NewSessionResponse{}, f.newErr
	}

	f.cwd = params.Cwd

	return acp.NewSessionResponse{SessionId: "s"}, nil
}

func (f *fakeAgentConnection) Prompt(_ context.Context, params acp.PromptRequest) (acp.PromptResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.promptErr != nil {
		return acp.PromptResponse{}, f.promptErr
	}
	if len(params.Prompt) > 0 && params.Prompt[0].Text != nil {
		f.prompts = append(f.prompts, params.Prompt[0].Text.Text)
	}

	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func (f *fakeAgentConnection) Cancel(_ context.Context, params acp.CancelNotification) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.cancelErr != nil {
		return f.cancelErr
	}

	f.cancelled = append(f.cancelled, params.SessionId)

	return nil
}

func (f *fakeAgentConnection) CloseSession(context.Context, acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.closed = true

	return acp.CloseSessionResponse{}, nil
}

func (f *fakeAgentConnection) initializedSnapshot() bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.initialized
}

func (f *fakeAgentConnection) cwdSnapshot() string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.cwd
}

func (f *fakeAgentConnection) promptsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.prompts...)
}

func (f *fakeAgentConnection) cancelledSnapshot() []acp.SessionId {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]acp.SessionId(nil), f.cancelled...)
}

func (f *fakeAgentConnection) closedSnapshot() bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.closed
}
