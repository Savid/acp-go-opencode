package opencodeacp

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

// configOptionMeta reads the `_meta` of whichever variant of the config-option
// union arrived.
func configOptionMeta(params acp.SetSessionConfigOptionRequest) map[string]any {
	switch {
	case params.ValueId != nil:
		return params.ValueId.Meta
	case params.Boolean != nil:
		return params.Boolean.Meta
	default:
		return nil
	}
}

func (a *Agent) SetSessionConfigOption(ctx context.Context, params acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	// The request is a union whose `_meta` lives on the chosen variant, so the
	// reserved literal is refused on whichever variant the host sent, before the
	// discriminator itself is judged.
	if refusal := refuseLifecycleMeta(configOptionMeta(params)); refusal != nil {
		return acp.SetSessionConfigOptionResponse{}, refusal
	}

	// Every option this agent advertises is a select, so the request member
	// that made this call unsupported is the discriminator that chose the
	// boolean form, not the value it carried.
	if params.Boolean != nil {
		return acp.SetSessionConfigOptionResponse{}, unsupportedField(jsonFieldType)
	}

	if params.ValueId == nil {
		return acp.SetSessionConfigOptionResponse{}, unsupportedField(jsonFieldValue)
	}

	session, err := a.session(params.ValueId.SessionId)
	if err != nil {
		return acp.SetSessionConfigOptionResponse{}, err
	}

	if err := session.ensureNotPoisoned(); err != nil {
		return acp.SetSessionConfigOptionResponse{}, err
	}

	value := string(params.ValueId.Value)
	if value == "" {
		return acp.SetSessionConfigOptionResponse{}, unsupportedField(jsonFieldValue)
	}

	// Neither value is judged against the advertisement. That advertisement is
	// whatever the native runtime resolved when it started and never reloads
	// within that runtime's life, so a value absent from it is not evidence the
	// value is unusable — only that this runtime has not heard of it yet. The
	// session-start door has always carried both through unjudged; this one now
	// says the same thing, because OpenCode owns model and agent resolution
	// alike and answers for a name it does not know at use time.
	switch params.ValueId.ConfigId {
	case configModel:
		session.setModel(value)
	case configMode:
		session.setMode(value)
	default:
		return acp.SetSessionConfigOptionResponse{}, unsupportedField("configId")
	}

	options := session.configOptions(ctx)
	_ = session.emitUpdate(ctx, acp.SessionUpdate{
		ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{ConfigOptions: options},
	})

	return acp.SetSessionConfigOptionResponse{ConfigOptions: options}, nil
}

// configOptions advertises the selects this session offers, each built from its
// own native read. A read that fails omits only the option it feeds, because the
// rest of the advertisement is still true and a session that already exists is
// never refused over a catalog the next call repeats. What such a failure must
// not be is silent: the omission always states its cause, so a host missing an
// option has a reason recorded for it rather than an empty list.
func (s *session) configOptions(ctx context.Context) []acp.SessionConfigOption {
	snapshot := s.snapshot()
	if snapshot.client == nil {
		return nil
	}

	var options []acp.SessionConfigOption

	if providers, err := snapshot.client.ConfigProviders(ctx); err != nil {
		s.reportConfigOptionUnavailable(ctx, configModel, err)
	} else if model := modelConfigOption(snapshot, providers); model.Select != nil {
		options = append(options, model)
	}

	if agents, err := snapshot.client.Agents(ctx); err != nil {
		s.reportConfigOptionUnavailable(ctx, configMode, err)
	} else if mode := modeConfigOption(snapshot, agents); mode.Select != nil {
		options = append(options, mode)
	}

	return options
}

// reportConfigOptionUnavailable names the native read that left one option out
// of a session's advertised config options.
func (s *session) reportConfigOptionUnavailable(ctx context.Context, configID acp.SessionConfigId, err error) {
	s.agent.log.WarnContext(ctx, "OpenCode config option is unavailable",
		slog.String("session_id", string(s.id)),
		slog.String("config_id", string(configID)),
		loggableError(err),
	)
}

