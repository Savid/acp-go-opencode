//go:build !unix

package opencode

import "os"

// signalLeasedProcessGroup terminates the leased process. These platforms carry
// no process-group signalling. Native server launch is unsupported, so this
// direct-process fallback does not claim descendant containment.
func signalLeasedProcessGroup(pid int, _ bool) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}

	return killOpenCodeProcess(process, pid)
}
