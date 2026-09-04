package opencode

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReviewedOAuthMethodsIsWellFormed(t *testing.T) {
	methods := ReviewedOAuthMethods()
	require.NotEmpty(t, methods)

	for providerID, labels := range methods {
		require.NotEmpty(t, providerID)
		require.NotEmpty(t, labels, providerID)

		seen := make(map[string]struct{}, len(labels))

		for _, label := range labels {
			require.NotEmpty(t, label, providerID)
			require.NotContains(t, seen, label, providerID)

			seen[label] = struct{}{}
		}
	}
}

func TestReviewedOAuthMethodsReturnsACopy(t *testing.T) {
	first := ReviewedOAuthMethods()

	for providerID := range first {
		first[providerID] = append(first[providerID], "mutated")
		delete(first, providerID)

		break
	}

	first["added"] = []string{"added"}

	require.Equal(t, reviewedOAuthMethods, ReviewedOAuthMethods())
}
