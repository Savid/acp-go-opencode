package opencodeacp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

func nativeOAuthMethod(label string) opencode.ProviderAuthMethod {
	return opencode.ProviderAuthMethod{Type: authMethodTypeOAuth, Label: label}
}

func TestMethodsMergesTheCatalogWithSpecialMethods(t *testing.T) {
	harness := newAuthAgent(t)
	broker, client, session := harness.broker, harness.runtime, harness.session

	client.providerCatalog = []opencode.ProviderCatalogEntry{
		{ID: "xai", Name: "xAI"},
		{ID: "deepseek", Name: "DeepSeek"},
		{ID: "", Name: "unnamed"},
	}
	client.providerAuthMethods = map[string][]opencode.ProviderAuthMethod{
		"xai": {
			nativeOAuthMethod("xAI Grok OAuth"),
			{Type: authMethodTypeAPI, Label: "Manually enter API Key"},
		},
		"special-only": {nativeOAuthMethod("Special")},
	}

	result, err := broker.methods(context.Background(), mustJSON(t, map[string]any{authFieldSessionID: string(session.id)}))
	require.NoError(t, err)

	methods, ok := result.(authMethodsResult)
	require.True(t, ok)
	require.NotEmpty(t, methods.Generation)

	require.Equal(t, []authMethodEntry{
		{ID: "0", Type: authMethodTypeOAuth, Label: "xAI Grok OAuth"},
		{ID: "1", Type: authMethodTypeAPI, Label: "Manually enter API Key"},
	}, methods.Providers["xai"])

	require.Equal(t, []authMethodEntry{
		{ID: authDefaultAPIMethodID, Type: authMethodTypeAPI, Label: "DeepSeek"},
	}, methods.Providers["deepseek"])

	require.Equal(t, []authMethodEntry{
		{ID: "0", Type: authMethodTypeOAuth, Label: "Special"},
	}, methods.Providers["special-only"])

	require.NotContains(t, methods.Providers, "")

	broker.mu.Lock()
	defer broker.mu.Unlock()
	require.Equal(t, methods.Generation, broker.generation)
	require.Equal(t, -1, broker.catalog["deepseek"][0].Index)
}

func TestMethodsFailures(t *testing.T) {
	harness := newAuthAgent(t)
	broker, client, session := harness.broker, harness.runtime, harness.session

	params := mustJSON(t, map[string]any{authFieldSessionID: string(session.id)})

	_, err := broker.methods(context.Background(), mustJSON(t, map[string]any{"extra": 1}))
	requireInvalidParams(t, err, "extra")

	_, err = broker.methods(context.Background(), mustJSON(t, map[string]any{authFieldSessionID: 1}))
	requireInvalidParams(t, err, authFieldSessionID)

	_, err = broker.methods(context.Background(), mustJSON(t, map[string]any{authFieldSessionID: "missing"}))
	requireInvalidParams(t, err, jsonFieldSessionID)

	client.providerCatalogErr = errors.New("http")

	_, err = broker.methods(context.Background(), params)
	requireAuthFailure(t, err, authCauseTransport)

	client.providerCatalogErr = nil
	client.providerAuthMethodsErr = errors.New("http")

	_, err = broker.methods(context.Background(), params)
	requireAuthFailure(t, err, authCauseTransport)

	client.providerAuthMethodsErr = nil
	client.providerCatalog = []opencode.ProviderCatalogEntry{{ID: "xai", Name: "xAI"}}
	client.providerAuthMethods = map[string][]opencode.ProviderAuthMethod{
		"xai": {{Type: authMethodTypeOAuth, Label: "ok", Prompts: []opencode.ProviderAuthPrompt{{Type: "text", Message: "m"}}}},
	}

	_, err = broker.methods(context.Background(), params)
	requireAuthFailure(t, err, authCauseNativeVeto)

	client.providerAuthMethods = nil

	originalRandRead := authRandRead
	authRandRead = func([]byte) (int, error) { return 0, errors.New("entropy") }

	t.Cleanup(func() { authRandRead = originalRandRead })

	_, err = broker.methods(context.Background(), params)
	requireAuthFailure(t, err, authCauseProcess)
}

