package lifecycle

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLifecycleCapabilityStrictScalar(t *testing.T) {
	t.Parallel()

	var decoded Negotiated
	require.NoError(t, json.Unmarshal([]byte(`{"version":1}`), &decoded))
	require.Equal(t, Version, decoded.Version)

	for _, test := range []struct {
		name string
		data string
	}{
		{"empty", `{}`},
		{"missing", `{"updatesOutsidePrompt":true}`},
		{"other integer", `{"version":2}`},
		{"fractional", `{"version":1.0}`},
		{"string", `{"version":"1"}`},
		{"boolean", `{"version":true}`},
		{"duplicate", `{"version":1,"version":1}`},
		{"unknown", `{"version":1,"unknown":true}`},
		{"trailing", `{"version":1} {}`},
		{"empty input", ``},
		{"array", `[]`},
		{"truncated member", `{"`},
		{"truncated object", `{"version":1`},
		{"updates outside prompt type", `{"version":1,"updatesOutsidePrompt":0}`},
		{"authoritative quiescence type", `{"version":1,"authoritativeQuiescence":0}`},
		{"quiescence source type", `{"version":1,"quiescenceSource":true}`},
		{"activity kinds type", `{"version":1,"activityKinds":true}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var value Negotiated
			require.Error(t, json.Unmarshal([]byte(test.data), &value))
		})
	}
}

func TestLifecycleCapabilityDirectDecoderFailures(t *testing.T) {
	t.Parallel()

	for _, data := range []string{
		`{"`,
		`{"version":1`,
		`{"version":1} {}`,
	} {
		var decoded Negotiated
		require.Error(t, decoded.UnmarshalJSON([]byte(data)))
	}
}
