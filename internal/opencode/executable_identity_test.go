package opencode

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// testNativeExecutable freezes a real file so a supervisor config carries the
// identity its exec boundary rechecks.
func testNativeExecutable(t *testing.T, path string) processExecutable {
	t.Helper()

	executable, err := freezeProcessExecutable(path)
	require.NoError(t, err)

	return executable
}

// TestProcessExecutableResolutionOutlivesAWorkingDirectoryChange is the
// deterministic proof that resolution cannot validate one file and execute
// another. Every shape that used to resolve to a relative string — a configured
// path carrying a separator, a bare name found through a relative PATH entry —
// is frozen into an absolute path plus a filesystem identity while the adapter
// still stands in the directory it validated the file from. Moving to / then
// changes nothing, and a same-named replacement is refused rather than run.
func TestProcessExecutableResolutionOutlivesAWorkingDirectoryChange(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "bin", "opencode")
	require.NoError(t, os.MkdirAll(filepath.Dir(binary), 0o700))
	require.NoError(t, os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o700))

	decoy := filepath.Join(root, "decoy")
	require.NoError(t, os.WriteFile(decoy, []byte("#!/bin/sh\nexit 1\n"), 0o700))

	t.Chdir(root)

	configured, err := resolveProcessExecutable(filepath.Join(".", "bin", "opencode"), nil, false)
	require.NoError(t, err)
	require.Equal(t, binary, configured.Path)

	searched, err := resolveProcessExecutable("opencode", []string{"PATH=bin"}, false)
	require.NoError(t, err)
	require.Equal(t, configured, searched)

	t.Chdir("/")

	require.NoError(t, configured.verify(), "an absolute identity survives the directory the supervisors move to")

	require.NoError(t, os.Rename(decoy, binary))
	require.ErrorContains(t, configured.verify(), "no longer the file the launch resolved")

	require.NoError(t, os.Remove(binary))
	require.ErrorContains(t, configured.verify(), "identify executable")
}

func TestProcessExecutableFreezeRefusesUnanchoredPaths(t *testing.T) {
	_, err := freezeProcessExecutable("bin/opencode")
	require.ErrorContains(t, err, `executable path "bin/opencode" is not absolute`)

	_, err = resolveProcessExecutable(" ", nil, false)
	require.ErrorContains(t, err, "executable path is empty")

	root := t.TempDir()
	binary := filepath.Join(root, "opencode")
	require.NoError(t, os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o700))

	t.Chdir(root)

	working := processWorkingDirectory
	t.Cleanup(func() { processWorkingDirectory = working })

	processWorkingDirectory = func() (string, error) { return "", errors.New("no working directory") }
	_, err = resolveProcessExecutable("./opencode", nil, false)
	require.ErrorContains(t, err, "resolve working directory for executable")
}
