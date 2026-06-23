package opencodeacp

import (
	"cmp"
	"slices"

	"github.com/coder/acp-go-sdk"
)

const (
	// OpenCodeConfigModel is the native OpenCode ACP session config option
	// used to choose the provider/model pair.
	OpenCodeConfigModel acp.SessionConfigId = "model"
	// OpenCodeConfigMode is the native OpenCode ACP session config option
	// used to choose the session mode/agent.
	OpenCodeConfigMode acp.SessionConfigId = "mode"
)

const (
	// OpenCodeModeBuild is OpenCode's default build mode.
	OpenCodeModeBuild acp.SessionConfigValueId = "build"
	// OpenCodeModePlan is OpenCode's plan mode.
	OpenCodeModePlan acp.SessionConfigValueId = "plan"
)

// SessionRequestOption configures embedded-Go ACP session lifecycle requests.
type SessionRequestOption func(*sessionRequestConfig)

type sessionRequestConfig struct {
	additionalDirectories []string
	mcpServers            []acp.McpServer
	meta                  map[string]any
}

// NewSessionRequest constructs a session/new request with ACP-required empty
// slices initialized for embedded Go callers.
func NewSessionRequest(cwd string, opts ...SessionRequestOption) acp.NewSessionRequest {
	config := newSessionRequestConfig(opts...)

	return acp.NewSessionRequest{
		Cwd:                   cwd,
		McpServers:            config.stableMCPServers(),
		AdditionalDirectories: config.additionalDirectoriesClone(),
		Meta:                  cloneAnyMap(config.meta),
	}
}

// LoadSessionRequest constructs a session/load request with ACP-required empty
// slices initialized for embedded Go callers.
func LoadSessionRequest(sessionID acp.SessionId, cwd string, opts ...SessionRequestOption) acp.LoadSessionRequest {
	config := newSessionRequestConfig(opts...)

	return acp.LoadSessionRequest{
		SessionId:             sessionID,
		Cwd:                   cwd,
		McpServers:            config.stableMCPServers(),
		AdditionalDirectories: config.additionalDirectoriesClone(),
		Meta:                  cloneAnyMap(config.meta),
	}
}

// ResumeSessionRequest constructs a session/resume request.
func ResumeSessionRequest(sessionID acp.SessionId, cwd string, opts ...SessionRequestOption) acp.ResumeSessionRequest {
	config := newSessionRequestConfig(opts...)

	return acp.ResumeSessionRequest{
		SessionId:             sessionID,
		Cwd:                   cwd,
		McpServers:            config.stableMCPServers(),
		AdditionalDirectories: config.additionalDirectoriesClone(),
		Meta:                  cloneAnyMap(config.meta),
	}
}

// ForkSessionRequest constructs an unstable session/fork request.
func ForkSessionRequest(sessionID acp.SessionId, cwd string, opts ...SessionRequestOption) acp.UnstableForkSessionRequest {
	config := newSessionRequestConfig(opts...)

	return acp.UnstableForkSessionRequest{
		SessionId:             sessionID,
		Cwd:                   cwd,
		McpServers:            unstableMCPServersFromStable(config.stableMCPServers()),
		AdditionalDirectories: config.additionalDirectoriesClone(),
		Meta:                  cloneAnyMap(config.meta),
	}
}

// WithSessionMCPServers sets MCP servers for a session lifecycle request.
func WithSessionMCPServers(servers ...acp.McpServer) SessionRequestOption {
	cloned := cloneMCPServers(servers)

	return func(config *sessionRequestConfig) {
		config.mcpServers = cloned
	}
}

// WithSessionAdditionalDirectories sets additional workspace directories for a
// session lifecycle request.
func WithSessionAdditionalDirectories(paths ...string) SessionRequestOption {
	cloned := append([]string(nil), paths...)

	return func(config *sessionRequestConfig) {
		config.additionalDirectories = cloned
	}
}

// WithSessionMeta merges metadata into a session lifecycle request.
func WithSessionMeta(meta map[string]any) SessionRequestOption {
	cloned := cloneAnyMap(meta)

	return func(config *sessionRequestConfig) {
		config.meta = mergeAnyMap(config.meta, cloned)
	}
}

// StdioMCPServer constructs an ACP stdio MCP server definition for native
// OpenCode session lifecycle requests.
func StdioMCPServer(name string, command string, args []string, env map[string]string) acp.McpServer {
	return acp.McpServer{
		Stdio: &acp.McpServerStdio{
			Name:    name,
			Command: command,
			Args:    append([]string(nil), args...),
			Env:     envVariables(env),
		},
	}
}

// HTTPMCPServer constructs an ACP HTTP MCP server definition for native
// OpenCode session lifecycle requests.
func HTTPMCPServer(name string, url string, headers map[string]string) acp.McpServer {
	return acp.McpServer{
		Http: &acp.McpServerHttpInline{
			Name:    name,
			Type:    "http",
			Url:     url,
			Headers: httpHeaders(headers),
		},
	}
}

