package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coder/acp-go-sdk"
	opencodeacp "github.com/savid/acp-go-opencode"
)

func TestReadTranscriptJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	writeFile(t, path, "\n"+
		`{"format":"opencode-sync-events-v1","session":{"sessionId":"session-1","cwd":"/repo"}}`+"\n"+
		`{"format":"opencode-sync-events-v1"}`+"\n"+
		`{"sessionId":"idmap-only"}`+"\n")

	entries, sessionID, cwd, err := readTranscriptJSONL(path)
	if err != nil {
		t.Fatalf("readTranscriptJSONL returned error: %v", err)
	}
	if len(entries) != 3 || sessionID != "session-1" || cwd != "/repo" {
		t.Fatalf("entries=%d sessionID=%q cwd=%q", len(entries), sessionID, cwd)
	}

	missingEntries, missingSessionID, missingCwd, missingErr := readTranscriptJSONL(filepath.Join(t.TempDir(), "missing.jsonl"))
	if missingErr == nil || missingEntries != nil || missingSessionID != "" || missingCwd != "" {
		t.Fatalf("missing file entries=%v sessionID=%q cwd=%q err=%v", missingEntries, missingSessionID, missingCwd, missingErr)
	}
}

func TestReadTranscriptJSONLInfersFromTopLevel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	writeFile(t, path, `{"sessionId":"session-1","cwd":"/repo"}`+"\n")

	entries, sessionID, cwd, err := readTranscriptJSONL(path)
	if err != nil {
		t.Fatalf("readTranscriptJSONL returned error: %v", err)
	}
	if len(entries) != 1 || sessionID != "session-1" || cwd != "/repo" {
		t.Fatalf("entries=%d sessionID=%q cwd=%q", len(entries), sessionID, cwd)
	}
}

func TestRunUsesInferredValuesAndLoadedSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	cwd := t.TempDir()
	writeFile(t, path,
		fmt.Sprintf(`{"format":"opencode-sync-events-v1","session":{"sessionId":"session-1","cwd":%q}}`+"\n", cwd)+
			`{"sessionId":"session-1","format":"opencode-sync-events-v1"}`+"\n")

	previousRunLoaded := runLoaded
	expectedSessionID := "session-1"
	expectedCwd := cwd
	expectedPrompt := "prompt"
	expectedPath := "/bin/opencode"
	expectedScratch := "/tmp/opencode-scratch"
	runLoaded = func(_ context.Context, store opencodeacp.SessionStore, sessionID string, gotCwd string, prompt string, opencodePath string, scratchDir string, stdout io.Writer) error {
		if sessionID != expectedSessionID || gotCwd != expectedCwd || prompt != expectedPrompt || opencodePath != expectedPath || scratchDir != expectedScratch {
			t.Fatalf("runLoaded args sessionID=%q cwd=%q prompt=%q path=%q scratch=%q", sessionID, gotCwd, prompt, opencodePath, scratchDir)
		}
		entries, err := store.Load(context.Background(), opencodeacp.SessionKey{SessionID: sessionID})
		if err != nil || len(entries) != 2 {
			t.Fatalf("store.Load entries=%d err=%v", len(entries), err)
		}
		fmt.Fprint(stdout, "loaded")

		return nil
	}
	t.Cleanup(func() { runLoaded = previousRunLoaded })

	var stdout bytes.Buffer
	if err := run(context.Background(), []string{"-file", path, "-prompt", "prompt", "-path", "/bin/opencode", "-scratch-dir", "/tmp/opencode-scratch"}, &stdout, io.Discard); err != nil {
		t.Fatalf("run returned error: %v", err)
	}
	if stdout.String() != "loaded" {
		t.Fatalf("stdout = %q", stdout.String())
	}

	stdout.Reset()
	expectedSessionID = "explicit"
	expectedPrompt = defaultPrompt
	expectedPath = ""
	expectedScratch = ""
	if err := run(context.Background(), []string{"-file", path, "-session", "explicit", "-cwd", cwd}, &stdout, io.Discard); err != nil {
		t.Fatalf("run with explicit flags returned error: %v", err)
	}
}

