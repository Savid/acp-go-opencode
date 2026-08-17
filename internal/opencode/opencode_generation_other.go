//go:build !darwin

package opencode

import (
	"context"
	"errors"
)

func prepareDarwinRuntimeGeneration(_ context.Context, options StartOptions) (string, func() error, error) {
	if options.DarwinBestEffort {
		return "", nil, errors.Join(ErrProcessContainmentIncomplete, errors.New("darwin best-effort containment is unavailable on this platform"))
	}

	return "", func() error { return nil }, nil
}
