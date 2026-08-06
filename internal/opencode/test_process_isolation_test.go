package opencode

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testProcessIsolation() *ProcessIsolation {
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		uid = 1
	}
	if gid == 0 {
		gid = 1
	}
	environment := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key != "" {
			environment[key] = value
		}
	}
	if environment["PATH"] == "" {
		environment["PATH"] = "/usr/bin:/bin"
	}

	return &ProcessIsolation{
		UID: uint32(uid), GID: uint32(gid), BaseEnvironment: environment,
		StandaloneOwnerID: "test-owner", StandaloneStateRoot: "/var/lib/acp-go-test",
	}
}

// testForeignProcessIsolation names an identity the current process is not.
// Both IDs stay nonzero so the policy is admissible and the refusal under test
// is the ownership handoff, not the identity validation that precedes it.
func testForeignProcessIsolation() *ProcessIsolation {
	isolation := testProcessIsolation()
	isolation.UID++
	isolation.GID++

	return isolation
}

// testUnhandoffableXDGDirs builds runtime XDG dirs beneath an ancestry the
// trusted identity keeps to itself, so a handoff to any foreign identity is
// refused on every platform.
func testUnhandoffableXDGDirs(t *testing.T) XDGDirs {
	t.Helper()
	root := filepath.Join(t.TempDir(), "runtime")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatalf("create unhandoffable test directory: %v", err)
	}

	return XDGDirs{
		Root:   root,
		Data:   filepath.Join(root, "data"),
		Config: filepath.Join(root, "config"),
		Cache:  filepath.Join(root, "cache"),
		State:  filepath.Join(root, "state"),
	}
}

func withTestProcessIsolation(options StartOptions) StartOptions {
	if options.ProcessIsolation == nil {
		options.ProcessIsolation = testProcessIsolation()
	}

	return options
}

func testTraversableTempDir(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("", "acp-go-opencode-test-")
	if err != nil {
		t.Fatalf("create traversable test directory: %v", err)
	}
	if err = os.Chmod(directory, 0o711); err != nil {
		_ = os.RemoveAll(directory)
		t.Fatalf("make test directory traversable: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })

	return directory
}

func testGeneratedTempDir(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("", "acp-go-opencode-runtime-")
	if err != nil {
		t.Fatalf("create generated test directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })

	return directory
}

func skipUnprivilegedDarwinIsolation(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "darwin" {
		t.Skip("requires a privileged two-principal fixture to clear supplementary groups")
	}
}
