//go:build darwin || freebsd || openbsd

package opencode

import (
	"path/filepath"
	"runtime"
	"testing"
)

func preservePlatformSupervisorGlobals(t *testing.T) {
	t.Helper()

	if runtime.GOOS != "darwin" {
		return
	}

	originalGuardian := supervisorNewGuardianContainment
	originalLiveness := supervisorOpenLivenessContainment
	t.Cleanup(func() {
		supervisorNewGuardianContainment = originalGuardian
		supervisorOpenLivenessContainment = originalLiveness
	})

	supervisorNewGuardianContainment = func(config supervisorConfig) (*guardianContainment, error) {
		config.DarwinBestEffort = true

		return newGuardianContainment(config)
	}
	supervisorOpenLivenessContainment = func(config supervisorConfig) (*livenessContainment, error) {
		config.DarwinBestEffort = true
		if config.ScratchParent == "" {
			config.ScratchParent = filepath.Dir(config.Scratch)
		}
		if config.LifecycleKind == "" {
			config.LifecycleKind = "runtime"
		}

		return openLivenessContainment(config)
	}
}