// SSEMCPServer constructs an ACP SSE MCP server definition for native
// OpenCode session lifecycle requests.
func SSEMCPServer(name string, url string, headers map[string]string) acp.McpServer {
	return acp.McpServer{
		Sse: &acp.McpServerSseInline{
			Name:    name,
			Type:    "sse",
			Url:     url,
			Headers: httpHeaders(headers),
		},
	}
}

func newSessionRequestConfig(opts ...SessionRequestOption) sessionRequestConfig {
	config := sessionRequestConfig{}
	for _, opt := range opts {
		opt(&config)
	}

	return config
}

func (config sessionRequestConfig) stableMCPServers() []acp.McpServer {
	if config.mcpServers == nil {
		return []acp.McpServer{}
	}

	return cloneMCPServers(config.mcpServers)
}

func (config sessionRequestConfig) additionalDirectoriesClone() []string {
	return append([]string(nil), config.additionalDirectories...)
}

// PromptRequest constructs a session/prompt request with a non-nil prompt
// slice for embedded Go callers.
func PromptRequest(sessionID acp.SessionId, blocks ...acp.ContentBlock) acp.PromptRequest {
	return acp.PromptRequest{
		SessionId: sessionID,
		Prompt:    append([]acp.ContentBlock{}, blocks...),
	}
}

// TextPromptRequest constructs a session/prompt request containing one text
// content block.
func TextPromptRequest(sessionID acp.SessionId, text string) acp.PromptRequest {
	return PromptRequest(sessionID, acp.TextBlock(text))
}

// SetConfigOptionRequest constructs a session/set_config_option request using
// the string value-id variant used by OpenCode for model and mode.
func SetConfigOptionRequest(
	sessionID acp.SessionId,
	configID acp.SessionConfigId,
	value acp.SessionConfigValueId,
) acp.SetSessionConfigOptionRequest {
	return acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: sessionID,
			ConfigId:  configID,
			Value:     value,
		},
	}
}

// SetModelRequest constructs a native OpenCode model config request. The value
// is the OpenCode provider/model ID, for example "opencode/big-pickle".
func SetModelRequest(sessionID acp.SessionId, model string) acp.SetSessionConfigOptionRequest {
	return SetConfigOptionRequest(sessionID, OpenCodeConfigModel, acp.SessionConfigValueId(model))
}

// SetModeConfigRequest constructs a native OpenCode mode config request using
// the session/set_config_option method.
func SetModeConfigRequest(sessionID acp.SessionId, mode acp.SessionConfigValueId) acp.SetSessionConfigOptionRequest {
	return SetConfigOptionRequest(sessionID, OpenCodeConfigMode, mode)
}

// ListSessionsRequestOption configures embedded-Go session/list requests.
type ListSessionsRequestOption func(*acp.ListSessionsRequest)

// ListSessionsRequest constructs a session/list request.
func ListSessionsRequest(opts ...ListSessionsRequestOption) acp.ListSessionsRequest {
	var req acp.ListSessionsRequest
	for _, opt := range opts {
		opt(&req)
	}

	return req
}

// WithListSessionsCwd filters session/list by cwd.
func WithListSessionsCwd(cwd string) ListSessionsRequestOption {
	return func(req *acp.ListSessionsRequest) {
		value := cwd
		req.Cwd = &value
	}
}

// WithListSessionsCursor sets the cursor for session/list pagination.
func WithListSessionsCursor(cursor string) ListSessionsRequestOption {
	return func(req *acp.ListSessionsRequest) {
		value := cursor
		req.Cursor = &value
	}
}

// WithListSessionsMeta sets metadata on a session/list request.
func WithListSessionsMeta(meta map[string]any) ListSessionsRequestOption {
	cloned := cloneAnyMap(meta)

	return func(req *acp.ListSessionsRequest) {
		req.Meta = mergeAnyMap(req.Meta, cloned)
	}
}

func cloneAny(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneAnyMap(typed)
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = cloneAny(item)
		}

		return out
	case []string:
		return append([]string(nil), typed...)
	case []int:
		return append([]int(nil), typed...)
	case []float64:
		return append([]float64(nil), typed...)
	case []bool:
		return append([]bool(nil), typed...)
	default:
		return typed
	}
}

func cloneAnyMap(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}

	out := make(map[string]any, len(values))
	for key, value := range values {
		out[key] = cloneAny(value)
	}

	return out
}

func mergeAnyMap(base map[string]any, overlay map[string]any) map[string]any {
	if len(base) == 0 && len(overlay) == 0 {
		return nil
	}

	out := cloneAnyMap(base)
	if out == nil {
		out = map[string]any{}
	}
	for key, value := range overlay {
		if left, ok := out[key].(map[string]any); ok {
			if right, ok := value.(map[string]any); ok {
				out[key] = mergeAnyMap(left, right)

				continue
			}
		}
		out[key] = cloneAny(value)
	}

	return out
}

