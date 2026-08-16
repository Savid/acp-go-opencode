package opencodeacp

import (
	"errors"
	"fmt"

	"github.com/savid/acp-go-opencode/internal/opencode"
)

// startupError retells a failed runtime or session start by the loopback route
// that failed. The native response body is dropped: it echoes the request that
// produced it, which at session start carries MCP headers and the session
// environment. The wrapped chain stays intact so the adapter's own containment
// and cancellation matching still sees it.
type startupError struct {
	message string
	err     error
}

func (e *startupError) Error() string { return e.message }

func (e *startupError) Unwrap() error { return e.err }

// startupFailure maps a native runtime or session start failure onto the
// adapter's ACP error surface: a field the native runtime refuses becomes the
// uniform unsupported-field rejection (-32602), and every other failure keeps
// the internal-error surface with a message that names the failing route and
// status rather than the native body behind it.
func startupFailure(err error) error {
	var unsupported *opencode.UnsupportedFieldError
	if errors.As(err, &unsupported) {
		return unsupportedField(unsupported.Field)
	}

	var httpErr *opencode.HTTPError
	if !errors.As(err, &httpErr) {
		return err
	}

	return &startupError{
		message: fmt.Sprintf("opencode %s %s returned %s", httpErr.Method, httpErr.Path, httpErr.Status),
		err:     err,
	}
}
