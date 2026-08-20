package opencodeacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
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

// TestUnknownModelReachesOpenCodeAndCarriesItsNativeError proves the adapter
// never judges a model name against the advertised catalog: a model that catalog
// does not list still opens a session, travels to the native runtime exactly as
// the host asked for it, and fails the turn with the words OpenCode itself used.
func TestUnknownModelReachesOpenCodeAndCarriesItsNativeError(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	client.createSession = testNativeSession("native-unknown-model")
	client.agents = []opencode.NativeAgent{{Name: "build"}}
	// One provider is resolved, and the host asks for a model outside it.
	client.providers = opencode.ProvidersResponse{Providers: []opencode.ProviderInfo{{
		ID:     "openai",
		Models: map[string]opencode.ProviderModel{"gpt-test": {ID: "gpt-test"}},
	}}}

	dispatched := make(chan opencode.MessageRequest, 1)
	client.dispatchMessage = func(_ context.Context, _ string, req opencode.MessageRequest) (opencode.NativeMessage, error) {
		dispatched <- req

		// An acknowledged frame with no message leaves the turn open, so the
		// native error below is what settles it.
		return opencode.NativeMessage{}, nil
	}

	agent := NewAgent(WithHome(t.TempDir()), WithLogger(slog.New(slog.DiscardHandler)), func(options *Options) {
		options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
			return client, nil
		}
	})
	agent.setAgentClient(newRecordingAgentClient())

	created, err := agent.NewSession(ctx, NewSessionRequest(t.TempDir(),
		WithSessionOpenCodeOptions(NewOpenCodeOptions(WithOpenCodeModel("anthropic/claude-sonnet-4-6"))),
	))
	require.NoError(t, err, "an unlisted model must not refuse session creation")

	done := make(chan error, 1)

	go func() {
		_, promptErr := agent.Prompt(ctx, TextPromptRequest(created.SessionId, "unknown-model", "hello"))
		done <- promptErr
	}()

	select {
	case req := <-dispatched:
		require.NotNil(t, req.Model, "the frame must name the model the host chose")
		require.Equal(t, "anthropic", req.Model.ProviderID)
		require.Equal(t, "claude-sonnet-4-6", req.Model.ModelID)
	case <-time.After(2 * time.Second):
		t.Fatal("the unlisted model never reached OpenCode")
	}

	// The answer OpenCode gives a model it cannot resolve: one session.error that
	// names the model, then the idle that ends the turn.
	nativeErr := &opencode.NativeError{Name: "UnknownError"}
	nativeErr.Data.Message = "Model not found: anthropic/claude-sonnet-4-6. Did you mean: claude-sonnet-4-6?"

	client.events <- opencode.Event{
		Type: opencode.EventSessionError,
		Properties: mustJSON(t, opencode.SessionError{
			SessionID: "native-unknown-model",
			Error:     nativeErr,
		}),
	}
	client.publishSessionIdle("native-unknown-model")

	select {
	case promptErr := <-done:
		data := assertTurnFailed(t, promptErr, causeProvider,
			"Model not found: anthropic/claude-sonnet-4-6. Did you mean: claude-sonnet-4-6?")
		// The native error names no status and no provider code, and the adapter
		// invents neither.
		if _, present := data[jsonFieldStatusCode]; present {
			t.Fatalf("status code was invented: %#v", data)
		}
		if _, present := data[jsonFieldProviderCode]; present {
			t.Fatalf("provider code was invented: %#v", data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("prompt did not return the native model error")
	}
}

// TestConfigOptionCatalogFailureIsReported proves an advertisement failure is
// never silent: the option whose native read failed is left out, every other
// option still stands, and the omission states its cause.
func TestConfigOptionCatalogFailureIsReported(t *testing.T) {
	ctx := context.Background()

	var logs bytes.Buffer

	agent := NewAgent(WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	client := newFakeOpenCodeClient()
	client.providersErr = errors.New("providers unreachable")
	client.agents = []opencode.NativeAgent{{Name: "build"}}
	sess := testSession(t, agent, client)

	options := sess.configOptions(ctx)
	require.Len(t, options, 1, "a failed catalog read must not remove the options it did not feed")
	require.Equal(t, acp.SessionConfigId(configMode), options[0].Select.Id)

	record := logs.String()
	require.Contains(t, record, "OpenCode config option is unavailable")
	require.Contains(t, record, `"config_id":"model"`)
	require.Contains(t, record, "providers unreachable")

	// The rule is per option, not per catalog: the agent read states its own
	// failure the same way.
	logs.Reset()

	modeless := newFakeOpenCodeClient()
	modeless.providers = testProviders()
	modeless.agentsErr = errors.New("agents unreachable")
	modelessSession := testSession(t, agent, modeless)

	options = modelessSession.configOptions(ctx)
	require.Len(t, options, 1)
	require.Equal(t, acp.SessionConfigId(configModel), options[0].Select.Id)

	record = logs.String()
	require.Contains(t, record, `"config_id":"mode"`)
	require.Contains(t, record, "agents unreachable")
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
