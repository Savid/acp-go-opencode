//go:build linux

package opencode

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProcessIsolationActualIdentityGroupsAndAmbientScrub(t *testing.T) {
	if os.Getenv("ACP_PROCESS_ISOLATION_HELPER") == "1" {
		groups, err := os.Getgroups()
		if err != nil {
			os.Exit(2)
		}
		fmt.Printf("%d:%d:%d:%s", os.Geteuid(), os.Getegid(), len(groups), os.Getenv("ACP_PROCESS_AMBIENT_CANARY"))
		os.Exit(0)
	}
	if os.Geteuid() != 0 {
		t.Skip("actual credential-drop proof requires a privileged Linux test process")
	}

	root := t.TempDir()
	require.NoError(t, os.Chmod(root, 0o755))
	binary := filepath.Join(root, "isolation-helper")
	data, err := os.ReadFile(os.Args[0])
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(binary, data, 0o755))
	t.Setenv("ACP_PROCESS_AMBIENT_CANARY", "must-not-leak")

	const target = uint32(65534)
	cmd := exec.Command(binary, "-test.run=^TestProcessIsolationActualIdentityGroupsAndAmbientScrub$")
	cmd.Dir = "/"
	cmd.Env = []string{"PATH=/usr/bin:/bin", "ACP_PROCESS_ISOLATION_HELPER=1"}
	require.NoError(t, applyProcessCredential(cmd, &ProcessIsolation{UID: target, GID: target, BaseEnvironment: map[string]string{"PATH": "/usr/bin:/bin"}}))
	output, err := cmd.Output()
	require.NoError(t, err)
	require.Equal(t, "65534:65534:0:", strings.TrimSpace(string(output)))
}
