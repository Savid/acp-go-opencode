package opencodeacp

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

func (a *Agent) NewSession(ctx context.Context, params acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	ctx = a.observe.Extract(ctx, params.Meta)
	if err := a.ensureOpen(); err != nil {
		return acp.NewSessionResponse{}, err
	}

	if err := validateSessionStartPaths(params.Cwd, params.AdditionalDirectories); err != nil {
		return acp.NewSessionResponse{}, err
	}

	if err := validateMCPServers(params.McpServers); err != nil {
		return acp.NewSessionResponse{}, err
	}

	meta, err := sessionMetaFromLifecycle(params.Meta)
	if err != nil {
		return acp.NewSessionResponse{}, lifecycleMetaError(err)
	}

	if meta.Model == "" {
		meta.Model = a.options.DefaultModel
	}

	idValue, err := newSessionID()
	if err != nil {
		return acp.NewSessionResponse{}, err
	}

	id := acp.SessionId(idValue)

	mcpConfigs := nativeMCPServerConfigs(params.McpServers)

	client, releaseDirectory, generation, err := a.newOpenCodeClient(ctx, id, params.Cwd, mcpConfigs)
	if err != nil {
		return acp.NewSessionResponse{}, err
	}

	if validateErr := validateModel(ctx, client, meta.Model, modelFieldSessionMeta); validateErr != nil {
		_ = client.Close(context.Background())

		releaseDirectory()

		return acp.NewSessionResponse{}, validateErr
	}

	sessionStarted := time.Now()
	native, err := client.CreateSessionWithPolicy(ctx, "", nativePermissionPolicy(meta.Permission))
	observeRuntimeStartupStage(ctx, a.options.RuntimeResourceHooks, RuntimeResourceSession, RuntimeStartupSession, sessionStarted, err)

	if err != nil {
		_ = client.Close(context.Background())

		releaseDirectory()

		return acp.NewSessionResponse{}, err
	}

	idmap := idmapRecord{
		SessionID:       string(id),
		NativeSessionID: native.ID,
		Format:          SessionStoreFormat,
	}

	session := newSession(a, id, params.Cwd, params.AdditionalDirectories, native, client, meta, idmap)
	session.directoryRelease = releaseDirectory
	session.secretNeedles = mcpSecretNeedles(mcpConfigs)
	session.mcpServers = cloneNativeMCPServerConfigs(mcpConfigs)
	session.runtimeGeneration = generation

	if err := a.storeStartedSession(session); err != nil {
		_ = session.Close(context.Background())

		return acp.NewSessionResponse{}, err
	}

	if err := session.snapshotToStore(context.WithoutCancel(ctx)); err != nil {
		_ = session.Close(context.Background())

		return acp.NewSessionResponse{}, err
	}

	return acp.NewSessionResponse{
		SessionId:     id,
		Meta:          sessionResponseMeta(session.snapshot()),
		ConfigOptions: session.configOptions(ctx),
	}, nil
}

func (a *Agent) LoadSession(ctx context.Context, params acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
	ctx = a.observe.Extract(ctx, params.Meta)

	session, err := a.loadOrResumeSession(ctx, params.SessionId, params.Cwd, params.AdditionalDirectories, params.McpServers, params.Meta)
	if err != nil {
		return acp.LoadSessionResponse{}, err
	}

	if err := session.replayMessages(ctx); err != nil {
		return acp.LoadSessionResponse{}, err
	}

	return acp.LoadSessionResponse{
		Meta:          sessionResponseMeta(session.snapshot()),
		ConfigOptions: session.configOptions(ctx),
	}, nil
}

func (a *Agent) ResumeSession(ctx context.Context, params acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	ctx = a.observe.Extract(ctx, params.Meta)
	if err := validateMCPServers(params.McpServers); err != nil {
		return acp.ResumeSessionResponse{}, err
	}

	session, err := a.loadOrResumeSession(ctx, params.SessionId, params.Cwd, params.AdditionalDirectories, params.McpServers, params.Meta)
	if err != nil {
		return acp.ResumeSessionResponse{}, err
	}

	return acp.ResumeSessionResponse{
		Meta:          sessionResponseMeta(session.snapshot()),
		ConfigOptions: session.configOptions(ctx),
	}, nil
}

