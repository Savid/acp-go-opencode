//go:build linux

package opencode

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/savid/acp-go-opencode/internal/homelock"
)

func TestOpenCodeNativeCannotMutateTrustedHomeLocks(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root")
	}

	parent, err := os.MkdirTemp("", "acp-go-opencode-ownership-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	if chmodErr := os.Chmod(parent, 0o711); chmodErr != nil {
		t.Fatal(chmodErr)
	}
	xdg, err := CreateRuntimeXDGDirs(filepath.Join(parent, "native"))
	if err != nil {
		t.Fatal(err)
	}
	control := ControlRootForXDG(xdg.Root)
	lock, err := homelock.Acquire(control)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if err := handoffGeneratedNativeTree(xdg.Root, &ProcessIsolation{UID: 65534, GID: 65534, BaseEnvironment: map[string]string{}, StandaloneOwnerID: "test-owner", StandaloneStateRoot: "/var/lib/acp-go-test"}); err != nil {
		t.Fatal(err)
	}

	claim := filepath.Join(control, homelock.ClaimFileName)
	cmd := exec.Command("/bin/sh", "-c", `: > "$1/native-ok" && ! cat "$2" >/dev/null 2>&1 && ! rm -f "$2"`, "sh", xdg.State, claim)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534, Groups: []uint32{}}}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("dropped identity proof: %v: %s", err, output)
	}
	if _, err := os.Stat(claim); err != nil {
		t.Fatalf("trusted home lock changed: %v", err)
	}
}
