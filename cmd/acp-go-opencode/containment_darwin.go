//go:build darwin

package main

import (
	"io"

	"github.com/savid/acp-go-opencode/internal/opencode"
)

var diagnoseContainment = func(scratchDir string, output io.Writer) error {
	return opencode.DiagnoseDarwinContainment(scratchDir, output)
}

var cleanupContainment = func(scratchDir, runtimeID string, force bool, output io.Writer) error {
	return opencode.CleanupDarwinContainment(scratchDir, runtimeID, force, output)
}
