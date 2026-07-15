//go:build !unix

package opencode

import (
	"errors"
	"os"
	"os/exec"
)

func configureOpenCodeProcess(*exec.Cmd) {}

func terminateOpenCodeProcess(cmd *exec.Cmd) error {
	return killOpenCodeProcess(cmd)
}

func killOpenCodeProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := cmd.Process.Kill(); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}
	return nil
}
