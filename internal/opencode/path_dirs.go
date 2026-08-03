package opencode

import (
	"os"
	"strings"
)

// pathEnv names the search path a child resolves every executable against.
const pathEnv = "PATH"

// prependPathDirs rewrites a child environment so dirs precede every inherited
// PATH entry, in the order given. The inherited value is kept behind them
// rather than replaced: the native executable, its interpreter, and every
// program a shell tool reaches for are resolved against it.
func prependPathDirs(env []string, dirs []string) []string {
	if len(dirs) == 0 {
		return env
	}

	kept := make([]string, 0, len(env)+1)
	search := strings.Join(dirs, string(os.PathListSeparator))

	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key == pathEnv {
			if value != "" {
				search += string(os.PathListSeparator) + value
			}

			continue
		}

		kept = append(kept, entry)
	}

	return append(kept, pathEnv+"="+search)
}
