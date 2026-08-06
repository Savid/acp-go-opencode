//go:build linux

package opencode

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSupervisorRefusesAHalfProvidedCapabilityPairPlatformValidationDoesNot
// proves the supervisor makes its own check that the UID lock and the authority
// domain arrive together, independently of the isolation policy.
//
// On Linux validateProcessIsolation already refuses a half-provided pair through
// validateStandaloneIdentityDisposition, which runs only when the isolation
// policy's platform is Linux; everywhere else nothing upstream of
// supervisorCommand looks at the pair at all. Its own check is therefore the
// only thing standing between a caller and a supervisor started with a lock but
// no domain — one that would go on to record IdentityLock without
// AuthorityDomain and be refused much later, inside the child, by
// runSupervisor's consistency check.
//
// The case puts the isolation policy on a platform whose validator does not look
// at the pair, hands the supervisor each half in turn, and requires the
// supervisor's own refusal by its exact text, so a refusal that came from the
// isolation policy instead could not be mistaken for it. Nothing may be built
// before that refusal: no config descriptor is written, and no command comes
// back.
func TestSupervisorRefusesAHalfProvidedCapabilityPairPlatformValidationDoesNot(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		apply func(*ProcessIsolation)
	}{
		{
			name:  "lock without domain",
			apply: func(isolation *ProcessIsolation) { isolation.IdentityLock = duplicateSupervisorCapability{} },
		},
		{
			name:  "domain without lock",
			apply: func(isolation *ProcessIsolation) { isolation.AuthorityDomain = duplicateSupervisorCapability{} },
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			preserveSupervisorGlobals(t)
			platform := processIsolationGOOS
			processIsolationGOOS = "darwin"
			t.Cleanup(func() { processIsolationGOOS = platform })

			config := supervisorConfig{Scratch: t.TempDir(), Isolation: borrowedTestIsolation(testProcessIsolation())}
			testCase.apply(config.Isolation)
			require.NoError(t, validateProcessIsolation(config.Isolation),
				"the fixture must be one this platform's isolation policy accepts")

			written := 0
			supervisorWriteConfig = func(string, supervisorConfig) (*os.File, error) {
				written++

				return nil, errors.New("the supervisor must not reach the config write")
			}

			cmd, proof, err := supervisorCommand(context.Background(), config)
			require.EqualError(t, err, "OpenCode supervisor requires the UID lock and authority domain together")
			require.Nil(t, cmd)
			require.Nil(t, proof)
			require.Zero(t, written, "a half-provided capability pair must be refused before anything is built")
		})
	}
}
