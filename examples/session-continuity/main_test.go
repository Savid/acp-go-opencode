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

func TestRunSessionContinuity(t *testing.T) {
	conn := &fakeSessionConnection{}
	var stdout bytes.Buffer

	result, err := runSessionContinuity(context.Background(), conn, "/repo", &stdout)
	if err != nil {
		t.Fatalf("runSessionContinuity returned error: %v", err)
	}
	if result.sessionID != "session-1" || result.forkID != "fork-1" {
		t.Fatalf("result = %#v", result)
	}
	if !conn.initialized || !conn.setMode || !conn.listed || !conn.loaded || !conn.resumed || !conn.forked {
		t.Fatalf("conn state = %#v", conn)
	}
	if !strings.Contains(stdout.String(), "session: session-1") ||
		!strings.Contains(stdout.String(), "listed sessions: 1") ||
		!strings.Contains(stdout.String(), "fork: fork-1") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if got := strings.Join(conn.closed, ","); got != "fork-1,session-1" {
		t.Fatalf("closed sessions = %#v", conn.closed)
	}

	for _, step := range []string{"initialize", "new", "set", "list", "load", "resume", "fork"} {
		if _, err := runSessionContinuity(context.Background(), &fakeSessionConnection{failStep: step}, "/repo", io.Discard); err == nil {
			t.Fatalf("%s error path succeeded", step)
		}
	}
}