func (a *Agent) refreshCommandsAfterResponse(id acp.SessionId) func() {
	return func() {
		defer recoverAgentGoroutine(context.Background(), agentLogger(a), "OpenCode command refresh")

		ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		defer cancel()

		session, err := a.session(id)
		if err != nil {
			a.log.DebugContext(ctx, "skip OpenCode command refresh for missing session", slog.String("session_id", string(id)), slog.String("error", err.Error()))

			return
		}

		if err := session.refreshCommands(ctx); err != nil {
			a.log.DebugContext(ctx, "refresh OpenCode commands failed", slog.String("session_id", string(id)), slog.String("error", err.Error()))
		}
	}
}

func (a *Agent) loadOrResumeSession(
	ctx context.Context,
	id acp.SessionId,
	cwd string,
	additionalDirectories []string,
	mcpServers []acp.McpServer,
	metaMap map[string]any,
) (*session, error) {
	if err := a.ensureOpen(); err != nil {
		return nil, err
	}

	if id == "" {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldSessionID: validationRequired})
	}

	if a.isDeleted(id) {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: errValueSessionUnknown, jsonFieldField: jsonFieldSessionID})
	}

	if err := validateSessionStartPaths(cwd, additionalDirectories); err != nil {
		return nil, err
	}

	if err := validateMCPServers(mcpServers); err != nil {
		return nil, err
	}

	meta, err := sessionMetaFromLifecycle(metaMap)
	if err != nil {
		return nil, lifecycleMetaError(err)
	}

	storeCtx, cancel := a.sessionStoreContext(ctx)
	idmap, snapshot, ok, err := hydrateStateFromStore(storeCtx, a.sessionStore(), string(id))

	cancel()

	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: errValueSessionUnknown, jsonFieldField: jsonFieldSessionID})
	}

	if meta.Model == "" {
		meta.Model = joinModelValue(snapshot.Session.Model.ProviderID, snapshot.Session.Model.ModelID)
	}

	if meta.Mode == "" {
		meta.Mode = snapshot.Session.Model.Agent
	}

	mcpConfigs := nativeMCPServerConfigs(mcpServers)

	client, releaseDirectory, generation, err := a.newOpenCodeClient(ctx, id, cwd, mcpConfigs)
	if err != nil {
		return nil, err
	}

	if validateErr := validateModel(ctx, client, meta.Model, modelFieldSessionMeta); validateErr != nil {
		_ = client.Close(context.Background())

		releaseDirectory()

		return nil, validateErr
	}

	a.restoreMu.Lock()
	native, err := restoreSyncState(ctx, client, snapshot, idmap.NativeSessionID, cwd)
	a.restoreMu.Unlock()

	if err != nil {
		_ = client.Close(context.Background())

		releaseDirectory()

		return nil, err
	}

	session := newSession(a, id, cwd, additionalDirectories, native, client, meta, idmap)
	session.directoryRelease = releaseDirectory
	session.secretNeedles = mcpSecretNeedles(mcpConfigs)
	session.mcpServers = cloneNativeMCPServerConfigs(mcpConfigs)
	session.runtimeGeneration = generation

	if err := a.storeStartedSession(session); err != nil {
		_ = session.Close(context.Background())

		return nil, err
	}

	return session, nil
}

func (a *Agent) ListSessions(ctx context.Context, params acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	if err := a.ensureOpen(); err != nil {
		return acp.ListSessionsResponse{}, err
	}

	if err := validateOptionalAbsolutePath(jsonFieldCwd, params.Cwd); err != nil {
		return acp.ListSessionsResponse{}, err
	}

	a.mu.Lock()

	active := make([]*session, 0, len(a.sessions))
	for _, session := range a.sessions {
		if params.Cwd != nil && session.cwd != *params.Cwd {
			continue
		}

		active = append(active, session)
	}
	a.mu.Unlock()

	infos := make([]acp.SessionInfo, 0, len(active))
	seen := map[acp.SessionId]struct{}{}

	for _, session := range active {
		info := session.info()
		infos = append(infos, info)
		seen[info.SessionId] = struct{}{}
	}

	storeCtx, cancel := a.sessionStoreContext(ctx)
	stored, err := a.sessionStore().ListSessions(storeCtx)

	cancel()

	if err != nil {
		return acp.ListSessionsResponse{}, err
	}

	for _, summary := range stored {
		id := acp.SessionId(summary.SessionID)
		if _, ok := seen[id]; ok || a.isDeleted(id) {
			continue
		}

		if params.Cwd != nil && summary.Cwd != "" && summary.Cwd != *params.Cwd {
			continue
		}

		title := summary.Title
		updated := time.UnixMilli(summary.UpdatedAtUnixMilli).UTC().Format(time.RFC3339)
		infos = append(infos, acp.SessionInfo{
			SessionId: id,
			Cwd:       summary.Cwd,
			Title:     &title,
			UpdatedAt: &updated,
			Meta:      summary.Meta,
		})
	}

	slices.SortFunc(infos, func(left, right acp.SessionInfo) int {
		l := *left.UpdatedAt
		r := *right.UpdatedAt

		if r != l {
			return strings.Compare(r, l)
		}

		return strings.Compare(string(left.SessionId), string(right.SessionId))
	})

	paged, next, err := paginateSessionInfos(infos, params.Cursor)
	if err != nil {
		return acp.ListSessionsResponse{}, err
	}

	return acp.ListSessionsResponse{Sessions: paged, NextCursor: next}, nil
}

