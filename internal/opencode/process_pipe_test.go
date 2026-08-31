package opencode

import (
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type failingProcessPipe struct {
	read bool
}

func (r *failingProcessPipe) Read(buffer []byte) (int, error) {
	if !r.read {
		r.read = true

		return copy(buffer, strings.Repeat("x", 70*1024)), nil
	}

	return 0, errors.New("pipe failed")
}

func TestDrainProcessPipeDiscardsLongAndFailedOutput(t *testing.T) {
	reader := &failingProcessPipe{}
	drainProcessPipe(slog.New(slog.DiscardHandler), "stderr", reader)
	require.True(t, reader.read)
}
