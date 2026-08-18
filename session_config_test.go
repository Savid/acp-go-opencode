package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

func TestModelConfigOptionMetadataMapping(t *testing.T) {
	providers := opencode.ProvidersResponse{Providers: []opencode.ProviderInfo{{
		ID:   "openai",
		Name: "OpenAI",
		Models: map[string]opencode.ProviderModel{
			"gpt-test": {
				ID:   "gpt-test",
				Name: "GPT Test",
				Limit: map[string]any{
					"context": float64(1000),
					"output":  float64(200),
				},
				Reasoning:    true,
				ToolCall:     true,
				Capabilities: &opencode.ProviderModelCapabilities{Input: opencode.ProviderModelInputCapabilities{Image: boolPtr(true)}},
				Options: map[string]any{
					"reasoningEffort": map[string]any{"options": []any{"low", "medium"}},
				},
			},
		},
	}}}
	option := modelConfigOption(sessionSnapshot{}, providers)
	if option.Select == nil {
		t.Fatal("missing select option")
	}
	if option.Select.Id != configModel {
		t.Fatalf("id = %s", option.Select.Id)
	}
	if option.Select.Category == nil || *option.Select.Category != acp.SessionConfigOptionCategoryModel {
		t.Fatalf("category = %#v", option.Select.Category)
	}
	group := (*option.Select.Options.Grouped)[0]
	value := group.Options[0]
	if value.Value != "openai/gpt-test" {
		t.Fatalf("value = %s", value.Value)
	}
	meta, ok := value.Meta[opencodeMetaKey].(map[string]any)
	if !ok {
		t.Fatalf("meta missing opencode key: %#v", value.Meta)
	}
	if meta["contextWindow"] != 1000 || meta["maxOutputTokens"] != 200 {
		t.Fatalf("limits meta = %#v", meta)
	}
	if got := meta["modelId"]; got != "openai/gpt-test" {
		t.Fatalf("modelId = %#v", got)
	}
	if got, present := meta["capabilities"]; present {
		t.Fatalf("capabilities meta unexpectedly present: %#v", got)
	}
	if got := meta["supportedEffortLevels"]; !containsStringAny(got, "low") || !containsStringAny(got, "medium") {
		t.Fatalf("effort meta = %#v", got)
	}
}

func TestSessionConfigBranchesAndValidation(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	client.providers = opencode.ProvidersResponse{Providers: []opencode.ProviderInfo{
		{ID: "", Models: map[string]opencode.ProviderModel{"skip": {}}},
		{ID: "p", Models: map[string]opencode.ProviderModel{
			"m": {
				Limit:   map[string]any{"context": int(42), "output": json.Number("7")},
				Options: map[string]any{"reasoningEffort": []any{"medium"}},
			},
		}},
	}}
	client.agents = []opencode.NativeAgent{
		{Name: "", Mode: ""},
		{Name: "build", Description: "Build"},
		{Name: "build", Description: "Duplicate"},
		{Mode: "plan"},
	}
	agent := NewAgent()
	conn := newRecordingAgentClient()
	agent.setAgentClient(conn)
	sess := testSession(t, agent, client)
	agent.mu.Lock()
	agent.sessions[sess.id] = sess
	agent.mu.Unlock()

	if options := (&session{agent: agent}).configOptions(ctx); options != nil {
		t.Fatalf("nil client config options = %#v", options)
	}
	if !sess.hasConfigValue(ctx, configModel, "p/m") {
		t.Fatal("known model config value was not found")
	}
	if sess.hasConfigValue(ctx, configModel, "missing/model") {
		t.Fatal("missing model config value was found")
	}
	fallbackClient := newFakeOpenCodeClient()
	fallbackClient.providers = opencode.ProvidersResponse{}
	fallbackSession := testSession(t, agent, fallbackClient)
	if !fallbackSession.hasConfigValue(ctx, configModel, "openai/gpt-test") {
		t.Fatal("fallback model config value was not found")
	}
	if err := fallbackSession.validateModel(ctx, "", "model"); err != nil {
		t.Fatalf("empty model validation = %v", err)
	}
	if err := validateModel(ctx, nil, "openai/gpt-test", "model"); err != nil {
		t.Fatalf("nil client model validation = %v", err)
	}
	errorClient := newFakeOpenCodeClient()
	errorClient.providersErr = errors.New("providers failed")
	errorSession := testSession(t, agent, errorClient)
	if err := errorSession.validateModel(ctx, "openai/gpt-test", "model"); err == nil {
		t.Fatal("provider catalog error was ignored")
	}
	if testProviders().HasModel("gpt-test") {
		t.Fatal("provider-less model was accepted")
	}
	assertSetSessionConfigOptionBranches(t, ctx, agent, sess, conn)
	assertConfigOptionBuilders(t, client)
}

