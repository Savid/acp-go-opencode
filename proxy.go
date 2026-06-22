package opencodeacp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

const (
	// OpenCodeSessionExportMethod exports the current persisted OpenCode
	// session JSON through the wrapper proxy.
	OpenCodeSessionExportMethod = "_opencode/session/export"
	// OpenCodeSessionImportMethod imports OpenCode session JSON through the
	// wrapper proxy.
	OpenCodeSessionImportMethod = "_opencode/session/import"
	// OpenCodeSessionDeleteMethod deletes an OpenCode session through the
	// wrapper proxy.
	OpenCodeSessionDeleteMethod = "_opencode/session/delete"

	proxyPackageMetaKey = "github.com/savid/acp-go-opencode"
)

var (
	exportSessionForProxy       = ExportSession
	importSessionFileForProxy   = ImportSessionFile
	deleteSessionForProxy       = DeleteSession
	sessionIDFromExportForProxy = SessionIDFromExport
)

type rpcEnvelope struct {
	ID     *json.RawMessage `json:"id,omitempty"`
	Method string           `json:"method,omitempty"`
	Params json.RawMessage  `json:"params,omitempty"`
	Result json.RawMessage  `json:"result,omitempty"`
	Error  *rpcError        `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type acpProxy struct {
	output       io.Writer
	childInput   io.Writer
	cliOptions   []Option
	mu           sync.Mutex
	pendingByID  map[string]string
	writeHostMu  sync.Mutex
	writeChildMu sync.Mutex
}

func serveProxy(
	ctx context.Context,
	input io.Reader,
	output io.Writer,
	stderr io.Writer,
	cliOptions []Option,
	processOptions opencode.Options,
) error {
	childInputReader, childInputWriter := io.Pipe()
	childOutputReader, childOutputWriter := io.Pipe()
	proxy := newACPProxy(childInputWriter, output, cliOptions)

	runErr := make(chan error, 1)
	go func() {
		err := runACP(ctx, childInputReader, childOutputWriter, stderr, processOptions)
		_ = childOutputWriter.Close()
		_ = childInputReader.Close()
		runErr <- err
	}()

	hostErr := make(chan error, 1)
	go func() {
		err := proxy.forwardHostToChild(ctx, input)
		_ = childInputWriter.Close()
		hostErr <- err
	}()

	nativeErr := make(chan error, 1)
	go func() {
		nativeErr <- proxy.forwardChildToHost(childOutputReader)
	}()

	var joined error
	var runDone bool
	var nativeDone bool
	for !runDone || !nativeDone {
		select {
		case err := <-hostErr:
			hostErr = nil
			if err != nil {
				joined = errors.Join(joined, err)
			}
		case err := <-nativeErr:
			nativeErr = nil
			nativeDone = true
			if err != nil {
				joined = errors.Join(joined, err)
			}
		case err := <-runErr:
			runErr = nil
			runDone = true
			_ = childOutputWriter.Close()
			if err != nil {
				joined = errors.Join(joined, err)
			}
		}
	}

	return collectReadyHostErr(joined, hostErr)
}

func collectReadyHostErr(joined error, hostErr <-chan error) error {
	if hostErr == nil {
		return joined
	}
	select {
	case err := <-hostErr:
		return errors.Join(joined, err)
	default:
		return joined
	}
}

func newACPProxy(childInput io.Writer, output io.Writer, cliOptions []Option) *acpProxy {
	return &acpProxy{
		childInput:  childInput,
		output:      output,
		cliOptions:  append([]Option(nil), cliOptions...),
		pendingByID: map[string]string{},
	}
}

func (p *acpProxy) forwardHostToChild(ctx context.Context, input io.Reader) error {
	return readJSONLines(input, func(line []byte) error {
		msg, ok := decodeRPCEnvelope(line)
		if !ok {
			return p.writeChildLine(line)
		}

		if wrapperExtensionMethod(msg.Method) {
			return p.handleWrapperExtension(ctx, msg)
		}

		if msg.ID != nil && msg.Method != "" {
			p.rememberPending(*msg.ID, msg.Method)
		}

		return p.writeChildLine(line)
	})
}

func (p *acpProxy) forwardChildToHost(input io.Reader) error {
	return readJSONLines(input, func(line []byte) error {
		msg, ok := decodeRPCEnvelope(line)
		if !ok {
			return p.writeHostLine(line)
		}

		if msg.ID != nil && msg.Method == "" {
			method := p.takePending(*msg.ID)
			if method == acp.AgentMethodInitialize && len(msg.Result) > 0 && msg.Error == nil {
				if patched, ok := patchInitializeResponse(line); ok {
					return p.writeHostLine(patched)
				}
			}
		}

		return p.writeHostLine(line)
	})
}

func readJSONLines(reader io.Reader, handle func([]byte) error) error {
	buffered := bufio.NewReader(reader)
	for {
		line, err := buffered.ReadBytes('\n')
		if len(line) > 0 && len(bytes.TrimSpace(line)) > 0 {
			if handleErr := handle(line); handleErr != nil {
				return handleErr
			}
		}
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			return nil
		}

		return err
	}
}

func decodeRPCEnvelope(line []byte) (rpcEnvelope, bool) {
	var msg rpcEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(line), &msg); err != nil {
		return rpcEnvelope{}, false
	}

	return msg, msg.Method != "" || msg.ID != nil
}

func (p *acpProxy) rememberPending(id json.RawMessage, method string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pendingByID[string(id)] = method
}

func (p *acpProxy) takePending(id json.RawMessage) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	method := p.pendingByID[string(id)]
	delete(p.pendingByID, string(id))

	return method
}

func wrapperExtensionMethod(method string) bool {
	return method == OpenCodeSessionExportMethod ||
		method == OpenCodeSessionImportMethod ||
		method == OpenCodeSessionDeleteMethod
}

func (p *acpProxy) handleWrapperExtension(ctx context.Context, msg rpcEnvelope) error {
	if msg.ID == nil {
		return nil
	}

	var result any
	var err error
	switch msg.Method {
	case OpenCodeSessionExportMethod:
		result, err = p.handleSessionExport(ctx, msg.Params)
	case OpenCodeSessionImportMethod:
		result, err = p.handleSessionImport(ctx, msg.Params)
	case OpenCodeSessionDeleteMethod:
		result, err = p.handleSessionDelete(ctx, msg.Params)
	default:
		err = requestError(-32601, fmt.Sprintf("method not found: %s", msg.Method))
	}

	return p.writeHostResponse(*msg.ID, result, err)
}

func (p *acpProxy) handleSessionExport(ctx context.Context, params json.RawMessage) (map[string]any, error) {
	var req struct {
		SessionID acp.SessionId `json:"sessionId"`
	}
	if len(bytes.TrimSpace(params)) > 0 {
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, requestError(-32602, fmt.Sprintf("invalid params: %v", err))
		}
	}

	exported, err := exportSessionForProxy(ctx, req.SessionID, p.cliOptions...)
	if err != nil {
		return nil, requestError(-32000, err.Error())
	}
	sessionID, err := sessionIDFromExportForProxy(exported)
	if err != nil {
		return nil, requestError(-32000, err.Error())
	}

	return map[string]any{
		"sessionId": sessionID,
		"bytes":     len(exported),
		"rawJSON":   string(exported),
	}, nil
}

func (p *acpProxy) handleSessionImport(ctx context.Context, params json.RawMessage) (map[string]any, error) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, requestError(-32602, fmt.Sprintf("invalid params: %v", err))
	}
	if strings.TrimSpace(req.Path) == "" {
		return nil, requestError(-32602, "path is required")
	}

	out, err := importSessionFileForProxy(ctx, req.Path, p.cliOptions...)
	if err != nil {
		return nil, requestError(-32000, err.Error())
	}

	return map[string]any{"output": string(out)}, nil
}

func (p *acpProxy) handleSessionDelete(ctx context.Context, params json.RawMessage) (map[string]any, error) {
	var req struct {
		SessionID acp.SessionId `json:"sessionId"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, requestError(-32602, fmt.Sprintf("invalid params: %v", err))
	}
	if strings.TrimSpace(string(req.SessionID)) == "" {
		return nil, requestError(-32602, "sessionId is required")
	}

	if err := deleteSessionForProxy(ctx, req.SessionID, p.cliOptions...); err != nil {
		return nil, requestError(-32000, err.Error())
	}

	return map[string]any{"deleted": true, "sessionId": req.SessionID}, nil
}

