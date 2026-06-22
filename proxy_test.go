package opencodeacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

func TestProxyForwardsAndPatchesInitialize(t *testing.T) {
	t.Parallel()

	var child bytes.Buffer
	var host bytes.Buffer
	proxy := newACPProxy(&child, &host, nil)

	err := proxy.forwardHostToChild(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n"))
	if err != nil {
		t.Fatalf("forwardHostToChild returned error: %v", err)
	}
	if got := child.String(); got != `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n" {
		t.Fatalf("child input = %q", got)
	}

	err = proxy.forwardChildToHost(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{"_meta":{"existing":{"ok":true}}}}}` + "\n"))
	if err != nil {
		t.Fatalf("forwardChildToHost returned error: %v", err)
	}

	var response map[string]any
	if err := json.Unmarshal(host.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	result := response["result"].(map[string]any)
	capabilities := result["agentCapabilities"].(map[string]any)
	meta := capabilities["_meta"].(map[string]any)
	if meta["existing"].(map[string]any)["ok"] != true {
		t.Fatalf("existing meta not preserved: %#v", meta)
	}
	packageMeta := meta[proxyPackageMetaKey].(map[string]any)
	extensions := packageMeta["extensions"].(map[string]any)
	if extensions["sessionExport"].(map[string]any)["method"] != OpenCodeSessionExportMethod ||
		extensions["sessionImport"].(map[string]any)["method"] != OpenCodeSessionImportMethod ||
		extensions["sessionDelete"].(map[string]any)["method"] != OpenCodeSessionDeleteMethod {
		t.Fatalf("extensions meta = %#v", extensions)
	}
}

func TestProxyPassesThroughInvalidJSONAndResponses(t *testing.T) {
	t.Parallel()

	var child bytes.Buffer
	var host bytes.Buffer
	proxy := newACPProxy(&child, &host, nil)

	if err := proxy.forwardHostToChild(context.Background(), strings.NewReader("not-json\n\n")); err != nil {
		t.Fatalf("forwardHostToChild invalid JSON returned error: %v", err)
	}
	if got := child.String(); got != "not-json\n" {
		t.Fatalf("child invalid JSON = %q", got)
	}

	if err := proxy.forwardChildToHost(strings.NewReader("not-json\n")); err != nil {
		t.Fatalf("forwardChildToHost invalid JSON returned error: %v", err)
	}
	if got := host.String(); got != "not-json\n" {
		t.Fatalf("host invalid JSON = %q", got)
	}

	host.Reset()
	if err := proxy.forwardChildToHost(strings.NewReader(`{"jsonrpc":"2.0","id":99,"result":{"ok":true}}` + "\n")); err != nil {
		t.Fatalf("forwardChildToHost unknown response returned error: %v", err)
	}
	if !strings.Contains(host.String(), `"id":99`) {
		t.Fatalf("unknown response was not forwarded: %q", host.String())
	}
}

func TestProxyExtensionMethods(t *testing.T) {
	restore := stubProxySessionHelpers(t)
	defer restore()

	var child bytes.Buffer
	var host bytes.Buffer
	proxy := newACPProxy(&child, &host, []Option{WithCwd("/repo")})

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":"export","method":"_opencode/session/export","params":{"sessionId":"ses_test"}}`,
		`{"jsonrpc":"2.0","id":"import","method":"_opencode/session/import","params":{"path":"/tmp/session.json"}}`,
		`{"jsonrpc":"2.0","id":"delete","method":"_opencode/session/delete","params":{"sessionId":"ses_test"}}`,
		`{"jsonrpc":"2.0","method":"_opencode/session/export","params":{"sessionId":"ignored"}}`,
	}, "\n") + "\n"

	if err := proxy.forwardHostToChild(context.Background(), strings.NewReader(input)); err != nil {
		t.Fatalf("forwardHostToChild extension returned error: %v", err)
	}
	if child.Len() != 0 {
		t.Fatalf("extension methods were forwarded to native child: %q", child.String())
	}

	responses := decodeProxyResponses(t, host.String())
	exportResult := responses["export"]["result"].(map[string]any)
	if exportResult["sessionId"] != "ses_test" ||
		exportResult["bytes"].(float64) == 0 ||
		!strings.Contains(exportResult["rawJSON"].(string), `"ses_test"`) {
		t.Fatalf("export result = %#v", exportResult)
	}
	if got := responses["import"]["result"].(map[string]any)["output"]; got != "Imported /tmp/session.json\n" {
		t.Fatalf("import output = %#v", got)
	}
	deleteResult := responses["delete"]["result"].(map[string]any)
	if deleteResult["deleted"] != true || deleteResult["sessionId"] != "ses_test" {
		t.Fatalf("delete result = %#v", deleteResult)
	}
}

