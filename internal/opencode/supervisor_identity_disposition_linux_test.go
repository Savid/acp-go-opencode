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
// proves Linux refuses either incomplete borrowed-authority shape before the
// supervisor builds anything. The policy validator is the one canonical gate:
// supervisorCommand must return its exact verdict without writing a config or
// constructing a command.
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
			require.Equal(t, processIsolationLinux, processIsolationGOOS,
				"the Linux-only fixture must exercise the real Linux policy validator")

			config := supervisorConfig{Scratch: t.TempDir(), Isolation: borrowedTestIsolation(testProcessIsolation())}
			testCase.apply(config.Isolation)
			require.EqualError(t, validateProcessIsolation(config.Isolation),
				"process identity lock and authority domain must be provided together")

			written := 0
			supervisorWriteConfig = func(string, supervisorConfig) (*os.File, error) {
				written++

				return nil, errors.New("the supervisor must not reach the config write")
			}

			cmd, proof, err := supervisorCommand(context.Background(), config)
			require.EqualError(t, err, "process identity lock and authority domain must be provided together")
			require.Nil(t, cmd)
			require.Nil(t, proof)
			require.Zero(t, written, "a half-provided capability pair must be refused before anything is built")
		})
	}
}
