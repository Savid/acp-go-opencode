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
	"testing"

	"github.com/coder/acp-go-sdk"
	opencodeacp "github.com/savid/acp-go-opencode"
)

func TestRunConversation(t *testing.T) {
	conn := &fakeAgentConnection{}
	var stdout bytes.Buffer

	err := runConversation(context.Background(), conn, "hello", "/repo", &stdout)
	if err != nil {
		t.Fatalf("runConversation returned error: %v", err)
	}
	if !conn.initialized || conn.prompt != "hello" || conn.cwd != "/repo" || !conn.closed {
		t.Fatalf("conn state = %#v", conn)
	}
	if !strings.Contains(stdout.String(), "stop reason") {
		t.Fatalf("stdout = %q", stdout.String())
	}

	for name, conn := range map[string]*fakeAgentConnection{
		"initialize": {initErr: errors.New("init failed")},
		"new":        {newErr: errors.New("new failed")},
		"prompt":     {promptErr: errors.New("prompt failed")},
	} {
		if err := runConversation(context.Background(), conn, "hello", "/repo", io.Discard); err == nil {
			t.Fatalf("%s error path succeeded", name)
		}
	}
}

func TestRunUsesInjectedAgent(t *testing.T) {
	originalStart := startAgent
	originalGetwd := getwd
	originalExit := exit
	originalArgs := os.Args
	t.Cleanup(func() {
		startAgent = originalStart
		getwd = originalGetwd
		exit = originalExit
		os.Args = originalArgs
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
	if code := run(context.Background(), []string{"hello", "opencode"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run code = %d stderr=%q", code, stderr.String())
	}
	if conn.startedCwd != "/repo" || conn.prompt != "hello opencode" || !conn.closedStarter || !conn.waited {
		t.Fatalf("conn state = %#v", conn)
	}

	exitCode := -1
	exit = func(code int) { exitCode = code }
	os.Args = []string{"minimal-client", "from-main"}
	main()
	if exitCode != 0 || conn.prompt != "from-main" {
		t.Fatalf("main exit=%d prompt=%q", exitCode, conn.prompt)
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
	if code := run(context.Background(), nil, &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "cwd failed") {
		t.Fatalf("cwd failure code=%d stderr=%q", code, stderr.String())
	}

	getwd = func() (string, error) { return "/repo", nil }
	startAgent = func(context.Context, string, io.Writer, io.Writer) (*startedAgent, error) {
		return nil, errors.New("start failed")
	}
	stderr.Reset()
	if code := run(context.Background(), nil, &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "start failed") {
		t.Fatalf("start failure code=%d stderr=%q", code, stderr.String())
	}

	startAgent = func(context.Context, string, io.Writer, io.Writer) (*startedAgent, error) {
		return &startedAgent{conn: &fakeAgentConnection{promptErr: errors.New("prompt failed")}}, nil
	}
	stderr.Reset()
	if code := run(context.Background(), nil, &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "prompt failed") {
		t.Fatalf("prompt failure code=%d stderr=%q", code, stderr.String())
	}

	startAgent = func(context.Context, string, io.Writer, io.Writer) (*startedAgent, error) {
		return &startedAgent{conn: &fakeAgentConnection{promptErr: context.Canceled}}, nil
	}
	stderr.Reset()
	if code := run(context.Background(), nil, &stdout, &stderr); code != interruptExitCode || stderr.String() != "" {
		t.Fatalf("cancel failure code=%d stderr=%q", code, stderr.String())
	}
}

func TestClientHelpers(t *testing.T) {
	var out bytes.Buffer
	c := &client{output: &out}

	if err := c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.UpdateAgentMessageText("hello"),
	}); err != nil {
		t.Fatalf("SessionUpdate returned error: %v", err)
	}
	if err := c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.UpdateAgentThoughtText("think"),
	}); err != nil {
		t.Fatalf("thought SessionUpdate returned error: %v", err)
	}
	if !strings.Contains(out.String(), "hello") {
		t.Fatalf("output = %q", out.String())
	}

	resp, err := c.RequestPermission(context.Background(), acp.RequestPermissionRequest{
		Options: []acp.PermissionOption{{OptionId: "allow", Kind: acp.PermissionOptionKindAllowOnce}},
	})
	if err != nil || resp.Outcome.Selected == nil || resp.Outcome.Selected.OptionId != "allow" {
		t.Fatalf("permission resp=%#v err=%v", resp, err)
	}
	cancelResp, err := c.RequestPermission(context.Background(), acp.RequestPermissionRequest{})
	if err != nil || cancelResp.Outcome.Cancelled == nil {
		t.Fatalf("cancel permission resp=%#v err=%v", cancelResp, err)
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
	if _, err := c.WriteTextFile(context.Background(), acp.WriteTextFileRequest{Path: "relative"}); err == nil {
		t.Fatal("WriteTextFile accepted relative path")
	}
	if _, err := c.ReadTextFile(context.Background(), acp.ReadTextFileRequest{Path: filepath.Join(dir, "missing.txt")}); err == nil {
		t.Fatal("ReadTextFile missing file succeeded")
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
	if nilWriterClient := (&client{}).writer(); nilWriterClient == nil {
		t.Fatal("nil writer client returned nil writer")
	}
	if err := c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.StartToolCall("tool", "Run"),
	}); err != nil {
		t.Fatalf("tool SessionUpdate returned error: %v", err)
	}
	if err := c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.UpdateToolCall("tool"),
	}); err != nil {
		t.Fatalf("tool update without status returned error: %v", err)
	}
	if err := c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.UpdateToolCall("tool", acp.WithUpdateStatus(acp.ToolCallStatusCompleted)),
	}); err != nil {
		t.Fatalf("tool update with status returned error: %v", err)
	}
	if terminal, err := c.CreateTerminal(context.Background(), acp.CreateTerminalRequest{}); err != nil ||
		terminal.TerminalId == "" {
		t.Fatalf("CreateTerminal = %#v err=%v", terminal, err)
	}
	if _, err := c.KillTerminal(context.Background(), acp.KillTerminalRequest{}); err != nil {
		t.Fatalf("KillTerminal returned error: %v", err)
	}
	if output, err := c.TerminalOutput(context.Background(), acp.TerminalOutputRequest{}); err != nil || output.Truncated {
		t.Fatalf("TerminalOutput = %#v err=%v", output, err)
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

	var display messageDisplay
	display.writeText(&out, "")
	display.writeText(&out, "x")
	display.writeText(&out, "x")
	display.writeText(&out, "xy")
	display.writeText(&out, "z")
	if got := c.messageDisplay(nil); got == nil {
		t.Fatal("nil message display missing")
	}
	messageID := "id"
	if got := c.messageDisplay(&messageID); got == nil || c.messageDisplay(&messageID) != got {
		t.Fatal("message display was not cached")
	}
}

