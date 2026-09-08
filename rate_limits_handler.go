package opencodeacp

import (
	"context"
	"encoding/json"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

const quotaCurrencyUSD = "USD"
const quotaStatusOK = "ok"

type quotaSessionSnapshot struct {
	session    *session
	client     opencode.Client
	providerID string
	modelID    string
	generation uint64
	revision   uint64
}

func (a *Agent) handleRateLimits(ctx context.Context, raw json.RawMessage) (RateLimitsResponse, error) {
	req, err := decodeRateLimitsRequest(raw)
	if err != nil {
		return RateLimitsResponse{}, err
	}

	snapshots, runtime, generation, revision, err := a.quotaContext(req.SessionID)
	if err != nil {
		return RateLimitsResponse{}, err
	}

	providerID := req.ProviderID
	if providerID == "" {
		providerID = quotaEffectiveProvider(snapshots, a.options.DefaultModel)
		if providerID == "" {
			return RateLimitsResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldError: valMissing, jsonFieldField: authFieldProviderID})
		}
	}

	if !opencode.SupportsProviderQuota(providerID) {
		return rateLimitsUnsupported(providerID), nil
	}

	if err := a.quotaRuntimeError(); err != nil {
		return RateLimitsResponse{}, err
	}

	clients := make([]opencode.Client, 0, len(snapshots)+1)
	modelID := ""

	if req.SessionID == "" && runtime != nil {
		clients = append(clients, runtime)
	}

	for _, snapshot := range snapshots {
		if snapshot.client != nil {
			clients = append(clients, snapshot.client)
		}

		if req.SessionID != "" && snapshot.providerID == providerID {
			modelID = snapshot.modelID
		}
	}

	readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	observation := opencode.ReadProviderQuota(readCtx, clients, providerID, modelID, a.options.DirectAPI, a.quotaHTTPClient)

	if err := ctx.Err(); err != nil {
		return RateLimitsResponse{}, err
	}

	if err := a.ensureOpen(); err != nil {
		return RateLimitsResponse{}, err
	}

	if req.SessionID != "" {
		if _, _, _, _, err := a.quotaContext(req.SessionID); err != nil {
			return RateLimitsResponse{}, err
		}
	}

	if err := a.quotaRuntimeError(); err != nil {
		return RateLimitsResponse{}, err
	}

	if !a.quotaContextCurrent(snapshots, runtime, generation, revision, req.SessionID == "") {
		return rateLimitsUnavailable(providerID, "read_failed"), nil
	}

	return normalizeProviderQuota(providerID, observation), nil
}

func (a *Agent) quotaContext(sessionID acp.SessionId) ([]quotaSessionSnapshot, opencode.Client, uint64, uint64, error) {
	a.mu.Lock()
	runtime, generation, revision := a.runtime, a.runtimeGeneration, a.quotaRevision

	sessions := make([]*session, 0, len(a.sessions))
	if sessionID != "" {
		_, deleted := a.deleted[sessionID]

		selected := a.sessions[sessionID]
		if selected == nil || deleted {
			a.mu.Unlock()

			return nil, nil, 0, 0, unknownQuotaSession()
		}

		sessions = append(sessions, selected)
	} else {
		for id, selected := range a.sessions {
			if _, deleted := a.deleted[id]; !deleted {
				sessions = append(sessions, selected)
			}
		}
	}
	a.mu.Unlock()

	snapshots := make([]quotaSessionSnapshot, 0, len(sessions))
	for _, selected := range sessions {
		selected.mu.Lock()
		closed := selected.closed
		poisonErr := selected.poisonedErrorLocked()
		snapshot := quotaSessionSnapshot{session: selected, client: selected.client, providerID: selected.providerID, modelID: selected.modelID, generation: selected.runtimeGeneration, revision: selected.quotaRevision}
		selected.mu.Unlock()

		if closed {
			if sessionID != "" {
				return nil, nil, 0, 0, unknownQuotaSession()
			}

			continue
		}

		if poisonErr != nil && sessionID != "" {
			return nil, nil, 0, 0, poisonErr
		}

		snapshots = append(snapshots, snapshot)
	}

	return snapshots, runtime, generation, revision, nil
}

func unknownQuotaSession() error {
	return acp.NewInvalidParams(map[string]any{jsonFieldError: valSessionUnknown, jsonFieldField: jsonFieldSessionID})
}

