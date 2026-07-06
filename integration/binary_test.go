//go:build integration

package integration

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestOpenCodeACPAgentBinaryClosedInput(t *testing.T) {
	requireRunIntegration(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := agentCommand(ctx,
		"-path", integrationOpenCodePath(t),
		"-home", t.TempDir(),
		"-opencode-pure",
		"-opencode-log-level", "INFO",
	)
	cmd.Stdin = strings.NewReader("")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("run acp-go-opencode: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty ACP stream for closed input", stdout.String())
	}
}