func TestMethodsFailsWhenTheRuntimeIsGone(t *testing.T) {
	harness := newAuthAgent(t)
	broker, session := harness.broker, harness.session

	session.mu.Lock()
	session.client = nil
	session.mu.Unlock()

	_, err := broker.methods(context.Background(), mustJSON(t, map[string]any{authFieldSessionID: string(session.id)}))
	requireAuthFailure(t, err, authCauseTransport)
}

func TestBuildProviderMethodsOmitsUnpublishableEntries(t *testing.T) {
	methods, published, err := buildProviderMethods("xai", []opencode.ProviderAuthMethod{
		{Type: "wellknown", Label: "unsupported variant"},
		{Type: authMethodTypeOAuth, Label: strings.Repeat("a", authMaxLabelBytes+1)},
		{Type: authMethodTypeOAuth, Label: "kept"},
	}, false)
	require.NoError(t, err)
	require.Len(t, methods, 1)
	require.Equal(t, "2", methods[0].ID)
	require.Equal(t, 2, methods[0].Index)
	require.Len(t, published, 1)
}

// TestBuildAuthCatalogDropsProvidersWithNoPublishableMethod drives the omission
// through the prompt github-copilot gates today, presented ungated. A harness
// cut that stops gating a deployment-chosen host leaves the method no branch
// that avoids the prompt, and the provider loses its only entry.
func TestBuildAuthCatalogDropsProvidersWithNoPublishableMethod(t *testing.T) {
	methods, entries, err := buildAuthCatalog(
		[]opencode.ProviderCatalogEntry{{ID: authProviderCopilot, Name: "GitHub Copilot"}},
		map[string][]opencode.ProviderAuthMethod{authProviderCopilot: {{
			Type:  authMethodTypeOAuth,
			Label: "Login with GitHub Copilot",
			Prompts: []opencode.ProviderAuthPrompt{
				{Type: authPromptTypeText, Key: authPromptKeyEnterprise, Message: "Enter your GitHub Enterprise URL"},
			},
		}}},
	)
	require.NoError(t, err)
	require.Empty(t, methods)
	require.Empty(t, entries)
}

// gitlabNativeMethods is gitlab's native method array exactly as OpenCode
// publishes it: a browser OAuth method and a personal-access-token method,
// each asking the same ungated instance URL.
func gitlabNativeMethods() []opencode.ProviderAuthMethod {
	prompts := []opencode.ProviderAuthPrompt{
		{Type: authPromptTypeText, Key: authPromptKeyInstance, Message: "GitLab instance URL", Placeholder: "https://gitlab.com"},
	}

	return []opencode.ProviderAuthMethod{
		{Type: authMethodTypeOAuth, Label: "GitLab OAuth", Prompts: prompts},
		{Type: authMethodTypeAPI, Label: "GitLab Personal Access Token", Prompts: prompts},
	}
}

