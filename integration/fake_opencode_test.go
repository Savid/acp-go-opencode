//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	opencodeacp "github.com/savid/acp-go-opencode"
)

const (
	envFakeOpenCodeHelper = "ACP_GO_OPENCODE_FAKE_HELPER"
	envFakeOpenCodeMode   = "ACP_GO_OPENCODE_FAKE_MODE"
	fakeModeOK            = "ok"
	fakeModeMissingDoc    = "missing-doc"
)

func TestOpenCodeACPAgentFakeExecutableStdoutNoise(t *testing.T) {
	requireRunIntegration(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	agent := startAgentWithOpenCodePath(t, ctx, fakeOpenCodeExecutable(t, fakeModeOK), t.TempDir())
	defer agent.close()

	conn := acp.NewClientSideConnection(&recordingClient{}, agent.stdin, agent.stdout)
	if _, err := conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}); err != nil {
		t.Fatalf("initialize with fake opencode: %v\nstderr:\n%s", err, agent.stderrString())
	}
	session, err := conn.NewSession(ctx, opencodeacp.NewSessionRequest(t.TempDir()))
	if err != nil {
		t.Fatalf("new session with stdout-noisy fake opencode: %v\nstderr:\n%s", err, agent.stderrString())
	}
	if session.SessionId == "" {
		t.Fatalf("empty fake session response: %#v", session)
	}
}

func TestOpenCodeACPAgentFakeExecutableLeaseReaper(t *testing.T) {
	requireRunIntegration(t)
	if runtime.GOOS == "windows" {
		t.Skip("lease reaper signal semantics are platform-specific")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	home := t.TempDir()
	orphan := exec.CommandContext(ctx, "sleep", "30")
	if err := orphan.Start(); err != nil {
		t.Fatalf("start orphan process: %v", err)
	}
	waitOrphan := make(chan error, 1)
	go func() { waitOrphan <- orphan.Wait() }()
	t.Cleanup(func() {
		if orphan.ProcessState == nil {
			_ = orphan.Process.Kill()
			<-waitOrphan
		}
	})

	leaseDir := filepath.Join(home, "orphan", "state")
	if err := os.MkdirAll(leaseDir, 0o700); err != nil {
		t.Fatalf("mkdir lease dir: %v", err)
	}
	lease := map[string]any{"pid": orphan.Process.Pid, "port": 0, "startedAtUnixMilli": time.Now().UnixMilli(), "passwordHash": "test"}
	data, err := json.Marshal(lease)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leaseDir, "server.lease"), data, 0o600); err != nil {
		t.Fatalf("write lease: %v", err)
	}

	agent := startAgentWithOpenCodePath(t, ctx, fakeOpenCodeExecutable(t, fakeModeOK), home)
	defer agent.close()

	conn := acp.NewClientSideConnection(&recordingClient{}, agent.stdin, agent.stdout)
	if _, err := conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}); err != nil {
		t.Fatalf("initialize: %v\nstderr:\n%s", err, agent.stderrString())
	}
	if _, err := conn.NewSession(ctx, opencodeacp.NewSessionRequest(t.TempDir())); err != nil {
		t.Fatalf("new session: %v\nstderr:\n%s", err, agent.stderrString())
	}

	select {
	case <-waitOrphan:
	case <-time.After(5 * time.Second):
		t.Fatal("stale lease process was not reaped")
	}
	if _, err := os.Stat(filepath.Join(leaseDir, "server.lease")); !os.IsNotExist(err) {
		t.Fatalf("stale lease file still present: %v", err)
	}
}

func TestOpenCodeACPAgentFakeExecutablePermissionDocFailClosed(t *testing.T) {
	requireRunIntegration(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	agent := startAgentWithOpenCodePath(t, ctx, fakeOpenCodeExecutable(t, fakeModeMissingDoc), t.TempDir())
	defer agent.close()

	conn := acp.NewClientSideConnection(&recordingClient{}, agent.stdin, agent.stdout)
	if _, err := conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}); err != nil {
		t.Fatalf("initialize: %v\nstderr:\n%s", err, agent.stderrString())
	}
	_, err := conn.NewSession(ctx, opencodeacp.NewSessionRequest(t.TempDir()))
	if err == nil || !strings.Contains(err.Error(), "/api/session/{sessionID}/permission/{requestID}/reply") {
		t.Fatalf("new session with missing permission /doc path err = %v\nstderr:\n%s", err, agent.stderrString())
	}
}

