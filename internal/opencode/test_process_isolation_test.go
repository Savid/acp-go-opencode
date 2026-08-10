package opencode

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// TestMain pins the platform the explicit-policy fixtures describe. The
// hardened identity boundary exists on Linux and nowhere else, so every case
// that builds a ProcessIsolation is a Linux case whatever host runs the suite;
// without this pin a developer machine would exercise the platform refusal in
// place of the behavior each case names. The refusal itself is asserted
// directly, from every non-Linux platform, in TestExplicitProcessIsolationIsLinuxOnly.
// Cases that mean another platform still set the seam themselves and restore it.
func TestMain(m *testing.M) {
	processIsolationGOOS = processIsolationLinux

	os.Exit(m.Run())
}

// testIsolationIdentity is the identity every fixture isolates to. Root cannot
// isolate to itself — the policy forbids UID or GID zero — and uid 1 is the
// system daemon account, which the standalone claim finds live on any host the
// suite can see through the initial PID namespace and rightly refuses as
// occupied. 65534 is the fleet-wide unprivileged stand-in, and the privileged
// lock serializes it across repos.
//
// The identity the fixture must not name is the one running the suite: an
// explicit policy naming the adapter's own identity is refused outright, and
// every fixture here describes the hardened launch that still has a privilege
// boundary to cross. Under root that is the pre-existing zero test and the
// identity is unchanged.
func testIsolationIdentity() (uint32, uint32) {
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 || gid == 0 || uid == os.Geteuid() || gid == os.Getegid() {
		uid, gid = 65534, 65534
	}

	return uint32(uid), uint32(gid)
}

// testStandaloneStateRootPath is the one state root every fixture in this
// package claims. It has to be exactly one path: the authority permanently
// binds a UID to a single owner and state root, so a second root for the same
// identity is refused for the lifetime of the authority tree.
const testStandaloneStateRootPath = "/var/lib/acp-go-opencode-test"

// testStandaloneStateRoot materializes the directory a standalone identity
// claim binds: mode 0700, owned by the claimed identity, beneath root-owned
// ancestry that is neither group- nor other-writable. The fixture used to name
// /var/lib/acp-go-test, a path nothing ever created, so every supervised launch
// died at the bind with ENOENT before the case under test began. Materializing
// it needs the privileges the claim needs anyway; an identity the caller cannot
// hand it to fails at the bind, which is the honest report.
var testStandaloneStateRoot = sync.OnceValue(func() string {
	uid, gid := testIsolationIdentity()
	if err := os.MkdirAll(testStandaloneStateRootPath, 0o700); err != nil {
		return testStandaloneStateRootPath
	}
	if err := os.Chown(testStandaloneStateRootPath, int(uid), int(gid)); err != nil {
		return testStandaloneStateRootPath
	}
	_ = os.Chmod(testStandaloneStateRootPath, 0o700)

	return testStandaloneStateRootPath
})

func testProcessIsolation() *ProcessIsolation {
	uid, gid := testIsolationIdentity()
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
		UID: uid, GID: gid, BaseEnvironment: environment,
		StandaloneOwnerID: "test-owner", StandaloneStateRoot: testStandaloneStateRoot(),
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
