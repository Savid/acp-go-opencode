package opencodeacp

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/lifecycle"
)

const (
	metaOptionsKey       = "options"
	metaModelKey         = "model"
	metaEnvKey           = "env"
	metaExtraPathDirsKey = "extraPathDirs"
	metaOutputSchemaKey  = "outputSchema"
	metaModeKey          = "mode"
	metaPermissionKey    = "permission"

	// envPathKey is the one environment name a session may not set: the map it
	// would arrive in replaces whole values, and dropping the inherited search
	// path unresolves every program a tool runs. ExtraPathDirs is the additive
	// mechanism that owns the search path instead.
	envPathKey = "PATH"
)

// OpenCodeOptions is the stable OpenCode-specific subset accepted at
// _meta.opencode.options.
type OpenCodeOptions struct {
	Model        string         `json:"model,omitempty"`
	OutputSchema map[string]any `json:"outputSchema,omitempty"`
	Mode         string         `json:"mode,omitempty"`
	Permission   string         `json:"permission,omitempty"`
	// Env is the environment this session's shell tools run under. It is
	// carried on the addressed native session, not on the shared runtime
	// process, so two sessions of one Agent hold different values at the same
	// time and a later session never inherits an earlier one's. PATH belongs in
	// ExtraPathDirs.
	Env map[string]string `json:"env,omitempty"`
	// ExtraPathDirs are absolute directories placed ahead of the inherited PATH
	// for this native session's shell tools, in order.
	ExtraPathDirs []string `json:"extraPathDirs,omitempty"`
}

// Meta returns an ACP _meta object for the supported OpenCode-specific options.
func (options OpenCodeOptions) Meta() map[string]any {
	values := map[string]any{}
	if options.Model != "" {
		values[metaModelKey] = options.Model
	}

	if options.OutputSchema != nil {
		values[metaOutputSchemaKey] = cloneAnyMap(options.OutputSchema)
	}

	if options.Mode != "" {
		values[metaModeKey] = options.Mode
	}

	if options.Permission != "" {
		values[metaPermissionKey] = options.Permission
	}

	if options.Env != nil {
		values[metaEnvKey] = cloneStringMap(options.Env)
	}

	if options.ExtraPathDirs != nil {
		values[metaExtraPathDirsKey] = append([]string{}, options.ExtraPathDirs...)
	}

	return map[string]any{
		opencodeMetaKey: map[string]any{
			metaOptionsKey: values,
		},
	}
}

type SessionRequestOption func(*sessionRequestConfig)

type sessionRequestConfig struct {
	additionalDirectories []string
	mcpServers            []acp.McpServer
	meta                  map[string]any
}

func NewSessionRequest(cwd string, opts ...SessionRequestOption) acp.NewSessionRequest {
	config := newSessionRequestConfig(opts...)

	return acp.NewSessionRequest{
		Cwd:                   cwd,
		McpServers:            config.stableMCPServers(),
		AdditionalDirectories: config.additionalDirectoriesClone(),
		Meta:                  cloneAnyMap(config.meta),
	}
}

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

func DeleteSessionRequest(sessionID acp.SessionId) acp.UnstableDeleteSessionRequest {
	return acp.UnstableDeleteSessionRequest{SessionId: sessionID}
}

func WithSessionMCPServers(servers ...acp.McpServer) SessionRequestOption {
	cloned := cloneMCPServers(servers)

	return func(config *sessionRequestConfig) {
		config.mcpServers = cloneMCPServers(cloned)
	}
}

func WithSessionAdditionalDirectories(paths ...string) SessionRequestOption {
	cloned := append([]string(nil), paths...)

	return func(config *sessionRequestConfig) {
		config.additionalDirectories = append([]string(nil), cloned...)
	}
}

// WithSessionMeta merges host-supplied metadata into the request's `_meta`.
//
// A caller key naming any family-reserved `acp-go.dev/*` literal is rejected
// rather than merged or overwritten. The namespace is family-global and closed,
// its values are minted by this package on the surfaces that carry them, and a
// host that stamps one by hand is naming a value it does not own. Silently
// dropping the key would be the "ignored as a no-op" treatment the contract
// forbids everywhere else it appears, and merging it would put an unvalidated
// family value on the wire; the collision is a defect in the calling code, in a
// closed set the caller can check before calling, so the builder refuses to
// build the request at all.
func WithSessionMeta(meta map[string]any) SessionRequestOption {
	rejectReservedMeta("WithSessionMeta", meta)

	cloned := cloneAnyMap(meta)

	return func(config *sessionRequestConfig) {
		config.meta = mergeAnyMap(config.meta, cloned)
	}
}

// reservedMetaLiterals is the closed family-global set. Adding a fifth is a
// contract amendment, so the set is written out rather than matched by prefix:
// a prefix test would also reject a literal this family has not defined and
// report it as though the contract already did.
var reservedMetaLiterals = []string{
	routeEnvelopeKey,
	mediaEnvelopeKey,
	handoffEnvelopeKey,
	lifecycle.MetaKey,
}