func TestRunSessionFileContinuity(t *testing.T) {
	originalExport := exportSession
	originalImport := importSessionFile
	originalDelete := deleteSession
	originalMkdirTemp := mkdirTemp
	originalRemoveAll := removeAll
	originalWriteFile := writeFile
	t.Cleanup(func() {
		exportSession = originalExport
		importSessionFile = originalImport
		deleteSession = originalDelete
		mkdirTemp = originalMkdirTemp
		removeAll = originalRemoveAll
		writeFile = originalWriteFile
	})

	var deleted []string
	removed := false
	exportSession = func(context.Context, acp.SessionId, ...opencodeacp.Option) ([]byte, error) {
		return []byte(`{"info":{"id":"session-1"},"messages":[]}`), nil
	}
	importSessionFile = func(_ context.Context, path string, _ ...opencodeacp.Option) ([]byte, error) {
		if filepath.Base(path) != "session.json" {
			return nil, errors.New("unexpected import path")
		}

		return []byte("imported"), nil
	}
	deleteSession = func(_ context.Context, sessionID acp.SessionId, _ ...opencodeacp.Option) error {
		deleted = append(deleted, string(sessionID))

		return nil
	}
	mkdirTemp = func(string, string) (string, error) {
		return t.TempDir(), nil
	}
	removeAll = func(string) error {
		removed = true

		return nil
	}

	var stdout bytes.Buffer
	if err := runSessionFileContinuity(context.Background(), "session-1", &stdout); err != nil {
		t.Fatalf("runSessionFileContinuity returned error: %v", err)
	}
	if !strings.Contains(stdout.String(), "exported bytes:") ||
		!strings.Contains(stdout.String(), "reimported session: session-1") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if strings.Join(deleted, ",") != "session-1" {
		t.Fatalf("deleted = %#v", deleted)
	}
	if !removed {
		t.Fatal("temporary export directory was not removed")
	}

	for _, tc := range []struct {
		name  string
		setup func()
	}{
		{
			name: "export",
			setup: func() {
				exportSession = func(context.Context, acp.SessionId, ...opencodeacp.Option) ([]byte, error) {
					return nil, errors.New("export failed")
				}
			},
		},
		{
			name: "export session id",
			setup: func() {
				exportSession = func(context.Context, acp.SessionId, ...opencodeacp.Option) ([]byte, error) {
					return []byte(`{"info":{}}`), nil
				}
			},
		},
		{
			name: "export id mismatch",
			setup: func() {
				exportSession = func(context.Context, acp.SessionId, ...opencodeacp.Option) ([]byte, error) {
					return []byte(`{"info":{"id":"other"}}`), nil
				}
			},
		},
		{
			name: "mkdir",
			setup: func() {
				mkdirTemp = func(string, string) (string, error) {
					return "", errors.New("mkdir failed")
				}
			},
		},
		{
			name: "write",
			setup: func() {
				writeFile = func(string, []byte, os.FileMode) error {
					return errors.New("write failed")
				}
			},
		},
		{
			name: "delete",
			setup: func() {
				deleteSession = func(context.Context, acp.SessionId, ...opencodeacp.Option) error {
					return errors.New("delete failed")
				}
			},
		},
		{
			name: "import",
			setup: func() {
				importSessionFile = func(context.Context, string, ...opencodeacp.Option) ([]byte, error) {
					return nil, errors.New("import failed")
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exportSession = originalExport
			importSessionFile = originalImport
			deleteSession = originalDelete
			mkdirTemp = originalMkdirTemp
			removeAll = originalRemoveAll
			writeFile = originalWriteFile

			exportSession = func(context.Context, acp.SessionId, ...opencodeacp.Option) ([]byte, error) {
				return []byte(`{"info":{"id":"session-1"},"messages":[]}`), nil
			}
			importSessionFile = func(context.Context, string, ...opencodeacp.Option) ([]byte, error) {
				return []byte("imported"), nil
			}
			deleteSession = func(context.Context, acp.SessionId, ...opencodeacp.Option) error {
				return nil
			}
			tc.setup()

			if err := runSessionFileContinuity(context.Background(), "session-1", io.Discard); err == nil {
				t.Fatalf("%s error path succeeded", tc.name)
			}
		})
	}
}

func TestRunImportedSessionContinuity(t *testing.T) {
	conn := &fakeSessionConnection{}
	var stdout bytes.Buffer

	if err := runImportedSessionContinuity(
		context.Background(),
		conn,
		"/repo",
		"session-1",
		"",
		strings.NewReader("\n"),
		&stdout,
	); err != nil {
		t.Fatalf("runImportedSessionContinuity returned error: %v", err)
	}
	if !conn.initialized || !conn.listed || !conn.loaded || !conn.resumed || conn.prompted {
		t.Fatalf("conn state = %#v", conn)
	}
	if !strings.Contains(stdout.String(), "loaded imported session: session-1") ||
		!strings.Contains(stdout.String(), "resumed imported session: session-1") {
		t.Fatalf("stdout = %q", stdout.String())
	}

	promptConn := &fakeSessionConnection{}
	stdout.Reset()
	if err := runImportedSessionContinuity(
		context.Background(),
		promptConn,
		"/repo",
		"session-1",
		"resume smoke",
		strings.NewReader(""),
		&stdout,
	); err != nil {
		t.Fatalf("prompted runImportedSessionContinuity returned error: %v", err)
	}
	if !promptConn.prompted || promptConn.promptText != "resume smoke" {
		t.Fatalf("prompt state = %#v", promptConn)
	}
	if !strings.Contains(stdout.String(), "== resumed prompt ==") ||
		!strings.Contains(stdout.String(), "prompt stop reason: end_turn") {
		t.Fatalf("prompt stdout = %q", stdout.String())
	}

	typedConn := &fakeSessionConnection{}
	stdout.Reset()
	if err := runImportedSessionContinuity(
		context.Background(),
		typedConn,
		"/repo",
		"session-1",
		"",
		strings.NewReader("typed resume\n"),
		&stdout,
	); err != nil {
		t.Fatalf("typed runImportedSessionContinuity returned error: %v", err)
	}
	if !typedConn.prompted || typedConn.promptText != "typed resume" {
		t.Fatalf("typed prompt state = %#v", typedConn)
	}
	if !strings.Contains(stdout.String(), "enter one prompt") ||
		!strings.Contains(stdout.String(), "== typed prompt ==") ||
		!strings.Contains(stdout.String(), "prompt stop reason: end_turn") {
		t.Fatalf("typed stdout = %q", stdout.String())
	}

	for _, step := range []string{"initialize", "list", "load", "resume"} {
		if err := runImportedSessionContinuity(context.Background(), &fakeSessionConnection{failStep: step}, "/repo", "session-1", "", strings.NewReader("\n"), io.Discard); err == nil {
			t.Fatalf("%s imported error path succeeded", step)
		}
	}
	if err := runImportedSessionContinuity(context.Background(), &fakeSessionConnection{listSessionID: "other"}, "/repo", "session-1", "", strings.NewReader("\n"), io.Discard); err == nil {
		t.Fatal("missing imported session path succeeded")
	}
	if err := runImportedSessionContinuity(context.Background(), &fakeSessionConnection{failStep: "prompt"}, "/repo", "session-1", "resume smoke", strings.NewReader(""), io.Discard); err == nil {
		t.Fatal("prompt imported error path succeeded")
	}
	if err := runImportedSessionContinuity(context.Background(), &fakeSessionConnection{}, "/repo", "session-1", "", errReader{}, io.Discard); err == nil {
		t.Fatal("typed prompt read error path succeeded")
	}

	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	stdout.Reset()
	if err := runImportedSessionContinuity(cancelCtx, &fakeSessionConnection{}, "/repo", "session-1", "", strings.NewReader("ignored\n"), &stdout); err != nil {
		t.Fatalf("interrupted typed prompt returned error: %v", err)
	}
	if !strings.Contains(stdout.String(), "interrupted; closing session") {
		t.Fatalf("interrupted stdout = %q", stdout.String())
	}
}

func TestParseConfig(t *testing.T) {
	originalGetwd := getwd
	originalAbsPath := absPath
	originalRuntimeCaller := runtimeCaller
	t.Cleanup(func() {
		getwd = originalGetwd
		absPath = originalAbsPath
		runtimeCaller = originalRuntimeCaller
	})

	getwd = func() (string, error) { return "/repo", nil }
	cfg, err := parseConfig(nil, io.Discard)
	if err != nil {
		t.Fatalf("parseConfig default returned error: %v", err)
	}
	if cfg.cwd != "/repo" || cfg.prompt != "" {
		t.Fatalf("default config = %#v", cfg)
	}
	if filepath.Base(cfg.sessionFile) != defaultSessionFile {
		t.Fatalf("default session file = %q", cfg.sessionFile)
	}

	cfg, err = parseConfig([]string{"-cwd", "/other", "-prompt", "flag prompt"}, io.Discard)
	if err != nil {
		t.Fatalf("parseConfig flags returned error: %v", err)
	}
	if cfg.cwd != "/other" || cfg.prompt != "flag prompt" {
		t.Fatalf("flag config = %#v", cfg)
	}

	cfg, err = parseConfig([]string{"-cwd", "/other", "positional", "prompt"}, io.Discard)
	if err != nil {
		t.Fatalf("parseConfig positional returned error: %v", err)
	}
	if cfg.cwd != "/other" || cfg.prompt != "positional prompt" {
		t.Fatalf("positional config = %#v", cfg)
	}

	cfg, err = parseConfig([]string{"-cwd", "/other", "-session-file", "session.json"}, io.Discard)
	if err != nil {
		t.Fatalf("parseConfig session file returned error: %v", err)
	}
	if !filepath.IsAbs(cfg.sessionFile) {
		t.Fatalf("session file is not absolute: %q", cfg.sessionFile)
	}

	runtimeCaller = func(int) (uintptr, string, int, bool) { return 0, "", 0, false }
	cfg, err = parseConfig([]string{"-cwd", "/other"}, io.Discard)
	if err != nil {
		t.Fatalf("parseConfig fallback session file returned error: %v", err)
	}
	if filepath.Base(cfg.sessionFile) != defaultSessionFile {
		t.Fatalf("fallback session file = %q", cfg.sessionFile)
	}

	if _, err := parseConfig([]string{"-bad"}, io.Discard); err == nil {
		t.Fatal("parseConfig accepted unknown flag")
	}
	if _, err := parseConfig([]string{"-cwd", "relative"}, io.Discard); err == nil {
		t.Fatal("parseConfig accepted relative cwd")
	}
	absPath = func(string) (string, error) {
		return "", errors.New("abs failed")
	}
	if _, err := parseConfig([]string{"-cwd", "/other", "-session-file", "relative.json"}, io.Discard); err == nil {
		t.Fatal("parseConfig accepted failed session-file abs")
	}
	if got := newOpenCodeID("ses_"); !strings.HasPrefix(got, "ses_acpgo") {
		t.Fatalf("newOpenCodeID = %q", got)
	}
}

func TestImportSavedSession(t *testing.T) {
	originalReadFile := readFile
	originalImport := importSessionFile
	originalMkdirTemp := mkdirTemp
	originalRemoveAll := removeAll
	originalWriteFile := writeFile
	originalNextID := nextID
	t.Cleanup(func() {
		readFile = originalReadFile
		importSessionFile = originalImport
		mkdirTemp = originalMkdirTemp
		removeAll = originalRemoveAll
		writeFile = originalWriteFile
		nextID = originalNextID
	})

	readFile = func(string) ([]byte, error) {
		return []byte(savedSessionFixtureJSON), nil
	}
	mkdirTemp = func(string, string) (string, error) {
		return t.TempDir(), nil
	}
	removed := false
	removeAll = func(string) error {
		removed = true

		return nil
	}
	var generated []byte
	writeFile = func(_ string, data []byte, perm os.FileMode) error {
		if perm != 0o600 {
			return errors.New("unexpected file mode")
		}
		generated = append([]byte(nil), data...)

		return nil
	}
	importSessionFile = func(_ context.Context, path string, _ ...opencodeacp.Option) ([]byte, error) {
		if filepath.Base(path) != "session.json" {
			return nil, errors.New("unexpected import path")
		}

		return []byte("imported"), nil
	}
	nextID = fixedIDs(
		"ses_saved",
		"msg_user",
		"prt_user",
		"msg_assistant",
		"prt_step",
		"prt_reason",
		"prt_text",
		"prt_finish",
	)

	var stdout bytes.Buffer
	sessionID, err := importSavedSession(context.Background(), "/fixture/session.json", "/repo", &stdout)
	if err != nil {
		t.Fatalf("importSavedSession returned error: %v", err)
	}
	if sessionID != "ses_saved" || !removed {
		t.Fatalf("sessionID=%q removed=%v", sessionID, removed)
	}
	if !strings.Contains(stdout.String(), "saved session file: /fixture/session.json") ||
		!strings.Contains(stdout.String(), "imported saved session: ses_saved") {
		t.Fatalf("stdout = %q", stdout.String())
	}

	var decoded savedSessionExport
	if err := json.Unmarshal(generated, &decoded); err != nil {
		t.Fatalf("generated JSON did not decode: %v\n%s", err, generated)
	}
	if decoded.Info["id"] != "ses_saved" ||
		decoded.Info["directory"] != "/repo" ||
		decoded.Info["path"] != "repo" {
		t.Fatalf("generated info = %#v", decoded.Info)
	}
	if decoded.Messages[0].Info["id"] != "msg_user" ||
		decoded.Messages[1].Info["id"] != "msg_assistant" ||
		decoded.Messages[1].Info["parentID"] != "msg_user" {
		t.Fatalf("generated messages = %#v", decoded.Messages)
	}
	if decoded.Messages[0].Parts[0]["id"] != "prt_user" ||
		decoded.Messages[1].Parts[2]["messageID"] != "msg_assistant" {
		t.Fatalf("generated parts = %#v", decoded.Messages)
	}
}

func TestImportSavedSessionErrors(t *testing.T) {
	originalReadFile := readFile
	originalImport := importSessionFile
	originalMkdirTemp := mkdirTemp
	originalWriteFile := writeFile
	originalNextID := nextID
	t.Cleanup(func() {
		readFile = originalReadFile
		importSessionFile = originalImport
		mkdirTemp = originalMkdirTemp
		writeFile = originalWriteFile
		nextID = originalNextID
	})

	for _, tc := range []struct {
		name  string
		setup func()
		want  string
	}{
		{
			name: "read",
			setup: func() {
				readFile = func(string) ([]byte, error) {
					return nil, errors.New("read failed")
				}
			},
			want: "read failed",
		},
		{
			name: "rewrite",
			setup: func() {
				readFile = func(string) ([]byte, error) {
					return []byte(`{`), nil
				}
			},
			want: "parse saved session JSON",
		},
		{
			name: "mkdir",
			setup: func() {
				mkdirTemp = func(string, string) (string, error) {
					return "", errors.New("mkdir failed")
				}
			},
			want: "mkdir failed",
		},
		{
			name: "write",
			setup: func() {
				writeFile = func(string, []byte, os.FileMode) error {
					return errors.New("write failed")
				}
			},
			want: "write failed",
		},
		{
			name: "import",
			setup: func() {
				importSessionFile = func(context.Context, string, ...opencodeacp.Option) ([]byte, error) {
					return nil, errors.New("import failed")
				}
			},
			want: "import failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			readFile = func(string) ([]byte, error) {
				return []byte(savedSessionFixtureJSON), nil
			}
			mkdirTemp = func(string, string) (string, error) {
				return t.TempDir(), nil
			}
			writeFile = func(string, []byte, os.FileMode) error {
				return nil
			}
			importSessionFile = func(context.Context, string, ...opencodeacp.Option) ([]byte, error) {
				return []byte("imported"), nil
			}
			nextID = fixedIDs("ses_saved", "msg_user", "prt_user", "msg_assistant", "prt_step", "prt_reason", "prt_text", "prt_finish")
			tc.setup()

			_, err := importSavedSession(context.Background(), "/fixture/session.json", "/repo", io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRewriteSavedSessionExportErrors(t *testing.T) {
	originalNextID := nextID
	t.Cleanup(func() { nextID = originalNextID })

	nextID = fixedIDs("ses_saved")
	if _, _, err := rewriteSavedSessionExport([]byte(`{}`), "/repo"); err == nil {
		t.Fatal("missing info succeeded")
	}
	if _, _, err := rewriteSavedSessionExport([]byte(`{"info":{}}`), "/repo"); err == nil {
		t.Fatal("missing info.id succeeded")
	}

	nextID = fixedIDs("ses_saved", "prt_only")
	rewritten, sessionID, err := rewriteSavedSessionExport([]byte(`{"info":{"id":"old-session"},"messages":[{"parts":[{"id":"old-part","sessionID":"old-session"}]}]}`), "/repo")
	if err != nil {
		t.Fatalf("rewrite sparse session returned error: %v", err)
	}
	if sessionID != "ses_saved" || !strings.Contains(string(rewritten), "prt_only") {
		t.Fatalf("sparse rewrite session=%q json=%s", sessionID, rewritten)
	}

	listValue := []any{map[string]any{"id": "old"}}
	replaceIDs(listValue, map[string]string{"old": "new"})
	if listValue[0].(map[string]any)["id"] != "new" {
		t.Fatalf("replaceIDs list value = %#v", listValue)
	}
}

func TestReadTypedPromptCancellationAfterReadStarts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := &blockingReader{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	done := make(chan error, 1)

	go func() {
		_, err := readTypedPrompt(ctx, reader, io.Discard)
		done <- err
	}()

	<-reader.started
	cancel()
	err := <-done
	close(reader.release)
	if !errors.Is(err, errPromptInterrupted) {
		t.Fatalf("readTypedPrompt error = %v", err)
	}
}

func TestRunUsesInjectedAgent(t *testing.T) {
	originalStart := startAgent
	originalGetwd := getwd
	originalExit := exit
	originalInput := input
	originalArgs := args
	originalExport := exportSession
	originalImport := importSessionFile
	originalImportSaved := importSaved
	originalDelete := deleteSession
	originalMkdirTemp := mkdirTemp
	t.Cleanup(func() {
		startAgent = originalStart
		getwd = originalGetwd
		exit = originalExit
		input = originalInput
		args = originalArgs
		exportSession = originalExport
		importSessionFile = originalImport
		importSaved = originalImportSaved
		deleteSession = originalDelete
		mkdirTemp = originalMkdirTemp
	})

	starts := 0
	closed := false
	waited := false
	startAgent = func(_ context.Context, cwd string, _ io.Writer, _ io.Writer) (*startedAgent, error) {
		if cwd != "/repo" {
			return nil, errors.New("unexpected cwd")
		}
		starts++
		conn := &fakeSessionConnection{}
		if starts%2 == 0 {
			conn.expectedSessionID = "saved-session"
		}

		return &startedAgent{
			conn:  conn,
			close: func() { closed = true },
			wait:  func() error { waited = true; return nil },
		}, nil
	}
	exportSession = func(context.Context, acp.SessionId, ...opencodeacp.Option) ([]byte, error) {
		return []byte(`{"info":{"id":"session-1"},"messages":[]}`), nil
	}
	importSessionFile = func(context.Context, string, ...opencodeacp.Option) ([]byte, error) {
		return []byte("imported"), nil
	}
	importSaved = func(context.Context, string, string, io.Writer, ...opencodeacp.Option) (acp.SessionId, error) {
		return "saved-session", nil
	}
	var deleted []string
	deleteSession = func(_ context.Context, sessionID acp.SessionId, _ ...opencodeacp.Option) error {
		deleted = append(deleted, string(sessionID))

		return nil
	}
	mkdirTemp = func(string, string) (string, error) {
		return t.TempDir(), nil
	}
	getwd = func() (string, error) { return "/repo", nil }
	input = strings.NewReader("\n\n")
	args = nil

	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), &stdout, &stderr); code != 0 {
		t.Fatalf("run code = %d stderr=%q", code, stderr.String())
	}
	if starts != 2 || !closed || !waited {
		t.Fatalf("starts=%d closed=%v waited=%v", starts, closed, waited)
	}
	if strings.Join(deleted, ",") != "session-1,saved-session,session-1,fork-1" {
		t.Fatalf("deleted = %#v", deleted)
	}

	exitCode := -1
	exit = func(code int) { exitCode = code }
	input = strings.NewReader("\n")
	main()
	if exitCode != 0 {
		t.Fatalf("main exit = %d", exitCode)
	}
}

func TestRunErrors(t *testing.T) {
	originalStart := startAgent
	originalGetwd := getwd
	originalInput := input
	originalArgs := args
	originalExport := exportSession
	originalImport := importSessionFile
	originalImportSaved := importSaved
	originalDelete := deleteSession
	originalMkdirTemp := mkdirTemp
	t.Cleanup(func() {
		startAgent = originalStart
		getwd = originalGetwd
		input = originalInput
		args = originalArgs
		exportSession = originalExport
		importSessionFile = originalImport
		importSaved = originalImportSaved
		deleteSession = originalDelete
		mkdirTemp = originalMkdirTemp
	})

	args = nil
	input = strings.NewReader(strings.Repeat("\n", 20))
	getwd = func() (string, error) { return "", errors.New("cwd failed") }
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "cwd failed") {
		t.Fatalf("cwd failure code=%d stderr=%q", code, stderr.String())
	}

	getwd = func() (string, error) { return "/repo", nil }
	startAgent = func(context.Context, string, io.Writer, io.Writer) (*startedAgent, error) {
		return nil, errors.New("start failed")
	}
	stderr.Reset()
	if code := run(context.Background(), &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "start failed") {
		t.Fatalf("start failure code=%d stderr=%q", code, stderr.String())
	}

	startAgent = func(context.Context, string, io.Writer, io.Writer) (*startedAgent, error) {
		return &startedAgent{conn: &fakeSessionConnection{failStep: "list"}}, nil
	}
	stderr.Reset()
	if code := run(context.Background(), &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "list failed") {
		t.Fatalf("conversation failure code=%d stderr=%q", code, stderr.String())
	}

	startAgent = func(context.Context, string, io.Writer, io.Writer) (*startedAgent, error) {
		return &startedAgent{conn: &fakeSessionConnection{}}, nil
	}
	exportSession = func(context.Context, acp.SessionId, ...opencodeacp.Option) ([]byte, error) {
		return nil, errors.New("export failed")
	}
	stderr.Reset()
	if code := run(context.Background(), &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "export failed") {
		t.Fatalf("export failure code=%d stderr=%q", code, stderr.String())
	}

	exportSession = func(context.Context, acp.SessionId, ...opencodeacp.Option) ([]byte, error) {
		return []byte(`{"info":{"id":"session-1"},"messages":[]}`), nil
	}
	importSessionFile = func(context.Context, string, ...opencodeacp.Option) ([]byte, error) {
		return []byte("imported"), nil
	}
	importSaved = func(context.Context, string, string, io.Writer, ...opencodeacp.Option) (acp.SessionId, error) {
		return "saved-session", nil
	}
	deleteSession = func(context.Context, acp.SessionId, ...opencodeacp.Option) error {
		return nil
	}
	mkdirTemp = func(string, string) (string, error) {
		return t.TempDir(), nil
	}

	importSaved = func(context.Context, string, string, io.Writer, ...opencodeacp.Option) (acp.SessionId, error) {
		return "", errors.New("saved import failed")
	}
	stderr.Reset()
	if code := run(context.Background(), &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "saved import failed") {
		t.Fatalf("saved import failure code=%d stderr=%q", code, stderr.String())
	}

	importSaved = func(context.Context, string, string, io.Writer, ...opencodeacp.Option) (acp.SessionId, error) {
		return "saved-session", nil
	}

	starts := 0
	startAgent = func(context.Context, string, io.Writer, io.Writer) (*startedAgent, error) {
		starts++
		if starts == 2 {
			return nil, errors.New("restart failed")
		}

		return &startedAgent{conn: &fakeSessionConnection{}}, nil
	}
	stderr.Reset()
	if code := run(context.Background(), &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "restart failed") {
		t.Fatalf("restart failure code=%d stderr=%q", code, stderr.String())
	}

	starts = 0
	startAgent = func(context.Context, string, io.Writer, io.Writer) (*startedAgent, error) {
		starts++
		conn := &fakeSessionConnection{}
		if starts == 2 {
			conn.expectedSessionID = "saved-session"
			conn.failStep = "load"
		}

		return &startedAgent{conn: conn}, nil
	}
	stderr.Reset()
	if code := run(context.Background(), &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "load failed") {
		t.Fatalf("imported load failure code=%d stderr=%q", code, stderr.String())
	}

	starts = 0
	args = []string{"-prompt", "resume smoke"}
	startAgent = func(context.Context, string, io.Writer, io.Writer) (*startedAgent, error) {
		starts++
		conn := &fakeSessionConnection{}
		if starts == 2 {
			conn.expectedSessionID = "saved-session"
			conn.failStep = "prompt"
		}

		return &startedAgent{conn: conn}, nil
	}
	stderr.Reset()
	if code := run(context.Background(), &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "prompt failed") {
		t.Fatalf("imported prompt failure code=%d stderr=%q", code, stderr.String())
	}

	starts = 0
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	stderr.Reset()
	if code := run(cancelCtx, &stdout, &stderr); code != 130 ||
		!strings.Contains(stderr.String(), "context canceled") {
		t.Fatalf("canceled imported prompt failure code=%d stderr=%q", code, stderr.String())
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
}

func TestClientHelpers(t *testing.T) {
	c := &client{}
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

	if err := c.SessionUpdate(context.Background(), acp.SessionNotification{}); err != nil {
		t.Fatalf("SessionUpdate returned error: %v", err)
	}
	var updateOutput bytes.Buffer
	display := &client{output: &updateOutput}
	status := acp.ToolCallStatusCompleted
	for _, update := range []acp.SessionUpdate{
		{UserMessageChunk: &acp.SessionUpdateUserMessageChunk{Content: acp.TextBlock("user text")}},
		{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("agent text")}},
		{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock(" plus")}},
		{AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{Content: acp.TextBlock("thought text")}},
		{AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{Content: acp.TextBlock(" more")}},
		{ToolCall: &acp.SessionUpdateToolCall{ToolCallId: "tool-1", Title: "Read file"}},
		{ToolCallUpdate: &acp.SessionToolCallUpdate{ToolCallId: "tool-1", Status: &status}},
	} {
		if err := display.SessionUpdate(context.Background(), acp.SessionNotification{Update: update}); err != nil {
			t.Fatalf("display SessionUpdate returned error: %v", err)
		}
	}
	for _, want := range []string{"[user] user text", "agent text", "[thought] thought text", "[tool] tool-1 Read file", "[tool] tool-1 completed"} {
		if !strings.Contains(updateOutput.String(), want) {
			t.Fatalf("display output missing %q: %q", want, updateOutput.String())
		}
	}
	if strings.Count(updateOutput.String(), "[thought]") != 1 {
		t.Fatalf("thought label count in output = %q", updateOutput.String())
	}
	if terminal, err := c.CreateTerminal(context.Background(), acp.CreateTerminalRequest{}); err != nil ||
		terminal.TerminalId == "" {
		t.Fatalf("CreateTerminal = %#v err=%v", terminal, err)
	}
	if _, err := c.KillTerminal(context.Background(), acp.KillTerminalRequest{}); err != nil {
		t.Fatalf("KillTerminal returned error: %v", err)
	}
	if output, err := c.TerminalOutput(context.Background(), acp.TerminalOutputRequest{}); err != nil || output.Output != "" {
		t.Fatalf("TerminalOutput = %#v err=%v", output, err)
	}
	if _, err := c.ReleaseTerminal(context.Background(), acp.ReleaseTerminalRequest{}); err != nil {
		t.Fatalf("ReleaseTerminal returned error: %v", err)
	}
	if _, err := c.WaitForTerminalExit(context.Background(), acp.WaitForTerminalExitRequest{}); err != nil {
		t.Fatalf("WaitForTerminalExit returned error: %v", err)
	}

	if result, err := c.HandleExtensionMethod(context.Background(), "_opencode/empty", nil); err != nil || result == nil {
		t.Fatalf("HandleExtensionMethod empty = %#v err=%v", result, err)
	}
	if result, err := c.HandleExtensionMethod(context.Background(), "_opencode/json", json.RawMessage(`{"ok":true}`)); err != nil ||
		result == nil {
		t.Fatalf("HandleExtensionMethod json = %#v err=%v", result, err)
	}
	if _, err := c.HandleExtensionMethod(context.Background(), "_opencode/bad-json", json.RawMessage(`{`)); err == nil {
		t.Fatal("HandleExtensionMethod invalid JSON succeeded")
	}
	if _, err := c.HandleExtensionMethod(context.Background(), "bad/method", nil); err == nil {
		t.Fatal("HandleExtensionMethod accepted non-extension method")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) {
	return 0, errors.New("read failed")
}

func fixedIDs(ids ...string) func(string) string {
	index := 0

	return func(prefix string) string {
		if index >= len(ids) {
			return prefix + "extra"
		}

		id := ids[index]
		index++

		return id
	}
}

const savedSessionFixtureJSON = `{
  "info": {
    "id": "old-session",
    "directory": "/old",
    "path": "old"
  },
  "messages": [
    {
      "info": {
        "id": "old-user-message",
        "role": "user",
        "sessionID": "old-session",
        "summary": {
          "diffs": []
        }
      },
      "parts": [
        {
          "id": "old-user-part",
          "type": "text",
          "text": "remember ORCHID-HARBOR-42",
          "sessionID": "old-session",
          "messageID": "old-user-message"
        }
      ]
    },
    {
      "info": {
        "id": "old-assistant-message",
        "parentID": "old-user-message",
        "role": "assistant",
        "sessionID": "old-session",
        "path": {
          "cwd": "/old",
          "root": "/"
        }
      },
      "parts": [
        {
          "id": "old-step-part",
          "type": "step-start",
          "sessionID": "old-session",
          "messageID": "old-assistant-message"
        },
        {
          "id": "old-reason-part",
          "type": "reasoning",
          "text": "remembering phrase",
          "sessionID": "old-session",
          "messageID": "old-assistant-message"
        },
        {
          "id": "old-text-part",
          "type": "text",
          "text": "Saved fixture response: ORCHID-HARBOR-42",
          "sessionID": "old-session",
          "messageID": "old-assistant-message"
        },
        {
          "id": "old-finish-part",
          "type": "step-finish",
          "sessionID": "old-session",
          "messageID": "old-assistant-message"
        }
      ]
    }
  ]
}`

type blockingReader struct {
	started chan struct{}
	release chan struct{}
}

func (r *blockingReader) Read([]byte) (int, error) {
	close(r.started)
	<-r.release

	return 0, io.EOF
}

type fakeSessionConnection struct {
	failStep          string
	expectedSessionID string
	listSessionID     string
	initialized       bool
	setMode           bool
	listed            bool
	loaded            bool
	resumed           bool
	forked            bool
	prompted          bool
	promptText        string
	closed            []string
}

func (f *fakeSessionConnection) fail(step string) error {
	if f.failStep == step {
		return errors.New(step + " failed")
	}

	return nil
}

func (f *fakeSessionConnection) expectedID() acp.SessionId {
	if f.expectedSessionID != "" {
		return acp.SessionId(f.expectedSessionID)
	}

	return "session-1"
}

func (f *fakeSessionConnection) Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error) {
	if err := f.fail("initialize"); err != nil {
		return acp.InitializeResponse{}, err
	}
	f.initialized = true

	return acp.InitializeResponse{}, nil
}