func TestClientThoughtDisplayCombinesChunks(t *testing.T) {
	var out bytes.Buffer
	c := &client{output: &out}

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

	got := out.String()
	if count := strings.Count(got, "[thought]"); count != 1 {
		t.Fatalf("thought prefix count = %d, output = %q", count, got)
	}
	if !strings.Contains(got, "[thought] The user is saying hello. I should respond concisely.\nHello") {
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

	thoughts.writeChunk(&out, "[thought] ", "   ")
	if out.Len() != 0 {
		t.Fatalf("empty chunk wrote output: %q", out.String())
	}

	thoughts.writeChunk(&out, "[thought] ", "same")
	first := out.String()
	thoughts.writeChunk(&out, "[thought] ", "same")
	if out.String() != first {
		t.Fatalf("duplicate chunk changed output: %q -> %q", first, out.String())
	}
	thoughts.writeChunk(&out, "[thought] ", "same plus")
	thoughts.writeChunk(&out, "[thought] ", "same")
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

type fakeAgentConnection struct {
	initialized   bool
	closed        bool
	closedStarter bool
	waited        bool
	cwd           string
	startedCwd    string
	prompt        string
	initErr       error
	newErr        error
	promptErr     error
}

func (c *fakeAgentConnection) Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error) {
	if c.initErr != nil {
		return acp.InitializeResponse{}, c.initErr
	}

	c.initialized = true

	return acp.InitializeResponse{}, nil
}

func (c *fakeAgentConnection) NewSession(_ context.Context, params acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	if c.newErr != nil {
		return acp.NewSessionResponse{}, c.newErr
	}

	c.cwd = params.Cwd

	return acp.NewSessionResponse{SessionId: "s"}, nil
}

func (c *fakeAgentConnection) Prompt(_ context.Context, params acp.PromptRequest) (acp.PromptResponse, error) {
	if c.promptErr != nil {
		return acp.PromptResponse{}, c.promptErr
	}
	if len(params.Prompt) > 0 && params.Prompt[0].Text != nil {
		c.prompt = params.Prompt[0].Text.Text
	}

	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func (c *fakeAgentConnection) CloseSession(context.Context, acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	c.closed = true

	return acp.CloseSessionResponse{}, nil
}
