//go:build unix

package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCurrentProcessIdentityFailsClosedOnAnUnreadableIdentity covers the guard
// that stands between an unreadable identity and an ordinary stamp built from
// it. The kernel never hands back a negative id, so the guard is reachable only
// through the seam; what it protects is real, because a stamp derived from a
// bad identity would be re-derived and refused by every child instead.
func TestCurrentProcessIdentityFailsClosedOnAnUnreadableIdentity(t *testing.T) {
	original := processEffectiveUID
	t.Cleanup(func() { processEffectiveUID = original })

	processEffectiveUID = func() int { return -1 }

	_, _, err := currentProcessIdentity()
	require.ErrorContains(t, err, "current process identity is unavailable")
}

// TestOrdinaryLaunchFailsClosedOnAnUnreadableIdentity walks the same guard
// through the two places an ordinary launch reads its own identity: the parent
// that stamps the sealed config, and the child that re-derives the stamp before
// following it. Neither one substitutes a guess for an identity it could not
// read.
func TestOrdinaryLaunchFailsClosedOnAnUnreadableIdentity(t *testing.T) {
	preserveSupervisorGlobals(t)

	original := processEffectiveUID
	t.Cleanup(func() { processEffectiveUID = original })

	scratch := t.TempDir()
	config := supervisorConfig{
		NativePath: "/bin/sh", Home: filepath.Join(scratch, "home"), Scratch: scratch,
	}

	processEffectiveUID = func() int { return -1 }

	cmd, proof, err := supervisorCommand(context.Background(), config)
	require.ErrorContains(t, err, "current process identity is unavailable")
	require.Nil(t, cmd)
	require.Nil(t, proof)

	sealed := config
	sealed.OrdinaryExecution = true
	sealed.SharedIdentity = true
	encoded, marshalErr := json.Marshal(sealed)
	require.NoError(t, marshalErr)

	require.ErrorContains(t, runSupervisor(supervisorModeGuardian, bytes.NewReader(encoded)),
		"current process identity is unavailable")
}