func requestError(code int, message string) error {
	return rpcRequestError{code: code, message: message}
}

type rpcRequestError struct {
	code    int
	message string
}

func (e rpcRequestError) Error() string { return e.message }

func (p *acpProxy) writeHostResponse(id json.RawMessage, result any, err error) error {
	response := map[string]any{"jsonrpc": "2.0", "id": id}
	if err != nil {
		requestErr := rpcRequestError{code: -32000, message: err.Error()}
		_ = errors.As(err, &requestErr)
		response["error"] = rpcError{Code: requestErr.code, Message: requestErr.message}
	} else {
		response["result"] = result
	}

	data, marshalErr := json.Marshal(response)
	if marshalErr != nil {
		return marshalErr
	}

	return p.writeHostLine(data)
}

func (p *acpProxy) writeHostLine(line []byte) error {
	p.writeHostMu.Lock()
	defer p.writeHostMu.Unlock()

	return writeJSONLine(p.output, line)
}

func (p *acpProxy) writeChildLine(line []byte) error {
	p.writeChildMu.Lock()
	defer p.writeChildMu.Unlock()

	return writeJSONLine(p.childInput, line)
}

func writeJSONLine(writer io.Writer, line []byte) error {
	if _, err := writer.Write(bytes.TrimRight(line, "\r\n")); err != nil {
		return err
	}
	_, err := writer.Write([]byte("\n"))

	return err
}

func patchInitializeResponse(line []byte) ([]byte, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(line), &raw); err != nil {
		return nil, false
	}
	resultRaw, ok := raw["result"]
	if !ok {
		return nil, false
	}

	patchedResult, ok := patchInitializeResult(resultRaw)
	if !ok {
		return nil, false
	}
	raw["result"] = patchedResult

	out, _ := json.Marshal(raw)

	return out, true
}

func patchInitializeResult(resultRaw json.RawMessage) (json.RawMessage, bool) {
	var result map[string]any
	if err := json.Unmarshal(resultRaw, &result); err != nil {
		return nil, false
	}
	if result == nil {
		return nil, false
	}

	agentCapabilities := ensureMap(result, "agentCapabilities")
	meta := ensureMap(agentCapabilities, "_meta")
	packageMeta := ensureMap(meta, proxyPackageMetaKey)
	packageMeta["extensions"] = map[string]any{
		"sessionExport": map[string]any{"method": OpenCodeSessionExportMethod},
		"sessionImport": map[string]any{"method": OpenCodeSessionImportMethod},
		"sessionDelete": map[string]any{"method": OpenCodeSessionDeleteMethod},
	}

	out, _ := json.Marshal(result)

	return out, true
}

func ensureMap(parent map[string]any, key string) map[string]any {
	if existing, ok := parent[key].(map[string]any); ok {
		return existing
	}
	next := map[string]any{}
	parent[key] = next

	return next
}
