package opencodeacp

import (
	"context"
	"encoding/json"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/savid/acp-go-opencode/internal/opencode"
	"golang.org/x/text/unicode/norm"
)

// Method entry types, mirroring the native login-method discriminator.
const (
	authMethodTypeOAuth = "oauth"
	authMethodTypeAPI   = "api"
)

// Native prompt vocabulary the catalog decodes and republishes.
const (
	authPromptTypeText   = "text"
	authPromptTypeSelect = "select"
	authWhenOpEq         = "eq"
	authWhenOpNeq        = "neq"
)

// Native providers whose prompt answers reach a hostname.
const (
	authProviderSnowflake   = "snowflake-cortex"
	authProviderAzure       = "azure"
	authProviderCopilot     = "github-copilot"
	authProviderGitLab      = "gitlab"
	authPromptKeyAccount    = "account"
	authPromptKeyEnterprise = "enterpriseUrl"
	authPromptKeyInstance   = "instanceUrl"
	authSnowflakeHost       = "snowflakecomputing.com"
	authGitLabHost          = "gitlab.com"
)

// authDefaultAPIMethodID is the reserved id of the adapter-synthesized default
// API-key method. Native ids are array indices, so a reserved literal can never
// collide with one.
const authDefaultAPIMethodID = "default-api"

// Display-field bounds. A value violating its bound is dropped, never
// truncated.
const (
	authMaxURLBytes      = 2048
	authMaxMessageBytes  = 2048
	authMaxUserCodeBytes = 64
	authMaxLabelBytes    = 256
)

// Input bounds.
const (
	authMaxTextInputBytes = 1024
	authMaxInputsBytes    = 8192
	authMaxInputsKeys     = 16
)

// authUserCodePattern is anchored: a substring match accepts a code with markup
// wrapped around it.
var authUserCodePattern = regexp.MustCompile(`\A[A-Za-z0-9-]+\z`)

// authCatalogMethod is one entry of the current catalog, paired with the native
// addressing the adapter needs to drive it.
type authCatalogMethod struct {
	ID      string
	Type    string
	Label   string
	Prompts []authPrompt
	// Index is the native method array index. A synthesized default API-key
	// method has no native array to index and carries -1.
	Index int
}

type authPrompt struct {
	Type        string             `json:"type"`
	Key         string             `json:"key"`
	Message     string             `json:"message"`
	Placeholder string             `json:"placeholder,omitempty"`
	Options     []authPromptOption `json:"options,omitempty"`
	When        *authPromptWhen    `json:"when,omitempty"`
}

type authPromptOption struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Hint  string `json:"hint,omitempty"`
}

type authPromptWhen struct {
	Key   string `json:"key"`
	Op    string `json:"op"`
	Value string `json:"value"`
}

type authMethodEntry struct {
	ID      string       `json:"id"`
	Type    string       `json:"type"`
	Label   string       `json:"label"`
	Prompts []authPrompt `json:"prompts,omitempty"`
}

type authMethodsResult struct {
	Providers  map[string][]authMethodEntry `json:"providers"`
	Generation string                       `json:"generation"`
}

// authHostFormingRule describes how a native prompt answer reaches a hostname.
type authHostFormingRule struct {
	// HostSuffix, when set, means the answer is a hostname label the harness
	// interpolates in front of it; empty means the answer is a whole URL.
	HostSuffix string
	// Domains is the registrable-domain allowlist. An empty list omits the
	// method from the catalog: the harness performs the dereference, so a
	// resolve-and-check here would be a TOCTOU and a blocklist would be
	// defeated by a hostname that resolves into a private range.
	Domains []string
}

// authReviewedOAuthMethods is the catalog's OAuth allowlist, keyed by provider
// id and native method label: the reviewed registry in internal/opencode
// projected to a lookup. The registry documents what an entry certifies. A
// native OAuth method with no entry is omitted rather than refused at
// authorize, because a method that binds a callback listener while it mints
// has already bound it by the time the adapter sees the answer; the one place
// the broker's no-listener property can be held is before the native call
// exists to make. authLoopbackHost stays as the check on the minted answer,
// for a reviewed method whose native flow drifts under its label.
var authReviewedOAuthMethods = reviewedOAuthMethodSet(opencode.ReviewedOAuthMethods())