func assertSetSessionConfigOptionBranches(t *testing.T, ctx context.Context, agent *Agent, sess *session, conn *recordingAgentClient) {
	t.Helper()
	// Every refusal on this method carries the uniform two-key rejection, so a
	// host reads one token rather than matching prose per sibling.
	unsupportedValue := map[string]any{jsonFieldError: errValueUnsupported, jsonFieldField: jsonFieldValue}

	_, err := agent.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{})
	requireInvalidParamsData(t, err, unsupportedValue)

	_, err = agent.SetSessionConfigOption(ctx, SetConfigOptionRequest("missing", configModel, "p/m"))
	requireInvalidParamsData(t, err, map[string]any{
		jsonFieldError: errValueSessionUnknown, jsonFieldField: jsonFieldSessionID,
	})

	_, err = agent.SetSessionConfigOption(ctx, SetConfigOptionRequest(sess.id, configModel, ""))
	requireInvalidParamsData(t, err, unsupportedValue)

	_, err = agent.SetSessionConfigOption(ctx, SetConfigOptionRequest(sess.id, "unknown", "x"))
	requireInvalidParamsData(t, err, map[string]any{
		jsonFieldError: errValueUnsupported, jsonFieldField: "configId",
	})

	_, err = agent.SetSessionConfigOption(ctx, SetConfigOptionRequest(sess.id, configModel, "missing/model"))
	requireInvalidParamsData(t, err, map[string]any{
		jsonFieldError: "invalid_model", jsonFieldField: jsonFieldValue, configModel: "missing/model",
	})

	_, err = agent.SetSessionConfigOption(ctx, SetConfigOptionRequest(sess.id, configMode, "missing"))
	requireInvalidParamsData(t, err, unsupportedValue)
	if _, err := agent.SetSessionConfigOption(ctx, SetConfigOptionRequest(sess.id, configModel, "p/m")); err != nil {
		t.Fatalf("set model: %v", err)
	}
	if conn.updateCount() == 0 {
		t.Fatal("set model did not emit config update")
	}
}

func assertConfigOptionBuilders(t *testing.T, client *fakeOpenCodeClient) {
	t.Helper()
	fallback := modelConfigOption(sessionSnapshot{providerID: "p", modelID: "m"}, opencode.ProvidersResponse{})
	if fallback.Select == nil || fallback.Select.Options.Ungrouped == nil || fallback.Select.CurrentValue != "p/m" {
		t.Fatalf("fallback model option = %#v", fallback)
	}
	if empty := modelConfigOption(sessionSnapshot{}, opencode.ProvidersResponse{}); empty.Select != nil {
		t.Fatalf("empty model option = %#v", empty)
	}
	mode := modeConfigOption(sessionSnapshot{mode: "missing"}, client.agents)
	if mode.Select == nil || len(*mode.Select.Options.Ungrouped) != 2 || mode.Select.CurrentValue != "build" {
		t.Fatalf("mode option = %#v", mode)
	}
	if empty := modeConfigOption(sessionSnapshot{}, []opencode.NativeAgent{{}}); empty.Select != nil {
		t.Fatalf("empty mode option = %#v", empty)
	}
	efforts := supportedEfforts(client.providers.Providers[1].Models["m"])
	if len(efforts) != 1 || efforts[0] != "medium" {
		t.Fatalf("supportedEfforts = %#v", efforts)
	}
	efforts = supportedEfforts(opencode.ProviderModel{Options: map[string]any{
		"temperature":     []any{"ignored"},
		"reasoningEffort": []string{"low", "", "high"},
		"effortOptions":   map[string]any{"values": []any{"medium"}},
	}})
	if len(efforts) != 3 || efforts[0] != "high" || efforts[1] != "low" || efforts[2] != "medium" {
		t.Fatalf("normalized efforts = %#v", efforts)
	}
	if values := optionStringValues(map[string]any{"unknown": []any{"x"}}); values != nil {
		t.Fatalf("unknown option values = %#v", values)
	}
	if values := optionStringValues(42); values != nil {
		t.Fatalf("numeric option values = %#v", values)
	}
	if unstableConfigOptions(nil) != nil {
		t.Fatal("empty unstable config options returned non-nil")
	}
	if got := unstableConfigOptions([]acp.SessionConfigOption{{
		Select: &acp.SessionConfigOptionSelect{
			Type:         "select",
			Id:           "bad",
			Name:         "Bad",
			CurrentValue: "bad",
			Meta:         map[string]any{"bad": func() {}},
		},
	}}); len(got) != 0 {
		t.Fatalf("bad unstable config option was not skipped: %#v", got)
	}
}

func containsStringAny(value any, want string) bool {
	values, _ := value.([]string)
	for _, value := range values {
		if value == want {
			return true
		}
	}
	anyValues, _ := value.([]any)
	for _, value := range anyValues {
		if value == want {
			return true
		}
	}

	return false
}

func TestTitleASCII(t *testing.T) {
	if titleASCII("") != "" || titleASCII("plan") != "Plan" {
		t.Fatal("titleASCII mismatch")
	}
}
