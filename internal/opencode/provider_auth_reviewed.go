package opencode

// Provider ids of the reviewed registry, as OpenCode spells them.
const (
	providerGitHubCopilot = "github-copilot"
	providerOpenAI        = "openai"
	providerXAI           = "xai"
)

// reviewedOAuthMethods names every native OAuth login method the adapter
// publishes, keyed by provider id and then by native method label. The catalog
// is an allowlist: a native OAuth method with no entry here is omitted, so a
// provider OpenCode adds or a method it renames stays unpublished until someone
// has read its plugin. API-key methods need no entry; they collect a secret
// over the adapter's own relay and open nothing.
//
// An entry certifies that the method's authorize step was read at the OpenCode
// version noted beside it and found to open no listener and exec no browser
// launcher before it returns: the credential arrives through the provider's own
// device or headless code flow, which OpenCode polls from inside the broker. A
// method that binds a callback listener while it mints cannot be refused after
// the fact, because the bind has already happened by the time the adapter sees
// the minted URL, so the one place the broker's no-listener property can be
// held is before the native call exists to make.
//
// The OAuth methods OpenCode 1.18.27 ships that bind while they mint, and are
// therefore absent here: OpenAI "ChatGPT Pro/Plus (browser)" and DigitalOcean
// "Login with DigitalOcean" listen on a fixed port on every interface of the
// worker host; GitLab "GitLab OAuth" listens on a fixed loopback port; Poe
// "Login with Poe (browser)" and Snowflake "Login with Snowflake (External
// Browser)" listen on an ephemeral loopback port. Every one of them also execs
// the platform browser launcher in the same call, which only the broker's own
// shim then stops.
var reviewedOAuthMethods = map[string][]string{
	// RFC 8628 device flow against the chosen GitHub deployment; 1.18.27.
	providerGitHubCopilot: {"Login with GitHub Copilot"},
	// Device-code flow against the ChatGPT device endpoint; 1.18.27.
	providerOpenAI: {"ChatGPT Pro/Plus (headless)"},
	// RFC 8628 device flow against accounts.x.ai; 1.18.27. It replaces the
	// "xAI Grok OAuth (Headless / Remote / VPS)" method reviewed at 1.18.5,
	// which no longer ships.
	providerXAI: {"SuperGrok Subscription"},
}

// ReviewedOAuthMethods returns the reviewed native OAuth methods by provider
// id. The result is a copy the caller may keep or mutate.
func ReviewedOAuthMethods() map[string][]string {
	methods := make(map[string][]string, len(reviewedOAuthMethods))

	for providerID, labels := range reviewedOAuthMethods {
		methods[providerID] = append([]string(nil), labels...)
	}

	return methods
}
