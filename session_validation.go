package opencodeacp

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/coder/acp-go-sdk"
)

// optionFieldProviderAuthDirectHome names the exact-home consent gate in the
// uniform unsupported-option error. OpenCode removes a credential with
// DELETE /auth/{providerId}, a scoped per-provider call that consents to
// nothing beyond the slot it names, so there is no canonical home for the gate
// to authorize.
const optionFieldProviderAuthDirectHome = "providerAuthDirectHome"

func validateProviderAuthOptions(options Options) error {
	if options.ProviderAuthDirectHome == "" {
		return nil
	}

	return unsupportedField(optionFieldProviderAuthDirectHome)
}

func validateSessionStartPaths(cwd string, additionalDirectories []string) error {
	if err := validateRequiredAbsolutePath(jsonFieldCwd, cwd); err != nil {
		return err
	}

	for index, path := range additionalDirectories {
		if err := validateRequiredAbsolutePath(fmt.Sprintf("additionalDirectories[%d]", index), path); err != nil {
			return err
		}
	}

	return nil
}

func validateRequiredAbsolutePath(field string, value string) error {
	if value == "" {
		return acp.NewInvalidParams(map[string]any{field: validationRequired})
	}

	if !filepath.IsAbs(value) {
		return acp.NewInvalidParams(map[string]any{jsonFieldError: "absolute_path_required", jsonFieldField: field})
	}

	return nil
}

func validateOptionalAbsolutePath(field string, value *string) error {
	if value == nil {
		return nil
	}

	return validateRequiredAbsolutePath(field, *value)
}

func validateMCPServers(servers []acp.McpServer) error {
	seen := make(map[string]struct{}, len(servers))

	for index, server := range servers {
		var name string

		switch {
		case server.Stdio != nil:
			name = server.Stdio.Name
		case server.Http != nil:
			name = server.Http.Name
		case server.Sse != nil:
			return acp.NewInvalidParams(map[string]any{
				jsonFieldError:  errValueUnsupported,
				jsonFieldField:  fmt.Sprintf("mcpServers[%d]", index),
				jsonFieldServer: server.Sse.Name,
			})
		case server.Acp != nil:
			return acp.NewInvalidParams(map[string]any{
				jsonFieldError:  errValueUnsupported,
				jsonFieldField:  fmt.Sprintf("mcpServers[%d]", index),
				jsonFieldServer: server.Acp.Name,
			})
		default:
			return acp.NewInvalidParams(map[string]any{
				jsonFieldError: errValueNoTransport,
				jsonFieldField: fmt.Sprintf("mcpServers[%d]", index),
			})
		}

		if strings.TrimSpace(name) == "" {
			return acp.NewInvalidParams(map[string]any{
				fmt.Sprintf("mcpServers[%d].name", index): validationRequired,
			})
		}

		if _, ok := seen[name]; ok {
			return acp.NewInvalidParams(map[string]any{
				fmt.Sprintf("mcpServers[%d].name", index): validationDuplicate,
			})
		}

		seen[name] = struct{}{}
	}

	return nil
}

func normalizeConcurrencyLimits(limits ConcurrencyLimits) (ConcurrencyLimits, error) {
	if limits.MaxActiveSessions < 0 || limits.MaxConcurrentClientCalls < 0 {
		return limits, fmt.Errorf("concurrency limits must be non-negative")
	}

	if limits.MaxActiveSessions == 0 {
		limits.MaxActiveSessions = defaultMaxActiveSessions
	}

	if limits.MaxConcurrentClientCalls == 0 {
		limits.MaxConcurrentClientCalls = defaultMaxConcurrentClientCalls
	}

	return limits, nil
}
