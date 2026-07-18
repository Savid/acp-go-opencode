package opencodeacp

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAcquireRuntimeResource(t *testing.T) {
	released := false
	releaseResource, err := acquireRuntimeResource(context.Background(), func(context.Context, RuntimeResourceKind) (func(), error) {
		return func() { released = true }, nil
	}, "kind")
	require.NoError(t, err)
	releaseResource()
	require.True(t, released)

	standaloneRelease, err := acquireRuntimeResource(context.Background(), nil, RuntimeResourceRuntime)
	require.NoError(t, err)
	standaloneRelease()

	_, err = acquireRuntimeResource(context.Background(), func(context.Context, RuntimeResourceKind) (func(), error) {
		return nil, errors.New("denied")
	}, "kind")
	require.ErrorContains(t, err, "denied")

	_, err = acquireRuntimeResource(context.Background(), func(context.Context, RuntimeResourceKind) (func(), error) {
		//nolint:nilnil // The test exercises rejection of a nil release without an acquisition error.
		return nil, nil
	}, "kind")
	require.ErrorContains(t, err, "nil release")
}
