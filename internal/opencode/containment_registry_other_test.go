//go:build !darwin

package opencode

import (
	"io"
	"testing"
)

func TestDarwinContainmentOperationsUnavailableOffDarwin(t *testing.T) {
	if _, err := NewDarwinGenerationRecord("parent", "root", "kind"); err == nil {
		t.Fatal("generation record unexpectedly available")
	}
	if _, err := newDarwinRuntimeGenerationRoot("parent"); err == nil {
		t.Fatal("generation root unexpectedly available")
	}
	if err := DiagnoseDarwinContainment("parent", io.Discard); err == nil {
		t.Fatal("diagnose unexpectedly available")
	}
	if err := CleanupDarwinContainment("parent", "runtime", true, io.Discard); err == nil {
		t.Fatal("cleanup unexpectedly available")
	}
}
