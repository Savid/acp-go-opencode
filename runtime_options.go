package opencodeacp

import (
	"errors"
	"path/filepath"
	"strings"
	"unicode"
)

func validateRuntimeOptions(options Options) error {
	for key := range options.Env {
		if reservedOpenCodeEnvKey(key) {
			return errors.New("environment key " + key + " is reserved for OpenCode runtime management")
		}
	}

	if options.Home != "" {
		if err := validateDurableHomePath(options.Home); err != nil {
			return err
		}
	}

	if options.hostAuthorityConfigured {
		return validateHostAuthority(options.HostAuthority)
	}

	return nil
}

func validateDurableHomePath(path string) error {
	if len(path) > 4096 || path == "/" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("OpenCode runtime home must be a canonical absolute non-root path of at most 4096 bytes")
	}

	for _, character := range path {
		if unicode.IsControl(character) {
			return errors.New("OpenCode runtime home must not contain control characters")
		}
	}

	return nil
}

const privateAdapterEnvPrefix = "ACP_" + "GO_OPENCODE_INTERNAL_"

func reservedOpenCodeEnvKey(key string) bool {
	upper := strings.ToUpper(key)

	if strings.HasPrefix(upper, privateAdapterEnvPrefix) {
		return true
	}

	switch upper {
	case "HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR", "XDG_STATE_HOME",
		"OPENCODE_CONFIG", "OPENCODE_CONFIG_CONTENT", "OPENCODE_CONFIG_DIR", "OPENCODE_DB":
		return true
	default:
		return false
	}
}

func adapterPrivateEnvKey(key string) bool {
	return strings.HasPrefix(strings.ToUpper(key), privateAdapterEnvPrefix)
}
