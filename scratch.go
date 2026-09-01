package opencodeacp

import (
	"fmt"
	"os"
)

var runtimeMkdirTemp = os.MkdirTemp

// scratchParent resolves the parent directory for all ephemeral on-disk
// materialization: dir when set, else the system temp directory. This is the
// only place in the module that consults the system temp directory.
func scratchParent(dir string) string {
	if dir != "" {
		return dir
	}

	return os.TempDir()
}

// ensureScratchParent resolves the scratch parent and creates it 0700 when
// missing.
func ensureScratchParent(dir string) (string, error) {
	parent := scratchParent(dir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", fmt.Errorf("create scratch parent: %w", err)
	}

	return parent, nil
}

func (a *Agent) scratchParent() string {
	return scratchParent(a.options.ScratchDir)
}

func (a *Agent) ensureScratchParent() (string, error) {
	return ensureScratchParent(a.options.ScratchDir)
}

func (a *Agent) newRuntimeRoot() (string, bool, error) {
	if a.options.Home != "" {
		return a.options.Home, false, nil
	}

	parent, err := ensureScratchParent(a.options.ScratchDir)
	if err != nil {
		return "", false, err
	}

	root, err := runtimeMkdirTemp(parent, "acp-go-opencode-runtime-")
	if err != nil {
		return "", false, fmt.Errorf("create OpenCode runtime root: %w", err)
	}

	return root, true, nil
}
