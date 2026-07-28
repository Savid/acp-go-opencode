//go:build !unix

package opencode

import "os"

// signalLeasedProcessGroup terminates the leased process. These platforms carry
// no process-group signalling, and the kill-on-close job object the server was
// started under is what takes its descendants with it.
func signalLeasedProcessGroup(pid int, _ bool) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}

	return killOpenCodeProcess(process, pid)
}
