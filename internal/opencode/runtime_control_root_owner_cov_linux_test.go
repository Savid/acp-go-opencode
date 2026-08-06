//go:build linux

package opencode

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// TestRuntimeControlRootOwnerRefusesADirectoryTheAdapterDoesNotOwn proves the
// mode check is not the whole contract. A control root that is 0700 but belongs
// to somebody else is 0700 *for them*: the adapter would be writing its
// supervisor scratch, its home lock and its lease into a directory whose owner
// can replace any of it. The refusal is asserted through the real entry point,
// and the directory is asserted unchanged afterwards so the refusal is provably
// about the owner rather than about a repair attempt.
func TestRuntimeControlRootOwnerRefusesADirectoryTheAdapterDoesNotOwn(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root")
	}

	parent := testTraversableTempDir(t)
	control := filepath.Join(parent, "control")
	require.NoError(t, os.Mkdir(control, 0o700))
	require.NoError(t, ensureRuntimeControlRoot(control))

	require.NoError(
		t, os.Chown(control, int(nativeOwnershipTargetUID), int(nativeOwnershipTargetGID)),
	)

	require.ErrorContains(
		t,
		ensureRuntimeControlRoot(control),
		"OpenCode runtime control root must be owned by the trusted identity",
	)

	var stat unix.Stat_t
	require.NoError(t, unix.Lstat(control, &stat))
	require.Equal(t, nativeOwnershipTargetUID, stat.Uid, "the refused control root was taken over anyway")
	require.Equal(t, nativeOwnershipTargetGID, stat.Gid)
	require.Equal(t, uint32(0o700), stat.Mode&0o7777)

	require.NoError(t, os.Chown(control, 0, 0))
	require.NoError(t, ensureRuntimeControlRoot(control))
}
