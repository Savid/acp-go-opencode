package opencodeacp

import (
	"os"
	"strings"
)

// captureAmbientEnvironment snapshots the adapter's own environment once, at
// construction, so every ordinary native launch and every provider-auth broker
// runs against the same base rather than against whatever os.Environ happens to
// hold later. Adapter-private carriers are dropped here rather than downstream:
// the snapshot is handed to several launchers, and a key scrubbed at one of
// them is a key that survived at the others.
func captureAmbientEnvironment() map[string]string {
	environment := make(map[string]string)

	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" || adapterPrivateEnvKey(key) {
			continue
		}

		environment[key] = value
	}

	return environment
}