func reviewedOAuthMethodSet(registry map[string][]string) map[string]map[string]struct{} {
	set := make(map[string]map[string]struct{}, len(registry))

	for providerID, labels := range registry {
		entries := make(map[string]struct{}, len(labels))

		for _, label := range labels {
			entries[label] = struct{}{}
		}

		set[providerID] = entries
	}

	return set
}

// authHostFormingRules names every native prompt whose value is interpolated
// into a URL or a hostname, per provider. A prompt key absent from a provider's
// map is an ordinary text answer.
//
// GitLab's instance URL names the vendor's own SaaS host as readily as a
// customer's self-hosted one, so the fixed vendor host is an entry the prompt
// can have even though a deployment-chosen host is not: the SaaS branch is
// answerable and every self-hosted answer is refused, which is the same split
// github-copilot's gated enterprise prompt makes with a select.
var authHostFormingRules = map[string]map[string]authHostFormingRule{
	authProviderSnowflake: {authPromptKeyAccount: {HostSuffix: authSnowflakeHost, Domains: []string{authSnowflakeHost}}},
	authProviderAzure:     {"resourceName": {HostSuffix: "openai.azure.com", Domains: []string{"azure.com"}}},
	authProviderCopilot:   {authPromptKeyEnterprise: {}},
	authProviderGitLab:    {authPromptKeyInstance: {Domains: []string{authGitLabHost}}},
}

// methods enumerates the catalog and mints the generation that names this exact
// result. The catalog is what the adapter enumerates: it publishes no
// completeness claim and offers no free entry of an unlisted provider.
func (p *providerAuth) methods(ctx context.Context, params json.RawMessage) (any, error) {
	fields, err := authParamFields(params, authFieldSessionID)
	if err != nil {
		return nil, err
	}

	sessionID, err := authRequiredString(fields, authFieldSessionID)
	if err != nil {
		return nil, err
	}

	session, err := p.authSession(sessionID)
	if err != nil {
		return nil, err
	}

	client := session.nativeClient()
	if client == nil {
		return nil, authFailed(authCauseTransport, "", "", "")
	}

	catalog, err := client.ProviderCatalog(ctx)
	if err != nil {
		return nil, authFailed(authCauseTransport, "", "", "")
	}

	special, err := client.ProviderAuthMethods(ctx)
	if err != nil {
		return nil, authFailed(authCauseTransport, "", "", "")
	}

	methods, entries, err := buildAuthCatalog(catalog, special)
	if err != nil {
		return nil, err
	}

	generation, err := newAuthToken()
	if err != nil {
		return nil, authFailed(authCauseProcess, "", "", "")
	}

	p.mu.Lock()
	p.generation = generation
	p.catalog = methods
	p.mu.Unlock()

	return authMethodsResult{Providers: entries, Generation: generation}, nil
}

// buildAuthCatalog merges the native `all` catalog with the special-method map.
// A provider with no special methods receives one synthesized default API-key
// method, so a catalog-only provider is still addressable.
func buildAuthCatalog(
	catalog []opencode.ProviderCatalogEntry,
	special map[string][]opencode.ProviderAuthMethod,
) (map[string][]authCatalogMethod, map[string][]authMethodEntry, error) {
	methods := make(map[string][]authCatalogMethod, len(catalog))
	entries := make(map[string][]authMethodEntry, len(catalog))

	names := make(map[string]string, len(catalog))
	ids := make([]string, 0, len(catalog)+len(special))

	for _, entry := range catalog {
		if entry.ID == "" {
			continue
		}

		names[entry.ID] = entry.Name
		ids = append(ids, entry.ID)
	}

	for providerID := range special {
		if _, ok := names[providerID]; !ok {
			ids = append(ids, providerID)
		}
	}

	sort.Strings(ids)

	for _, providerID := range ids {
		native, ok := special[providerID]
		if !ok {
			native = []opencode.ProviderAuthMethod{{Type: authMethodTypeAPI, Label: names[providerID]}}
		}

		resolved, published, err := buildProviderMethods(providerID, native, !ok)
		if err != nil {
			return nil, nil, err
		}

		if len(resolved) == 0 {
			continue
		}

		methods[providerID] = resolved
		entries[providerID] = published
	}

	return methods, entries, nil
}

