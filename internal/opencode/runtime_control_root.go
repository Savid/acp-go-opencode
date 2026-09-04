package opencode

import (
	"errors"
	"fmt"
	"os"
)

var runtimeControlLstat = os.Lstat
var runtimeControlMkdir = os.Mkdir

func ensureRuntimeControlRoot(root string) error {
	info, err := runtimeControlLstat(root)
	if errors.Is(err, os.ErrNotExist) {
		if mkdirErr := runtimeControlMkdir(root, 0o700); mkdirErr != nil {
			return fmt.Errorf("create OpenCode runtime control root: %w", mkdirErr)
		}

		info, err = runtimeControlLstat(root)
	}

	if err != nil {
		return fmt.Errorf("inspect OpenCode runtime control root: %w", err)
	}

	if !info.IsDir() || !runtimeControlRootTrusted(info) {
		return errors.New("OpenCode runtime control root must be a directory this user alone may write")
	}

	return nil
}