func (a *Agent) CloseSession(ctx context.Context, params acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	session, err := a.session(params.SessionId)
	if err != nil {
		return acp.CloseSessionResponse{}, err
	}

	snapshotErr := session.snapshotToStore(context.WithoutCancel(ctx))

	closeCtx, closeCancel := context.WithTimeout(context.Background(), closeTimeout)
	closeErr := session.Close(closeCtx)

	closeCancel()

	if a.removeSessionIf(params.SessionId, session) {
		a.observe.AddActiveSession(ctx, -1)
	}

	return acp.CloseSessionResponse{}, errors.Join(snapshotErr, closeErr)
}

func (a *Agent) UnstableDeleteSession(ctx context.Context, params acp.UnstableDeleteSessionRequest) (acp.UnstableDeleteSessionResponse, error) {
	ctx = a.observe.Extract(ctx, params.Meta)
	if params.SessionId == "" {
		return acp.UnstableDeleteSessionResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldSessionID: validationRequired})
	}

	a.mu.Lock()
	session := a.sessions[params.SessionId]
	a.mu.Unlock()

	storeCtx, cancel := a.sessionStoreContext(ctx)
	err := a.sessionStore().Delete(storeCtx, SessionKey{SessionID: string(params.SessionId)})

	cancel()

	if err != nil {
		return acp.UnstableDeleteSessionResponse{}, err
	}

	a.mu.Lock()
	if session == nil || a.sessions[params.SessionId] == session {
		delete(a.sessions, params.SessionId)
	}

	a.deleted[params.SessionId] = struct{}{}
	a.mu.Unlock()

	if session != nil {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), closeTimeout)
		err = session.DeleteNativeAndClose(closeCtx)

		closeCancel()

		a.observe.AddActiveSession(ctx, -1)
	}

	return acp.UnstableDeleteSessionResponse{}, err
}

func (a *Agent) forkSession(ctx context.Context, params acp.UnstableForkSessionRequest) (acp.UnstableForkSessionResponse, error) {
	ctx = a.observe.Extract(ctx, params.Meta)
	if err := validateSessionStartPaths(params.Cwd, params.AdditionalDirectories); err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}

	if err := validateUnstableMCPServers(params.McpServers); err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}

	meta, err := sessionMetaFromLifecycle(params.Meta)
	if err != nil {
		return acp.UnstableForkSessionResponse{}, lifecycleMetaError(err)
	}

	parent, err := a.session(params.SessionId)
	if err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}

	parentSnapshot := parent.snapshot()
	if meta.PermissionSet && normalizeOpenCodePermission(meta.Permission) != parentSnapshot.permission {
		return acp.UnstableForkSessionResponse{}, acp.NewInvalidParams(map[string]any{
			jsonFieldError: "child_permission_must_inherit",
			jsonFieldField: "_meta.opencode.options.permission",
		})
	}

	meta.Permission = parentSnapshot.permission

	nativeChild, err := parentSnapshot.client.Fork(ctx, parentSnapshot.idmap.NativeSessionID, "")
	if err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}

	idValue, err := newSessionID()
	if err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}

	id := acp.SessionId(idValue)

	if meta.Model == "" {
		meta.Model = joinModelValue(parentSnapshot.providerID, parentSnapshot.modelID)
	}

	if meta.Mode == "" {
		meta.Mode = parentSnapshot.mode
	}

	mcpConfigs := nativeMCPServerConfigsFromUnstable(params.McpServers)

	client, releaseDirectory, generation, err := a.newOpenCodeClient(ctx, id, params.Cwd, mcpConfigs)
	if err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}

	if validateErr := validateModel(ctx, client, meta.Model, modelFieldSessionMeta); validateErr != nil {
		_ = client.Close(context.Background())

		releaseDirectory()

		return acp.UnstableForkSessionResponse{}, validateErr
	}

	native, err := client.GetSession(ctx, nativeChild.ID)
	if err != nil {
		_ = client.Close(context.Background())

		releaseDirectory()

		return acp.UnstableForkSessionResponse{}, err
	}

	idmap := idmapRecord{
		SessionID:             string(id),
		NativeSessionID:       native.ID,
		ParentSessionID:       string(params.SessionId),
		NativeParentSessionID: parentSnapshot.idmap.NativeSessionID,
		Format:                SessionStoreFormat,
	}

	session := newSession(a, id, params.Cwd, params.AdditionalDirectories, native, client, meta, idmap)
	session.directoryRelease = releaseDirectory
	session.secretNeedles = mcpSecretNeedles(mcpConfigs)
	session.mcpServers = cloneNativeMCPServerConfigs(mcpConfigs)
	session.runtimeGeneration = generation

	if err := a.storeStartedSession(session); err != nil {
		_ = session.Close(context.Background())

		return acp.UnstableForkSessionResponse{}, err
	}

	if err := session.snapshotToStore(context.WithoutCancel(ctx)); err != nil {
		_ = session.Close(context.Background())

		return acp.UnstableForkSessionResponse{}, err
	}

	return acp.UnstableForkSessionResponse{
		SessionId:     id,
		Meta:          sessionResponseMeta(session.snapshot()),
		ConfigOptions: unstableConfigOptions(session.configOptions(ctx)),
	}, nil
}

