//go:build darwin

package main

import (
	"io"

	"github.com/savid/acp-go-opencode/internal/opencode"
)

func diagnoseContainment(scratchDir string, output io.Writer) error {
	return opencode.DiagnoseDarwinContainment(scratchDir, output)
}

func cleanupContainment(scratchDir, runtimeID string, force bool, output io.Writer) error {
	return opencode.CleanupDarwinContainment(scratchDir, runtimeID, force, output)
}
