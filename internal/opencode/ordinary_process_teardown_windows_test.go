//go:build windows

package opencode

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

const windowsTeardownChildMarker = "acp-go-opencode-teardown-child"

const windowsTeardownChildImage = "acp-go-opencode-teardown-child.exe"

type windowsProcess struct {
	pid    uint32
	parent uint32
	name   string
}

func windowsProcesses(t *testing.T) []windowsProcess {
	t.Helper()

	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	require.NoError(t, err)

	defer func() { _ = windows.CloseHandle(snapshot) }()

	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))

	processes := make([]windowsProcess, 0, 256)
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		processes = append(processes, windowsProcess{
			pid:    entry.ProcessID,
			parent: entry.ParentProcessID,
			name:   windows.UTF16ToString(entry.ExeFile[:]),
		})
	}

	return processes
}

func windowsProcessNamed(t *testing.T, name string) (windowsProcess, bool) {
	t.Helper()

	for _, process := range windowsProcesses(t) {
		if process.name == name {
			return process, true
		}
	}

	return windowsProcess{}, false
}

// TestOrdinaryWindowsTeardownKillsTheShimGrandchild pins the containment the
// adapter has to provide on Windows, in the shape the platform actually hands
// it: the native harness is reached through an npm `.cmd` shim, so the direct
// child is a cmd.exe and the process that owns the loopback port and the native
// database is that cmd.exe's own child.
//
// Windows offers no process group that outlives its leader, so stopping the
// direct child leaves the grandchild running — which is what kept a runtime
// root locked open after shutdown. The proof reproduces the shape with a batch
// file and a copy of this test binary, so it needs nothing installed, and
// asserts what teardown must guarantee: the grandchild is gone.
func TestOrdinaryWindowsTeardownKillsTheShimGrandchild(t *testing.T) {
	if slices.Contains(os.Args, windowsTeardownChildMarker) {
		time.Sleep(10 * time.Minute)
		os.Exit(0)
	}

	if _, running := windowsProcessNamed(t, windowsTeardownChildImage); running {
		t.Fatalf("a %s from an earlier run is still alive", windowsTeardownChildImage)
	}

	directory := t.TempDir()

	sourcePath, err := os.Executable()
	require.NoError(t, err)
	source, err := os.Open(sourcePath)
	require.NoError(t, err)

	defer func() { _ = source.Close() }()

	childPath := filepath.Join(directory, windowsTeardownChildImage)
	child, err := os.Create(childPath)
	require.NoError(t, err)
	_, err = io.Copy(child, source)
	require.NoError(t, err)
	require.NoError(t, child.Close())

	// The shim runs the child in the foreground and waits for it, which is what
	// npm's generated `.cmd` does. Output goes nowhere because nothing on this
	// side reads the pipes the process handle opens.
	shimPath := filepath.Join(directory, "opencode.cmd")
	shim := "@echo off\r\n\"" + childPath + "\" -test.run=" +
		"TestOrdinaryWindowsTeardownKillsTheShimGrandchild -- " +
		windowsTeardownChildMarker + " >nul 2>&1\r\n"
	require.NoError(t, os.WriteFile(shimPath, []byte(shim), 0o600))

	environment := []string{
		"PATH=" + os.Getenv("PATH"),
		"PATHEXT=" + os.Getenv("PATHEXT"),
		"SystemRoot=" + os.Getenv("SystemRoot"),
		"COMSPEC=" + os.Getenv("COMSPEC"),
		"TEMP=" + os.Getenv("TEMP"),
		"TMP=" + os.Getenv("TMP"),
	}

	handle, err := startOrdinaryProcess(t.Context(), shimPath, nil, environment, directory)
	require.NoError(t, err)
	require.True(t, handle.valid())

	var grandchild windowsProcess

	require.Eventually(t, func() bool {
		found, running := windowsProcessNamed(t, windowsTeardownChildImage)
		grandchild = found

		return running
	}, 90*time.Second, 100*time.Millisecond, "the shim never launched its child")

	var parentName string

	for _, process := range windowsProcesses(t) {
		if process.pid == grandchild.parent {
			parentName = process.name
		}
	}

	require.Equal(t, "cmd.exe", parentName,
		"the proof is only the shim shape if the direct child is the interpreter, not the harness")

	require.NoError(t, handle.Stop(t.Context()))

	_, awaitErr := handle.Await(t.Context())
	require.NoError(t, awaitErr)

	require.Eventually(t, func() bool {
		for _, process := range windowsProcesses(t) {
			if process.pid == grandchild.pid && process.name == grandchild.name {
				return false
			}
		}

		return true
	}, 30*time.Second, 100*time.Millisecond,
		"the harness behind the shim outlived teardown, so a runtime root would stay locked open")
}
