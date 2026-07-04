package opencodeacp

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/coder/acp-go-sdk"
)

func (a *Agent) SetSessionConfigOption(ctx context.Context, params acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	if params.Boolean != nil {
		return acp.SetSessionConfigOptionResponse{}, acp.NewInvalidParams(map[string]any{"error": "unsupported", "field": "value"})
	}
	if params.ValueId == nil {
		return acp.SetSessionConfigOptionResponse{}, acp.NewInvalidParams(map[string]any{"field": "value"})
	}
	session, err := a.session(params.ValueId.SessionId)
	if err != nil {
		return acp.SetSessionConfigOptionResponse{}, err
	}
	value := string(params.ValueId.Value)
	if value == "" {
		return acp.SetSessionConfigOptionResponse{}, acp.NewInvalidParams(map[string]any{"field": "value"})
	}
	switch params.ValueId.ConfigId {
	case configModel:
		if !session.hasConfigValue(ctx, configModel, value) {
			return acp.SetSessionConfigOptionResponse{}, acp.NewInvalidParams(map[string]any{"field": "value"})
		}
		session.setModel(value)
	case configMode:
		if !session.hasConfigValue(ctx, configMode, value) {
			return acp.SetSessionConfigOptionResponse{}, acp.NewInvalidParams(map[string]any{"field": "value"})
		}
		session.setMode(value)
	default:
		return acp.SetSessionConfigOptionResponse{}, acp.NewInvalidParams(map[string]any{"field": "configId"})
	}
	options := session.configOptions(ctx)
	_ = session.emitUpdate(ctx, acp.SessionUpdate{
		ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{ConfigOptions: options},
	})

	return acp.SetSessionConfigOptionResponse{ConfigOptions: options}, nil
}

func (s *session) hasConfigValue(ctx context.Context, configID acp.SessionConfigId, value string) bool {
	for _, option := range s.configOptions(ctx) {
		if option.Select == nil || option.Select.Id != configID {
			continue
		}
		if option.Select.Options.Ungrouped != nil {
			for _, item := range *option.Select.Options.Ungrouped {
				if string(item.Value) == value {
					return true
				}
			}
		}
		if option.Select.Options.Grouped != nil {
			for _, group := range *option.Select.Options.Grouped {
				for _, item := range group.Options {
					if string(item.Value) == value {
						return true
					}
				}
			}
		}
	}
	return false
}

func (s *session) configOptions(ctx context.Context) []acp.SessionConfigOption {
	snapshot := s.snapshot()
	if snapshot.client == nil {
		return nil
	}
	var options []acp.SessionConfigOption
	if providers, err := snapshot.client.ConfigProviders(ctx); err == nil {
		if model := modelConfigOption(snapshot, providers); model.Select != nil {
			options = append(options, model)
		}
	}
	if agents, err := snapshot.client.Agents(ctx); err == nil && len(agents) > 0 {
		if mode := modeConfigOption(snapshot, agents); mode.Select != nil {
			options = append(options, mode)
		}
	}
	return options
}

func modelConfigOption(snapshot sessionSnapshot, providers providersResponse) acp.SessionConfigOption {
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

func modeConfigOption(snapshot sessionSnapshot, agents []nativeAgent) acp.SessionConfigOption {
	category := acp.SessionConfigOptionCategoryMode
	current := firstNonEmpty(snapshot.mode, "build")
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
	foundCurrent := false
	for _, value := range values {
		if string(value.Value) == current {
			foundCurrent = true
			break
		}
	}
	if !foundCurrent {
		current = string(values[0].Value)
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

func modelMeta(providerID string, modelID string, model providerModel) map[string]any {
	meta := map[string]any{"modelId": providerID + "/" + modelID}
	if n, ok := intFromNumber(model.Limit["context"]); ok {
		meta["contextWindow"] = n
	}
	if n, ok := intFromNumber(model.Limit["output"]); ok {
		meta["maxOutputTokens"] = n
	}
	capabilities := modelCapabilities(model)
	if len(capabilities) > 0 {
		meta["capabilities"] = capabilities
	}
	efforts := supportedEfforts(model)
	if len(efforts) > 0 {
		meta["supportedEffortLevels"] = efforts
	}
	return meta
}

func modelCapabilities(model providerModel) []string {
	var caps []string
	if boolCapability(model.Capabilities, "reasoning") {
		caps = append(caps, "reasoning")
	}
	if boolCapability(model.Capabilities, "tool_call") || boolCapability(model.Capabilities, "toolcall") {
		caps = append(caps, "tools")
	}
	if input, _ := model.Capabilities["input"].(map[string]any); len(input) > 0 {
		for _, key := range []string{"image", "audio", "pdf", "video"} {
			if boolCapability(input, key) {
				caps = append(caps, key)
			}
		}
	}
	slices.Sort(caps)
	return slices.Compact(caps)
}

func boolCapability(values map[string]any, key string) bool {
	value, _ := values[key].(bool)
	return value
}

func supportedEfforts(model providerModel) []string {
	seen := map[string]struct{}{}
	for key, raw := range model.Options {
		if strings.Contains(strings.ToLower(key), "effort") {
			if value, ok := raw.(string); ok && value != "" {
				seen[value] = struct{}{}
			}
		}
	}
	for name, raw := range model.Variants {
		if strings.TrimSpace(name) != "" {
			seen[name] = struct{}{}
		}
		if value, ok := raw.(map[string]any); ok {
			if effort, _ := value["reasoningEffort"].(string); effort != "" {
				seen[effort] = struct{}{}
			}
			if reasoning, _ := value["reasoning"].(map[string]any); reasoning != nil {
				if effort, _ := reasoning["effort"].(string); effort != "" {
					seen[effort] = struct{}{}
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}
	slices.Sort(out)
	return out
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
