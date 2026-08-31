//go:build !windows

package opencode

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func resolveOrdinaryProcessExecutable(path string, environment []string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("executable path is empty")
	}

	resolve := func(candidate string) (string, error) {
		info, err := os.Stat(candidate)
		if err != nil {
			return "", err
		}

		if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
			return "", fmt.Errorf("executable %q is not executable", candidate)
		}

		return candidate, nil
	}

	if strings.ContainsRune(path, filepath.Separator) {
		resolved, err := resolve(path)
		if err != nil {
			return "", fmt.Errorf("stat executable %q: %w", path, err)
		}

		return resolved, nil
	}

	search := environmentValue(environment, pathEnv)
	if search == "" {
		return "", fmt.Errorf("find %s: PATH is empty", path)
	}

	for _, directory := range filepath.SplitList(search) {
		candidate := filepath.Join(directory, path)

		resolved, err := resolve(candidate)
		if err == nil {
			return resolved, nil
		}
	}

	return "", fmt.Errorf("find %s in PATH: %w", path, exec.ErrNotFound)
}
