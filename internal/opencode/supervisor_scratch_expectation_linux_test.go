//go:build linux

package opencode

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// guardianWithoutScratchRootRefusal is how Linux refuses a guardian that names
// no scratch root. The containment marker lives in the kernel-side proof
// namespace rather than under the scratch root, so the refusal lands one step
// later than elsewhere: the private config the liveness supervisor reads is
// incomplete without a scratch root, and the guardian dies on that frame.
const guardianWithoutScratchRootRefusal = "private supervisor config is incomplete"

// requireSupervisorCommandWithoutScratchRoot asserts what building a supervisor
// command without a scratch root does. Linux builds it: the marker it would
// otherwise place under the scratch root lives in the proof namespace instead,
// so nothing is missing yet. The scratch root is still required, and
// guardianWithoutScratchRootRefusal names where Linux enforces it.
func requireSupervisorCommandWithoutScratchRoot(t *testing.T, isolation *ProcessIsolation) {
	t.Helper()
	_, proof, err := supervisorCommand(context.Background(), supervisorConfig{Scratch: "", Isolation: isolation})
	require.NoError(t, err)
	require.NoError(t, proof.closeInherited())
}