func modelConfigOption(snapshot sessionSnapshot, providers opencode.ProvidersResponse) acp.SessionConfigOption {
	category := acp.SessionConfigOptionCategoryModel
	current := snapshot.modelValue()

	groups := make(acp.SessionConfigSelectOptionsGrouped, 0, len(providers.Providers))
	for _, provider := range providers.Providers {
		if provider.ID == "" || len(provider.Models) == 0 {
			continue
		}

		keys := make([]string, 0, len(provider.Models))
		for key := range provider.Models {
			keys = append(keys, key)
		}

		slices.Sort(keys)

		group := acp.SessionConfigSelectGroup{
			Group: acp.SessionConfigGroupId(provider.ID),
			Name:  firstNonEmpty(provider.Name, provider.ID),
		}
		for _, key := range keys {
			model := provider.Models[key]
			modelID := firstNonEmpty(model.ID, key)

			value := provider.ID + "/" + modelID
			if current == "" {
				current = value
			}

			group.Options = append(group.Options, acp.SessionConfigSelectOption{
				Name:  firstNonEmpty(model.Name, value),
				Value: acp.SessionConfigValueId(value),
				Meta:  map[string]any{opencodeMetaKey: modelMeta(provider.ID, modelID, model)},
			})
		}

		if len(group.Options) > 0 {
			groups = append(groups, group)
		}
	}

	if len(groups) == 0 {
		if current == "" {
			return acp.SessionConfigOption{}
		}

		options := acp.SessionConfigSelectOptionsUngrouped{{Name: current, Value: acp.SessionConfigValueId(current)}}

		return acp.SessionConfigOption{Select: &acp.SessionConfigOptionSelect{
			Id:           configModel,
			Name:         "Model",
			Category:     &category,
			Type:         configTypeSelect,
			CurrentValue: acp.SessionConfigValueId(current),
			Options:      acp.SessionConfigSelectOptions{Ungrouped: &options},
		}}
	}

	return acp.SessionConfigOption{Select: &acp.SessionConfigOptionSelect{
		Id:           configModel,
		Name:         "Model",
		Category:     &category,
		Type:         configTypeSelect,
		CurrentValue: acp.SessionConfigValueId(current),
		Options:      acp.SessionConfigSelectOptions{Grouped: &groups},
	}}
}

func modeConfigOption(snapshot sessionSnapshot, agents []opencode.NativeAgent) acp.SessionConfigOption {
	category := acp.SessionConfigOptionCategoryMode
	// The current value is the mode this session will actually address its native
	// frames with, listed or not. The model option already answers this way, and
	// an advertisement that renamed the session's mode to one the list happens to
	// carry would describe a session that does not exist.
	current := firstNonEmpty(snapshot.mode, defaultMode)
	values := make(acp.SessionConfigSelectOptionsUngrouped, 0, len(agents))
	seen := map[string]struct{}{}

	for _, agent := range agents {
		value := firstNonEmpty(agent.Name, agent.Mode)
		if value == "" {
			continue
		}

		if _, ok := seen[value]; ok {
			continue
		}

		seen[value] = struct{}{}
		description := agent.Description

		item := acp.SessionConfigSelectOption{
			Name:  titleASCII(value),
			Value: acp.SessionConfigValueId(value),
		}
		if description != "" {
			item.Description = &description
		}

		values = append(values, item)
	}

	if len(values) == 0 {
		return acp.SessionConfigOption{}
	}

	return acp.SessionConfigOption{Select: &acp.SessionConfigOptionSelect{
		Id:           configMode,
		Name:         "Mode",
		Category:     &category,
		Type:         configTypeSelect,
		CurrentValue: acp.SessionConfigValueId(current),
		Options:      acp.SessionConfigSelectOptions{Ungrouped: &values},
	}}
}

func (snapshot sessionSnapshot) modelValue() string {
	return joinModelValue(snapshot.providerID, snapshot.modelID)
}

func modelMeta(providerID string, modelID string, model opencode.ProviderModel) map[string]any {
	meta := map[string]any{"modelId": providerID + "/" + modelID}
	if n, ok := opencode.IntFromNumber(model.Limit["context"]); ok {
		meta["contextWindow"] = n
	}

	if n, ok := opencode.IntFromNumber(model.Limit["output"]); ok {
		meta["maxOutputTokens"] = n
	}

	efforts := supportedEfforts(model)
	if len(efforts) > 0 {
		meta["supportedEffortLevels"] = efforts
	}

	return meta
}

func supportedEfforts(model opencode.ProviderModel) []string {
	seen := map[string]struct{}{}

	for key, raw := range model.Options {
		if !strings.Contains(strings.ToLower(key), "effort") {
			continue
		}

		for _, value := range optionStringValues(raw) {
			seen[value] = struct{}{}
		}
	}

	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}

	slices.Sort(out)

	return out
}

func optionStringValues(raw any) []string {
	switch value := raw.(type) {
	case []string:
		return compactNonEmptyStrings(value)
	case []any:
		out := make([]string, 0, len(value))
		for _, item := range value {
			if str, _ := item.(string); str != "" {
				out = append(out, str)
			}
		}

		return compactNonEmptyStrings(out)
	case map[string]any:
		for _, key := range []string{metaOptionsKey, "values", "enum"} {
			if values := optionStringValues(value[key]); len(values) > 0 {
				return values
			}
		}
	}

	return nil
}

func compactNonEmptyStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}

	slices.Sort(out)

	return slices.Compact(out)
}

func unstableConfigOptions(options []acp.SessionConfigOption) []acp.UnstableSessionConfigOption {
	if len(options) == 0 {
		return nil
	}

	out := make([]acp.UnstableSessionConfigOption, 0, len(options))
	for _, option := range options {
		data, err := json.Marshal(option)
		if err != nil {
			continue
		}

		var unstable acp.UnstableSessionConfigOption
		if err := json.Unmarshal(data, &unstable); err == nil {
			out = append(out, unstable)
		}
	}

	return out
}

func titleASCII(value string) string {
	if value == "" {
		return ""
	}

	return strings.ToUpper(value[:1]) + value[1:]
}
