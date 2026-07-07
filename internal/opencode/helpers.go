package opencode

import (
	"fmt"
	"strings"
)

const (
	permissionAsk   = "ask"
	permissionAllow = "allow"
)

// firstNonEmpty returns the first non-empty string from values.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return ""
}

// validateOpenCodePermission rejects permission values the native config cannot express.
func validateOpenCodePermission(permission string) error {
	switch permission {
	case "", permissionAsk, permissionAllow:
		return nil
	default:
		return fmt.Errorf("unsupported opencode permission %q", permission)
	}
}

// normalizeOpenCodePermission maps the empty permission to the native default.
func normalizeOpenCodePermission(permission string) string {
	if permission == "" {
		return permissionAsk
	}

	return permission
}

// HasModel reports whether the provider catalog contains the "provider/model" value.
func (p ProvidersResponse) HasModel(value string) bool {
	providerID, modelID, ok := strings.Cut(value, "/")
	if !ok || providerID == "" || modelID == "" {
		return false
	}

	for _, provider := range p.Providers {
		if provider.ID != providerID {
			continue
		}

		for key, model := range provider.Models {
			if firstNonEmpty(model.ID, key) == modelID {
				return true
			}
		}
	}

	return false
}

// ModelContextWindow returns the "provider/model" model's context-window size
// in tokens, or (0, false) when the catalog does not advertise one.
func (p ProvidersResponse) ModelContextWindow(value string) (int, bool) {
	providerID, modelID, ok := strings.Cut(value, "/")
	if !ok || providerID == "" || modelID == "" {
		return 0, false
	}

	for _, provider := range p.Providers {
		if provider.ID != providerID {
			continue
		}

		for key, model := range provider.Models {
			if firstNonEmpty(model.ID, key) == modelID {
				return IntFromNumber(model.Limit["context"])
			}
		}
	}

	return 0, false
}