// rejectReservedMeta refuses a caller `_meta` map that names a family literal.
// The builders return values rather than errors, so the refusal is a panic: the
// only alternatives inside the signature the family fixes are merging the key or
// dropping it, and the contract forbids both.
func rejectReservedMeta(builder string, meta map[string]any) {
	for _, literal := range reservedMetaLiterals {
		if _, present := meta[literal]; present {
			panic(builder + ": caller metadata used the family-reserved key " + strconv.Quote(literal))
		}
	}
}

func WithSessionOpenCodeOptions(options OpenCodeOptions) SessionRequestOption {
	cloned := cloneOpenCodeOptions(options)

	return func(config *sessionRequestConfig) {
		config.meta = mergeAnyMap(config.meta, cloned.Meta())
	}
}

func WithSessionOutputSchema(schema map[string]any) SessionRequestOption {
	cloned := cloneAnyMap(schema)

	return func(config *sessionRequestConfig) {
		config.meta = mergeAnyMap(config.meta, OpenCodeOptions{OutputSchema: cloned}.Meta())
	}
}

func WithSessionRawEvents(enabled bool) SessionRequestOption {
	return func(config *sessionRequestConfig) {
		if config.meta == nil {
			config.meta = map[string]any{}
		}

		opencodeMeta := ensureMetaMap(config.meta, opencodeMetaKey)
		opencodeMeta[rawEventKey] = map[string]any{rawEventEnabledKey: enabled}
		config.meta[opencodeMetaKey] = opencodeMeta
	}
}

func StdioMCPServer(name string, command string, args []string, env map[string]string) acp.McpServer {
	variables := make([]acp.EnvVariable, 0, len(env))
	for key, value := range env {
		variables = append(variables, acp.EnvVariable{Name: key, Value: value})
	}

	return acp.McpServer{Stdio: &acp.McpServerStdio{
		Name:    name,
		Command: command,
		Args:    append([]string(nil), args...),
		Env:     variables,
	}}
}

func HTTPMCPServer(name string, url string, headers map[string]string) acp.McpServer {
	values := make([]acp.HttpHeader, 0, len(headers))
	for key, value := range headers {
		values = append(values, acp.HttpHeader{Name: key, Value: value})
	}

	return acp.McpServer{Http: &acp.McpServerHttpInline{
		Name:    name,
		Url:     url,
		Headers: values,
	}}
}

func PromptRequest(sessionID acp.SessionId, turnNonce string, blocks ...acp.ContentBlock) acp.PromptRequest {
	return acp.PromptRequest{
		SessionId: sessionID,
		Prompt:    append([]acp.ContentBlock{}, blocks...),
		Meta:      requestRouteCarrier(turnNonce),
	}
}

func TextPromptRequest(sessionID acp.SessionId, turnNonce, text string) acp.PromptRequest {
	return PromptRequest(sessionID, turnNonce, acp.TextBlock(text))
}

func CancelRequest(sessionID acp.SessionId, turnNonce string) acp.CancelNotification {
	return acp.CancelNotification{SessionId: sessionID, Meta: requestRouteCarrier(turnNonce)}
}

func SetConfigOptionRequest(sessionID acp.SessionId, configID acp.SessionConfigId, value acp.SessionConfigValueId) acp.SetSessionConfigOptionRequest {
	return acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: sessionID,
			ConfigId:  configID,
			Value:     value,
		},
	}
}

func SetModelRequest(sessionID acp.SessionId, model string) acp.SetSessionConfigOptionRequest {
	return SetConfigOptionRequest(sessionID, configModel, acp.SessionConfigValueId(model))
}

func CallForkSession(ctx context.Context, conn *acp.ClientSideConnection, params acp.UnstableForkSessionRequest) (acp.UnstableForkSessionResponse, error) {
	raw, err := conn.CallExtension(ctx, ForkSessionMethod, params)
	if err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}

	var resp acp.UnstableForkSessionResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}

	return resp, nil
}

type ListSessionsRequestOption func(*acp.ListSessionsRequest)

func ListSessionsRequest(opts ...ListSessionsRequestOption) acp.ListSessionsRequest {
	var req acp.ListSessionsRequest
	for _, opt := range opts {
		opt(&req)
	}

	return req
}

func WithListSessionsCwd(cwd string) ListSessionsRequestOption {
	return func(req *acp.ListSessionsRequest) {
		value := cwd
		req.Cwd = &value
	}
}

func WithListSessionsCursor(cursor string) ListSessionsRequestOption {
	return func(req *acp.ListSessionsRequest) {
		value := cursor
		req.Cursor = &value
	}
}

