package opencodeacp

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/savid/acp-go-core/usage"
	"github.com/savid/acp-go-core/usage/opencodego"
	"github.com/savid/acp-go-core/usage/openrouter"
	"github.com/savid/acp-go-core/wire"
)

func (a *Agent) accountUsage(ctx context.Context, params json.RawMessage) (response wire.AccountUsageResponse, err error) {
	request, refusal := wire.DecodeAccountUsageRequest(params, wire.AccountUsageScopeSession)

	ctx, finish := a.observe.StartACP(ctx, request.Meta, AccountUsageMethod)
	defer func() { finish(err) }()

	if refusal != nil {
		return wire.AccountUsageResponse{}, refusal
	}

	if request.ProviderID == "" {
		return wire.AccountUsageResponse{}, wire.Missing("providerId")
	}

	var reader usage.Reader

	switch request.ProviderID {
	case opencodego.ProviderID:
		reader = opencodego.Reader{Transport: a.usageTransport}
	case openrouter.ProviderID:
		reader = openrouter.Reader{Transport: a.usageTransport}
	default:
		return wire.AccountUsageResponse{}, wire.Unsupported("providerId")
	}

	s, err := a.session(ctx, request.SessionID)
	if err != nil {
		return wire.AccountUsageResponse{}, err
	}

	if admission := s.admissionError(); admission != nil {
		return wire.AccountUsageResponse{}, admission
	}

	release, err := s.acquireGate(limitSessionPrompt)
	if err != nil {
		return wire.AccountUsageResponse{}, err
	}
	defer release()

	readCtx, cancel := context.WithTimeout(ctx, wire.AccountUsageReadTimeout)
	defer cancel()

	rt, err := s.ensureRuntime(readCtx)
	if err != nil {
		return wire.AccountUsageResponse{}, err
	}

	response, err = s.readProviderUsage(readCtx, rt, request.ProviderID, reader)

	if ctx.Err() != nil {
		return wire.AccountUsageResponse{}, ctx.Err()
	}

	if admission := s.admissionError(); admission != nil {
		return wire.AccountUsageResponse{}, admission
	}

	if err != nil || !rt.alive() {
		return wire.AccountUsageResponse{}, usage.RequestError(vendor, err)
	}

	return response, nil
}

func (s *session) readProviderUsage(ctx context.Context, rt *binding, providerID string, reader usage.Reader) (wire.AccountUsageResponse, error) {
	s.mu.Lock()
	selected := s.model
	s.mu.Unlock()

	selectedProvider, modelID, _ := strings.Cut(selected, "/")
	if selectedProvider != providerID {
		modelID = ""
	}

	return usage.ReadVerified(ctx, func(ctx context.Context) (usage.Access, error) {
		return rt.client.UsageAccess(ctx, s.cwd, providerID, modelID)
	}, reader)
}
