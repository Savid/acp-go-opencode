package opencodeacp

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAmbientEnvironmentSnapshotDropsAdapterPrivateCarriers proves the snapshot
// every ordinary launch and every provider-auth broker is built from never
// carries adapter-private state, under any spelling of the private prefix.
func TestAmbientEnvironmentSnapshotDropsAdapterPrivateCarriers(t *testing.T) {
	const privateCanary = privateAdapterEnvPrefix + "SPOOF"

	captured := ambientEnvironmentSnapshot([]string{
		privateCanary + "=leaked",
		strings.ToLower(privateCanary) + "=leaked",
		"KEPT=kept",
		"PATH=/usr/bin",
		"MALFORMED",
		"=empty-key",
	})

	require.Equal(t, map[string]string{"KEPT": "kept", "PATH": "/usr/bin"}, captured)
}

func TestWithAmbientEnvironmentReplacesTheAdapterEnvironment(t *testing.T) {
	original := captureAmbientEnvironment
	t.Cleanup(func() { captureAmbientEnvironment = original })

	captureAmbientEnvironment = func() []string {
		t.Error("the adapter environment was read despite a supplied ambient block")

		return []string{"PATH=/adapter/bin"}
	}

	agent := NewAgent(WithAmbientEnvironment(map[string]string{
		"HOME":                            "/host/home",
		"PATH":                            "/host/bin",
		privateAdapterEnvPrefix + "SPOOF": "leaked",
	}))
	require.NoError(t, agent.optionsErr)
	require.Equal(t, map[string]string{"HOME": "/host/home", "PATH": "/host/bin"}, agent.options.implicitEnvironment)

	require.Empty(t, NewAgent(WithAmbientEnvironment(map[string]string{})).options.implicitEnvironment)
}

func TestWithAmbientEnvironmentRefusesMalformedEntries(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"empty key":  {env: map[string]string{"": "x"}, want: `key "" is not a variable name`},
		"equals key": {env: map[string]string{"A=B": "x"}, want: `key "A=B" is not a variable name`},
		"nul key":    {env: map[string]string{"A\x00B": "x"}, want: "is not a variable name"},
		"nul value":  {env: map[string]string{"A": "x\x00y"}, want: `value for "A" contains NUL`},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorContains(t, NewAgent(WithAmbientEnvironment(tc.env)).optionsErr, tc.want)
		})
	}
}