// TestBuildAuthCatalogPublishesGitLabAgainstItsVendorHost pins what the
// instance URL costs and what it does not. The prompt is ungated on both
// methods, so nothing about `when` reaches gitlab; the fixed vendor host is
// still an allowlist entry the prompt can have, which is what keeps the
// token method addressable instead of dropping the provider outright. The
// OAuth method goes for the other reason entirely: its callback lands on a
// listener the harness binds on the worker host while it mints.
func TestBuildAuthCatalogPublishesGitLabAgainstItsVendorHost(t *testing.T) {
	methods, entries, err := buildAuthCatalog(
		[]opencode.ProviderCatalogEntry{{ID: authProviderGitLab, Name: "GitLab"}},
		map[string][]opencode.ProviderAuthMethod{authProviderGitLab: gitlabNativeMethods()},
	)
	require.NoError(t, err)
	require.Equal(t, []authMethodEntry{{
		ID:    "1",
		Type:  authMethodTypeAPI,
		Label: "GitLab Personal Access Token",
		Prompts: []authPrompt{
			{Type: authPromptTypeText, Key: authPromptKeyInstance, Message: "GitLab instance URL", Placeholder: "https://gitlab.com"},
		},
	}}, entries[authProviderGitLab])
	require.Equal(t, 1, methods[authProviderGitLab][0].Index)

	method := methods[authProviderGitLab][0]
	require.NoError(t, validateAuthInputs(authProviderGitLab, method, map[string]string{authPromptKeyInstance: "https://gitlab.com"}))

	// A deployment-chosen host has no entry it could have, so the self-hosted
	// branch stays unanswerable rather than riding in on the vendor one.
	for _, value := range []string{"https://gitlab.example.com", "https://gitlab.com.example.com", "http://gitlab.com"} {
		requireInvalidParams(t, validateAuthInputs(authProviderGitLab, method, map[string]string{
			authPromptKeyInstance: value,
		}), authFieldInputs+"."+authPromptKeyInstance)
	}
}

// copilotNativeMethod is github-copilot's single native method exactly as
// OpenCode publishes it: one select that chooses the deployment, and one
// hostname-forming text prompt gated on the enterprise branch of that select.
func copilotNativeMethod() opencode.ProviderAuthMethod {
	return opencode.ProviderAuthMethod{
		Type:  authMethodTypeOAuth,
		Label: "Login with GitHub Copilot",
		Prompts: []opencode.ProviderAuthPrompt{
			{Type: authPromptTypeSelect, Key: "deploymentType", Message: "Select GitHub deployment type", Options: []opencode.ProviderAuthPromptOption{
				{Label: "GitHub.com", Value: "github.com", Hint: "Public"},
				{Label: "GitHub Enterprise", Value: "enterprise", Hint: "Self-hosted"},
			}},
			{Type: authPromptTypeText, Key: "enterpriseUrl", Message: "Enter your GitHub Enterprise URL or domain",
				Placeholder: "company.ghe.com",
				When:        &opencode.ProviderAuthPromptWhen{Key: "deploymentType", Op: authWhenOpEq, Value: "enterprise"}},
		},
	}
}

// TestBuildAuthCatalogKeepsAMethodWhoseUnallowlistedPromptIsGated pins the two
// resolvers against each other: buildAuthPrompts must read `when` the same way
// visibleAuthPromptKeys does. A prompt that has no allowlist entry it could
// have costs the method its catalog entry only when the method always asks it;
// github-copilot's public path never does, and dropping the provider made the
// single most likely OpenCode subscription unbrokerable.
func TestBuildAuthCatalogKeepsAMethodWhoseUnallowlistedPromptIsGated(t *testing.T) {
	methods, entries, err := buildAuthCatalog(
		[]opencode.ProviderCatalogEntry{{ID: authProviderCopilot, Name: "GitHub Copilot"}},
		map[string][]opencode.ProviderAuthMethod{authProviderCopilot: {copilotNativeMethod()}},
	)
	require.NoError(t, err)
	require.Len(t, methods[authProviderCopilot], 1)
	require.Len(t, entries[authProviderCopilot], 1)

	prompts := entries[authProviderCopilot][0].Prompts
	require.Len(t, prompts, 2)
	require.Equal(t, "enterpriseUrl", prompts[1].Key)
	require.NotNil(t, prompts[1].When)

	// The gated branch is still the one with no allowlist entry it could have,
	// so it stays unanswerable rather than becoming reachable.
	require.Equal(t, []string{"deploymentType"}, visibleAuthPromptKeys(prompts, map[string]string{"deploymentType": "github.com"}))
	require.NoError(t, validateAuthInputs(authProviderCopilot, methods[authProviderCopilot][0], map[string]string{"deploymentType": "github.com"}))
	requireInvalidParams(t, validateAuthInputs(authProviderCopilot, methods[authProviderCopilot][0], map[string]string{
		"deploymentType": "enterprise",
		"enterpriseUrl":  "company.ghe.com",
	}), authFieldInputs+".enterpriseUrl")
}