// nativeMCPServerConfigs maps validated ACP MCP server declarations onto the
// native launch config: HTTP servers become remote entries, stdio servers
// become local entries. Call validateMCPServers first; unsupported transports
// are skipped here.
func nativeMCPServerConfigs(servers []acp.McpServer) []opencode.MCPServerConfig {
	if len(servers) == 0 {
		return nil
	}

	configs := make([]opencode.MCPServerConfig, 0, len(servers))

	for _, server := range servers {
		switch {
		case server.Http != nil:
			configs = append(configs, opencode.MCPServerConfig{
				Name:    server.Http.Name,
				URL:     server.Http.Url,
				Headers: httpHeaderMap(server.Http.Headers),
			})
		case server.Stdio != nil:
			configs = append(configs, nativeStdioMCPServerConfig(server.Stdio))
		}
	}

	return configs
}

// nativeMCPServerConfigsFromUnstable is the fork-path equivalent of
// nativeMCPServerConfigs for the unstable MCP server union.
func nativeMCPServerConfigsFromUnstable(servers []acp.UnstableMcpServer) []opencode.MCPServerConfig {
	if len(servers) == 0 {
		return nil
	}

	configs := make([]opencode.MCPServerConfig, 0, len(servers))

	for _, server := range servers {
		switch {
		case server.Http != nil:
			configs = append(configs, opencode.MCPServerConfig{
				Name:    server.Http.Name,
				URL:     server.Http.Url,
				Headers: httpHeaderMap(server.Http.Headers),
			})
		case server.Stdio != nil:
			configs = append(configs, nativeStdioMCPServerConfig(server.Stdio))
		}
	}

	return configs
}

func nativeStdioMCPServerConfig(server *acp.McpServerStdio) opencode.MCPServerConfig {
	command := make([]string, 0, len(server.Args)+1)
	command = append(command, server.Command)
	command = append(command, server.Args...)

	var env map[string]string

	if len(server.Env) > 0 {
		env = make(map[string]string, len(server.Env))
		for _, variable := range server.Env {
			env[variable.Name] = variable.Value
		}
	}

	return opencode.MCPServerConfig{Name: server.Name, Command: command, Env: env}
}

func httpHeaderMap(headers []acp.HttpHeader) map[string]string {
	if len(headers) == 0 {
		return nil
	}

	values := make(map[string]string, len(headers))
	for _, header := range headers {
		values[header.Name] = header.Value
	}

	return values
}

