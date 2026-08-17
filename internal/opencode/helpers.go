package opencode

import "strings"

// firstNonEmpty returns the first non-empty string from values.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return ""
}

// Model returns the catalog record for the "provider/model" value.
func (p ProvidersResponse) Model(value string) (ProviderModel, bool) {
	providerID, modelID, ok := strings.Cut(value, "/")
	if !ok || providerID == "" || modelID == "" {
		return ProviderModel{}, false
	}

	for _, provider := range p.Providers {
		if provider.ID != providerID {
			continue
		}

		for key, model := range provider.Models {
			if firstNonEmpty(model.ID, key) == modelID {
				return model, true
			}
		}
	}

	return ProviderModel{}, false
}

// HasModel reports whether the provider catalog contains the "provider/model" value.
func (p ProvidersResponse) HasModel(value string) bool {
	_, ok := p.Model(value)

	return ok
}

// ModelContextWindow returns the "provider/model" model's context-window size
// in tokens, or (0, false) when the catalog does not advertise one.
func (p ProvidersResponse) ModelContextWindow(value string) (int, bool) {
	model, ok := p.Model(value)
	if !ok {
		return 0, false
	}

	return IntFromNumber(model.Limit["context"])
}