func TestRunErrors(t *testing.T) {
	entries, sessionID, cwd, err := readTranscriptJSONL("")
	if err == nil || entries != nil || sessionID != "" || cwd != "" {
		t.Fatalf("empty path entries=%v sessionID=%q cwd=%q err=%v", entries, sessionID, cwd, err)
	}

	if err := run(context.Background(), []string{"-bad"}, io.Discard, io.Discard); err == nil {
		t.Fatal("run accepted unknown flag")
	}
	if err := run(context.Background(), []string{"-file", filepath.Join(t.TempDir(), "missing.jsonl")}, io.Discard, io.Discard); err == nil {
		t.Fatal("run accepted missing transcript")
	}

	path := filepath.Join(t.TempDir(), "session.jsonl")
	writeFile(t, path, `{"format":"opencode-sync-events-v1"}`+"\n")
	if err := run(context.Background(), []string{"-file", path}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "session id is required") {
		t.Fatalf("expected session id error, got %v", err)
	}

	previousGetwd := getwd
	previousRunLoaded := runLoaded
	getwd = func() (string, error) { return "", errors.New("getwd failed") }
	runLoaded = func(context.Context, opencodeacp.SessionStore, string, string, string, string, string, io.Writer) error {
		return nil
	}
	t.Cleanup(func() {
		getwd = previousGetwd
		runLoaded = previousRunLoaded
	})
	writeFile(t, path, `{"sessionId":"session-1","format":"opencode-sync-events-v1"}`+"\n")
	if err := run(context.Background(), []string{"-file", path}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "getwd failed") {
		t.Fatalf("expected getwd error, got %v", err)
	}

	getwd = func() (string, error) { return t.TempDir(), nil }
	runLoaded = func(context.Context, opencodeacp.SessionStore, string, string, string, string, string, io.Writer) error {
		return errors.New("load failed")
	}
	if err := run(context.Background(), []string{"-file", path}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "load failed") {
		t.Fatalf("expected load error, got %v", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(cancelled, []string{"-file", path, "-session", "session-1", "-cwd", t.TempDir()}, io.Discard, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestRunLoadedSessionWithFakeServe(t *testing.T) {
	previousServe := serve
	serve = fakeServe(t, "", nil)
	t.Cleanup(func() { serve = previousServe })

	var stdout bytes.Buffer
	if err := runLoadedSession(context.Background(), opencodeacp.NewInMemorySessionStore(), "session-1", t.TempDir(), "prompt", "", "", &stdout); err != nil {
		t.Fatalf("runLoadedSession returned error: %v", err)
	}
	if !strings.Contains(stdout.String(), "== resume smoke test ==") || !strings.Contains(stdout.String(), "stop reason: end_turn") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "RESUME_OK") {
		t.Fatalf("replayed conversation not printed: %q", stdout.String())
	}
}

func TestRunLoadedSessionErrors(t *testing.T) {
	previousServe := serve
	previousNewTurnNonce := newTurnNonce
	t.Cleanup(func() {
		serve = previousServe
		newTurnNonce = previousNewTurnNonce
	})

	for name, method := range map[string]string{
		"initialize": acp.AgentMethodInitialize,
		"load":       acp.AgentMethodSessionLoad,
		"prompt":     acp.AgentMethodSessionPrompt,
	} {
		t.Run(name, func(t *testing.T) {
			serve = fakeServe(t, method, acp.NewInternalError(map[string]any{"error": name}))
			if err := runLoadedSession(context.Background(), opencodeacp.NewInMemorySessionStore(), "session-1", t.TempDir(), "prompt", "", "", io.Discard); err == nil {
				t.Fatalf("%s error path succeeded", name)
			}
		})
	}

	serve = fakeServe(t, "", nil)
	newTurnNonce = func() (string, error) { return "", errors.New("entropy failed") }
	if err := runLoadedSession(context.Background(), opencodeacp.NewInMemorySessionStore(), "session-1", t.TempDir(), "prompt", "", "", io.Discard); err == nil || !strings.Contains(err.Error(), "entropy failed") {
		t.Fatalf("expected turn nonce error, got %v", err)
	}
}

// fakeServe returns a Serve stand-in that drives a fixed ACP agent over the
// pipes so runLoadedSession exercises a real client/agent connection without a
// live opencode process. On the happy path it streams an agent message chunk so
// the client's SessionUpdate handler prints the replayed conversation.
func fakeServe(t *testing.T, failMethod string, failErr *acp.RequestError) func(context.Context, io.Reader, io.Writer, ...opencodeacp.Option) error {
	t.Helper()

	return func(ctx context.Context, input io.Reader, output io.Writer, _ ...opencodeacp.Option) error {
		var connHolder atomic.Pointer[acp.Connection]
		conn := acp.NewConnection(func(_ context.Context, method string, params json.RawMessage) (any, *acp.RequestError) {
			if method == failMethod {
				return nil, failErr
			}

			switch method {
			case acp.AgentMethodInitialize:
				return acp.InitializeResponse{ProtocolVersion: acp.ProtocolVersionNumber}, nil
			case acp.AgentMethodSessionLoad:
				var req acp.LoadSessionRequest
				if err := json.Unmarshal(params, &req); err != nil {
					return nil, acp.NewInvalidParams(map[string]any{"error": err.Error()})
				}

				return acp.LoadSessionResponse{}, nil
			case acp.AgentMethodSessionPrompt:
				var req acp.PromptRequest
				if err := json.Unmarshal(params, &req); err != nil {
					return nil, acp.NewInvalidParams(map[string]any{"error": err.Error()})
				}
				if peer := connHolder.Load(); peer != nil {
					_ = peer.SendNotification(ctx, acp.ClientMethodSessionUpdate, acp.SessionNotification{
						SessionId: req.SessionId,
						Update:    acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("RESUME_OK")}},
					})
				}

				return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
			case acp.AgentMethodSessionClose:
				return acp.CloseSessionResponse{}, nil
			default:
				return nil, acp.NewMethodNotFound(method)
			}
		}, output, input)
		connHolder.Store(conn)
		<-ctx.Done()

		return nil
	}
}

func TestMainUsesRunMainAndExit(t *testing.T) {
	previousRunMain := runMain
	previousExit := exit
	previousArgs := os.Args
	t.Cleanup(func() {
		runMain = previousRunMain
		exit = previousExit
		os.Args = previousArgs
	})

	runMain = func(context.Context, []string, io.Writer, io.Writer) error { return nil }
	exit = func(code int) { panic(fmt.Sprintf("exit %d", code)) }
	os.Args = []string{"resume-from-file"}
	main()

	runMain = func(context.Context, []string, io.Writer, io.Writer) error { return errors.New("boom") }
	assertPanicsWithExit1(t)
}

func TestClientMethods(t *testing.T) {
	var stdout bytes.Buffer
	c := &client{output: &stdout}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "nested", "file.txt")
	if _, err := c.WriteTextFile(ctx, acp.WriteTextFileRequest{Path: path, Content: "body"}); err != nil {
		t.Fatalf("WriteTextFile: %v", err)
	}
	read, readErr := c.ReadTextFile(ctx, acp.ReadTextFileRequest{Path: path})
	if readErr != nil || read.Content != "body" {
		t.Fatalf("ReadTextFile content=%q err=%v", read.Content, readErr)
	}
	if _, err := c.ReadTextFile(ctx, acp.ReadTextFileRequest{Path: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("ReadTextFile accepted missing file")
	}
	notDir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.WriteTextFile(ctx, acp.WriteTextFileRequest{Path: filepath.Join(notDir, "child"), Content: "body"}); err == nil {
		t.Fatal("WriteTextFile accepted path under a file")
	}

	permission, permErr := c.RequestPermission(ctx, acp.RequestPermissionRequest{})
	if permErr != nil || permission.Outcome.Cancelled == nil {
		t.Fatalf("RequestPermission outcome=%#v err=%v", permission.Outcome, permErr)
	}
	if err := c.SessionUpdate(ctx, acp.SessionNotification{}); err != nil {
		t.Fatalf("SessionUpdate empty: %v", err)
	}
	if err := c.SessionUpdate(ctx, acp.SessionNotification{Update: acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("hi")}}}); err != nil {
		t.Fatalf("SessionUpdate chunk: %v", err)
	}
	if stdout.String() != "hi" || c.text.String() != "hi" {
		t.Fatalf("stdout=%q text=%q", stdout.String(), c.text.String())
	}
	if err := (&client{}).SessionUpdate(ctx, acp.SessionNotification{Update: acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("stdout")}}}); err != nil {
		t.Fatalf("SessionUpdate default writer: %v", err)
	}

	terminal, termErr := c.CreateTerminal(ctx, acp.CreateTerminalRequest{})
	if termErr != nil || terminal.TerminalId != "terminal-1" {
		t.Fatalf("CreateTerminal id=%q err=%v", terminal.TerminalId, termErr)
	}
	if _, err := c.KillTerminal(ctx, acp.KillTerminalRequest{}); err != nil {
		t.Fatalf("KillTerminal: %v", err)
	}
	output, outErr := c.TerminalOutput(ctx, acp.TerminalOutputRequest{})
	if outErr != nil || output.Truncated {
		t.Fatalf("TerminalOutput truncated=%v err=%v", output.Truncated, outErr)
	}
	if _, err := c.ReleaseTerminal(ctx, acp.ReleaseTerminalRequest{}); err != nil {
		t.Fatalf("ReleaseTerminal: %v", err)
	}
	if _, err := c.WaitForTerminalExit(ctx, acp.WaitForTerminalExitRequest{}); err != nil {
		t.Fatalf("WaitForTerminalExit: %v", err)
	}
}

func TestReadTranscriptScannerError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "long.jsonl")
	writeFile(t, path, strings.Repeat("x", bufio.MaxScanTokenSize+1))
	entries, sessionID, cwd, err := readTranscriptJSONL(path)
	if err == nil || entries != nil || sessionID != "" || cwd != "" {
		t.Fatalf("scanner error entries=%v sessionID=%q cwd=%q err=%v", entries, sessionID, cwd, err)
	}
}

func assertPanicsWithExit1(t *testing.T) {
	t.Helper()
	defer func() {
		if recovered := recover(); recovered != "exit 1" {
			t.Fatalf("expected panic \"exit 1\", got %v", recovered)
		}
	}()
	main()
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
