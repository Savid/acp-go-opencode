//go:build !darwin

package opencode

import (
	"errors"
	"io"
)

func newDarwinRuntimeGenerationRoot(string) (string, error) {
	return "", errors.New("Darwin best-effort containment is unavailable on this platform")
}

func DiagnoseDarwinContainment(string, io.Writer) error {
	return errors.New("containment diagnose is available only on darwin")
}

func CleanupDarwinContainment(string, string, bool, io.Writer) error {
	return errors.New("containment cleanup is available only on darwin")
}
