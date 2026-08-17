package opencode

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

const (
	DarwinRuntimeIDEnv     = "ACP_GO_OPENCODE_RUNTIME_ID"
	DarwinScratchRootEnv   = "ACP_GO_OPENCODE_SCRATCH_ROOT"
	darwinLifecycleRuntime = "runtime"
	parentPathSegment      = ".."
)

// DarwinGeneration binds one native launch to its wrapper-created writable
// generation root and persistent operator-diagnostic record.
type DarwinGeneration struct {
	RuntimeID      string
	ScratchRoot    string
	RecordStarted  func(pid, pgid int) error
	RecordFinished func(complete bool) error

	finishOnce sync.Once
	finishErr  error
}

func (g *DarwinGeneration) prepareCommand(cmd *exec.Cmd) error {
	if g == nil || cmd == nil || g.RuntimeID == "" || g.ScratchRoot == "" {
		return errors.New("darwin containment generation is unavailable")
	}

	overrides := map[string]string{
		DarwinRuntimeIDEnv:   g.RuntimeID,
		DarwinScratchRootEnv: g.ScratchRoot,
	}
	env := make([]string, 0, len(cmd.Env)+len(overrides))

	for _, entry := range cmd.Env {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, private := overrides[key]; private {
				continue
			}
		}

		env = append(env, entry)
	}

	for _, key := range []string{DarwinRuntimeIDEnv, DarwinScratchRootEnv} {
		env = append(env, key+"="+overrides[key])
	}

	cmd.Env = env

	return nil
}

func (g *DarwinGeneration) started(pid, pgid int) error {
	if g == nil || g.RecordStarted == nil {
		return nil
	}

	return g.RecordStarted(pid, pgid)
}

func (g *DarwinGeneration) finish(complete bool) error {
	if g == nil {
		return nil
	}

	g.finishOnce.Do(func() {
		if g.RecordFinished != nil {
			if err := g.RecordFinished(complete); err != nil {
				g.finishErr = fmt.Errorf("%w: update Darwin containment record: %v", ErrProcessContainmentIncomplete, err)
			}
		}
	})

	return g.finishErr
}

func pathWithin(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)

	return err == nil && relative != "." && relative != parentPathSegment &&
		!strings.HasPrefix(relative, parentPathSegment+string(filepath.Separator))
}
