package opencodeacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/coder/acp-go-sdk"
)

// ExportSession runs `opencode export` and returns the exported session JSON.
// OpenCode prints a status line before the JSON, so the returned bytes are
// normalized to the JSON object only.
func ExportSession(ctx context.Context, sessionID acp.SessionId, opts ...Option) ([]byte, error) {
	args := []string{"export"}
	if sessionID != "" {
		args = append(args, string(sessionID))
	}

	out, err := RunOpenCodeCommand(ctx, args, opts...)
	if err != nil {
		return nil, err
	}

	data, err := exportSessionJSON(out)
	if err != nil {
		return nil, err
	}

	return data, nil
}

// ImportSessionFile runs `opencode import <path>` for an OpenCode session JSON
// file or URL. It returns the CLI output for callers that want diagnostics.
func ImportSessionFile(ctx context.Context, path string, opts ...Option) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("opencode import path is empty")
	}

	out, err := RunOpenCodeCommand(ctx, []string{"import", path}, opts...)
	if err != nil {
		return nil, err
	}
	if err := openCodeOutputError("opencode import", out); err != nil {
		return nil, err
	}

	return out, nil
}

// DeleteSession runs `opencode session delete <sessionID>`.
func DeleteSession(ctx context.Context, sessionID acp.SessionId, opts ...Option) error {
	if strings.TrimSpace(string(sessionID)) == "" {
		return errors.New("opencode session id is empty")
	}

	out, err := RunOpenCodeCommand(ctx, []string{"session", "delete", string(sessionID)}, opts...)
	if err != nil {
		return err
	}

	return openCodeOutputError("opencode session delete", out)
}

// SessionIDFromExport extracts the OpenCode session ID from exported session
// JSON. It accepts either clean JSON or raw `opencode export` output.
func SessionIDFromExport(data []byte) (acp.SessionId, error) {
	payload, err := exportSessionJSON(data)
	if err != nil {
		return "", err
	}

	var decoded struct {
		Info struct {
			ID string `json:"id"`
		} `json:"info"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return "", fmt.Errorf("parse opencode export JSON: %w", err)
	}
	if strings.TrimSpace(decoded.Info.ID) == "" {
		return "", errors.New("opencode export JSON missing info.id")
	}

	return acp.SessionId(strings.TrimSpace(decoded.Info.ID)), nil
}

func exportSessionJSON(output []byte) ([]byte, error) {
	start := bytes.IndexByte(output, '{')
	if start < 0 {
		return nil, fmt.Errorf("opencode export output did not contain JSON: %q", strings.TrimSpace(string(output)))
	}

	out := bytes.TrimSpace(output[start:])
	if !json.Valid(out) {
		return nil, fmt.Errorf("opencode export output contained invalid JSON")
	}

	return append([]byte(nil), out...), nil
}

func openCodeOutputError(command string, output []byte) error {
	text := strings.TrimSpace(string(output))
	if text == "" {
		return nil
	}
	if strings.Contains(text, "Error:") {
		return fmt.Errorf("%s: %s", command, text)
	}

	return nil
}
