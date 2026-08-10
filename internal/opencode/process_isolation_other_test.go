//go:build !unix

package opencode

import (
	"os/exec"
	"testing"
)

// requireNoProcessCredential has nothing to read here: this platform's
// SysProcAttr carries no credential field at all, which is the strongest form
// of the same assertion.
func requireNoProcessCredential(*testing.T, *exec.Cmd) {}
