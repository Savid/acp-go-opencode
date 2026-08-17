//go:build !darwin

package opencode

import (
	"context"
	"errors"
	"testing"
)

func TestPrepareDarwinRuntimeGenerationOffDarwin(t *testing.T) {
	root, release, err := prepareDarwinRuntimeGeneration(context.Background(), StartOptions{})
	if err != nil || root != "" || release == nil || release() != nil {
		t.Fatalf("ordinary generation = root %q, has release %t, err %v", root, release != nil, err)
	}

	root, release, err = prepareDarwinRuntimeGeneration(context.Background(), StartOptions{DarwinBestEffort: true})
	if root != "" || release != nil || !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("best-effort generation = root %q, has release %t, err %v", root, release != nil, err)
	}
}