// TestBuildAuthCatalogOmitsLoopbackCompletingMethods pins the one place the
// adapter can hold the broker's bind-loopback-only property. The harness opens
// its wildcard callback listener while it mints, so a method refused after the
// mint has already exposed a port on every interface of the worker host.
func TestBuildAuthCatalogOmitsLoopbackCompletingMethods(t *testing.T) {
	methods, entries, err := buildAuthCatalog(
		[]opencode.ProviderCatalogEntry{{ID: "openai", Name: "OpenAI"}},
		map[string][]opencode.ProviderAuthMethod{"openai": {
			nativeOAuthMethod("ChatGPT Pro/Plus (browser)"),
			nativeOAuthMethod("ChatGPT Pro/Plus (headless)"),
			{Type: authMethodTypeAPI, Label: "Manually enter API Key"},
		}},
	)
	require.NoError(t, err)
	require.Equal(t, []authMethodEntry{
		{ID: "1", Type: authMethodTypeOAuth, Label: "ChatGPT Pro/Plus (headless)"},
		{ID: "2", Type: authMethodTypeAPI, Label: "Manually enter API Key"},
	}, entries["openai"])

	// The published ids stay the native array indices the omitted method left
	// behind, so the remaining methods still address their own native slots.
	require.Equal(t, 1, methods["openai"][0].Index)
	require.Equal(t, 2, methods["openai"][1].Index)
}

func TestBuildAuthCatalogPropagatesPromptDrift(t *testing.T) {
	_, _, err := buildAuthCatalog(
		[]opencode.ProviderCatalogEntry{{ID: "xai", Name: "xAI"}},
		map[string][]opencode.ProviderAuthMethod{"xai": {{
			Type:    authMethodTypeOAuth,
			Label:   "xAI",
			Prompts: []opencode.ProviderAuthPrompt{{Type: "text", Key: "a", Message: "m"}, {Type: "text", Key: "a", Message: "m"}},
		}}},
	)
	requireAuthFailure(t, err, authCauseNativeVeto)
}

func TestBuildAuthPromptsRejectsSchemaDrift(t *testing.T) {
	cases := []struct {
		name    string
		prompts []opencode.ProviderAuthPrompt
	}{
		{name: "missing key", prompts: []opencode.ProviderAuthPrompt{{Type: "text", Message: "m"}}},
		{name: "missing message", prompts: []opencode.ProviderAuthPrompt{{Type: "text", Key: "a"}}},
		{name: "duplicate key", prompts: []opencode.ProviderAuthPrompt{
			{Type: "text", Key: "a", Message: "m"},
			{Type: "text", Key: "a", Message: "m"},
		}},
		{name: "when names a later prompt", prompts: []opencode.ProviderAuthPrompt{
			{Type: "text", Key: "a", Message: "m", When: &opencode.ProviderAuthPromptWhen{Key: "b", Op: "eq", Value: "v"}},
		}},
		{name: "when names itself", prompts: []opencode.ProviderAuthPrompt{
			{Type: "text", Key: "a", Message: "m", When: &opencode.ProviderAuthPromptWhen{Key: "a", Op: "eq", Value: "v"}},
		}},
		{name: "unknown when op", prompts: []opencode.ProviderAuthPrompt{
			{Type: "text", Key: "a", Message: "m"},
			{Type: "text", Key: "b", Message: "m", When: &opencode.ProviderAuthPromptWhen{Key: "a", Op: "gt", Value: "v"}},
		}},
		{name: "unknown prompt type", prompts: []opencode.ProviderAuthPrompt{{Type: "checkbox", Key: "a", Message: "m"}}},
		{name: "text with options", prompts: []opencode.ProviderAuthPrompt{
			{Type: "text", Key: "a", Message: "m", Options: []opencode.ProviderAuthPromptOption{{Label: "l", Value: "v"}}},
		}},
		{name: "select with no options", prompts: []opencode.ProviderAuthPrompt{{Type: "select", Key: "a", Message: "m"}}},
		{name: "select option with no value", prompts: []opencode.ProviderAuthPrompt{
			{Type: "select", Key: "a", Message: "m", Options: []opencode.ProviderAuthPromptOption{{Label: "l"}}},
		}},
		{name: "duplicate select value", prompts: []opencode.ProviderAuthPrompt{
			{Type: "select", Key: "a", Message: "m", Options: []opencode.ProviderAuthPromptOption{
				{Label: "one", Value: "v"}, {Label: "two", Value: "v"},
			}},
		}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, err := buildAuthPrompts("xai", testCase.prompts)
			requireAuthFailure(t, err, authCauseNativeVeto)
		})
	}
}

