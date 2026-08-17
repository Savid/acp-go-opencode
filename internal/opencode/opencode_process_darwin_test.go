//go:build darwin

package opencode

import (
	"os/exec"
	"testing"
)

func TestConfigureOpenCodeProcessDarwin(t *testing.T) {
	cmd := exec.Command("true")
	configureOpenCodeProcess(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatalf("SysProcAttr = %#v, want Setpgid", cmd.SysProcAttr)
	}
}
