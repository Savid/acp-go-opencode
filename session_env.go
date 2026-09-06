package opencodeacp

import (
	"maps"
	"runtime"
	"slices"
	"strings"

	"github.com/coder/acp-go-sdk"
)

const (
	errValueAmbiguous = "ambiguous"

	platformWindows = "windows"

	envNodeOptionsKey = "NODE_OPTIONS"
	envBashEnvKey     = "BASH_ENV"
	envShellEnvKey    = "ENV"
)

var sessionEnvPlatform = runtime.GOOS

// sessionEnvIdentity is the name the target platform resolves an environment
// key by: the exact bytes on Unix, where PATH and path are two variables, and
// the upper-cased spelling on Windows, where they are one.
func sessionEnvIdentity(key string) string {
	if sessionEnvPlatform == platformWindows {
		return strings.ToUpper(key)
	}

	return key
}

func validEnvName(key string) bool {
	return key != "" && !strings.ContainsAny(key, "=\x00")
}

// blockedSessionEnvKey reports whether a session env key names a variable the
// adapter refuses to install on the addressed native session. The private
// adapter namespace is refused under every spelling. PATH is owned by
// extraPathDirs alone; the managed runtime roots and the loader, node, and
// shell injection names are read by the native process under an exact
// platform spelling, so those compare through the platform identity.
func blockedSessionEnvKey(key string) bool {
	if strings.HasPrefix(strings.ToUpper(key), privateAdapterEnvPrefix) {
		return true
	}

	name := sessionEnvIdentity(key)
	if managedOpenCodeRootEnvKey(name) {
		return true
	}

	switch name {
	case envPathKey, envNodeOptionsKey, envBashEnvKey, envShellEnvKey:
		return true
	default:
		return strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_")
	}
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

		identity := sessionEnvIdentity(key)
		if _, duplicate := seen[identity]; duplicate {
			return ambiguousField(path + "." + key)
		}

		seen[identity] = struct{}{}
	}

	return nil
}

func ambiguousField(path string) *acp.RequestError {
	return acp.NewInvalidParams(map[string]any{
		jsonFieldError: errValueAmbiguous,
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
