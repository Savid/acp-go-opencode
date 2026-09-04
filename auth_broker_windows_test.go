//go:build windows

package opencodeacp

import (
	"context"
	"os"
	"testing"

	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

// neutralizeBrowserShimWhereUnsupported supplies the shim the product refuses to
// build here. CreateProcess resolves the launchers a shim would shadow out of
// the system directory ahead of PATH, so opencode.NewBrowserShim fails closed on
// Windows and every ordinary provider-auth flow would stop at that refusal. The
// flows under test are about the flow machine rather than the shim, so the seam
// hands them a broker with no shim; the refusal itself is proved by
// TestStartBrokerRefusesWhereTheBrowserCannotBeNeutralised.
func neutralizeBrowserShimWhereUnsupported(t *testing.T) {
	t.Helper()

	original := brokerNewBrowserShim
	brokerNewBrowserShim = func(string) (*opencode.BrowserShim, error) { return nil, nil }

	t.Cleanup(func() { brokerNewBrowserShim = original })
}

// TestStartBrokerRefusesWhereTheBrowserCannotBeNeutralised is the Windows half
// of the shim contract: a leg that cannot prove the operator's browser stays
// shut never starts, and the broker home it had already created does not
// survive the refusal.
func TestStartBrokerRefusesWhereTheBrowserCannotBeNeutralised(t *testing.T) {
	harness := newAuthAgent(t)
	agent, broker := harness.agent, harness.broker
	withBrokerFactory(t, agent)
	restoreBrokerSeams(t)

	brokerNewBrowserShim = opencode.NewBrowserShim

	created, err := broker.startBroker(context.Background())
	require.Nil(t, created)
	require.ErrorContains(t, err, "neutralize provider auth broker browser launch")
	require.ErrorContains(t, err, "browser launch cannot be neutralised on this platform")

	entries, readErr := os.ReadDir(agent.scratchParent())
	require.NoError(t, readErr)

	for _, entry := range entries {
		require.NotContains(t, entry.Name(), authBrokerPrefix, "the refused broker left its home behind")
	}
}
