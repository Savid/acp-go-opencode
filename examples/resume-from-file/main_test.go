package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	opencodeacp "github.com/savid/acp-go-opencode"
)

func TestReadTranscriptJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	writeFile(t, path, "\n"+
		`{"format":"opencode-state-v1","session":{"sessionId":"session-1","cwd":"/repo"}}`+"\n"+
		`{"format":"opencode-state-v1"}`+"\n"+
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
		fmt.Sprintf(`{"format":"opencode-state-v1","session":{"sessionId":"session-1","cwd":%q}}`+"\n", cwd)+
			`{"sessionId":"session-1","format":"opencode-state-v1"}`+"\n")

	previousRunLoaded := runLoaded
	expectedSessionID := "session-1"
	expectedCwd := cwd
	expectedPrompt := "prompt"
	expectedPath := "/bin/opencode"
	expectedHome := "/home/opencode"
	runLoaded = func(_ context.Context, store opencodeacp.SessionStore, sessionID string, gotCwd string, prompt string, opencodePath string, opencodeHome string, stdout io.Writer) error {
		if sessionID != expectedSessionID || gotCwd != expectedCwd || prompt != expectedPrompt || opencodePath != expectedPath || opencodeHome != expectedHome {
			t.Fatalf("runLoaded args sessionID=%q cwd=%q prompt=%q path=%q home=%q", sessionID, gotCwd, prompt, opencodePath, opencodeHome)
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
	if err := run(context.Background(), []string{"-file", path, "-prompt", "prompt", "-path", "/bin/opencode", "-home", "/home/opencode"}, &stdout, io.Discard); err != nil {
		t.Fatalf("run returned error: %v", err)
	}
	if stdout.String() != "loaded" {
		t.Fatalf("stdout = %q", stdout.String())
	}

	stdout.Reset()
	expectedSessionID = "explicit"
	expectedPrompt = defaultPrompt
	expectedPath = ""
	expectedHome = ""
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
	writeFile(t, path, `{"format":"opencode-state-v1"}`+"\n")
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
	writeFile(t, path, `{"sessionId":"session-1","format":"opencode-state-v1"}`+"\n")
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

func TestRunLoadedSessionWithFakeAgent(t *testing.T) {
	previousNewAgent := newAgent
	t.Cleanup(func() { newAgent = previousNewAgent })

	store := opencodeacp.NewInMemorySessionStore()
	fake := &fakeAgent{}
	newAgent = func(gotStore opencodeacp.SessionStore, opencodePath string, opencodeHome string) agentConnection {
		if gotStore != store || opencodePath != "/bin/opencode" || opencodeHome != "/home/opencode" {
			t.Fatalf("newAgent args store=%v path=%q home=%q", gotStore, opencodePath, opencodeHome)
		}

		return fake
	}

	var stdout bytes.Buffer
	if err := runLoadedSession(context.Background(), store, "session-1", t.TempDir(), "prompt", "/bin/opencode", "/home/opencode", &stdout); err != nil {
		t.Fatalf("runLoadedSession returned error: %v", err)
	}
	if !strings.Contains(stdout.String(), "== resume smoke test ==") || !strings.Contains(stdout.String(), "stop reason: end_turn") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if !fake.initialized || fake.loadedID != acp.SessionId("session-1") || fake.prompt != "prompt" || !fake.closed {
		t.Fatalf("fake state = %#v", fake)
	}
}

func TestRunLoadedSessionErrors(t *testing.T) {
	previousNewAgent := newAgent
	t.Cleanup(func() { newAgent = previousNewAgent })

	for name, fake := range map[string]*fakeAgent{
		"initialize": {initErr: errors.New("init")},
		"load":       {loadErr: errors.New("load")},
		"prompt":     {promptErr: errors.New("prompt")},
	} {
		t.Run(name, func(t *testing.T) {
			newAgent = func(opencodeacp.SessionStore, string, string) agentConnection { return fake }
			if err := runLoadedSession(context.Background(), opencodeacp.NewInMemorySessionStore(), "session-1", t.TempDir(), "prompt", "", "", io.Discard); err == nil {
				t.Fatalf("%s error path succeeded", name)
			}
		})
	}
}

func TestNewAgentBuildsRealAgent(t *testing.T) {
	if newAgent(opencodeacp.NewInMemorySessionStore(), "", "") == nil {
		t.Fatal("default newAgent returned nil")
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

type fakeAgent struct {
	initialized bool
	closed      bool
	loadedID    acp.SessionId
	prompt      string
	initErr     error
	loadErr     error
	promptErr   error
}

func (f *fakeAgent) Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error) {
	f.initialized = true

	return acp.InitializeResponse{ProtocolVersion: acp.ProtocolVersionNumber}, f.initErr
}

func (f *fakeAgent) LoadSession(_ context.Context, params acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
	f.loadedID = params.SessionId

	return acp.LoadSessionResponse{}, f.loadErr
}

func (f *fakeAgent) Prompt(_ context.Context, params acp.PromptRequest) (acp.PromptResponse, error) {
	if len(params.Prompt) > 0 && params.Prompt[0].Text != nil {
		f.prompt = params.Prompt[0].Text.Text
	}

	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, f.promptErr
}

func (f *fakeAgent) CloseSession(context.Context, acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	f.closed = true

	return acp.CloseSessionResponse{}, nil
}
