package opencodeacp

import (
	"github.com/savid/acp-go-opencode/internal/homelock"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

// ErrProcessContainmentIncomplete means the selected native containment
// boundary did not complete. Callers must keep the runtime quarantined.
var ErrProcessContainmentIncomplete = opencode.ErrProcessContainmentIncomplete

// ErrRuntimeLockUnsupported means this platform has no advisory-lock primitive
// to build the runtime home's single-writer fence from, so the shared runtime
// cannot be constructed here at all. It is a public sentinel because a host has
// to tell it apart from a lock another process is holding: the second is worth
// retrying and the first never is. Construction fails with it rather than
// proceeding unlocked, because a home nobody can claim exclusively would let two
// runtimes write one native database and call it ownership.
var ErrRuntimeLockUnsupported = homelock.ErrRuntimeLockUnsupported