func TestProxyExtensionErrors(t *testing.T) {
	restore := stubProxySessionHelpers(t)
	defer restore()

	var child bytes.Buffer
	var host bytes.Buffer
	proxy := newACPProxy(&child, &host, nil)

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":"bad-export","method":"_opencode/session/export","params":"bad"}`,
		`{"jsonrpc":"2.0","id":"bad-import-json","method":"_opencode/session/import","params":"bad"}`,
		`{"jsonrpc":"2.0","id":"bad-import","method":"_opencode/session/import","params":{}}`,
		`{"jsonrpc":"2.0","id":"bad-delete-json","method":"_opencode/session/delete","params":"bad"}`,
		`{"jsonrpc":"2.0","id":"bad-delete","method":"_opencode/session/delete","params":{}}`,
		`{"jsonrpc":"2.0","id":"missing","method":"_opencode/missing","params":{}}`,
	}, "\n") + "\n"

	if err := proxy.forwardHostToChild(context.Background(), strings.NewReader(input)); err != nil {
		t.Fatalf("forwardHostToChild extension errors returned error: %v", err)
	}

	responses := decodeProxyResponses(t, host.String())
	assertProxyErrorCode(t, responses["bad-export"], -32602)
	assertProxyErrorCode(t, responses["bad-import-json"], -32602)
	assertProxyErrorCode(t, responses["bad-import"], -32602)
	assertProxyErrorCode(t, responses["bad-delete-json"], -32602)
	assertProxyErrorCode(t, responses["bad-delete"], -32602)
	if !strings.Contains(child.String(), `"_opencode/missing"`) {
		t.Fatalf("unknown _opencode method was not forwarded: %q", child.String())
	}
}

func TestProxyExtensionCommandErrors(t *testing.T) {
	restore := stubProxySessionHelpers(t)
	defer restore()

	exportSessionForProxy = func(context.Context, acp.SessionId, ...Option) ([]byte, error) {
		return nil, errors.New("export failed")
	}
	importSessionFileForProxy = func(context.Context, string, ...Option) ([]byte, error) {
		return nil, errors.New("import failed")
	}
	deleteSessionForProxy = func(context.Context, acp.SessionId, ...Option) error {
		return errors.New("delete failed")
	}
	sessionIDFromExportForProxy = func([]byte) (acp.SessionId, error) {
		return "", errors.New("session id failed")
	}

	var host bytes.Buffer
	proxy := newACPProxy(io.Discard, &host, nil)
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":"export","method":"_opencode/session/export","params":{"sessionId":"ses_test"}}`,
		`{"jsonrpc":"2.0","id":"import","method":"_opencode/session/import","params":{"path":"/tmp/session.json"}}`,
		`{"jsonrpc":"2.0","id":"delete","method":"_opencode/session/delete","params":{"sessionId":"ses_test"}}`,
	}, "\n") + "\n"

	if err := proxy.forwardHostToChild(context.Background(), strings.NewReader(input)); err != nil {
		t.Fatalf("forwardHostToChild command errors returned error: %v", err)
	}
	responses := decodeProxyResponses(t, host.String())
	assertProxyErrorMessage(t, responses["export"], "export failed")
	assertProxyErrorMessage(t, responses["import"], "import failed")
	assertProxyErrorMessage(t, responses["delete"], "delete failed")

	host.Reset()
	exportSessionForProxy = func(context.Context, acp.SessionId, ...Option) ([]byte, error) {
		return []byte(`{"info":{"id":"ses_test"}}`), nil
	}
	if err := proxy.forwardHostToChild(context.Background(), strings.NewReader(
		`{"jsonrpc":"2.0","id":"export-id","method":"_opencode/session/export","params":{"sessionId":"ses_test"}}`+"\n",
	)); err != nil {
		t.Fatalf("forwardHostToChild export ID error returned error: %v", err)
	}
	responses = decodeProxyResponses(t, host.String())
	assertProxyErrorMessage(t, responses["export-id"], "session id failed")
}