func buildProviderMethods(
	providerID string,
	native []opencode.ProviderAuthMethod,
	synthesized bool,
) ([]authCatalogMethod, []authMethodEntry, error) {
	resolved := make([]authCatalogMethod, 0, len(native))
	published := make([]authMethodEntry, 0, len(native))

	for index, method := range native {
		if method.Type != authMethodTypeOAuth && method.Type != authMethodTypeAPI {
			continue
		}

		label, ok := authDisplayText(method.Label, authMaxLabelBytes)
		if !ok {
			continue
		}

		if method.Type == authMethodTypeOAuth {
			if _, reviewed := authReviewedOAuthMethods[providerID][method.Label]; !reviewed {
				continue
			}
		}

		prompts, ok, err := buildAuthPrompts(providerID, method.Prompts)
		if err != nil {
			return nil, nil, err
		}

		if !ok {
			continue
		}

		id := strconv.Itoa(index)
		nativeIndex := index

		if synthesized {
			id = authDefaultAPIMethodID
			nativeIndex = -1
		}

		resolved = append(resolved, authCatalogMethod{ID: id, Type: method.Type, Label: label, Prompts: prompts, Index: nativeIndex})
		published = append(published, authMethodEntry{ID: id, Type: method.Type, Label: label, Prompts: prompts})
	}

	return resolved, published, nil
}

// buildAuthPrompts converts a native prompt array. The second result reports
// whether the method may be published at all: a method carrying a
// hostname-forming prompt with no allowlist entry loses its only safe form and
// is omitted from the catalog.
func buildAuthPrompts(providerID string, native []opencode.ProviderAuthPrompt) ([]authPrompt, bool, error) {
	if len(native) == 0 {
		return nil, true, nil
	}

	prompts := make([]authPrompt, 0, len(native))
	seen := make(map[string]struct{}, len(native))

	for _, prompt := range native {
		if prompt.Key == "" || prompt.Message == "" {
			return nil, false, authFailed(authCauseNativeVeto, providerID, "", "")
		}

		if _, duplicate := seen[prompt.Key]; duplicate {
			return nil, false, authFailed(authCauseNativeVeto, providerID, "", "")
		}

		seen[prompt.Key] = struct{}{}

		if prompt.When != nil {
			if _, earlier := seen[prompt.When.Key]; !earlier || prompt.When.Key == prompt.Key {
				return nil, false, authFailed(authCauseNativeVeto, providerID, "", "")
			}

			if prompt.When.Op != authWhenOpEq && prompt.When.Op != authWhenOpNeq {
				return nil, false, authFailed(authCauseNativeVeto, providerID, "", "")
			}
		}

		// A prompt with no allowlist entry it could have has no safe answer, but
		// it only costs the method its whole catalog entry when the method always
		// asks it. A `when`-gated one leaves the branch where it stays invisible
		// intact, which is the branch visibleAuthPromptKeys already resolves, and
		// authorize refuses every visible answer to it under the same rule.
		if rule, hostForming := authHostFormingRules[providerID][prompt.Key]; hostForming && len(rule.Domains) == 0 && prompt.When == nil {
			return nil, false, nil
		}

		converted, ok, err := convertAuthPrompt(providerID, prompt)
		if err != nil {
			return nil, false, err
		}

		if !ok {
			return nil, false, nil
		}

		prompts = append(prompts, converted)
	}

	return prompts, true, nil
}