func quotaEffectiveProvider(snapshots []quotaSessionSnapshot, defaultModel string) string {
	if len(snapshots) == 0 {
		provider, _ := splitModelValue(defaultModel, "", "")

		return provider
	}

	provider := snapshots[0].providerID
	for _, snapshot := range snapshots {
		if snapshot.providerID != provider {
			return ""
		}
	}

	return provider
}

func (a *Agent) quotaContextCurrent(snapshots []quotaSessionSnapshot, runtime opencode.Client, generation, revision uint64, providerWide bool) bool {
	a.mu.Lock()

	valid := !a.closed && a.runtime == runtime && a.runtimeGeneration == generation && a.quotaRevision == revision && a.quotaAuthMutations == 0
	if providerWide && len(a.sessions) != len(snapshots) {
		valid = false
	}

	for _, snapshot := range snapshots {
		_, deleted := a.deleted[snapshot.session.id]
		valid = valid && !deleted && a.sessions[snapshot.session.id] == snapshot.session
	}
	a.mu.Unlock()

	if !valid {
		return false
	}

	for _, snapshot := range snapshots {
		selected := snapshot.session
		selected.mu.Lock()
		valid = !selected.closed && selected.client == snapshot.client && selected.runtimeGeneration == snapshot.generation && selected.quotaRevision == snapshot.revision && selected.providerID == snapshot.providerID && selected.modelID == snapshot.modelID
		selected.mu.Unlock()

		if !valid {
			return false
		}
	}

	return true
}

func (a *Agent) beginQuotaAuthMutation() func() {
	a.mu.Lock()
	a.quotaRevision++
	a.quotaAuthMutations++
	a.mu.Unlock()

	return func() {
		a.mu.Lock()
		a.quotaRevision++
		a.quotaAuthMutations--
		a.mu.Unlock()
	}
}

func normalizeProviderQuota(providerID string, observation opencode.ProviderQuota) RateLimitsResponse {
	switch observation.Availability {
	case valUnsupported:
		return rateLimitsUnsupported(providerID)
	case "available":
	default:
		return rateLimitsUnavailable(providerID, observation.Reason)
	}

	observedAt := observation.ObservedAt.UTC().Format(time.RFC3339Nano)
	pool := RateLimitPool{ID: providerID, Windows: []RateLimitWindow{}}

	if usage := observation.Go; usage != nil {
		for _, id := range []string{"rolling", "weekly", "monthly"} {
			window, ok := usage.Usage[id]
			if !ok {
				continue
			}

			status := quotaStatusOK
			if window.Status == "rate-limited" {
				status = "exhausted"
			}

			pool.Windows = append(pool.Windows, RateLimitWindow{ID: id, UsedPercent: window.Percent, Status: status, ObservedAt: observedAt, ResetsAt: window.ResetsAt})
		}
	}

	if usage := observation.OpenRouter; usage != nil {
		balance := RateLimitBalance{ID: jsonFieldKey, ObservedAt: observedAt}
		if usage.Limit == nil {
			balance.Uncapped = true
			balance.Used = &RateLimitMoney{Amount: *usage.Usage, Currency: quotaCurrencyUSD}
		} else {
			balance.Limit = &RateLimitMoney{Amount: *usage.Limit, Currency: quotaCurrencyUSD}
			balance.Remaining = &RateLimitMoney{Amount: *usage.LimitRemaining, Currency: quotaCurrencyUSD}
		}

		if usage.LimitReset != nil {
			balance.ResetInterval = *usage.LimitReset
		}

		pool.Balances = []RateLimitBalance{balance}
	}

	pools := []RateLimitPool{pool}
	if credits := observation.OpenRouterCredits; credits != nil {
		pools = append(pools, RateLimitPool{
			ID: "openrouter-account", Windows: []RateLimitWindow{},
			Balances: []RateLimitBalance{{
				ID: "credits", Used: &RateLimitMoney{Amount: credits.Used, Currency: quotaCurrencyUSD},
				Remaining:  &RateLimitMoney{Amount: credits.Remaining, Currency: quotaCurrencyUSD},
				ObservedAt: credits.ObservedAt.UTC().Format(time.RFC3339Nano),
			}},
		})
	}

	return RateLimitsResponse{ProviderID: providerID, Availability: "available", Pools: pools}
}

func (a *Agent) quotaRuntimeError() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.runtimeFatalErr
}
