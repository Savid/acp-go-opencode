package opencodeacp

import "github.com/savid/acp-go-opencode/internal/opencode"

// ErrProcessContainmentIncomplete means the selected native containment
// boundary did not complete. Callers must keep the runtime quarantined.
var ErrProcessContainmentIncomplete = opencode.ErrProcessContainmentIncomplete
