package opencodeacp

import (
	"context"
	"net/http"
	"slices"
	"sort"
	"strings"

	acp "github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-core/wire"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

const configModel acp.SessionConfigId = "model"
const configMode acp.SessionConfigId = "mode"
const configEffort acp.SessionConfigId = "effort"

// refreshCatalogs fetches the model, agent, and command catalogs this session
// advertises.
func (s *session) refreshCatalogs(ctx context.Context, rt *binding) error {
	var models opencode.Catalog
	if err := rt.client.Do(ctx, s.cwd, http.MethodGet, "/config/providers", nil, &models); err != nil {
		return err
	}

	var agents []opencode.NativeAgent
	if err := rt.client.Do(ctx, s.cwd, http.MethodGet, "/agent", nil, &agents); err != nil {
		return err
	}

	var commands []opencode.NativeCommand
	if err := rt.client.Do(ctx, s.cwd, http.MethodGet, nativeCommandPath, nil, &commands); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.models = models
	s.agents = agents
	s.commands = commands

	return nil
}
func (s *session) modelImageCapability() *bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	provider, model, _ := strings.Cut(s.model, "/")
	for _, p := range s.models.Providers {
		if p.ID == provider {
			if value, ok := p.Models[model]; ok && value.Capabilities != nil {
				return value.Capabilities.Input.Image
			}
		}
	}

	return nil
}
func (s *session) configOptions() []acp.SessionConfigOption {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows := make([]wire.ModelRow, 0, len(s.models.Providers))

	for _, p := range s.models.Providers {
		keys := make([]string, 0, len(p.Models))
		for k := range p.Models {
			keys = append(keys, k)
		}

		sort.Strings(keys)

		for _, key := range keys {
			m := p.Models[key]
			id := p.ID + "/" + key

			variants := make([]string, 0, len(m.Variants))
			for v := range m.Variants {
				variants = append(variants, v)
			}

			sort.Strings(variants)

			meta := map[string]any{}
			if len(variants) > 0 {
				meta["supportedEffortLevels"] = variants
			}

			if window, ok := m.Limit["context"]; ok {
				meta["contextWindow"] = window
			}

			rows = append(rows, wire.ModelRow{ID: id, Name: p.Name + " / " + m.Name, Meta: meta})
		}
	}

	models := wire.ModelSelectOptions(vendor, s.model, rows, s.agent.options.ConfiguredModels)

	options := []acp.SessionConfigOption{}

	selectOption := func(id acp.SessionConfigId, name, current string, category acp.SessionConfigOptionCategory, values acp.SessionConfigSelectOptionsUngrouped) {
		options = append(options, acp.SessionConfigOption{Select: &acp.SessionConfigOptionSelect{Id: id, Name: name, Type: "select", Category: &category, CurrentValue: acp.SessionConfigValueId(current), Options: acp.SessionConfigSelectOptions{Ungrouped: &values}}})
	}
	if len(models) > 0 {
		selectOption(configModel, "Model", s.model, acp.SessionConfigOptionCategoryModel, models)
	}

	modes := acp.SessionConfigSelectOptionsUngrouped{}
	found := false

	for _, a := range s.agents {
		if a.Mode == "subagent" {
			continue
		}

		modes = append(modes, acp.SessionConfigSelectOption{Value: acp.SessionConfigValueId(a.Name), Name: a.Name})
		found = found || a.Name == s.mode
	}

	if !found && s.mode != "" {
		modes = append(modes, acp.SessionConfigSelectOption{Value: acp.SessionConfigValueId(s.mode), Name: s.mode})
	}

	if len(modes) > 0 {
		selectOption(configMode, "Agent", s.mode, acp.SessionConfigOptionCategoryMode, modes)
	}

	if s.effort != "" {
		values := []string{s.effort}

		p, m, _ := strings.Cut(s.model, "/")
		for _, provider := range s.models.Providers {
			if provider.ID == p {
				for key := range provider.Models[m].Variants {
					if !slices.Contains(values, key) {
						values = append(values, key)
					}
				}
			}
		}

		sort.Strings(values)

		efforts := make(acp.SessionConfigSelectOptionsUngrouped, 0, len(values))
		for _, v := range values {
			efforts = append(efforts, acp.SessionConfigSelectOption{Value: acp.SessionConfigValueId(v), Name: v})
		}

		selectOption(configEffort, "Effort", s.effort, acp.SessionConfigOptionCategoryThoughtLevel, efforts)
	}

	return options
}
func (s *session) setConfigOption(ctx context.Context, id acp.SessionConfigId, value string) ([]acp.SessionConfigOption, error) {
	if id != configModel && id != configMode && id != configEffort {
		return nil, wire.Unsupported("configId")
	}

	if value == "" || (id == configModel && validateOptionalModel(value) != nil) {
		return nil, wire.Unsupported(fieldValue)
	}

	if err := s.admissionError(); err != nil {
		return nil, err
	}

	release, err := s.acquireGate(limitSessionPrompt)
	if err != nil {
		return nil, err
	}
	defer release()

	s.mu.Lock()
	busy := s.cycle != nil
	s.mu.Unlock()

	if busy {
		return nil, wire.Backpressure(limitSessionPrompt)
	}

	rt, err := s.ensureRuntime(ctx)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	switch id {
	case configModel:
		s.model = value
	case configMode:
		s.mode = value
	case configEffort:
		s.effort = value
	}
	s.mu.Unlock()

	if err := s.commitMirror(ctx, rt); err != nil {
		return nil, wire.InternalFailure(vendor, "")
	}

	options := s.configOptions()
	_ = s.emit(ctx, acp.SessionUpdate{ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{ConfigOptions: options}})

	return options, nil
}
func (s *session) emitCommands(ctx context.Context) error {
	s.mu.Lock()
	native := slices.Clone(s.commands)
	s.mu.Unlock()

	commands := []acp.AvailableCommand{}

	seen := map[string]bool{}
	for _, c := range native {
		if !wire.ValidCommandName(c.Name) || seen[c.Name] {
			continue
		}

		seen[c.Name] = true

		command := acp.AvailableCommand{Name: c.Name, Description: c.Description}
		if len(c.Hints) > 0 {
			command.Input = &acp.AvailableCommandInput{Unstructured: &acp.UnstructuredCommandInput{Hint: strings.Join(c.Hints, " ")}}
		}

		commands = append(commands, command)
	}

	return s.emit(ctx, acp.SessionUpdate{AvailableCommandsUpdate: &acp.SessionAvailableCommandsUpdate{AvailableCommands: commands}})
}
