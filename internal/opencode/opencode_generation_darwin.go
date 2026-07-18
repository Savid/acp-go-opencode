//go:build darwin

package opencode

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

func prepareDarwinRuntimeGeneration(ctx context.Context, options StartOptions) (string, func() error, error) {
	if !options.DarwinBestEffort {
		return "", func() error { return nil }, nil
	}

	if options.ReserveContainmentScratch == nil {
		return "", nil, errors.New("darwin containment scratch reservation is required")
	}

	reservationRelease, err := options.ReserveContainmentScratch(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("reserve Darwin containment generation scratch: %w", err)
	}

	if reservationRelease == nil {
		return "", nil, errors.New("reserve Darwin containment generation scratch returned a nil release")
	}

	parent := options.ContainmentScratchParent
	if parent == "" {
		parent = options.ScratchParent
	}

	root, err := newDarwinRuntimeGenerationRoot(parent)
	if err != nil {
		if root == "" {
			reservationRelease()
		}

		return root, nil, err
	}

	var (
		cleanupOnce sync.Once
		cleanupErr  error
	)

	return root, func() error {
		cleanupOnce.Do(func() {
			if err := openCodeRemoveAll(root); err != nil {
				cleanupErr = errors.Join(
					ErrRuntimeScratchCleanup,
					fmt.Errorf("remove Darwin containment generation: %w", err),
				)

				return
			}

			reservationRelease()
		})

		return cleanupErr
	}, nil
}
