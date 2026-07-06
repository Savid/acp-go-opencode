package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/coder/acp-go-sdk"
	opencodeacp "github.com/savid/acp-go-opencode"
)

const (
	defaultSessionFile = "session.jsonl"
	defaultPrompt      = "Reply with exactly RESUME_OK and do not use tools."
)

type agentConnection interface {
	Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error)
	LoadSession(context.Context, acp.LoadSessionRequest) (acp.LoadSessionResponse, error)
	Prompt(context.Context, acp.PromptRequest) (acp.PromptResponse, error)
	CloseSession(context.Context, acp.CloseSessionRequest) (acp.CloseSessionResponse, error)
}

var _ agentConnection = (*opencodeacp.Agent)(nil)

var (
	runMain   = run
	runLoaded = runLoadedSession
	getwd     = os.Getwd
	exit      = os.Exit
	newAgent  = func(store opencodeacp.SessionStore, opencodePath string, opencodeHome string) agentConnection {
		return opencodeacp.NewAgent(
			opencodeacp.WithSessionStore(store),
			opencodeacp.WithExecutablePath(opencodePath),
			opencodeacp.WithHome(opencodeHome),
			opencodeacp.WithLogger(slog.New(slog.DiscardHandler)),
		)
	}
)

func main() {
	if err := runMain(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "resume-from-file: %v\n", err)
		exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) error {
	flags := flag.NewFlagSet("resume-from-file", flag.ContinueOnError)
	flags.SetOutput(stderr)

	sessionFile := flags.String("file", defaultSessionFile, "OpenCode state transcript JSONL file")
	sessionID := flags.String("session", "", "session id; defaults to the sessionId found in the JSONL")
	cwd := flags.String("cwd", "", "session cwd; defaults to the JSONL cwd or current directory")
	prompt := flags.String("prompt", defaultPrompt, "prompt to send after loading history")
	opencodePath := flags.String("path", "", "path to opencode CLI")
	opencodeHome := flags.String("home", "", "parent root for isolated OpenCode session state")

	if err := flags.Parse(args); err != nil {
		return err
	}

	entries, inferredSessionID, inferredCwd, err := readTranscriptJSONL(*sessionFile)
	if err != nil {
		return err
	}

	if *sessionID == "" {
		*sessionID = inferredSessionID
	}

	if *sessionID == "" {
		return errors.New("session id is required")
	}

	if *cwd == "" {
		*cwd = inferredCwd
	}

	if *cwd == "" {
		*cwd, err = getwd()
		if err != nil {
			return err
		}
	}

	store := opencodeacp.NewInMemorySessionStore()
	if err := store.Replace(ctx, opencodeacp.SessionKey{SessionID: *sessionID}, []opencodeacp.SessionStoreReplacement{{
		Key:     opencodeacp.SessionKey{SessionID: *sessionID},
		Entries: entries,
	}}); err != nil {
		return err
	}

	return runLoaded(ctx, store, *sessionID, *cwd, *prompt, *opencodePath, *opencodeHome, stdout)
}

func runLoadedSession(
	ctx context.Context,
	store opencodeacp.SessionStore,
	sessionID string,
	cwd string,
	prompt string,
	opencodePath string,
	opencodeHome string,
	stdout io.Writer,
) error {
	agent := newAgent(store, opencodePath, opencodeHome)

	if _, err := agent.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}); err != nil {
		return err
	}

	id := acp.SessionId(sessionID)

	if _, err := agent.LoadSession(ctx, opencodeacp.LoadSessionRequest(id, cwd)); err != nil {
		return err
	}

	defer func() {
		_, _ = agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: id})
	}()

	fmt.Fprintln(stdout, "== resume smoke test ==")

	resp, err := agent.Prompt(ctx, opencodeacp.TextPromptRequest(id, prompt))
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "\n\nstop reason: %s\n", resp.StopReason)

	return nil
}

func readTranscriptJSONL(path string) ([]opencodeacp.SessionStoreEntry, string, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, "", "", err
	}
	defer file.Close()

	var (
		entries   []opencodeacp.SessionStoreEntry
		sessionID string
		cwd       string
	)

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		entry := opencodeacp.SessionStoreEntry(append([]byte(nil), line...))
		entries = append(entries, entry)

		var record struct {
			SessionID string `json:"sessionId"`
			Cwd       string `json:"cwd"`
			Session   struct {
				SessionID string `json:"sessionId"`
				Cwd       string `json:"cwd"`
			} `json:"session"`
		}
		if json.Unmarshal(entry, &record) == nil {
			if sessionID == "" {
				sessionID = coalesce(record.SessionID, record.Session.SessionID)
			}

			if cwd == "" {
				cwd = coalesce(record.Cwd, record.Session.Cwd)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, "", "", err
	}

	return entries, sessionID, cwd, nil
}

func coalesce(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return ""
}
