package opencodeacp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"

	"github.com/coder/acp-go-sdk"
)

// establishmentHookParam carries, inside an establishing request's own params,
// the JSON-RPC id its response will be written under. The id is what pairs the
// request the handler answered with the line the transport wrote, and a handler
// is not otherwise told which id it is answering.
const (
	establishmentHookParam = "_acp_go_opencode_establishment_hook"
	jsonRPCVersion         = "2.0"
)

// establishmentHooks holds the work an established session owes its host until
// the establishing response has actually been written to the transport. The
// opening lifecycle snapshot and the initial command catalog are both ordered
// after that write, so neither can reach a host before the response that names
// the session they speak for.
type establishmentHooks struct {
	log *slog.Logger
	mu  sync.Mutex
	all []establishmentHook
}

type establishmentHook struct {
	responseID string
	admit      func()
	run        func()
	fail       func(error)
}

func newEstablishmentHooks(log *slog.Logger) *establishmentHooks {
	return &establishmentHooks{log: log}
}

// wrap returns the transport writer the connection writes responses through.
func (h *establishmentHooks) wrap(writer io.Writer) io.Writer {
	return &establishmentWriter{writer: writer, hooks: h}
}

func (h *establishmentHooks) queue(responseID string, run func(), callbacks ...func()) {
	h.mu.Lock()
	defer h.mu.Unlock()

	hook := establishmentHook{responseID: responseID, run: run}
	if len(callbacks) > 0 {
		hook.admit = callbacks[0]
	}

	h.all = append(h.all, hook)
}

func (h *establishmentHooks) queueSession(
	responseID string,
	admit func(),
	run func(),
	fail func(error),
) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.all = append(h.all, establishmentHook{
		responseID: responseID, admit: admit, run: run, fail: fail,
	})
}

// runAfterWrite releases the hook the written response answers. A frame with no
// id is a notification and answers nothing; a frame carrying an error
// established no session, so its hook is dropped rather than run.
func (h *establishmentHooks) finishWrite(data []byte, writeErr error, complete bool) {
	responseID, success, response := establishmentResponseFrame(data)
	if !response {
		h.log.Debug("read written ACP frame for session establishment failed")

		return
	}

	h.mu.Lock()

	for index, hook := range h.all {
		if hook.responseID != responseID {
			continue
		}

		h.all = append(h.all[:index], h.all[index+1:]...)
		h.mu.Unlock()

		if !success {
			return
		}

		if writeErr != nil || !complete {
			if hook.fail != nil {
				runEstablishmentFailure(h.log, hook.fail,
					errors.New("establishing response was not fully written"))
			}

			return
		}

		if hook.admit != nil {
			if err := runEstablishmentCallback(hook.admit); err != nil {
				runEstablishmentFailure(h.log, hook.fail, err)

				return
			}
		}
		go runEstablishmentHook(h.log, hook)

		return
	}
	h.mu.Unlock()
}