func envVariables(values map[string]string) []acp.EnvVariable {
	if values == nil {
		return []acp.EnvVariable{}
	}

	out := make([]acp.EnvVariable, 0, len(values))
	for name, value := range values {
		out = append(out, acp.EnvVariable{Name: name, Value: value})
	}
	slices.SortFunc(out, func(a acp.EnvVariable, b acp.EnvVariable) int {
		return cmp.Compare(a.Name, b.Name)
	})

	return out
}

func httpHeaders(values map[string]string) []acp.HttpHeader {
	if values == nil {
		return []acp.HttpHeader{}
	}

	out := make([]acp.HttpHeader, 0, len(values))
	for name, value := range values {
		out = append(out, acp.HttpHeader{Name: name, Value: value})
	}
	slices.SortFunc(out, func(a acp.HttpHeader, b acp.HttpHeader) int {
		return cmp.Compare(a.Name, b.Name)
	})

	return out
}

func cloneMCPServers(servers []acp.McpServer) []acp.McpServer {
	if servers == nil {
		return nil
	}

	out := make([]acp.McpServer, len(servers))
	for i, server := range servers {
		out[i] = cloneMCPServer(server)
	}

	return out
}

func cloneMCPServer(server acp.McpServer) acp.McpServer {
	switch {
	case server.Http != nil:
		value := *server.Http
		value.Meta = cloneAnyMap(value.Meta)
		value.Headers = cloneHTTPHeaders(value.Headers)

		return acp.McpServer{Http: &value}
	case server.Sse != nil:
		value := *server.Sse
		value.Meta = cloneAnyMap(value.Meta)
		value.Headers = cloneHTTPHeaders(value.Headers)

		return acp.McpServer{Sse: &value}
	case server.Acp != nil:
		value := *server.Acp
		value.Meta = cloneAnyMap(value.Meta)

		return acp.McpServer{Acp: &value}
	case server.Stdio != nil:
		return acp.McpServer{Stdio: cloneMCPServerStdio(server.Stdio)}
	default:
		return acp.McpServer{}
	}
}

func cloneMCPServerStdio(server *acp.McpServerStdio) *acp.McpServerStdio {
	if server == nil {
		return nil
	}

	out := *server
	out.Meta = cloneAnyMap(server.Meta)
	out.Args = append([]string(nil), server.Args...)
	out.Env = cloneEnvVariables(server.Env)

	return &out
}

func cloneHTTPHeaders(headers []acp.HttpHeader) []acp.HttpHeader {
	if headers == nil {
		return nil
	}

	out := make([]acp.HttpHeader, len(headers))
	for i, header := range headers {
		out[i] = header
		out[i].Meta = cloneAnyMap(header.Meta)
	}

	return out
}

func cloneEnvVariables(env []acp.EnvVariable) []acp.EnvVariable {
	if env == nil {
		return nil
	}

	out := make([]acp.EnvVariable, len(env))
	for i, variable := range env {
		out[i] = variable
		out[i].Meta = cloneAnyMap(variable.Meta)
	}

	return out
}

func unstableMCPServersFromStable(servers []acp.McpServer) []acp.UnstableMcpServer {
	if servers == nil {
		return nil
	}

	out := make([]acp.UnstableMcpServer, len(servers))
	for i, server := range servers {
		out[i] = unstableMCPServerFromStable(server)
	}

	return out
}

func unstableMCPServerFromStable(server acp.McpServer) acp.UnstableMcpServer {
	switch {
	case server.Http != nil:
		value := acp.UnstableMcpServerHttp{
			Meta:    cloneAnyMap(server.Http.Meta),
			Headers: cloneHTTPHeaders(server.Http.Headers),
			Name:    server.Http.Name,
			Type:    server.Http.Type,
			Url:     server.Http.Url,
		}

		return acp.UnstableMcpServer{Http: &value}
	case server.Sse != nil:
		value := acp.UnstableMcpServerSse{
			Meta:    cloneAnyMap(server.Sse.Meta),
			Headers: cloneHTTPHeaders(server.Sse.Headers),
			Name:    server.Sse.Name,
			Type:    server.Sse.Type,
			Url:     server.Sse.Url,
		}

		return acp.UnstableMcpServer{Sse: &value}
	case server.Acp != nil:
		value := acp.UnstableMcpServerAcpInline{
			Meta: cloneAnyMap(server.Acp.Meta),
			Id:   acp.UnstableMcpServerAcpId(server.Acp.Id),
			Name: server.Acp.Name,
			Type: server.Acp.Type,
		}

		return acp.UnstableMcpServer{Acp: &value}
	case server.Stdio != nil:
		return acp.UnstableMcpServer{Stdio: cloneMCPServerStdio(server.Stdio)}
	default:
		return acp.UnstableMcpServer{}
	}
}
