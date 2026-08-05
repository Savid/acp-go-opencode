package opencode

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ProcessIsolation is the mandatory credential and complete environment base
// applied to every provider process.
type ProcessIdentityLockCapability interface {
	Duplicate() (*os.File, error)
}

type ProcessIsolation struct {
	UID                      uint32
	GID                      uint32
	BaseEnvironment          map[string]string
	StandaloneOwnerID        string
	StandaloneStateRoot      string
	IdentityLock             ProcessIdentityLockCapability
	AuthorityDomain          ProcessIdentityLockCapability
	identityAuthorityAdopted bool
}

var processIsolationGOOS = runtime.GOOS

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

	if processIsolationGOOS == "linux" {
		if err := validateStandaloneIdentityDisposition(isolation); err != nil {
			return err
		}
	}

	return validateProcessIsolationPlatform()
}

func validateStandaloneIdentityDisposition(isolation *ProcessIsolation) error {
	identityLock, authorityDomain := isolation.IdentityLock != nil, isolation.AuthorityDomain != nil

	if isolation.identityAuthorityAdopted {
		if identityLock || authorityDomain {
			return errors.New("adopted process identity authority cannot carry duplicable capabilities")
		}

		identityLock = true
		authorityDomain = true
	}

	if identityLock != authorityDomain {
		return errors.New("process identity lock and authority domain must be provided together")
	}

	if identityLock {
		if isolation.StandaloneOwnerID != "" || isolation.StandaloneStateRoot != "" {
			return errors.New("borrowed process identity forbids standalone owner fields")
		}

		return nil
	}

	if !validStandaloneOwnerID(isolation.StandaloneOwnerID) {
		return errors.New("standalone owner id must be 1..256 canonical ASCII bytes")
	}

	if !validStandaloneStateRootPath(isolation.StandaloneStateRoot) {
		return errors.New("standalone state root must be a clean absolute path outside the authority root")
	}

	return nil
}

func validStandaloneOwnerID(value string) bool {
	if value == "" || len(value) > 256 || !standaloneOwnerIDAlphanumeric(value[0]) {
		return false
	}

	for index := 1; index < len(value); index++ {
		if !standaloneOwnerIDAlphanumeric(value[index]) && !strings.ContainsRune("._:@/-", rune(value[index])) {
			return false
		}
	}

	return true
}

func standaloneOwnerIDAlphanumeric(value byte) bool {
	return value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z' || value >= '0' && value <= '9'
}

func validStandaloneStateRootPath(value string) bool {
	if value == "" || len(value) > 4096 || !utf8.ValidString(value) || !filepath.IsAbs(value) ||
		filepath.Clean(value) != value || value == "/" || strings.IndexByte(value, 0) >= 0 {
		return false
	}

	const authorityRoot = "/var/lib/acp-go/agent-identities"
	if value == authorityRoot || strings.HasPrefix(value, authorityRoot+string(filepath.Separator)) {
		return false
	}

	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}

	return true
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

	if err := validateProcessSearchPath(values["PATH"]); err != nil {
		return nil, err
	}

	return values, nil
}

func withoutManagedRootOverrides(environment map[string]string) map[string]string {
	filtered := make(map[string]string, len(environment))
	for key, value := range environment {
		switch strings.ToUpper(key) {
		case "HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR", "XDG_STATE_HOME",
			"OPENCODE_CONFIG", "OPENCODE_CONFIG_CONTENT", "OPENCODE_CONFIG_DIR", "OPENCODE_DB":
			continue
		default:
			filtered[key] = value
		}
	}

	return filtered
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
