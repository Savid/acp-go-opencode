//go:build unix

package opencodeacp

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

// handoffVerdictWithin runs prompt validation on its own goroutine and fails
// rather than parking the suite when the read never comes back. An open that
// blocks in the kernel cannot be interrupted, so the wait is what turns a
// regression into a failure instead of a hung run.
func handoffVerdictWithin(t *testing.T, session *session, block acp.ContentBlock, errValue, message string) {
	t.Helper()

	done := make(chan error, 1)
	go func() { done <- validatePromptMediaError(session, block) }()

	select {
	case err := <-done:
		requireInvalidParamsData(t, err, map[string]any{
			jsonFieldField:   fieldPromptImage,
			jsonFieldError:   errValue,
			jsonFieldIndex:   0,
			jsonFieldMessage: message,
		})
	case <-time.After(30 * time.Second):
		t.Fatal("the handoff read never returned")
	}
}

// TestHandoffFIFOInsideTheRootDoesNotBlock pins the read against the one file
// kind that can hold a turn open indefinitely. A FIFO with no writer parks
// open(2) in the kernel until one appears, so it must be opened without
// blocking and then refused for what the descriptor says it is.
func TestHandoffFIFOInsideTheRootDoesNotBlock(t *testing.T) {
	png := fixtureImage(t, "valid.png")

	t.Run("named by the block", func(t *testing.T) {
		root := t.TempDir()
		fifo := filepath.Join(root, "shot.png")
		require.NoError(t, syscall.Mkfifo(fifo, 0o600))

		session := handoffSession(t, root)
		handoffVerdictWithin(t, session, handoffBlock(mimePNG, fifo, handoffEnvelope(png)),
			imageErrorPathNotAllowed, handoffCauseNotRegular)
	})

	t.Run("swapped in behind a regular file", func(t *testing.T) {
		root := t.TempDir()
		path := writeHandoffFile(t, root, "shot.png", png)

		swapped := false
		restore := imageReadAll
		imageReadAll = func(reader io.Reader) ([]byte, error) {
			if !swapped {
				swapped = true

				require.NoError(t, os.Remove(path))
				require.NoError(t, syscall.Mkfifo(path, 0o600))
			}

			return io.ReadAll(reader)
		}
		t.Cleanup(func() { imageReadAll = restore })

		session := handoffSession(t, root)
		block := handoffBlock(mimePNG, path, handoffEnvelope(png))

		_, err := session.validatePromptMedia(context.Background(), []acp.ContentBlock{block})
		require.NoError(t, err)

		handoffVerdictWithin(t, session, block, imageErrorPathNotAllowed, handoffCauseNotRegular)
	})
}