func (a *Agent) newOpenCodeClient(ctx context.Context, id acp.SessionId, cwd string, mcpServers []opencode.MCPServerConfig) (opencode.Client, func(), uint64, error) {
	for {
		releaseDirectory, err := a.bindDirectory(id, cwd, mcpServers)
		if err != nil {
			return nil, nil, 0, err
		}

		runtime, generation, err := a.sharedRuntimeBinding(ctx)
		if err != nil {
			releaseDirectory()

			return nil, nil, 0, err
		}

		configurationStarted := time.Now()
		client, err := runtime.Scope(ctx, opencode.ScopeOptions{Directory: cwd, MCPServers: mcpServers})
		observeRuntimeStartupStage(ctx, a.options.RuntimeResourceHooks, RuntimeResourceSession, RuntimeStartupConfiguration, configurationStarted, err)

		if err == nil {
			return client, releaseDirectory, generation, nil
		}

		releaseDirectory()

		if a.runtimeGenerationIsCurrent(generation) {
			return nil, nil, 0, err
		}

		if err := ctx.Err(); err != nil {
			return nil, nil, 0, err
		}
	}
}

func cloneNativeMCPServerConfigs(configs []opencode.MCPServerConfig) []opencode.MCPServerConfig {
	if configs == nil {
		return nil
	}

	cloned := make([]opencode.MCPServerConfig, len(configs))
	for index := range configs {
		cloned[index] = configs[index]
		cloned[index].Headers = cloneStringMap(configs[index].Headers)
		cloned[index].Command = append([]string(nil), configs[index].Command...)
		cloned[index].Env = cloneStringMap(configs[index].Env)
	}

	return cloned
}

func nativePermissionPolicy(permission string) []opencode.PermissionRule {
	return []opencode.PermissionRule{{Permission: "*", Pattern: "*", Action: normalizeOpenCodePermission(permission)}}
}

func mcpSecretNeedles(configs []opencode.MCPServerConfig) []string {
	var needles []string

	for _, config := range configs {
		for _, value := range config.Headers {
			if value != "" {
				needles = append(needles, value)
			}
		}

		for _, value := range config.Env {
			if value != "" {
				needles = append(needles, value)
			}
		}
	}

	return needles
}

// homeRoot returns the single shared runtime XDG root.
func (a *Agent) homeRoot() string {
	if a.options.Home != "" {
		return a.options.Home
	}

	return filepath.Join(scratchParent(a.options.ScratchDir), defaultAgentName, "runtime")
}

func validateUnstableMCPServers(servers []acp.UnstableMcpServer) error {
	seen := make(map[string]struct{}, len(servers))

	for index, server := range servers {
		if server.Sse != nil {
			return acp.NewInvalidParams(map[string]any{jsonFieldError: errValueUnsupported, jsonFieldField: fmt.Sprintf("mcpServers[%d]", index), jsonFieldServer: server.Sse.Name})
		}

		if server.Acp != nil {
			return acp.NewInvalidParams(map[string]any{jsonFieldError: errValueUnsupported, jsonFieldField: fmt.Sprintf("mcpServers[%d]", index), jsonFieldServer: server.Acp.Name})
		}

		var name string

		switch {
		case server.Http != nil:
			name = server.Http.Name
		case server.Stdio != nil:
			name = server.Stdio.Name
		default:
			continue
		}

		if strings.TrimSpace(name) == "" {
			return acp.NewInvalidParams(map[string]any{fmt.Sprintf("mcpServers[%d].name", index): validationRequired})
		}

		if _, ok := seen[name]; ok {
			return acp.NewInvalidParams(map[string]any{fmt.Sprintf("mcpServers[%d].name", index): validationDuplicate})
		}

		seen[name] = struct{}{}
	}

	return nil
}

func paginateSessionInfos(infos []acp.SessionInfo, cursor *string) ([]acp.SessionInfo, *string, error) {
	offset, err := decodeListCursor(cursor)
	if err != nil {
		return nil, nil, acp.NewInvalidParams(map[string]any{"cursor": "invalid cursor"})
	}

	if offset > len(infos) {
		return nil, nil, acp.NewInvalidParams(map[string]any{"cursor": "cursor is past end"})
	}

	end := offset + listSessionsPageSize
	if end >= len(infos) {
		return infos[offset:], nil, nil
	}

	next := encodeListCursor(end)

	return infos[offset:end], &next, nil
}

func decodeListCursor(cursor *string) (int, error) {
	if cursor == nil || *cursor == "" {
		return 0, nil
	}

	data, err := base64.RawURLEncoding.DecodeString(*cursor)
	if err != nil {
		return 0, err
	}

	offset, err := strconv.Atoi(string(data))
	if err != nil || offset < 0 {
		return 0, strconv.ErrSyntax
	}

	return offset, nil
}

func encodeListCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}
