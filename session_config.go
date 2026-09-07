package opencodeacp

import (
	"cmp"
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
	case configEffort:
		session.setEffort(effortVariant(value))
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

	// The effort select is built from the same catalog read as the model
	// select: a model's presets live on its catalog entry, so one read feeds
	// both and a failed read withholds both.
	if providers, err := snapshot.client.ConfigProviders(ctx); err != nil {
		s.reportConfigOptionUnavailable(ctx, configModel, err)
	} else {
		if model := modelConfigOption(snapshot, providers); model.Select != nil {
			options = append(options, model)
		}

		if effort := effortConfigOption(snapshot, providers); effort.Select != nil {
			options = append(options, effort)
		}
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

	if levels := effortLevels(model); len(levels) > 0 {
		meta["supportedEffortLevels"] = levels
	}

	return meta
}

// effortLevels names the model's request presets from least to most reasoning.
// OpenCode keys a reasoning model's presets by effort name and a prompt selects
// one by that key, so the keys are the effort levels the model supports.
func effortLevels(model opencode.ProviderModel) []string {
	levels := make([]string, 0, len(model.Variants))

	for name := range model.Variants {
		if name != "" {
			levels = append(levels, name)
		}
	}

	slices.SortFunc(levels, compareEffort)

	return levels
}

// effortOrder ranks the preset names OpenCode's catalog uses. A name outside
// it is still a preset the model declares; it sorts after the ranked ones,
// alphabetically.
var effortOrder = map[string]int{
	"none": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6, //nolint:goconst // "high" is a preset name here, not the todo priority the existing constant names.
}

func compareEffort(a, b string) int {
	rankA, knownA := effortOrder[a]
	rankB, knownB := effortOrder[b]

	switch {
	case knownA && knownB:
		return cmp.Compare(rankA, rankB)
	case knownA:
		return -1
	case knownB:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

// effortConfigOption advertises the current model's presets as the effort
// select. A model the catalog gives no presets advertises nothing, because
// there is no level a frame could carry. The first value names OpenCode's
// no-variant state, in which the model runs its own default; as with mode, the
// current value is what the session will send, listed or not.
func effortConfigOption(snapshot sessionSnapshot, providers opencode.ProvidersResponse) acp.SessionConfigOption {
	model, ok := catalogModel(providers, snapshot.providerID, snapshot.modelID)
	if !ok {
		return acp.SessionConfigOption{}
	}

	levels := effortLevels(model)
	if len(levels) == 0 {
		return acp.SessionConfigOption{}
	}

	category := acp.SessionConfigOptionCategoryThoughtLevel
	values := make(acp.SessionConfigSelectOptionsUngrouped, 0, len(levels)+1)
	values = append(values, acp.SessionConfigSelectOption{Name: effortDefaultName, Value: effortDefault})

	for _, level := range levels {
		values = append(values, acp.SessionConfigSelectOption{
			Name:  titleASCII(level),
			Value: acp.SessionConfigValueId(level),
		})
	}

	return acp.SessionConfigOption{Select: &acp.SessionConfigOptionSelect{
		Id:           configEffort,
		Name:         "Effort",
		Category:     &category,
		Type:         configTypeSelect,
		CurrentValue: acp.SessionConfigValueId(firstNonEmpty(snapshot.variant, effortDefault)),
		Options:      acp.SessionConfigSelectOptions{Ungrouped: &values},
	}}
}

// catalogModel finds the session's model in the authenticated catalog under
// the same provider id and model id the model select advertises it by.
func catalogModel(providers opencode.ProvidersResponse, providerID string, modelID string) (opencode.ProviderModel, bool) {
	if providerID == "" || modelID == "" {
		return opencode.ProviderModel{}, false
	}

	for _, provider := range providers.Providers {
		if provider.ID != providerID {
			continue
		}

		for key, model := range provider.Models {
			if firstNonEmpty(model.ID, key) == modelID {
				return model, true
			}
		}
	}

	return opencode.ProviderModel{}, false
}

// effortVariant maps a host's effort value onto the variant a frame carries.
// The default value clears it; any other travels to the runtime unjudged, as
// model and mode do.
func effortVariant(value string) string {
	if value == effortDefault {
		return ""
	}

	return value
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