func convertAuthPrompt(providerID string, prompt opencode.ProviderAuthPrompt) (authPrompt, bool, error) {
	message, ok := authDisplayText(prompt.Message, authMaxMessageBytes)
	if !ok {
		return authPrompt{}, false, nil
	}

	converted := authPrompt{Type: prompt.Type, Key: prompt.Key, Message: message}

	if prompt.Placeholder != "" {
		placeholder, ok := authDisplayText(prompt.Placeholder, authMaxMessageBytes)
		if !ok {
			return authPrompt{}, false, nil
		}

		converted.Placeholder = placeholder
	}

	if prompt.When != nil {
		converted.When = &authPromptWhen{Key: prompt.When.Key, Op: prompt.When.Op, Value: prompt.When.Value}
	}

	switch prompt.Type {
	case authPromptTypeText:
		if len(prompt.Options) > 0 {
			return authPrompt{}, false, authFailed(authCauseNativeVeto, providerID, "", "")
		}
	case authPromptTypeSelect:
		if len(prompt.Options) == 0 {
			return authPrompt{}, false, authFailed(authCauseNativeVeto, providerID, "", "")
		}

		values := make(map[string]struct{}, len(prompt.Options))

		for _, option := range prompt.Options {
			if option.Value == "" {
				return authPrompt{}, false, authFailed(authCauseNativeVeto, providerID, "", "")
			}

			if _, duplicate := values[option.Value]; duplicate {
				return authPrompt{}, false, authFailed(authCauseNativeVeto, providerID, "", "")
			}

			values[option.Value] = struct{}{}

			label, ok := authDisplayText(option.Label, authMaxLabelBytes)
			if !ok {
				return authPrompt{}, false, nil
			}

			hint := ""

			if option.Hint != "" {
				// A hint is display text like every other string beside it, and
				// an unnormalised unbounded one crosses the boundary carrying
				// whatever the catalog put there.
				hint, ok = authDisplayText(option.Hint, authMaxLabelBytes)
				if !ok {
					return authPrompt{}, false, nil
				}
			}

			converted.Options = append(converted.Options, authPromptOption{Label: label, Value: option.Value, Hint: hint})
		}
	default:
		return authPrompt{}, false, authFailed(authCauseNativeVeto, providerID, "", "")
	}

	return converted, true, nil
}

// authDisplayText normalises a native presentation string to NFC and measures
// its bounds and categories on that normalised form, which is also the form the
// adapter relays, persists, and returns. Normalising after measuring bounds a
// string nobody sends.
func authDisplayText(value string, maxBytes int) (string, bool) {
	normalized := norm.NFC.String(value)
	if normalized == "" || len(normalized) > maxBytes || !utf8.ValidString(normalized) {
		return "", false
	}

	for _, r := range normalized {
		if !authDisplayRune(r) {
			return "", false
		}
	}

	return normalized, true
}

// authDisplayRune restricts free text to Unicode categories L, N, P, S, and Zs.
// Every C* category is rejected, which is also what excludes every
// bidirectional override and embedding character: a label is the provider name
// in the one place a human decides which account to bind.
func authDisplayRune(r rune) bool {
	switch {
	case unicode.IsLetter(r), unicode.IsNumber(r), unicode.IsPunct(r), unicode.IsSymbol(r):
		return true
	case unicode.Is(unicode.Zs, r):
		return true
	default:
		return false
	}
}

// authDisplayURL applies the url bound: at most 2048 bytes, scheme exactly
// https, no userinfo, no fragment.
func authDisplayURL(value string) (string, bool) {
	normalized := norm.NFC.String(value)
	if normalized == "" || len(normalized) > authMaxURLBytes {
		return "", false
	}

	parsed, err := url.Parse(normalized)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Fragment != "" || parsed.Host == "" {
		return "", false
	}

	return normalized, true
}

// authDisplayUserCode applies the userCode bound with an anchored pattern.
func authDisplayUserCode(value string) (string, bool) {
	normalized := norm.NFC.String(value)
	if normalized == "" || len(normalized) > authMaxUserCodeBytes || !authUserCodePattern.MatchString(normalized) {
		return "", false
	}

	return normalized, true
}