func TestProxyHelpersEdges(t *testing.T) {
	t.Parallel()

	if _, ok := decodeRPCEnvelope([]byte(`[]`)); ok {
		t.Fatal("decodeRPCEnvelope accepted non-envelope")
	}
	if wrapperExtensionMethod("session/new") {
		t.Fatal("wrapperExtensionMethod accepted native method")
	}
	if wrapperExtensionMethod("_opencode/missing") {
		t.Fatal("wrapperExtensionMethod accepted unknown opencode method")
	}
	if err := writeJSONLine(errorWriter{}, []byte(`{}`)); err == nil {
		t.Fatal("writeJSONLine accepted failing writer")
	}
	if err := writeJSONLine(&shortWriter{}, []byte(`{}`)); err == nil {
		t.Fatal("writeJSONLine accepted short writer")
	}
	if err := readJSONLines(strings.NewReader("{}\n"), func([]byte) error {
		return errors.New("handle failed")
	}); err == nil || !strings.Contains(err.Error(), "handle failed") {
		t.Fatalf("readJSONLines handler error = %v", err)
	}
	if err := readJSONLines(errorReader{}, func([]byte) error { return nil }); err == nil ||
		!strings.Contains(err.Error(), "read failed") {
		t.Fatalf("readJSONLines read error = %v", err)
	}

	proxy := newACPProxy(io.Discard, io.Discard, nil)
	if err := proxy.writeHostResponse(json.RawMessage("1"), map[string]any{"bad": func() {}}, nil); err == nil {
		t.Fatal("writeHostResponse accepted unmarshalable result")
	}
	if err := proxy.handleWrapperExtension(context.Background(), rpcEnvelope{
		ID:     ptrRawMessage(json.RawMessage("1")),
		Method: "_opencode/missing",
	}); err != nil {
		t.Fatalf("handleWrapperExtension unknown method returned error: %v", err)
	}

	if _, ok := patchInitializeResponse([]byte(`not-json`)); ok {
		t.Fatal("patchInitializeResponse accepted invalid JSON")
	}
	if _, ok := patchInitializeResponse([]byte(`{"id":1}`)); ok {
		t.Fatal("patchInitializeResponse accepted response without result")
	}
	if _, ok := patchInitializeResponse([]byte(`{"id":1,"result":null}`)); ok {
		t.Fatal("patchInitializeResponse accepted non-object result")
	}
	if _, ok := patchInitializeResult(json.RawMessage(`{bad}`)); ok {
		t.Fatal("patchInitializeResult accepted invalid JSON")
	}
}

func ptrRawMessage(value json.RawMessage) *json.RawMessage {
	return &value
}

func TestCollectReadyHostErr(t *testing.T) {
	t.Parallel()

	if err := collectReadyHostErr(errors.New("joined"), nil); err == nil || !strings.Contains(err.Error(), "joined") {
		t.Fatalf("nil hostErr result = %v", err)
	}

	hostErr := make(chan error, 1)
	hostErr <- errors.New("host failed")
	err := collectReadyHostErr(errors.New("joined"), hostErr)
	if err == nil || !strings.Contains(err.Error(), "joined") || !strings.Contains(err.Error(), "host failed") {
		t.Fatalf("ready hostErr result = %v", err)
	}

	if err := collectReadyHostErr(nil, make(chan error)); err != nil {
		t.Fatalf("unready hostErr result = %v", err)
	}
}

func TestServeRunsProxy(t *testing.T) {
	originalRunACP := runACP
	t.Cleanup(func() { runACP = originalRunACP })

	runACP = func(
		_ context.Context,
		input io.Reader,
		output io.Writer,
		_ io.Writer,
		options opencode.Options,
	) error {
		if options.CLIPath != "/bin/opencode" || options.Port == nil || *options.Port != 0 {
			return errors.New("unexpected options")
		}
		if _, err := io.Copy(io.Discard, input); err != nil {
			return err
		}
		_, err := io.WriteString(output, `{"jsonrpc":"2.0","id":1,"result":{"agentCapabilities":{}}}`+"\n")

		return err
	}

	input := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n")
	var output bytes.Buffer
	err := Serve(context.Background(), input, &output, WithOpenCodePath("/bin/opencode"), WithPort(0))
	if err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}
	if !strings.Contains(output.String(), OpenCodeSessionExportMethod) {
		t.Fatalf("Serve output was not proxied: %q", output.String())
	}
}

