package opencodeacp

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"
)

const (
	platformDarwin              = "darwin"
	platformLinux               = "linux"
	platformWindows             = "windows"
	managedEnvXDGDataHome       = "XDG_DATA_HOME"
	managedEnvOpenCodeConfigDir = "OPENCODE_CONFIG_DIR"

	privateAdapterEnvPrefix = "ACP_" + "GO_OPENCODE_INTERNAL_"
)

var runtimeGOOS = runtime.GOOS

func containmentMode(options Options) RuntimeContainmentMode {
	if options.DarwinBestEffortContainment && runtimeGOOS != platformDarwin {
		return RuntimeContainmentUnavailable
	}

	switch runtimeGOOS {
	case platformLinux:
		return RuntimeContainmentAuthoritative
	case platformDarwin:
		if options.DarwinBestEffortContainment {
			return RuntimeContainmentBestEffort
		}
	}

	return RuntimeContainmentUnavailable
}

func validateContainmentOptions(options Options) error {
	if options.DarwinBestEffortContainment && runtimeGOOS != platformDarwin {
		return errors.New("darwin best-effort containment is supported only on darwin")
	}

	for key := range options.Env {
		if reservedOpenCodeEnvKey(key) {
			return errors.New("environment key " + key + " is reserved for OpenCode runtime management")
		}
	}

	if options.ProcessIsolation != nil && options.Home != "" {
		if err := validateDurableHomePath(options.Home); err != nil {
			return err
		}
	}

	return nil
}

func validateDurableHomePath(path string) error {
	if path == "" || len(path) > 4096 || path == "/" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("OpenCode runtime home must be a canonical absolute non-root path of at most 4096 bytes")
	}

	for _, character := range path {
		if unicode.IsControl(character) {
			return errors.New("OpenCode runtime home must not contain control characters")
		}
	}

	return nil
}

func reservedOpenCodeEnvKey(key string) bool {
	upper := strings.ToUpper(key)

	return strings.HasPrefix(upper, privateAdapterEnvPrefix) || managedOpenCodeRootEnvKey(upper)
}

func managedOpenCodeRootEnvKey(key string) bool {
	switch strings.ToUpper(key) {
	case "HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", managedEnvXDGDataHome, "XDG_RUNTIME_DIR", "XDG_STATE_HOME",
		"OPENCODE_CONFIG", "OPENCODE_CONFIG_CONTENT", managedEnvOpenCodeConfigDir, "OPENCODE_DB":
		return true
	default:
		return false
	}
}

func normalizeStandaloneHome(options *Options) error {
	if options == nil || options.ProcessIsolation == nil {
		return nil
	}

	isolation := options.ProcessIsolation
	if isolation.IdentityLock != nil || isolation.AuthorityDomain != nil || isolation.StandaloneStateRoot == "" {
		return nil
	}

	if options.Home == "" {
		options.Home = isolation.StandaloneStateRoot

		return nil
	}

	if options.Home != isolation.StandaloneStateRoot {
		return errors.New("WithHome must equal ProcessIsolation.StandaloneStateRoot " + isolation.StandaloneStateRoot)
	}

	return nil
}