// authLoopbackHost reports whether a minted authorization URL redirects through
// a loopback listener. Such a method is not brokered: the adapter cannot relay
// a URL whose completion lands on a socket the owner's browser cannot reach.
func authLoopbackHost(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}

	if isLoopbackHostname(parsed.Hostname()) {
		return true
	}

	redirect := parsed.Query().Get("redirect_uri")
	if redirect == "" {
		return false
	}

	target, err := url.Parse(redirect)
	if err != nil {
		return false
	}

	return isLoopbackHostname(target.Hostname())
}

func isLoopbackHostname(host string) bool {
	switch strings.ToLower(host) {
	case "127.0.0.1", "::1", "localhost":
		return true
	default:
		return strings.HasSuffix(strings.ToLower(host), ".localhost")
	}
}

// visibleAuthPromptKeys resolves which prompts a method actually asks, given
// the answers supplied so far. A prompt gated on an earlier answer is invisible
// while that condition does not hold.
func visibleAuthPromptKeys(prompts []authPrompt, inputs map[string]string) []string {
	visible := make([]string, 0, len(prompts))
	answered := make(map[string]string, len(prompts))

	for _, prompt := range prompts {
		if prompt.When != nil {
			value, ok := answered[prompt.When.Key]
			if !ok {
				continue
			}

			if (prompt.When.Op == authWhenOpEq) != (value == prompt.When.Value) {
				continue
			}
		}

		visible = append(visible, prompt.Key)

		if value, ok := inputs[prompt.Key]; ok {
			answered[prompt.Key] = value
		}
	}

	return visible
}

// validateAuthInputs checks the values of an authorize request, not merely its
// key set. A rejected value is never persisted, logged, or echoed.
func validateAuthInputs(providerID string, method authCatalogMethod, inputs map[string]string) error {
	if len(inputs) > authMaxInputsKeys {
		return invalidAuthField(authFieldInputs)
	}

	total := 0
	for key, value := range inputs {
		total += len(key) + len(value)
	}

	if total > authMaxInputsBytes {
		return invalidAuthField(authFieldInputs)
	}

	visible := visibleAuthPromptKeys(method.Prompts, inputs)
	if len(visible) != len(inputs) {
		return invalidAuthField(authFieldInputs)
	}

	byKey := make(map[string]authPrompt, len(method.Prompts))
	for _, prompt := range method.Prompts {
		byKey[prompt.Key] = prompt
	}

	for _, key := range visible {
		value, ok := inputs[key]
		if !ok {
			return invalidAuthField(authFieldInputs + "." + key)
		}

		if err := validateAuthInput(providerID, byKey[key], value); err != nil {
			return err
		}
	}

	return nil
}

func validateAuthInput(providerID string, prompt authPrompt, value string) error {
	path := authFieldInputs + "." + prompt.Key

	if prompt.Type == authPromptTypeSelect {
		for _, option := range prompt.Options {
			if option.Value == value {
				return nil
			}
		}

		return invalidAuthField(path)
	}

	if value == "" || len(value) > authMaxTextInputBytes || !utf8.ValidString(value) {
		return invalidAuthField(path)
	}

	for _, r := range value {
		if r == '\n' || r == '\r' || unicode.IsControl(r) {
			return invalidAuthField(path)
		}
	}

	if rule, hostForming := authHostFormingRules[providerID][prompt.Key]; hostForming {
		if !authHostAllowed(rule, value) {
			return invalidAuthField(path)
		}
	}

	return nil
}

// authHostAllowed validates the URL a host-forming answer produces: scheme
// exactly https, no userinfo, no fragment, port 443 or none, and a registrable
// domain on the provider's allowlist.
func authHostAllowed(rule authHostFormingRule, value string) bool {
	raw := value
	if rule.HostSuffix != "" {
		if strings.ContainsAny(value, "./:@?#") {
			return false
		}

		raw = "https://" + value + "." + rule.HostSuffix
	}

	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Fragment != "" {
		return false
	}

	if port := parsed.Port(); port != "" && port != "443" {
		return false
	}

	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return false
	}

	for _, domain := range rule.Domains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}

	return false
}