func (f *fakeSessionConnection) NewSession(_ context.Context, params acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	if err := f.fail("new"); err != nil {
		return acp.NewSessionResponse{}, err
	}
	if params.Cwd != "/repo" {
		return acp.NewSessionResponse{}, errors.New("unexpected new-session cwd")
	}

	return acp.NewSessionResponse{SessionId: "session-1"}, nil
}

func (f *fakeSessionConnection) SetSessionConfigOption(
	_ context.Context,
	params acp.SetSessionConfigOptionRequest,
) (acp.SetSessionConfigOptionResponse, error) {
	if err := f.fail("set"); err != nil {
		return acp.SetSessionConfigOptionResponse{}, err
	}
	if params.ValueId == nil ||
		params.ValueId.ConfigId != opencodeacp.OpenCodeConfigMode ||
		params.ValueId.Value != opencodeacp.OpenCodeModeBuild {
		return acp.SetSessionConfigOptionResponse{}, errors.New("unexpected config option")
	}
	f.setMode = true

	return acp.SetSessionConfigOptionResponse{ConfigOptions: []acp.SessionConfigOption{}}, nil
}

func (f *fakeSessionConnection) ListSessions(
	_ context.Context,
	params acp.ListSessionsRequest,
) (acp.ListSessionsResponse, error) {
	if err := f.fail("list"); err != nil {
		return acp.ListSessionsResponse{}, err
	}
	if params.Cwd == nil || *params.Cwd != "/repo" {
		return acp.ListSessionsResponse{}, errors.New("unexpected list cwd")
	}
	f.listed = true
	sessionID := f.expectedID()
	if f.listSessionID != "" {
		sessionID = acp.SessionId(f.listSessionID)
	}

	return acp.ListSessionsResponse{
		Sessions: []acp.SessionInfo{{SessionId: sessionID, Cwd: "/repo"}},
	}, nil
}

