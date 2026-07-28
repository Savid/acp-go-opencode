//go:build !windows

package opencode

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func restoreBrowserShimSeams(t *testing.T) {
	t.Helper()

	mkdirTemp := browserShimMkdirTemp
	writeFile := browserShimWriteFile

	t.Cleanup(func() {
		browserShimMkdirTemp = mkdirTemp
		browserShimWriteFile = writeFile
	})
}

// browserProbeDir holds a launcher for each name a harness may exec, every one
// of which records the call instead of opening anything.
func browserProbeDir(t *testing.T, marker string) string {
	t.Helper()

	probe := t.TempDir()
	body := fmt.Sprintf("#!/bin/sh\necho \"$0 $*\" >> %q\nexit 0\n", marker)

	for _, name := range browserLauncherNames {
		require.NoError(t, os.WriteFile(filepath.Join(probe, name), []byte(body), 0o700))
	}

	return probe
}

// browserLaunchingOpenCodeExecutable stands in for a native binary whose login
// leg opens a URL. The launcher names are bare so they resolve through the
// child's PATH, which is the resolution the shim has to win.
func browserLaunchingOpenCodeExecutable(t *testing.T) string {
	t.Helper()

	testBinary, err := os.Executable()
	require.NoError(t, err)

	var body strings.Builder

	body.WriteString("#!/bin/sh\n")

	for _, name := range browserLauncherNames {
		fmt.Fprintf(&body, "%s \"https://example.invalid/\"\n", name)
	}

	fmt.Fprintf(
		&body,
		"ACP_GO_OPENCODE_FAKE_SERVER_HELPER=1 exec %q -test.run=TestFakeOpenCodeServerProcessHelper -- \"$@\"\n",
		testBinary,
	)

	script := filepath.Join(t.TempDir(), "fake-opencode")
	require.NoError(t, os.WriteFile(script, []byte(body.String()), 0o700))

	return script
}

func TestLoginNeverExecsABrowserLauncher(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "launched")
	probe := browserProbeDir(t, marker)

	t.Setenv("PATH", probe+string(os.PathListSeparator)+os.Getenv("PATH"))

	for _, name := range browserLauncherNames {
		require.NoError(t, exec.Command(name, "https://example.invalid/").Run())
	}

	require.FileExists(t, marker, "the probe launchers never recorded a call, so a missing marker proves nothing")
	require.NoError(t, os.Remove(marker))

	shim, err := NewBrowserShim(t.TempDir())
	require.NoError(t, err)

	client, err := StartServer(context.Background(), platformStartOptions(t, StartOptions{
		Root:            t.TempDir(),
		ExecutablePath:  browserLaunchingOpenCodeExecutable(t),
		BrowserShim:     shim,
		HealthTimeout:   30 * time.Second,
		Logger:          slog.New(slog.DiscardHandler),
		SkipVersionGate: true,
	}))
	require.NoError(t, err)

	t.Cleanup(func() { require.NoError(t, client.Shutdown(context.Background())) })

	_, statErr := os.Stat(marker)
	require.ErrorIs(t, statErr, os.ErrNotExist, "the login leg reached a browser launcher on PATH")

	require.NoError(t, shim.Remove())
	require.NoDirExists(t, shim.Dir())
}

func TestBrowserShimEnvironShadowsPathAndBrowser(t *testing.T) {
	dir := "/scratch/shim"

	cases := []struct {
		name string
		env  []string
		want []string
	}{
		{
			name: "no path",
			env:  []string{"HOME=/home/dev"},
			want: []string{"HOME=/home/dev", "PATH=" + dir, "BROWSER=" + dir + "/open"},
		},
		{
			name: "existing path",
			env:  []string{"PATH=/usr/bin", "HOME=/home/dev"},
			want: []string{"HOME=/home/dev", "PATH=" + dir + string(os.PathListSeparator) + "/usr/bin", "BROWSER=" + dir + "/open"},
		},
		{
			name: "existing browser",
			env:  []string{"BROWSER=/usr/bin/firefox"},
			want: []string{"PATH=" + dir, "BROWSER=" + dir + "/open"},
		},
		{
			name: "entry without a separator",
			env:  []string{"MALFORMED"},
			want: []string{"MALFORMED", "PATH=" + dir, "BROWSER=" + dir + "/open"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			require.Equal(t, testCase.want, browserShimEnviron(testCase.env, dir))
		})
	}
}

func TestNewBrowserShimWritesAnExecutableNoOpPerLauncher(t *testing.T) {
	shim, err := NewBrowserShim(t.TempDir())
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(filepath.Base(shim.Dir()), browserShimPrefix))

	info, err := os.Stat(shim.Dir())
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	for _, name := range browserLauncherNames {
		launcher := filepath.Join(shim.Dir(), name)

		require.NoError(t, exec.Command(launcher, "https://example.invalid/").Run())

		body, readErr := os.ReadFile(launcher)
		require.NoError(t, readErr)
		require.Equal(t, browserShimScript, body)
	}
}

func TestNewBrowserShimFailures(t *testing.T) {
	failure := errors.New("refused")

	t.Run("directory", func(t *testing.T) {
		restoreBrowserShimSeams(t)

		browserShimMkdirTemp = func(string, string) (string, error) { return "", failure }

		shim, err := NewBrowserShim(t.TempDir())
		require.ErrorIs(t, err, failure)
		require.Nil(t, shim)
	})

	t.Run("launcher", func(t *testing.T) {
		restoreBrowserShimSeams(t)

		browserShimWriteFile = func(string, []byte, os.FileMode) error { return failure }

		parent := t.TempDir()

		shim, err := NewBrowserShim(parent)
		require.ErrorIs(t, err, failure)
		require.Nil(t, shim)

		entries, readErr := os.ReadDir(parent)
		require.NoError(t, readErr)
		require.Empty(t, entries)
	})
}

func TestBrowserShimRemoveToleratesANilShim(t *testing.T) {
	var shim *BrowserShim

	require.NoError(t, shim.Remove())
}
