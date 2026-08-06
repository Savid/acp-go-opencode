//go:build !linux

package opencode

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// guardianWithoutScratchRootRefusal is how every non-Linux platform refuses a
// guardian that names no scratch root: the containment marker belongs under the
// scratch root, so the supervisor refuses before it writes anything.
const guardianWithoutScratchRootRefusal = "scratch root"

// requireSupervisorCommandWithoutScratchRoot asserts that building a supervisor
// command without a scratch root is refused outright.
func requireSupervisorCommandWithoutScratchRoot(t *testing.T, isolation *ProcessIsolation) {
	t.Helper()
	_, _, err := supervisorCommand(context.Background(), supervisorConfig{Scratch: "", Isolation: isolation})
	require.ErrorContains(t, err, guardianWithoutScratchRootRefusal)
}
