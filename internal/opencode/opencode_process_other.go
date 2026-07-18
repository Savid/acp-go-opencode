//go:build !unix

package opencode

import (
	"errors"
	"os"
	"os/exec"
)

func configureOpenCodeProcess(*exec.Cmd) {}

func terminateOpenCodeProcess(process *os.Process, originalGroup int) error {
	return killOpenCodeProcess(process, originalGroup)
}

func killOpenCodeProcess(process *os.Process, _ int) error {
	if process == nil {
		return nil
	}
	if err := process.Kill(); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}
	return nil
}