func TestServeReturnsNativeError(t *testing.T) {
	originalRunACP := runACP
	t.Cleanup(func() { runACP = originalRunACP })

	runACP = func(context.Context, io.Reader, io.Writer, io.Writer, opencode.Options) error {
		return errors.New("native failed")
	}

	err := Serve(context.Background(), strings.NewReader(""), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "native failed") {
		t.Fatalf("Serve error = %v", err)
	}
}

func TestServeReturnsWithoutWaitingForBlockedHostInput(t *testing.T) {
	originalRunACP := runACP
	t.Cleanup(func() { runACP = originalRunACP })

	runACP = func(context.Context, io.Reader, io.Writer, io.Writer, opencode.Options) error {
		return nil
	}

	reader := &blockingReader{done: make(chan struct{})}
	err := Serve(context.Background(), reader, io.Discard)
	close(reader.done)
	if err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}
}

func TestServeReturnsHostReadError(t *testing.T) {
	originalRunACP := runACP
	t.Cleanup(func() { runACP = originalRunACP })

	runACP = func(_ context.Context, input io.Reader, _ io.Writer, _ io.Writer, _ opencode.Options) error {
		_, err := io.Copy(io.Discard, input)

		return err
	}

	err := Serve(context.Background(), errorReader{}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "read failed") {
		t.Fatalf("Serve host read error = %v", err)
	}
}

func TestServeReturnsHostWriteError(t *testing.T) {
	originalRunACP := runACP
	t.Cleanup(func() { runACP = originalRunACP })

	runACP = func(_ context.Context, input io.Reader, output io.Writer, _ io.Writer, _ opencode.Options) error {
		if _, err := io.Copy(io.Discard, input); err != nil {
			return err
		}
		_, err := io.WriteString(output, `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`+"\n")

		return err
	}

	err := Serve(context.Background(), strings.NewReader(""), errorWriter{})
	if err == nil || !strings.Contains(err.Error(), "write failed") {
		t.Fatalf("Serve host write error = %v", err)
	}
}

func stubProxySessionHelpers(t *testing.T) func() {
	t.Helper()

	originalExport := exportSessionForProxy
	originalImport := importSessionFileForProxy
	originalDelete := deleteSessionForProxy
	originalID := sessionIDFromExportForProxy

	exportSessionForProxy = func(_ context.Context, sessionID acp.SessionId, _ ...Option) ([]byte, error) {
		if sessionID == "" {
			sessionID = "ses_latest"
		}

		return []byte(`{"info":{"id":"` + string(sessionID) + `"}}`), nil
	}
	importSessionFileForProxy = func(_ context.Context, path string, _ ...Option) ([]byte, error) {
		return []byte("Imported " + path + "\n"), nil
	}
	deleteSessionForProxy = func(context.Context, acp.SessionId, ...Option) error {
		return nil
	}
	sessionIDFromExportForProxy = SessionIDFromExport

	return func() {
		exportSessionForProxy = originalExport
		importSessionFileForProxy = originalImport
		deleteSessionForProxy = originalDelete
		sessionIDFromExportForProxy = originalID
	}
}

func decodeProxyResponses(t *testing.T, raw string) map[string]map[string]any {
	t.Helper()

	out := map[string]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("decode response %q: %v", line, err)
		}
		out[msg["id"].(string)] = msg
	}

	return out
}

func assertProxyErrorCode(t *testing.T, msg map[string]any, code float64) {
	t.Helper()

	errObj := msg["error"].(map[string]any)
	if errObj["code"] != code {
		t.Fatalf("error code = %#v, want %v in %#v", errObj["code"], code, msg)
	}
}

func assertProxyErrorMessage(t *testing.T, msg map[string]any, want string) {
	t.Helper()

	errObj := msg["error"].(map[string]any)
	if errObj["message"] != want {
		t.Fatalf("error message = %#v, want %q in %#v", errObj["message"], want, msg)
	}
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

type shortWriter struct{}

func (*shortWriter) Write([]byte) (int, error) {
	return 0, io.ErrShortWrite
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) {
	return 0, errors.New("read failed")
}

type blockingReader struct {
	done chan struct{}
}

func (r *blockingReader) Read([]byte) (int, error) {
	<-r.done

	return 0, io.EOF
}
