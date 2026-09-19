package opencodeacp

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"strings"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-core/wire"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

func (s *session) launch(ctx context.Context) (*binding, error) {
	server, err := s.agent.ensureRuntime(ctx)
	if err != nil {
		return nil, err
	}

	readCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	rt := &binding{server: server, client: server.client, cancel: cancel, ending: readCtx.Done(), bound: make(chan struct{}), done: make(chan struct{}), events: make(chan opencode.Event, 256), results: make(chan nativePromptResult, 1)}

	s.mu.Lock()
	closing := s.closing

	if !closing {
		s.runtime = rt
	}
	s.mu.Unlock()

	// A close that began during this launch has already sampled the binding it
	// releases, so one bound now would outlive the session.
	if closing {
		cancel()

		return nil, wire.UnknownSession()
	}

	go s.pump(readCtx, rt)

	return rt, nil
}

func (s *session) startFailure(ctx context.Context, err error) error {
	s.agent.log.ErrorContext(ctx, "opencode session start failed", slog.String("reason", err.Error()))

	return wire.InternalFailure(vendor, internalClassNativeStart)
}

func (s *session) configureRuntime(ctx context.Context, rt *binding, model, expectID string) error {
	var native opencode.NativeSession

	if expectID == "" {
		body := map[string]any{"metadata": s.carrierMetadata(nil), metaPermissionKey: s.permissionRules()}
		if s.options.Mode != "" {
			body["agent"] = s.options.Mode
		}

		if model != "" {
			p, m, _ := strings.Cut(model, "/")
			body["model"] = map[string]string{"providerID": p, fieldID: m, "variant": s.options.Effort}
		}

		if err := rt.client.Do(ctx, s.cwd, http.MethodPost, "/session", body, &native); err != nil {
			return s.startFailure(ctx, err)
		}

		s.nativeID = native.ID
		s.id = acp.SessionId(native.ID)
	} else {
		var err error

		native, err = rt.client.Session(ctx, s.cwd, expectID)
		if err != nil {
			return s.agent.restoreRefused(ctx, s.id, err)
		}
	}

	if native.ID == "" || (expectID != "" && native.ID != expectID) {
		return s.startFailure(ctx, errors.New("native session identity missing or changed"))
	}

	var statuses map[string]opencode.NativeSessionStatus
	if err := rt.client.Do(ctx, s.cwd, http.MethodGet, "/session/status", nil, &statuses); err != nil {
		return s.startFailure(ctx, err)
	}

	if status := statuses[native.ID].Type; status != "" && status != statusIdle {
		return wire.Backpressure(limitSessionRestore)
	}

	if err := rt.client.Do(ctx, s.cwd, http.MethodPatch, opencode.SessionPath(native.ID), map[string]any{"metadata": s.carrierMetadata(native.Metadata), metaPermissionKey: s.permissionRules()}, &native); err != nil {
		return s.startFailure(ctx, err)
	}

	s.mu.Lock()

	s.model = model
	if s.model == "" && native.Model.ID != "" {
		s.model = native.Model.ProviderID + "/" + native.Model.ID
	}

	s.mode = s.options.Mode
	if s.mode == "" {
		s.mode = native.Agent
	}

	if s.mode == "" {
		s.mode = modeBuild
	}

	s.effort = s.options.Effort
	s.title = native.Title
	s.updatedAt = time.UnixMilli(native.Time.Updated).UTC().Format(time.RFC3339)
	s.mu.Unlock()

	if err := s.refreshCatalogs(ctx, rt); err != nil {
		return s.startFailure(ctx, err)
	}

	if err := opencode.CheckPlugin(rt.server.root, s.cwd); err != nil {
		return s.startFailure(ctx, err)
	}

	rt.server.mu.Lock()
	defer rt.server.mu.Unlock()

	if existing := rt.server.bindings[native.ID]; existing != nil && existing != rt {
		return wire.Backpressure(limitSessionRestore)
	}

	if !rt.server.alive() {
		return wire.RuntimeUnavailable(vendor)
	}

	rt.server.bindings[native.ID] = rt

	return nil
}

func (s *session) carrierMetadata(base map[string]any) map[string]any {
	result := wire.CloneMap(base)
	if result == nil {
		result = map[string]any{}
	}

	env := maps.Clone(s.options.Env)
	if env == nil {
		env = map[string]string{}
	}

	result[opencode.CarrierKey] = map[string]any{metaEnvKey: env, metaExtraPathDirsKey: append([]string{}, s.options.ExtraPathDirs...)}

	return result
}

func (s *session) permissionRules() []map[string]string {
	permission := s.options.Permission
	if permission == "" {
		permission = "ask"
	}

	return []map[string]string{{metaPermissionKey: "*", "pattern": "*", "action": permission}}
}
