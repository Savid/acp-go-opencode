package opencodeacp

import (
	"context"
	"fmt"
)

func acquireRuntimeResource(
	ctx context.Context,
	acquire func(context.Context, RuntimeResourceKind) (func(), error),
	kind RuntimeResourceKind,
) (func(), error) {
	if acquire == nil {
		return func() {}, nil
	}

	release, err := acquire(ctx, kind)
	if err != nil {
		return nil, fmt.Errorf("acquire %s runtime resource: %w", kind, err)
	}

	if release == nil {
		return nil, fmt.Errorf("acquire %s runtime resource returned a nil release", kind)
	}

	return release, nil
}