// WithListSessionsMeta merges host-supplied metadata into the request's `_meta`,
// and rejects a family-reserved `acp-go.dev/*` literal in it on the same terms
// as [WithSessionMeta].
func WithListSessionsMeta(meta map[string]any) ListSessionsRequestOption {
	rejectReservedMeta("WithListSessionsMeta", meta)

	cloned := cloneAnyMap(meta)

	return func(req *acp.ListSessionsRequest) {
		req.Meta = mergeAnyMap(req.Meta, cloned)
	}
}

type OpenCodeOption func(*OpenCodeOptions)

func NewOpenCodeOptions(opts ...OpenCodeOption) OpenCodeOptions {
	options := OpenCodeOptions{}
	for _, opt := range opts {
		opt(&options)
	}

	return cloneOpenCodeOptions(options)
}

func WithOpenCodeModel(model string) OpenCodeOption {
	return func(options *OpenCodeOptions) {
		options.Model = model
	}
}

func WithOpenCodeOutputSchema(schema map[string]any) OpenCodeOption {
	cloned := cloneAnyMap(schema)

	return func(options *OpenCodeOptions) {
		options.OutputSchema = cloneAnyMap(cloned)
	}
}

func WithOpenCodeMode(mode string) OpenCodeOption {
	return func(options *OpenCodeOptions) {
		options.Mode = mode
	}
}

func WithOpenCodePermission(permission string) OpenCodeOption {
	return func(options *OpenCodeOptions) {
		options.Permission = permission
	}
}

// WithOpenCodeEnv sets the environment this session's shell tools run under.
// The values reach the addressed native session, so a concurrent session of the
// same Agent keeps its own and a rotated value replaces the old one on the next
// command. PATH is refused: use WithOpenCodeExtraPathDirs.
func WithOpenCodeEnv(env map[string]string) OpenCodeOption {
	cloned := cloneStringMap(env)

	return func(options *OpenCodeOptions) {
		options.Env = cloneStringMap(cloned)
	}
}

// WithOpenCodeExtraPathDirs places absolute directories ahead of the inherited
// PATH of this native session's shell tools, in the order given.
func WithOpenCodeExtraPathDirs(dirs ...string) OpenCodeOption {
	cloned := append([]string{}, dirs...)

	return func(options *OpenCodeOptions) {
		options.ExtraPathDirs = append([]string{}, cloned...)
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

func cloneOpenCodeOptions(options OpenCodeOptions) OpenCodeOptions {
	cloned := OpenCodeOptions{
		Model:        options.Model,
		OutputSchema: cloneAnyMap(options.OutputSchema),
		Mode:         options.Mode,
		Permission:   options.Permission,
		Env:          cloneStringMap(options.Env),
	}
	if options.ExtraPathDirs != nil {
		cloned.ExtraPathDirs = append([]string{}, options.ExtraPathDirs...)
	}

	return cloned
}

func mergeAnyMap(base map[string]any, overlay map[string]any) map[string]any {
	result := cloneAnyMap(base)
	if result == nil {
		result = map[string]any{}
	}

	for key, value := range overlay {
		if valueMap, ok := value.(map[string]any); ok {
			if existingMap, ok := result[key].(map[string]any); ok {
				result[key] = mergeAnyMap(existingMap, valueMap)

				continue
			}
		}

		result[key] = cloneAny(value)
	}

	return result
}

func ensureMetaMap(meta map[string]any, key string) map[string]any {
	current, _ := meta[key].(map[string]any)
	if current == nil {
		current = map[string]any{}
	} else {
		current = cloneAnyMap(current)
	}

	meta[key] = current

	return current
}

func cloneMCPServers(servers []acp.McpServer) []acp.McpServer {
	if servers == nil {
		return nil
	}

	cloned := make([]acp.McpServer, len(servers))
	for index, server := range servers {
		cloned[index] = cloneMCPServer(server)
	}

	return cloned
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

	value := *server
	value.Meta = cloneAnyMap(value.Meta)
	value.Args = append([]string(nil), value.Args...)
	value.Env = cloneEnvVariables(value.Env)

	return &value
}

func cloneHTTPHeaders(headers []acp.HttpHeader) []acp.HttpHeader {
	if headers == nil {
		return nil
	}

	cloned := make([]acp.HttpHeader, len(headers))
	for index, header := range headers {
		cloned[index] = header
		cloned[index].Meta = cloneAnyMap(header.Meta)
	}

	return cloned
}

func cloneEnvVariables(env []acp.EnvVariable) []acp.EnvVariable {
	if env == nil {
		return nil
	}

	cloned := make([]acp.EnvVariable, len(env))
	for index, variable := range env {
		cloned[index] = variable
		cloned[index].Meta = cloneAnyMap(variable.Meta)
	}

	return cloned
}

func unstableMCPServersFromStable(servers []acp.McpServer) []acp.UnstableMcpServer {
	if servers == nil {
		return nil
	}

	cloned := make([]acp.UnstableMcpServer, len(servers))
	for index, server := range servers {
		cloned[index] = unstableMCPServerFromStable(server)
	}

	return cloned
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
