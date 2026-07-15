package opencodeacp

import "github.com/savid/acp-go-opencode/internal/opencode"

// ErrProcessTreeUnproven means shutdown could not prove that every native
// OpenCode descendant exited. Callers must keep the runtime quarantined.
var ErrProcessTreeUnproven = opencode.ErrProcessTreeUnproven
