//go:build windows

package opencode

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func resolveOrdinaryProcessExecutable(path string, environment []string) (string, error) {
	exts := windowsPathExtensions(environment)
	resolve := func(candidate string) (string, error) {
		if !filepath.IsAbs(candidate) {
			absolute, err := filepath.Abs(candidate)
			if err != nil {
				return "", err
			}
			candidate = absolute
		}

		return windowsExecutableFile(candidate, exts)
	}

	if strings.ContainsAny(path, `:\/`) {
		return resolve(path)
	}

	search := environmentValue(environment, pathEnv)
	if search == "" {
		return "", fmt.Errorf("find %s: PATH is empty", path)
	}
	for _, directory := range filepath.SplitList(search) {
		if directory == "" {
			continue
		}
		if resolved, err := resolve(filepath.Join(directory, path)); err == nil {
			return resolved, nil
		}
	}

	return "", fmt.Errorf("find %s in PATH: %w", path, exec.ErrNotFound)
}

func windowsPathExtensions(environment []string) []string {
	value := environmentValue(environment, "PATHEXT")
	if value == "" {
		value = ".COM;.EXE;.BAT;.CMD"
	}

	exts := make([]string, 0, 4)
	for extension := range strings.SplitSeq(value, ";") {
		if extension == "" {
			continue
		}
		if extension[0] != '.' {
			extension = "." + extension
		}
		exts = append(exts, strings.ToLower(extension))
	}

	return exts
}

func windowsExecutableFile(path string, extensions []string) (string, error) {
	if filepath.Ext(path) != "" {
		if resolved, err := regularExecutableFile(path); err == nil {
			return resolved, nil
		}
	}
	for _, extension := range extensions {
		if resolved, err := regularExecutableFile(path + extension); err == nil {
			return resolved, nil
		}
	}

	return "", fmt.Errorf("executable %q is not executable", path)
}

func regularExecutableFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("executable %q is not executable", path)
	}

	return path, nil
}
