package opencodeacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/lifecycle"
)

type hostRequestKey struct {
	method   string
	streamID string
	actionID string
}

type permissionRequestResult struct {
	response acp.RequestPermissionResponse
	err      error
}

type elicitationRequestResult struct {
	response acp.UnstableCreateElicitationResponse
	err      error
}

type registeredPermissionRequest struct {
	registered <-chan error
	answered   <-chan permissionRequestResult
}

type registeredElicitationRequest struct {
	registered <-chan error
	answered   <-chan elicitationRequestResult
}

// hostRequestRegistrations joins an action's lifecycle correlation to the
// exact JSON-RPC request written for it. A registration is installed before the
// request goroutine starts and is released only after that full request frame
// has left the process; acp.Connection installs its pending response entry
// before it writes the frame, so release proves the request is answerable.
type hostRequestRegistrations struct {
	mu      sync.Mutex
	pending map[hostRequestKey]chan error
}

func newHostRequestRegistrations() *hostRequestRegistrations {
	return &hostRequestRegistrations{pending: map[hostRequestKey]chan error{}}
}

func (r *hostRequestRegistrations) wrap(writer io.Writer) io.Writer {
	return &hostRequestRegistrationWriter{writer: writer, registrations: r}
}

func (r *hostRequestRegistrations) expect(key hostRequestKey) <-chan error {
	registered := make(chan error, 1)
	if r == nil || key.method == "" || key.streamID == "" || key.actionID == "" {
		registered <- errors.New("host action request has incomplete lifecycle correlation")

		return registered
	}

	r.mu.Lock()
	if _, exists := r.pending[key]; exists {
		r.mu.Unlock()

		registered <- errors.New("host action request correlation is already registered")

		return registered
	}

	r.pending[key] = registered
	r.mu.Unlock()

	return registered
}

func (r *hostRequestRegistrations) complete(key hostRequestKey, err error) {
	if r == nil {
		return
	}

	r.mu.Lock()
	registered := r.pending[key]

	if registered != nil {
		delete(r.pending, key)
	}
	r.mu.Unlock()

	if registered != nil {
		registered <- err
	}
}

func (r *hostRequestRegistrations) failIfPending(key hostRequestKey, err error) {
	if err == nil {
		err = errors.New("host action request returned before its JSON-RPC registration was observed")
	}

	r.complete(key, err)
}

type hostRequestRegistrationWriter struct {
	writer        io.Writer
	registrations *hostRequestRegistrations
}

func (w *hostRequestRegistrationWriter) Write(data []byte) (int, error) {
	written, err := w.writer.Write(data)
	if err == nil && written != len(data) {
		err = io.ErrShortWrite
	}

	if err == nil && written == len(data) {
		if key, ok := hostRequestKeyFromFrame(data); ok {
			w.registrations.complete(key, nil)
		}
	}

	return written, err
}

func hostRequestKeyFromFrame(data []byte) (hostRequestKey, bool) {
	var frame struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(data), &frame); err != nil {
		return hostRequestKey{}, false
	}

	if frame.Method != acp.ClientMethodSessionRequestPermission && frame.Method != acp.ClientMethodElicitationCreate {
		return hostRequestKey{}, false
	}

	var params map[string]json.RawMessage
	if err := json.Unmarshal(frame.Params, &params); err != nil {
		return hostRequestKey{}, false
	}

	var meta map[string]json.RawMessage
	if err := json.Unmarshal(params["_meta"], &meta); err != nil {
		return hostRequestKey{}, false
	}

	var correlation struct {
		StreamID string `json:"streamId"`
		Action   struct {
			ActionID string `json:"actionId"`
		} `json:"action"`
	}
	if err := json.Unmarshal(meta[lifecycle.MetaKey], &correlation); err != nil {
		return hostRequestKey{}, false
	}

	key := hostRequestKey{method: frame.Method, streamID: correlation.StreamID, actionID: correlation.Action.ActionID}

	return key, key.streamID != "" && key.actionID != ""
}

func (c *localAgentConnection) BeginRequestPermission(
	ctx context.Context,
	params acp.RequestPermissionRequest,
	key hostRequestKey,
) registeredPermissionRequest {
	registered := c.registrations.expect(key)
	answered := make(chan permissionRequestResult, 1)

	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				err := errors.New("host permission request panicked")
				c.registrations.failIfPending(key, err)

				answered <- permissionRequestResult{err: err}
			}
		}()

		response, err := c.RequestPermission(ctx, params)
		c.registrations.failIfPending(key, err)

		answered <- permissionRequestResult{response: response, err: err}
	}()

	return registeredPermissionRequest{registered: registered, answered: answered}
}

func (c *localAgentConnection) BeginCreateElicitation(
	ctx context.Context,
	params acp.UnstableCreateElicitationRequest,
	scope elicitationScope,
	key hostRequestKey,
) registeredElicitationRequest {
	registered := c.registrations.expect(key)
	answered := make(chan elicitationRequestResult, 1)

	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				err := errors.New("host elicitation request panicked")
				c.registrations.failIfPending(key, err)

				answered <- elicitationRequestResult{err: err}
			}
		}()

		response, err := c.CreateElicitation(ctx, params, scope)
		c.registrations.failIfPending(key, err)

		answered <- elicitationRequestResult{response: response, err: err}
	}()

	return registeredElicitationRequest{registered: registered, answered: answered}
}