func (f *fakeSessionConnection) LoadSession(
	_ context.Context,
	params acp.LoadSessionRequest,
) (acp.LoadSessionResponse, error) {
	if err := f.fail("load"); err != nil {
		return acp.LoadSessionResponse{}, err
	}
	if params.SessionId != f.expectedID() || params.Cwd != "/repo" {
		return acp.LoadSessionResponse{}, errors.New("unexpected load request")
	}
	f.loaded = true

	return acp.LoadSessionResponse{}, nil
}

func (f *fakeSessionConnection) ResumeSession(
	_ context.Context,
	params acp.ResumeSessionRequest,
) (acp.ResumeSessionResponse, error) {
	if err := f.fail("resume"); err != nil {
		return acp.ResumeSessionResponse{}, err
	}
	if params.SessionId != f.expectedID() || params.Cwd != "/repo" {
		return acp.ResumeSessionResponse{}, errors.New("unexpected resume request")
	}
	f.resumed = true

	return acp.ResumeSessionResponse{}, nil
}

func (f *fakeSessionConnection) UnstableForkSession(
	_ context.Context,
	params acp.UnstableForkSessionRequest,
) (acp.UnstableForkSessionResponse, error) {
	if err := f.fail("fork"); err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}
	if params.SessionId != f.expectedID() || params.Cwd != "/repo" {
		return acp.UnstableForkSessionResponse{}, errors.New("unexpected fork request")
	}
	f.forked = true

	return acp.UnstableForkSessionResponse{SessionId: "fork-1"}, nil
}

func (f *fakeSessionConnection) Prompt(
	_ context.Context,
	params acp.PromptRequest,
) (acp.PromptResponse, error) {
	if err := f.fail("prompt"); err != nil {
		return acp.PromptResponse{}, err
	}
	if params.SessionId != f.expectedID() ||
		len(params.Prompt) != 1 ||
		params.Prompt[0].Text == nil ||
		params.Prompt[0].Text.Text == "" {
		return acp.PromptResponse{}, errors.New("unexpected prompt request")
	}
	f.prompted = true
	f.promptText = params.Prompt[0].Text.Text

	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func (f *fakeSessionConnection) CloseSession(
	_ context.Context,
	params acp.CloseSessionRequest,
) (acp.CloseSessionResponse, error) {
	f.closed = append(f.closed, string(params.SessionId))

	return acp.CloseSessionResponse{}, nil
}
