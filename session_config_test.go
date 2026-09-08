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
				Capabilities: &opencode.ProviderModelCapabilities{Input: opencode.ProviderModelInputCapabilities{Image: new(true)}},
				Variants: map[string]map[string]any{
					"medium": {"reasoningEffort": "medium"},
					"low":    {"reasoningEffort": "low"},
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
	// The levels are the model's preset names in effort order, never the
	// map's iteration order.
	require.Equal(t, []string{"low", "medium"}, meta["supportedEffortLevels"])
}

func TestSessionConfigBranchesAndValidation(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient(t)
	client.providers = opencode.ProvidersResponse{Providers: []opencode.ProviderInfo{
		{ID: "", Models: map[string]opencode.ProviderModel{"skip": {}}},
		{ID: "p", Models: map[string]opencode.ProviderModel{
			"m": {
				Limit:    map[string]any{"context": int(42), "output": json.Number("7")},
				Variants: map[string]map[string]any{"medium": {"reasoningEffort": "medium"}},
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
	assertSetSessionConfigOptionBranches(t, ctx, agent, sess, conn)
	assertConfigOptionBuilders(t, client)
}

func assertSetSessionConfigOptionBranches(t *testing.T, ctx context.Context, agent *Agent, sess *session, conn *recordingAgentClient) {
	t.Helper()
	// Every refusal on this method carries the uniform two-key rejection, so a
	// host reads one token rather than matching prose per sibling.
	unsupportedValue := map[string]any{jsonFieldError: valUnsupported, jsonFieldField: jsonFieldValue}

	_, err := agent.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{})
	requireInvalidParamsData(t, err, unsupportedValue)

	_, err = agent.SetSessionConfigOption(ctx, SetConfigOptionRequest("missing", configModel, "p/m"))
	requireInvalidParamsData(t, err, map[string]any{
		jsonFieldError: valSessionUnknown, jsonFieldField: jsonFieldSessionID,
	})

	_, err = agent.SetSessionConfigOption(ctx, SetConfigOptionRequest(sess.id, configModel, ""))
	requireInvalidParamsData(t, err, unsupportedValue)

	_, err = agent.SetSessionConfigOption(ctx, SetConfigOptionRequest(sess.id, "unknown", "x"))
	requireInvalidParamsData(t, err, map[string]any{
		jsonFieldError: valUnsupported, jsonFieldField: "configId",
	})

	set, err := agent.SetSessionConfigOption(ctx, SetConfigOptionRequest(sess.id, configModel, "p/m"))
	if err != nil {
		t.Fatalf("set model: %v", err)
	}
	if conn.updateCount() == 0 {
		t.Fatal("set model did not emit config update")
	}
	// Selecting a model with presets advertises its effort select, starting on
	// the no-variant state.
	require.Equal(t, acp.SessionConfigValueId(effortDefault),
		requireConfigOption(t, set.ConfigOptions, configEffort).CurrentValue)

	// The effort door is the model door: the value is stored and advertised
	// as current, listed or not, and the default value clears it again.
	set, err = agent.SetSessionConfigOption(ctx, SetConfigOptionRequest(sess.id, configEffort, "xhigh"))
	require.NoError(t, err)
	require.Equal(t, acp.SessionConfigValueId("xhigh"),
		requireConfigOption(t, set.ConfigOptions, configEffort).CurrentValue)
	require.Equal(t, "xhigh", sess.currentVariant())

	set, err = agent.SetSessionConfigOption(ctx, SetConfigOptionRequest(sess.id, configEffort, effortDefault))
	require.NoError(t, err)
	require.Equal(t, acp.SessionConfigValueId(effortDefault),
		requireConfigOption(t, set.ConfigOptions, configEffort).CurrentValue)
	require.Empty(t, sess.currentVariant())
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
	// A mode the list does not carry is still the mode this session uses, so it
	// is what the advertisement names as current.
	mode := modeConfigOption(sessionSnapshot{mode: "missing"}, client.agents)
	if mode.Select == nil || len(*mode.Select.Options.Ungrouped) != 2 || mode.Select.CurrentValue != "missing" {
		t.Fatalf("mode option = %#v", mode)
	}
	if named := modeConfigOption(sessionSnapshot{}, client.agents); named.Select.CurrentValue != defaultMode {
		t.Fatalf("unnamed mode option = %#v", named)
	}
	if empty := modeConfigOption(sessionSnapshot{}, []opencode.NativeAgent{{}}); empty.Select != nil {
		t.Fatalf("empty mode option = %#v", empty)
	}
	require.Equal(t, []string{"medium"}, effortLevels(client.providers.Providers[1].Models["m"]))
	// Ranked names sort by effort, an unranked preset sorts after them, and an
	// empty name is not a preset.
	require.Equal(t, []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "custom", "deep"},
		effortLevels(opencode.ProviderModel{Variants: map[string]map[string]any{
			"deep": {}, "max": {}, "high": {}, "": {}, "custom": {}, "none": {},
			"low": {}, "xhigh": {}, "medium": {}, "minimal": {},
		}}))
	require.Empty(t, effortLevels(opencode.ProviderModel{}))

	// The effort select follows the session's model: a preset-less or unknown
	// model advertises none, a model with presets advertises them behind the
	// default value, and the current value is the session's own, listed or not.
	effort := effortConfigOption(sessionSnapshot{providerID: "p", modelID: "m"}, client.providers)
	require.NotNil(t, effort.Select)
	require.Equal(t, acp.SessionConfigId(configEffort), effort.Select.Id)
	require.Equal(t, acp.SessionConfigOptionCategoryThoughtLevel, *effort.Select.Category)
	require.Equal(t, acp.SessionConfigValueId(effortDefault), effort.Select.CurrentValue)
	require.Equal(t, acp.SessionConfigSelectOptionsUngrouped{
		{Name: effortDefaultName, Value: effortDefault},
		{Name: "Medium", Value: "medium"},
	}, *effort.Select.Options.Ungrouped)
	require.Equal(t, acp.SessionConfigValueId("unlisted"),
		effortConfigOption(sessionSnapshot{providerID: "p", modelID: "m", variant: "unlisted"}, client.providers).Select.CurrentValue)
	require.Nil(t, effortConfigOption(sessionSnapshot{providerID: "p", modelID: "other"}, client.providers).Select)
	require.Nil(t, effortConfigOption(sessionSnapshot{providerID: "p", modelID: "m"}, opencode.ProvidersResponse{}).Select)
	require.Nil(t, effortConfigOption(sessionSnapshot{}, client.providers).Select)
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

// TestUnknownModelReachesOpenCodeAndFailsClosedWithoutExactAssistant proves the adapter
// never judges a model name against the advertised catalog: a model that catalog
// does not list still opens a session and travels to the native runtime exactly as
// the host asked for it, but no unrelated native error can replace the missing
// request-specific assistant identity.
func TestUnknownModelReachesOpenCodeAndFailsClosedWithoutExactAssistant(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient(t)
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
	establishCreatedSession(t, agent, created.SessionId)

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

	event := opencode.Event{
		Type: opencode.EventSessionError,
		Properties: mustJSON(t, opencode.SessionError{
			SessionID: "native-unknown-model",
			Error:     nativeErr,
		}),
	}
	client.publishEvent(event)
	client.publishSessionIdle("native-unknown-model")

	select {
	case promptErr := <-done:
		// A native failure that produced no assistant step is a provider
		// refusal in the one contracted turn-failure envelope, never an
		// adapter-symptom token of its own.
		assertTurnFailed(t, promptErr, causeProvider, "")
	case <-time.After(2 * time.Second):
		t.Fatal("prompt did not return the native model error")
	}
}

// nativeAgentNotFoundMessage is what a real `opencode serve` publishes for an
// agent name it does not know. The async prompt route still answers 204, and the
// session stream then carries this error naming the agents it does know.
const nativeAgentNotFoundMessage = `Agent not found: "does-not-exist-mode". Available agents: build, explore, general, plan`

// TestUnadvertisedModeReachesOpenCodeAndFailsClosedWithoutExactAssistant proves the mode
// door is no narrower than the model one. A mode the advertisement does not list
// is set rather than refused, it is what the session then advertises as current,
// and it travels to the native runtime on the frame's own agent field, but no
// unrelated native error can replace the missing request-specific assistant.
func TestUnadvertisedModeReachesOpenCodeAndFailsClosedWithoutExactAssistant(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient(t)
	client.createSession = testNativeSession("native-unknown-mode")
	client.providers = testProviders()
	// The advertisement carries one agent, and the host asks for another.
	client.agents = []opencode.NativeAgent{{Name: "build"}}

	dispatched := make(chan opencode.MessageRequest, 1)
	client.dispatchMessage = func(_ context.Context, _ string, req opencode.MessageRequest) (opencode.NativeMessage, error) {
		dispatched <- req

		// The native route acknowledges the frame and publishes nothing further,
		// so the errors below are what settle the turn.
		return opencode.NativeMessage{}, nil
	}

	agent := NewAgent(WithHome(t.TempDir()), WithLogger(slog.New(slog.DiscardHandler)), func(options *Options) {
		options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
			return client, nil
		}
	})
	agent.setAgentClient(newRecordingAgentClient())

	created, err := agent.NewSession(ctx, NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	establishCreatedSession(t, agent, created.SessionId)

	set, err := agent.SetSessionConfigOption(ctx,
		SetConfigOptionRequest(created.SessionId, configMode, "does-not-exist-mode"))
	require.NoError(t, err, "an unlisted mode must be set, not refused")
	require.Equal(t, acp.SessionConfigValueId("does-not-exist-mode"),
		requireConfigOption(t, set.ConfigOptions, configMode).CurrentValue,
		"the advertisement must name the mode the session will actually use")

	done := make(chan error, 1)

	go func() {
		_, promptErr := agent.Prompt(ctx, TextPromptRequest(created.SessionId, "unknown-mode", "hello"))
		done <- promptErr
	}()

	select {
	case req := <-dispatched:
		require.Equal(t, "does-not-exist-mode", req.Agent, "the frame must name the mode the host chose")
	case <-time.After(2 * time.Second):
		t.Fatal("the unlisted mode never reached OpenCode")
	}

	// The answer OpenCode gives an agent it cannot resolve: the readable refusal
	// first, then a second error carrying only the native stack. No idle follows
	// — the failure precedes the run that would have published one — so the
	// readable message is the whole of what the turn may report.
	notFound := &opencode.NativeError{Name: "UnknownError"}
	notFound.Data.Message = nativeAgentNotFoundMessage

	trace := &opencode.NativeError{Name: "UnknownError"}
	trace.Data.Message = "UnknownError: UnknownError\n    at SessionPrompt.createUserMessage"

	for _, nativeErr := range []*opencode.NativeError{notFound, trace} {
		event := opencode.Event{
			Type: opencode.EventSessionError,
			Properties: mustJSON(t, opencode.SessionError{
				SessionID: "native-unknown-mode",
				Error:     nativeErr,
			}),
		}
		client.publishEvent(event)
	}

	select {
	case promptErr := <-done:
		// A native failure that produced no assistant step is a provider
		// refusal in the one contracted turn-failure envelope, never an
		// adapter-symptom token of its own.
		assertTurnFailed(t, promptErr, causeProvider, "")
	case <-time.After(5 * time.Second):
		t.Fatal("prompt did not return the native agent error")
	}
}

// TestUnknownModelSetThroughConfigOptionReachesOpenCode proves the model door is
// one door however it is opened: a model outside the advertised catalog set
// through `session/set_config_option` travels to the native runtime on the next
// frame exactly as the session-start route's does.
func TestUnknownModelSetThroughConfigOptionReachesOpenCode(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient(t)
	client.createSession = testNativeSession("native-config-model")
	client.agents = []opencode.NativeAgent{{Name: "build"}}
	client.providers = opencode.ProvidersResponse{Providers: []opencode.ProviderInfo{{
		ID:     "openai",
		Models: map[string]opencode.ProviderModel{"gpt-test": {ID: "gpt-test"}},
	}}}

	dispatched := make(chan opencode.MessageRequest, 1)
	client.dispatchMessage = func(_ context.Context, _ string, req opencode.MessageRequest) (opencode.NativeMessage, error) {
		dispatched <- req

		return opencode.NativeMessage{}, nil
	}

	agent := NewAgent(WithHome(t.TempDir()), WithLogger(slog.New(slog.DiscardHandler)), func(options *Options) {
		options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
			return client, nil
		}
	})
	agent.setAgentClient(newRecordingAgentClient())

	created, err := agent.NewSession(ctx, NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	establishCreatedSession(t, agent, created.SessionId)

	set, err := agent.SetSessionConfigOption(ctx,
		SetConfigOptionRequest(created.SessionId, configModel, "anthropic/claude-sonnet-4-6"))
	require.NoError(t, err, "an unlisted model must be set, not refused")
	require.Equal(t, acp.SessionConfigValueId("anthropic/claude-sonnet-4-6"),
		requireConfigOption(t, set.ConfigOptions, configModel).CurrentValue)

	done := make(chan error, 1)

	go func() {
		_, promptErr := agent.Prompt(ctx, TextPromptRequest(created.SessionId, "config-model", "hello"))
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

	nativeErr := &opencode.NativeError{Name: "UnknownError"}
	nativeErr.Data.Message = "Model not found: anthropic/claude-sonnet-4-6. Did you mean: claude-sonnet-4-6?"

	event := opencode.Event{
		Type: opencode.EventSessionError,
		Properties: mustJSON(t, opencode.SessionError{
			SessionID: "native-config-model",
			Error:     nativeErr,
		}),
	}
	client.publishEvent(event)
	client.publishSessionIdle("native-config-model")

	select {
	case promptErr := <-done:
		// A native failure that produced no assistant step is a provider
		// refusal in the one contracted turn-failure envelope, never an
		// adapter-symptom token of its own.
		assertTurnFailed(t, promptErr, causeProvider, "")
	case <-time.After(5 * time.Second):
		t.Fatal("prompt did not return the native model error")
	}
}

// TestSetModeSurvivesAgentCatalogFailure proves what a failed `/agent` read now
// means for setting a mode: nothing. No read stands between the host's value and
// the session, so the mode is set and used even while the option it would have
// been listed under cannot be advertised at all.
func TestSetModeSurvivesAgentCatalogFailure(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient(t)
	client.createSession = testNativeSession("native-agentless")
	client.providers = testProviders()
	client.agentsErr = errors.New("agents unreachable")

	agent := NewAgent(WithHome(t.TempDir()), WithLogger(slog.New(slog.DiscardHandler)), func(options *Options) {
		options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
			return client, nil
		}
	})
	agent.setAgentClient(newRecordingAgentClient())

	created, err := agent.NewSession(ctx, NewSessionRequest(t.TempDir()))
	require.NoError(t, err)

	set, err := agent.SetSessionConfigOption(ctx,
		SetConfigOptionRequest(created.SessionId, configMode, "plan"))
	require.NoError(t, err, "an unreadable agent list must not refuse a mode")

	// The option itself is still omitted, because a read that failed advertises
	// nothing; the value it could not list is set all the same.
	for _, option := range set.ConfigOptions {
		require.NotEqual(t, acp.SessionConfigId(configMode), option.Select.Id)
	}

	session, err := agent.session(created.SessionId)
	require.NoError(t, err)
	require.Equal(t, "plan", session.currentMode(), "the mode the host set is the one the session carries")
}

// requireConfigOption returns the advertised select with the given id.
func requireConfigOption(t *testing.T, options []acp.SessionConfigOption, id acp.SessionConfigId) *acp.SessionConfigOptionSelect {
	t.Helper()

	for _, option := range options {
		if option.Select != nil && option.Select.Id == id {
			return option.Select
		}
	}

	t.Fatalf("config option %q was not advertised: %#v", id, options)

	return nil
}

// TestConfigOptionCatalogFailureIsReported proves an advertisement failure is
// never silent: the option whose native read failed is left out, every other
// option still stands, and the omission states its cause.
func TestConfigOptionCatalogFailureIsReported(t *testing.T) {
	ctx := context.Background()

	var logs bytes.Buffer

	agent := NewAgent(WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	client := newFakeOpenCodeClient(t)
	client.providersErr = errors.New("providers unreachable")
	client.agents = []opencode.NativeAgent{{Name: "build"}}
	sess := testSession(t, agent, client)

	options := sess.configOptions(ctx)
	require.Len(t, options, 1, "a failed catalog read must not remove the options it did not feed")
	require.Equal(t, acp.SessionConfigId(configMode), options[0].Select.Id)

	record := logs.String()
	require.Contains(t, record, "OpenCode config option is unavailable")
	require.Contains(t, record, `"config_id":"model"`)
	require.Contains(t, record, "operation failed")
	require.NotContains(t, record, "providers unreachable")

	// The rule is per option, not per catalog: the agent read states its own
	// failure the same way.
	logs.Reset()

	modeless := newFakeOpenCodeClient(t)
	modeless.providers = testProviders()
	modeless.agentsErr = errors.New("agents unreachable")
	modelessSession := testSession(t, agent, modeless)

	options = modelessSession.configOptions(ctx)
	require.Len(t, options, 1)
	require.Equal(t, acp.SessionConfigId(configModel), options[0].Select.Id)

	record = logs.String()
	require.Contains(t, record, `"config_id":"mode"`)
	require.Contains(t, record, "operation failed")
	require.NotContains(t, record, "agents unreachable")
}

// TestEffortSetThroughConfigOptionReachesOpenCode proves the effort door is
// the model door: a preset chosen through `session/set_config_option` is what
// the session advertises as current and what the next frame carries as its
// native variant, and choosing the default value takes it off the frame again.
func TestEffortSetThroughConfigOptionReachesOpenCode(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient(t)
	client.createSession = testNativeSession("native-effort")
	client.agents = []opencode.NativeAgent{{Name: "build"}}
	client.providers = opencode.ProvidersResponse{Providers: []opencode.ProviderInfo{{
		ID: "openai",
		Models: map[string]opencode.ProviderModel{"gpt-test": {ID: "gpt-test", Variants: map[string]map[string]any{
			"high": {"reasoningEffort": "high"},
			"low":  {"reasoningEffort": "low"},
		}}},
	}}}

	dispatched := make(chan opencode.MessageRequest, 2)
	client.dispatchMessage = func(_ context.Context, _ string, req opencode.MessageRequest) (opencode.NativeMessage, error) {
		dispatched <- req

		return opencode.NativeMessage{}, nil
	}

	agent := NewAgent(WithHome(t.TempDir()), WithLogger(slog.New(slog.DiscardHandler)), func(options *Options) {
		options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
			return client, nil
		}
	})
	agent.setAgentClient(newRecordingAgentClient())

	created, err := agent.NewSession(ctx, NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	establishCreatedSession(t, agent, created.SessionId)

	// The session starts on the model's default, and the advertisement lists
	// that state ahead of the presets.
	effort := requireConfigOption(t, created.ConfigOptions, configEffort)
	require.Equal(t, acp.SessionConfigValueId(effortDefault), effort.CurrentValue)
	require.Equal(t, acp.SessionConfigSelectOptionsUngrouped{
		{Name: effortDefaultName, Value: effortDefault},
		{Name: "Low", Value: "low"},
		{Name: "High", Value: "high"},
	}, *effort.Options.Ungrouped)

	set, err := agent.SetSessionConfigOption(ctx, SetConfigOptionRequest(created.SessionId, configEffort, "high"))
	require.NoError(t, err)
	require.Equal(t, acp.SessionConfigValueId("high"), requireConfigOption(t, set.ConfigOptions, configEffort).CurrentValue)

	promptWithVariant := func(label string, want string) {
		t.Helper()

		done := make(chan error, 1)

		go func() {
			_, promptErr := agent.Prompt(ctx, TextPromptRequest(created.SessionId, label, "hello"))
			done <- promptErr
		}()

		select {
		case req := <-dispatched:
			require.Equal(t, want, req.Variant, "the frame must carry the preset the host chose")
		case <-time.After(2 * time.Second):
			t.Fatal("the prompt never reached OpenCode")
		}

		nativeErr := &opencode.NativeError{Name: "UnknownError"}
		nativeErr.Data.Message = "settled"
		client.publishEvent(opencode.Event{
			Type:       opencode.EventSessionError,
			Properties: mustJSON(t, opencode.SessionError{SessionID: "native-effort", Error: nativeErr}),
		})
		client.publishSessionIdle("native-effort")

		select {
		case promptErr := <-done:
			assertTurnFailed(t, promptErr, causeProvider, "")
		case <-time.After(5 * time.Second):
			t.Fatal("prompt did not settle")
		}
	}

	promptWithVariant("effort-high", "high")

	_, err = agent.SetSessionConfigOption(ctx, SetConfigOptionRequest(created.SessionId, configEffort, effortDefault))
	require.NoError(t, err)
	promptWithVariant("effort-default", "")
}

// TestResumedSessionCarriesStoredEffort proves the preset survives the store:
// a resumed session advertises and sends the variant its snapshot recorded.
func TestResumedSessionCarriesStoredEffort(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	snapshot := validSyncSnapshot("session", "native", cwd)
	snapshot.Session.Model = stateSnapshotModel{ProviderID: "openai", ModelID: "gpt-test", Agent: "build", Variant: "high"}
	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)

	store := NewInMemorySessionStore()
	require.NoError(t, store.Replace(ctx, SessionKey{SessionID: "session"}, []SessionStoreReplacement{{
		Key: SessionKey{SessionID: "session", Subpath: SessionStoreMainSubpath}, Entries: []SessionStoreEntry{encoded},
	}}))

	client := newFakeOpenCodeClient(t)
	client.getSession = testNativeSession("native")
	agent := negotiatedAgent(t, WithSessionStore(store))
	agent.setAgentClient(newRecordingAgentClient())
	agent.runtime = client

	_, err = agent.ResumeSession(ctx, ResumeSessionRequest("session", cwd))
	require.NoError(t, err)

	resumed, err := agent.session("session")
	require.NoError(t, err)
	require.Equal(t, "high", resumed.currentVariant())
}

func TestTitleASCII(t *testing.T) {
	if titleASCII("") != "" || titleASCII("plan") != "Plan" {
		t.Fatal("titleASCII mismatch")
	}
}
