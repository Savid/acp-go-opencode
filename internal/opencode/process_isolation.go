package opencode

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	processIsolationUIDEnv = "ACP_GO_OPENCODE_INTERNAL_UID"
	processIsolationGIDEnv = "ACP_GO_OPENCODE_INTERNAL_GID"
)

// ProcessIsolation is the mandatory credential and complete environment base
// applied to every provider process and self-exec supervisor.
type ProcessIsolation struct {
	UID             uint32
	GID             uint32
	BaseEnvironment map[string]string
}

func validateProcessIsolation(isolation *ProcessIsolation) error {
	if isolation == nil {
		return errors.New("process isolation is required")
	}

	if isolation.UID == 0 || isolation.GID == 0 {
		return errors.New("process isolation uid and gid must be nonzero")
	}

	if isolation.BaseEnvironment == nil {
		return errors.New("process isolation base environment is required")
	}

	if err := validateEnvironmentMap(isolation.BaseEnvironment); err != nil {
		return fmt.Errorf("validate process isolation base environment: %w", err)
	}

	return validateProcessIsolationPlatform()
}

func validateEnvironmentMap(environment map[string]string) error {
	for key, value := range environment {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("invalid environment entry for %q", key)
		}
	}

	return nil
}

func environmentMap(entries []string) map[string]string {
	values := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key != "" {
			values[key] = value
		}
	}

	return values
}

func environmentList(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}

	return env
}

func buildProcessEnvironment(isolation *ProcessIsolation, overlays ...map[string]string) (map[string]string, error) {
	if err := validateProcessIsolation(isolation); err != nil {
		return nil, err
	}

	values := make(map[string]string, len(isolation.BaseEnvironment))
	for key, value := range isolation.BaseEnvironment {
		values[key] = value
	}

	for _, overlay := range overlays {
		if err := validateEnvironmentMap(overlay); err != nil {
			return nil, err
		}

		for key, value := range overlay {
			values[key] = value
		}
	}

	delete(values, supervisorModeEnv)
	delete(values, processIsolationUIDEnv)
	delete(values, processIsolationGIDEnv)

	if err := validateProcessSearchPath(values["PATH"]); err != nil {
		return nil, err
	}

	return values, nil
}

func validateProcessSearchPath(search string) error {
	if search == "" {
		return nil
	}

	for _, directory := range filepath.SplitList(search) {
		if directory == "" || !filepath.IsAbs(directory) {
			return fmt.Errorf("process isolation PATH contains non-absolute entry %q", directory)
		}
	}

	return nil
}

func resolveProcessExecutable(path string, env []string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("executable path is empty")
	}

	if strings.ContainsRune(path, filepath.Separator) {
		if !filepath.IsAbs(path) {
			return "", fmt.Errorf("executable path %q is not absolute", path)
		}

		info, err := os.Stat(path)
		if err != nil {
			return "", fmt.Errorf("stat executable %q: %w", path, err)
		}

		if info.IsDir() || info.Mode()&0o111 == 0 {
			return "", fmt.Errorf("executable %q is not executable", path)
		}

		return path, nil
	}

	search := environmentMap(env)["PATH"]
	if search == "" {
		return "", fmt.Errorf("find %s: process isolation PATH is empty", path)
	}

	if err := validateProcessSearchPath(search); err != nil {
		return "", fmt.Errorf("find %s: %w", path, err)
	}

	for _, directory := range filepath.SplitList(search) {
		candidate := filepath.Join(directory, path)

		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}

		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("find %s in process isolation PATH: %w", path, err)
		}
	}

	return "", fmt.Errorf("find %s in process isolation PATH: %w", path, exec.ErrNotFound)
}

func supervisorIdentityEnvironment(env []string, mode string, isolation ProcessIsolation) []string {
	values := environmentMap(env)
	values[supervisorModeEnv] = mode
	values[processIsolationUIDEnv] = strconv.FormatUint(uint64(isolation.UID), 10)
	values[processIsolationGIDEnv] = strconv.FormatUint(uint64(isolation.GID), 10)

	return environmentList(values)
}

func expectedSupervisorIdentity() (uint32, uint32, error) {
	uid, err := strconv.ParseUint(os.Getenv(processIsolationUIDEnv), 10, 32)
	if err != nil || uid == 0 {
		return 0, 0, errors.New("missing or invalid process isolation uid")
	}

	gid, err := strconv.ParseUint(os.Getenv(processIsolationGIDEnv), 10, 32)
	if err != nil || gid == 0 {
		return 0, 0, errors.New("missing or invalid process isolation gid")
	}

	return uint32(uid), uint32(gid), nil
}
