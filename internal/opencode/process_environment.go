package opencode

import (
	"fmt"
	"os"
	"slices"
	"strings"
)

const pathEnv = "PATH"

var processEnviron = os.Environ

func ValidateEnvironment(environment map[string]string) error {
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
			values[canonicalEnvironmentKey(key)] = value
		}
	}

	return values
}

func buildProcessEnvironmentFrom(base map[string]string, overlays ...map[string]string) (map[string]string, error) {
	if base == nil {
		base = captureProcessEnvironment()
	}

	if err := ValidateEnvironment(base); err != nil {
		return nil, err
	}

	values := composeEnvironment(withoutAdapterOwnedState(base))

	for _, overlay := range overlays {
		if err := ValidateEnvironment(overlay); err != nil {
			return nil, err
		}

		values = composeEnvironment(values, withoutAdapterOwnedState(overlay))
	}

	return values, nil
}

func composeEnvironment(phases ...map[string]string) map[string]string {
	values := map[string]string{}

	for _, phase := range phases {
		keys := make([]string, 0, len(phase))
		for key := range phase {
			keys = append(keys, key)
		}

		slices.Sort(keys)

		for _, key := range keys {
			values[canonicalEnvironmentKey(key)] = phase[key]
		}
	}

	return values
}

func environmentValue(environment []string, name string) string {
	for index := len(environment) - 1; index >= 0; index-- {
		key, value, ok := strings.Cut(environment[index], "=")
		if ok && EnvironmentKeyEqual(key, name) {
			return value
		}
	}

	return ""
}

func withoutAdapterOwnedState(environment map[string]string) map[string]string {
	filtered := make(map[string]string, len(environment))
	for key, value := range environment {
		if adapterPrivateEnvKey(key) || managedRuntimeRootEnvKey(key) {
			continue
		}

		filtered[key] = value
	}

	return filtered
}

const privateEnvironmentPrefix = "ACP_" + "GO_OPENCODE_INTERNAL_"

func adapterPrivateEnvKey(key string) bool {
	return strings.HasPrefix(strings.ToUpper(key), privateEnvironmentPrefix)
}

func managedRuntimeRootEnvKey(key string) bool {
	switch strings.ToUpper(key) {
	case "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR", "XDG_STATE_HOME",
		"OPENCODE_CONFIG", "OPENCODE_CONFIG_CONTENT", "OPENCODE_CONFIG_DIR", "OPENCODE_DB":
		return true
	default:
		return false
	}
}

func captureProcessEnvironment() map[string]string {
	return environmentMap(processEnviron())
}

func withoutManagedRootOverrides(environment map[string]string) map[string]string {
	filtered := withoutAdapterOwnedState(environment)
	for key := range filtered {
		if strings.EqualFold(key, "HOME") {
			delete(filtered, key)
		}
	}

	return filtered
}
