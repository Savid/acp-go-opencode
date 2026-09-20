package opencodeacp

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKeepArtifactEvictsTheOldestPastTheBounds(t *testing.T) {
	t.Parallel()

	s := &session{}
	for i := range maxArtifacts + 4 {
		s.keepArtifact("file-"+strconv.Itoa(i), imageArtifact{Data: "x"})
	}

	require.Len(t, s.artifacts, maxArtifacts)
	require.NotContains(t, s.artifacts, "file-0")
	require.NotContains(t, s.artifacts, "file-3")
	require.Contains(t, s.artifacts, "file-4")

	large := strings.Repeat("y", maxArtifactBytes/2+1)
	s.keepArtifact("large-1", imageArtifact{Data: large})
	s.keepArtifact("large-2", imageArtifact{Data: large})
	require.NotContains(t, s.artifacts, "large-1", "retained bytes stay under the bound")
	require.Contains(t, s.artifacts, "large-2")
}