func TestBuildAuthPromptsOmitsMethodsWithUnpublishableText(t *testing.T) {
	cases := []struct {
		name    string
		prompts []opencode.ProviderAuthPrompt
	}{
		{name: "message over bound", prompts: []opencode.ProviderAuthPrompt{
			{Type: "text", Key: "a", Message: strings.Repeat("m", authMaxMessageBytes+1)},
		}},
		{name: "placeholder over bound", prompts: []opencode.ProviderAuthPrompt{
			{Type: "text", Key: "a", Message: "m", Placeholder: strings.Repeat("p", authMaxMessageBytes+1)},
		}},
		{name: "option label over bound", prompts: []opencode.ProviderAuthPrompt{
			{Type: "select", Key: "a", Message: "m", Options: []opencode.ProviderAuthPromptOption{
				{Label: strings.Repeat("l", authMaxLabelBytes+1), Value: "v"},
			}},
		}},
		// A hint is display text like every string beside it, and it crosses
		// the boundary through the same path rather than raw.
		{name: "option hint over bound", prompts: []opencode.ProviderAuthPrompt{
			{Type: "select", Key: "a", Message: "m", Options: []opencode.ProviderAuthPromptOption{
				{Label: "l", Value: "v", Hint: strings.Repeat("h", authMaxLabelBytes+1)},
			}},
		}},
		{name: "option hint carrying a bidi override", prompts: []opencode.ProviderAuthPrompt{
			{Type: "select", Key: "a", Message: "m", Options: []opencode.ProviderAuthPromptOption{
				{Label: "l", Value: "v", Hint: "a\u202Eb"},
			}},
		}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			prompts, ok, err := buildAuthPrompts("xai", testCase.prompts)
			require.NoError(t, err)
			require.False(t, ok)
			require.Nil(t, prompts)
		})
	}
}

func TestBuildAuthPromptsKeepsAWellFormedSchema(t *testing.T) {
	prompts, ok, err := buildAuthPrompts("snowflake-cortex", []opencode.ProviderAuthPrompt{
		{Type: "select", Key: "deploymentType", Message: "Deployment", Options: []opencode.ProviderAuthPromptOption{
			{Label: "Cloud", Value: "cloud", Hint: "hosted"},
			{Label: "Self", Value: "self"},
		}},
		{Type: "text", Key: "account", Message: "Account", Placeholder: "myorg-myaccount", When: &opencode.ProviderAuthPromptWhen{
			Key: "deploymentType", Op: "eq", Value: "cloud",
		}},
	})
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, prompts, 2)
	require.Equal(t, "cloud", prompts[0].Options[0].Value)
	require.Equal(t, "hosted", prompts[0].Options[0].Hint)
	require.Empty(t, prompts[0].Options[1].Hint)
	require.Equal(t, "myorg-myaccount", prompts[1].Placeholder)
	require.Equal(t, &authPromptWhen{Key: "deploymentType", Op: "eq", Value: "cloud"}, prompts[1].When)

	empty, ok, err := buildAuthPrompts("xai", nil)
	require.NoError(t, err)
	require.True(t, ok)
	require.Nil(t, empty)
}