func TestFakeOpenCodeExecutable(t *testing.T) {
	if os.Getenv(envFakeOpenCodeHelper) != "1" {
		return
	}
	if err := runFakeOpenCodeServer(os.Args, os.Getenv(envFakeOpenCodeMode)); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

func startAgentWithOpenCodePath(t *testing.T, ctx context.Context, opencodePath string, home string) *liveAgent {
	t.Helper()
	cmd := agentCommand(ctx,
		"-path", opencodePath,
		"-home", home,
		"-opencode-pure",
		"-opencode-health-timeout", "5s",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	agent := &liveAgent{stdin: stdin, stdout: stdout, wait: cmd.Wait}
	cmd.Stderr = &agent.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	agent.close = func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	return agent
}

func fakeOpenCodeExecutable(t *testing.T, mode string) string {
	t.Helper()
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fake-opencode")
	script := fmt.Sprintf(`#!/bin/sh
%s=1 %s=%s exec %q -test.run '^TestFakeOpenCodeExecutable$' -- "$@"
`, envFakeOpenCodeHelper, envFakeOpenCodeMode, mode, testBinary)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake opencode executable: %v", err)
	}
	return path
}

func runFakeOpenCodeServer(args []string, mode string) error {
	port := ""
	for i, arg := range args {
		if arg == "--port" && i+1 < len(args) {
			port = args[i+1]
			break
		}
	}
	if port == "" {
		return fmt.Errorf("fake opencode missing --port in args %q", strings.Join(args, " "))
	}
	if mode == "" {
		mode = fakeModeOK
	}

	_, _ = fmt.Fprintln(os.Stdout, "native stdout noise before HTTP readiness")
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/global/health":
			writeFakeJSON(w, map[string]any{"healthy": true, "version": "9.0.0"})
		case r.URL.Path == "/doc":
			writeFakeJSON(w, fakeOpenCodeDoc(mode))
		case r.URL.Path == "/event":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"type\":\"server.connected\",\"properties\":{}}\n\n")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			<-r.Context().Done()
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			writeFakeJSON(w, fakeNativeSession("native-fake"))
		case r.URL.Path == "/session/native-fake" && r.Method == http.MethodGet:
			writeFakeJSON(w, fakeNativeSession("native-fake"))
		case r.URL.Path == "/session/native-fake/todo" && r.Method == http.MethodGet:
			writeFakeJSON(w, []any{})
		case r.URL.Path == "/session/native-fake/abort" && r.Method == http.MethodPost:
			writeFakeJSON(w, map[string]any{"ok": true})
		case r.URL.Path == "/config/providers":
			writeFakeJSON(w, map[string]any{"providers": []map[string]any{{
				"id":   "openai",
				"name": "OpenAI",
				"models": map[string]any{
					"gpt-test": map[string]any{"id": "gpt-test", "name": "GPT Test"},
				},
			}}})
		case r.URL.Path == "/agent":
			writeFakeJSON(w, []map[string]any{{"name": "build", "description": "Build"}})
		default:
			http.NotFound(w, r)
		}
	})
	server := &http.Server{Addr: "127.0.0.1:" + port, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	return server.ListenAndServe()
}

func fakeOpenCodeDoc(mode string) map[string]any {
	required := []string{
		"/config/providers",
		"/event",
		"/session",
		"/session/{sessionID}",
		"/session/{sessionID}/message",
		"/session/{sessionID}/abort",
		"/session/{sessionID}/fork",
		"/session/{sessionID}/todo",
		"/session/{sessionID}/revert",
		"/session/{sessionID}/unrevert",
		"/api/session/{sessionID}/permission/{requestID}/reply",
		"/api/permission/request",
		"/api/session/{sessionID}/question/{requestID}/reply",
		"/api/session/{sessionID}/question/{requestID}/reject",
		"/api/question/request",
	}
	paths := map[string]any{}
	for _, path := range required {
		if mode == fakeModeMissingDoc && path == "/api/session/{sessionID}/permission/{requestID}/reply" {
			continue
		}
		paths[path] = map[string]any{}
	}
	return map[string]any{"paths": paths}
}

func fakeNativeSession(id string) map[string]any {
	return map[string]any{
		"id":    id,
		"title": "Fake",
		"agent": "build",
		"model": map[string]any{
			"providerID": "openai",
			"modelID":    "gpt-test",
		},
		"time": map[string]any{"updated": time.Now().UnixMilli()},
	}
}

func writeFakeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