func establishmentResponseFrame(data []byte) (string, bool, bool) {
	var members map[string]json.RawMessage
	if json.Unmarshal(bytes.TrimSpace(data), &members) != nil {
		return "", false, false
	}

	if len(members) != 3 {
		return "", false, false
	}

	var version string
	if json.Unmarshal(members["jsonrpc"], &version) != nil || version != jsonRPCVersion {
		return "", false, false
	}

	id, hasID := members["id"]
	_, hasResult := members["result"]

	errorValue, hasError := members["error"]
	if !hasID || hasResult == hasError || members["method"] != nil || members["params"] != nil {
		return "", false, false
	}

	trimmedID := bytes.TrimSpace(id)

	if hasError {
		var rpcError struct {
			Code    *int   `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(errorValue, &rpcError) != nil || rpcError.Code == nil || rpcError.Message == "" {
			return "", false, false
		}
	}

	return string(trimmedID), hasResult, true
}

func (h *establishmentHooks) runAfterWrite(data []byte) {
	h.finishWrite(data, nil, true)
}

func runEstablishmentHook(log *slog.Logger, hook establishmentHook) {
	if err := runEstablishmentCallback(hook.run); err != nil {
		runEstablishmentFailure(log, hook.fail, err)
	}
}

func runEstablishmentCallback(callback func()) (err error) {
	if callback == nil {
		return nil
	}

	defer func() {
		if recover() != nil {
			err = errors.New("session establishment callback panicked")
		}
	}()

	callback()

	return nil
}

func runEstablishmentFailure(log *slog.Logger, fail func(error), err error) {
	if fail == nil {
		return
	}

	defer recoverAgentGoroutine(context.Background(), log, "OpenCode session establishment failure")

	fail(err)
}

// establishmentWriter runs a queued hook once the response it waits on has left
// this process. A short or failed write delivered no response, so it releases
// nothing.
type establishmentWriter struct {
	writer io.Writer
	hooks  *establishmentHooks
}

func (w *establishmentWriter) Write(data []byte) (written int, err error) {
	defer func() {
		if recover() != nil {
			written = 0
			err = errors.New("ACP transport writer panicked")
			w.hooks.finishWrite(data, err, false)
		}
	}()

	written, err = w.writer.Write(data)
	complete := err == nil && written == len(data)
	w.hooks.finishWrite(data, err, complete)

	if err == nil && written != len(data) {
		err = io.ErrShortWrite
	}

	return written, err
}

// establishmentTagReader stamps every establishing request with the id its
// response will carry, so the handler that answers it can queue work against
// that exact response line.
type establishmentTagReader struct {
	reader  *bufio.Reader
	pending []byte
	err     error
}

func newEstablishmentTagReader(reader io.Reader) *establishmentTagReader {
	return &establishmentTagReader{reader: bufio.NewReader(reader)}
}

func (r *establishmentTagReader) Read(p []byte) (int, error) {
	if len(r.pending) == 0 {
		if r.err != nil {
			err := r.err
			r.err = nil

			return 0, err
		}

		line, err := r.reader.ReadBytes('\n')
		if len(line) == 0 {
			return 0, err
		}

		r.pending = tagEstablishingRequest(line)
		if err != nil {
			r.err = err
		}
	}

	read := copy(p, r.pending)
	r.pending = r.pending[read:]

	return read, nil
}

// tagEstablishingRequest adds the hook id to one establishing request. Anything
// else — another method, a notification, an unreadable line — is passed through
// byte for byte.
func tagEstablishingRequest(line []byte) []byte {
	var frame struct {
		JSONRPC string           `json:"jsonrpc,omitempty"`
		ID      *json.RawMessage `json:"id,omitempty"`
		Method  string           `json:"method,omitempty"`
		Params  json.RawMessage  `json:"params,omitempty"`
	}
	if err := json.Unmarshal(line, &frame); err != nil {
		return line
	}

	if frame.ID == nil || !establishingMethod(frame.Method) || len(bytes.TrimSpace(frame.Params)) == 0 {
		return line
	}

	members := make(map[string]json.RawMessage)
	if err := json.Unmarshal(frame.Params, &members); err != nil {
		return line
	}

	// Nothing here can fail to marshal: the id is a string, and every member is
	// raw JSON this function has already decoded, so the errors are discarded
	// rather than turned into branches no input can reach.
	hookID, _ := json.Marshal(establishmentResponseID(frame.ID))
	members[establishmentHookParam] = hookID
	frame.Params, _ = json.Marshal(members)

	encoded, _ := json.Marshal(frame)

	if bytes.HasSuffix(line, []byte("\n")) {
		encoded = append(encoded, '\n')
	}

	return encoded
}

// establishingMethod names the four requests that establish a session and owe a
// host an opening snapshot and a command catalog.
func establishingMethod(method string) bool {
	switch method {
	case acp.AgentMethodSessionNew, acp.AgentMethodSessionLoad, acp.AgentMethodSessionResume, ForkSessionMethod:
		return true
	default:
		return false
	}
}

func establishmentResponseID(id *json.RawMessage) string {
	return string(bytes.TrimSpace(*id))
}

// establishmentHookID reads back the id this request was tagged with. An
// untagged request reached the handler some other way than the transport, so it
// has no response line to wait for.
func establishmentHookID(params json.RawMessage) string {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(params, &members); err != nil {
		return ""
	}

	var responseID string
	if err := json.Unmarshal(members[establishmentHookParam], &responseID); err != nil {
		return ""
	}

	return responseID
}

// queueEstablishment schedules what a newly established session owes its host
// once the establishing response has been written: the incarnation's opening
// lifecycle snapshot, which must be that session's first lifecycle-bearing
// notification, and then its initial command catalog. Both are asynchronous by
// contract, and neither can retroactively fail the response it follows.
func (c *localAgentConnection) queueEstablishment(ctx context.Context, method string, params json.RawMessage, result any) {
	sessionID, established := establishedSessionID(method, params, result)
	if !established || c.hooks == nil {
		return
	}

	responseID := establishmentHookID(params)
	if responseID == "" {
		return
	}

	hookCtx := context.WithoutCancel(ctx)

	session, err := c.agent.session(sessionID)
	if err != nil {
		return
	}

	c.hooks.queueSession(
		responseID,
		session.markEstablishmentResponseWritten,
		func() { c.establishSession(hookCtx, method, sessionID) },
		func(err error) { session.failEstablishment(err) },
	)
}

// establishSession opens the session's stream and publishes its first command
// catalog. Every failure here is diagnostic: the response it follows is already
// on the wire, and a catalog is republished on the next prompt.
func (c *localAgentConnection) establishSession(ctx context.Context, method string, sessionID acp.SessionId) {
	session, err := c.agent.session(sessionID)
	if err != nil {
		c.logEstablishmentFailure(ctx, "session lookup", method, sessionID, err)

		return
	}

	if err := session.ensureEstablished(ctx); err != nil {
		c.logEstablishmentFailure(ctx, "lifecycle stream", method, sessionID, err)
	}

	if err := session.refreshCommands(ctx); err != nil {
		c.logEstablishmentFailure(ctx, "command catalog", method, sessionID, err)
	}
}

func (c *localAgentConnection) logEstablishmentFailure(
	ctx context.Context,
	stage string,
	method string,
	sessionID acp.SessionId,
	_ error,
) {
	c.agent.log.ErrorContext(ctx, "post-response OpenCode session establishment failed",
		slog.String("stage", stage),
		slog.String(jsonFieldMethod, method),
		slog.String("session_id", string(sessionID)),
	)
}

// establishedSessionID names the session an answered request established. The
// two creating routes read it off their own response, and the two reusing routes
// read it off the request, because that is where each one carries it.
func establishedSessionID(method string, params json.RawMessage, result any) (acp.SessionId, bool) {
	switch method {
	case acp.AgentMethodSessionNew:
		response, ok := result.(acp.NewSessionResponse)

		return response.SessionId, ok && response.SessionId != ""
	case acp.AgentMethodSessionLoad, acp.AgentMethodSessionResume:
		var request struct {
			SessionId acp.SessionId `json:"sessionId"`
		}
		if err := json.Unmarshal(params, &request); err != nil {
			return "", false
		}

		return request.SessionId, request.SessionId != ""
	case ForkSessionMethod:
		response, ok := result.(acp.UnstableForkSessionResponse)

		return response.SessionId, ok && response.SessionId != ""
	default:
		return "", false
	}
}