func TestAuthDisplayTextNormalisesFirstAndBoundsTheNormalisedForm(t *testing.T) {
	value, ok := authDisplayText("école", authMaxLabelBytes)
	require.True(t, ok)
	require.Equal(t, "école", value)

	cases := []struct {
		name  string
		value string
		max   int
	}{
		{name: "empty", value: "", max: authMaxLabelBytes},
		{name: "over bound", value: strings.Repeat("a", 10), max: 9},
		{name: "control character", value: "a\ab", max: authMaxLabelBytes},
		{name: "bidi override", value: "a\u202Eb", max: authMaxLabelBytes},
		{name: "invalid utf8", value: "a\xffb", max: authMaxLabelBytes},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, accepted := authDisplayText(testCase.value, testCase.max)
			require.False(t, accepted)
		})
	}

	spaced, ok := authDisplayText("a b-1 (2) +", authMaxLabelBytes)
	require.True(t, ok)
	require.Equal(t, "a b-1 (2) +", spaced)
}

func TestAuthDisplayURLBound(t *testing.T) {
	value, ok := authDisplayURL("https://accounts.x.ai/oauth2/device?user_code=AB-CD")
	require.True(t, ok)
	require.Equal(t, "https://accounts.x.ai/oauth2/device?user_code=AB-CD", value)

	for _, raw := range []string{
		"",
		"http://accounts.x.ai/",
		"https://user:pass@accounts.x.ai/",
		"https://accounts.x.ai/#fragment",
		"https:///path",
		"https://accounts.x.ai/?q=" + strings.Repeat("a", authMaxURLBytes),
		"://",
	} {
		_, ok := authDisplayURL(raw)
		require.False(t, ok, raw)
	}
}

func TestAuthDisplayUserCodeIsAnchored(t *testing.T) {
	value, ok := authDisplayUserCode("2XRG-QGNV")
	require.True(t, ok)
	require.Equal(t, "2XRG-QGNV", value)

	for _, raw := range []string{"", "</script>ABCD", strings.Repeat("A", authMaxUserCodeBytes+1), "AB CD"} {
		_, ok := authDisplayUserCode(raw)
		require.False(t, ok, raw)
	}
}

func TestAuthLoopbackHostDetection(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want bool
	}{
		{name: "hosted", url: "https://accounts.x.ai/oauth2/device"},
		{name: "loopback host", url: "http://127.0.0.1:39999/oauth/authorize", want: true},
		{name: "ipv6 loopback", url: "https://[::1]/authorize", want: true},
		{name: "localhost subdomain", url: "https://app.localhost/authorize", want: true},
		{name: "loopback redirect", url: "https://accounts.x.ai/authorize?redirect_uri=http%3A%2F%2Flocalhost%3A1455%2Fcb", want: true},
		{name: "hosted redirect", url: "https://accounts.x.ai/authorize?redirect_uri=https%3A%2F%2Fx.ai%2Fcb"},
		{name: "unparseable", url: "://"},
		{name: "unparseable redirect", url: "https://accounts.x.ai/authorize?redirect_uri=%3A%2F%2F"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			require.Equal(t, testCase.want, authLoopbackHost(testCase.url))
		})
	}
}

func TestVisibleAuthPromptKeysFollowsConditions(t *testing.T) {
	prompts := []authPrompt{
		{Type: "select", Key: "kind", Message: "Kind"},
		{Type: "text", Key: "account", Message: "Account", When: &authPromptWhen{Key: "kind", Op: "eq", Value: "cloud"}},
		{Type: "text", Key: "host", Message: "Host", When: &authPromptWhen{Key: "kind", Op: "neq", Value: "cloud"}},
		{Type: "text", Key: "orphan", Message: "Orphan", When: &authPromptWhen{Key: "absent", Op: "eq", Value: "x"}},
	}

	require.Equal(t, []string{"kind", "account"}, visibleAuthPromptKeys(prompts, map[string]string{"kind": "cloud"}))
	require.Equal(t, []string{"kind", "host"}, visibleAuthPromptKeys(prompts, map[string]string{"kind": "self"}))
	require.Equal(t, []string{"kind"}, visibleAuthPromptKeys(prompts, nil))
}

