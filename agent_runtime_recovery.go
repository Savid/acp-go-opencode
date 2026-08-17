package opencodeacp

// directoryBindingIncarnation fences release functions across shared-runtime
// recovery. A release captured by an old runtime generation must not delete a
// same-session principal rebound at the same canonical directory.
type directoryBindingIncarnation uint64

// nextDirectoryBindingIncarnationLocked returns a non-zero, Agent-local token.
// The caller must hold a.mu.
func (a *Agent) nextDirectoryBindingIncarnationLocked() directoryBindingIncarnation {
	a.directoryIncarnation++
	if a.directoryIncarnation == 0 {
		// Reserve zero as the uninitialized value even after theoretical wrap.
		a.directoryIncarnation++
	}

	return a.directoryIncarnation
}
