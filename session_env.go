package opencodeacp

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-opencode/internal/opencode"
)

const (
	valAmbiguous = "ambiguous"

	envNodeOptionsKey = "NODE_OPTIONS"
	envBashEnvKey     = "BASH_ENV"
	envShellEnvKey    = "ENV"
	envHomeKey        = "HOME"
)

func validEnvName(key string) bool {
	return key != "" && !strings.ContainsAny(key, "=\x00")
}

// blockedAgentEnvKey reports whether a caller-supplied env key names a
// variable the adapter refuses on every surface. The private adapter
// namespace is refused under every spelling; the managed runtime roots and
// the loader, node, and shell injection names are read by the native process
// under an exact platform spelling, so those compare through the platform
// identity.
func blockedAgentEnvKey(key string) bool {
	if strings.HasPrefix(strings.ToUpper(key), privateAdapterEnvPrefix) {
		return true
	}

	name := opencode.EnvironmentKey(key)
	if managedOpenCodeRootEnvKey(name) {
		return true
	}

	switch name {
	case envNodeOptionsKey, envBashEnvKey, envShellEnvKey:
		return true
	default:
		return strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_")
	}
}

// blockedSessionEnvKey additionally refuses PATH in a session env: the ordered
// extraPathDirs option is the only session-scoped search-path authority.
func blockedSessionEnvKey(key string) bool {
	return blockedAgentEnvKey(key) || opencode.EnvironmentKey(key) == envPathKey
}

// validateAgentEnv applies the session name rule to the static Agent-scoped
// environment, with PATH allowed because that surface establishes the shared
// server's native base search path. A refusal fails Agent construction.
func validateAgentEnv(env map[string]string) error {
	seen := make(map[string]string, len(env))

	for _, key := range slices.Sorted(maps.Keys(env)) {
		if !validEnvName(key) || strings.ContainsRune(env[key], '\x00') {
			return fmt.Errorf("environment key %q is not a variable name", key)
		}

		if blockedAgentEnvKey(key) {
			return fmt.Errorf("environment key %q is reserved for OpenCode runtime management", key)
		}

		identity := opencode.EnvironmentKey(key)
		if previous, duplicate := seen[identity]; duplicate {
			return fmt.Errorf("environment keys %q and %q name the same variable", previous, key)
		}

		seen[identity] = key
	}

	return nil
}

// validateSessionEnv checks a session environment in sorted key order, so the
// first refusal is the same on every call. A key that cannot be a variable
// name, a value carrying a NUL, and a blocked name each fail as unsupported at
// the key exactly as the host sent it. Two keys that name one variable under
// the platform identity fail as ambiguous at the later key: a Go map carries
// no order, so the value such a map would deliver is unknowable.
func validateSessionEnv(env map[string]string, path string) error {
	seen := make(map[string]struct{}, len(env))

	for _, key := range slices.Sorted(maps.Keys(env)) {
		if !validEnvName(key) || strings.ContainsRune(env[key], '\x00') || blockedSessionEnvKey(key) {
			return unsupportedField(path + "." + key)
		}

		identity := opencode.EnvironmentKey(key)
		if _, duplicate := seen[identity]; duplicate {
			return ambiguousField(path + "." + key)
		}

		seen[identity] = struct{}{}
	}

	return nil
}

func ambiguousField(path string) error {
	return acp.NewInvalidParams(map[string]any{
		jsonFieldError: valAmbiguous,
		jsonFieldField: path,
	})
}

// ValidateOpenCodeSessionMeta reports the refusal a session/new, session/load,
// session/resume, or fork request carrying meta receives from this package's
// _meta.opencode parsing, or nil when the vendor namespace is accepted.
func ValidateOpenCodeSessionMeta(meta map[string]any) error {
	_, err := sessionMetaFromVendorOptions(meta)

	return err
}