func TestValidateAuthInputs(t *testing.T) {
	method := authCatalogMethod{Prompts: []authPrompt{
		{Type: "select", Key: "kind", Message: "Kind", Options: []authPromptOption{{Label: "Cloud", Value: "cloud"}}},
		{Type: "text", Key: "account", Message: "Account", When: &authPromptWhen{Key: "kind", Op: "eq", Value: "cloud"}},
	}}

	require.NoError(t, validateAuthInputs("snowflake-cortex", method, map[string]string{
		"kind": "cloud", "account": "myorg-myaccount",
	}))

	oversize := make(map[string]string, authMaxInputsKeys+1)
	for index := range authMaxInputsKeys + 1 {
		oversize[string(rune('a'+index))] = "v"
	}

	require.Error(t, validateAuthInputs("xai", method, oversize))

	err := validateAuthInputs("xai", method, map[string]string{"kind": strings.Repeat("v", authMaxInputsBytes)})
	requireInvalidParams(t, err, authFieldInputs)

	err = validateAuthInputs("xai", method, map[string]string{"kind": "cloud"})
	requireInvalidParams(t, err, authFieldInputs)

	badValue := authCatalogMethod{Prompts: []authPrompt{{Type: "text", Key: "note", Message: "Note"}}}

	requireInvalidParams(t, validateAuthInputs("xai", badValue, map[string]string{"note": "line\nbreak"}), authFieldInputs+".note")

	single := authCatalogMethod{Prompts: []authPrompt{{Type: "text", Key: "a", Message: "m"}}}

	err = validateAuthInputs("xai", single, map[string]string{"b": "v"})
	requireInvalidParams(t, err, authFieldInputs+".a")
}

func TestValidateAuthInput(t *testing.T) {
	selectPrompt := authPrompt{Type: "select", Key: "kind", Options: []authPromptOption{{Value: "cloud"}}}
	textPrompt := authPrompt{Type: "text", Key: "note"}

	require.NoError(t, validateAuthInput("xai", selectPrompt, "cloud"))
	requireInvalidParams(t, validateAuthInput("xai", selectPrompt, "TOTALLY-BOGUS"), authFieldInputs+".kind")

	require.NoError(t, validateAuthInput("xai", textPrompt, "value"))

	for _, value := range []string{"", strings.Repeat("v", authMaxTextInputBytes+1), "a\xffb", "a\nb", "a\x07b"} {
		requireInvalidParams(t, validateAuthInput("xai", textPrompt, value), authFieldInputs+".note")
	}

	account := authPrompt{Type: "text", Key: "account"}
	require.NoError(t, validateAuthInput("snowflake-cortex", account, "myorg-myaccount"))
	requireInvalidParams(t, validateAuthInput("snowflake-cortex", account, "evil.example.com"), authFieldInputs+".account")
}

func TestAuthHostAllowed(t *testing.T) {
	suffix := authHostFormingRule{HostSuffix: "snowflakecomputing.com", Domains: []string{"snowflakecomputing.com"}}
	whole := authHostFormingRule{Domains: []string{"example.com"}}

	require.True(t, authHostAllowed(suffix, "myorg-myaccount"))
	require.False(t, authHostAllowed(suffix, "myorg.evil"))

	require.True(t, authHostAllowed(whole, "https://git.example.com/"))
	require.True(t, authHostAllowed(whole, "https://example.com:443/"))
	require.False(t, authHostAllowed(whole, "http://example.com/"))
	require.False(t, authHostAllowed(whole, "https://user@example.com/"))
	require.False(t, authHostAllowed(whole, "https://example.com/#frag"))
	require.False(t, authHostAllowed(whole, "https://example.com:8443/"))
	require.False(t, authHostAllowed(whole, "https://attacker.example.net/"))
	require.False(t, authHostAllowed(whole, "https:///path"))
	require.False(t, authHostAllowed(whole, "://"))
}
