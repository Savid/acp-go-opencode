package opencodeacp

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
)

// captureAmbientEnvironment is the seam the adapter's own environment is read
// through. Tests select a fixed environment rather than mutating the process's.
var captureAmbientEnvironment = os.Environ

// runtimeUserCacheDir is the seam the adapter account's cache root is read
// through for the default plugin seed directory.
var runtimeUserCacheDir = os.UserCacheDir

// ambientEnvironmentSnapshot folds the ambient block once, at construction, so
// every ordinary native launch and every provider-auth broker runs against the
// same base. Adapter-private carriers are dropped here rather than downstream:
// the snapshot is handed to several launchers, and a key scrubbed at one of
// them is a key that survived at the others.
func ambientEnvironmentSnapshot(entries []string) map[string]string {
	environment := make(map[string]string)

	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" || adapterPrivateEnvKey(key) {
			continue
		}

		environment[canonicalAmbientEnvironmentKey(key)] = value
	}

	return environment
}

// ambientEnvironmentEntries is the ordered block ordinary execution inherits
// from: the adapter's own process environment, or the one
// WithAmbientEnvironment supplied in its place, folded in sorted key order.
func ambientEnvironmentEntries(options Options) []string {
	if options.AmbientEnvironment == nil {
		return captureAmbientEnvironment()
	}

	keys := slices.Sorted(maps.Keys(options.AmbientEnvironment))
	entries := make([]string, 0, len(keys))

	for _, key := range keys {
		entries = append(entries, key+"="+options.AmbientEnvironment[key])
	}

	return entries
}

// validateAmbientEnvironment refuses a supplied block whose entries could not
// be environment entries at all. Which names the block then contributes is
// decided by the ordinary inheritance rules, never here.
func validateAmbientEnvironment(env map[string]string) error {
	for _, key := range slices.Sorted(maps.Keys(env)) {
		switch {
		case key == "" || strings.ContainsAny(key, "=\x00"):
			return fmt.Errorf("ambient environment key %q is not a variable name", key)
		case strings.ContainsRune(env[key], '\x00'):
			return fmt.Errorf("ambient environment value for %q contains NUL", key)
		}
	}

	return nil
}
