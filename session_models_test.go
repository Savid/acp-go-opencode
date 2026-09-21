package opencodeacp

import (
	"testing"

	acp "github.com/coder/acp-go-sdk"
	acpcore "github.com/savid/acp-go-core"
	"github.com/savid/acp-go-core/wire"
	"github.com/stretchr/testify/require"
)

// commandCatalog returns the names of the first advertised command catalog.
func commandCatalog(t *testing.T, h *harness) []string {
	t.Helper()

	var names []string

	h.rec.waitFor(t, func(updates []acp.SessionNotification) bool {
		for _, update := range updates {
			catalog := update.Update.AvailableCommandsUpdate
			if catalog == nil {
				continue
			}

			names = make([]string, 0, len(catalog.AvailableCommands))
			for _, command := range catalog.AvailableCommands {
				names = append(names, command.Name)
			}

			return true
		}

		return false
	})

	return names
}

// configOption returns the advertised select with the given id.
func configOption(options []acp.SessionConfigOption, id acp.SessionConfigId) *acp.SessionConfigOptionSelect {
	for index := range options {
		if selected := options[index].Select; selected != nil && selected.Id == id {
			return selected
		}
	}

	return nil
}

func TestCommandCatalogIsSanitizedAndRoutes(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()
	session := h.newSession()

	require.Equal(t, []string{"inspect"}, commandCatalog(t, h),
		"only names the shared sanitizer accepts are advertised")

	_, err := h.prompt(session.SessionId, "/inspect the tree", nil)
	require.NoError(t, err)
	require.Contains(t, agentText(h.rec.snapshot()), "command:inspect args:the tree")

	_, err = h.prompt(session.SessionId, "/group/nested the tree", nil)
	require.NoError(t, err)
	require.Contains(t, agentText(h.rec.snapshot()), "hello /group/nested the tree",
		"a name the catalog filtered out is plain prompt text")
}

func TestSetConfigOptionAppliesAndRefuses(t *testing.T) {
	t.Parallel()

	store := acpcore.NewInMemorySessionStore()
	h := newHarness(t, WithSessionStore(store))
	h.initialize()

	session, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)

	models := configOption(session.ConfigOptions, configModel)
	require.NotNil(t, models)
	require.Equal(t, acp.SessionConfigValueId("fake/vision"), models.CurrentValue)
	require.NotNil(t, configOption(session.ConfigOptions, configMode))
	require.Nil(t, configOption(session.ConfigOptions, configEffort), "effort is advertised only while set")

	applied, err := h.conn.SetSessionConfigOption(h.ctx(), wire.SetConfigOptionRequest(session.SessionId, configModel, "fake/text"))
	require.NoError(t, err)
	require.Equal(t, acp.SessionConfigValueId("fake/text"), configOption(applied.ConfigOptions, configModel).CurrentValue)

	applied, err = h.conn.SetSessionConfigOption(h.ctx(), wire.SetConfigOptionRequest(session.SessionId, configEffort, "high"))
	require.NoError(t, err)

	effort := configOption(applied.ConfigOptions, configEffort)
	require.NotNil(t, effort)
	require.Equal(t, acp.SessionConfigValueId("high"), effort.CurrentValue)

	_, err = h.conn.SetSessionConfigOption(h.ctx(), wire.SetConfigOptionRequest(session.SessionId, "bogus", "x"))
	require.Equal(t, "configId", requestErrorData(t, err)["field"])

	_, err = h.conn.SetSessionConfigOption(h.ctx(), wire.SetConfigOptionRequest(session.SessionId, configModel, ""))
	require.Equal(t, fieldValue, requestErrorData(t, err)["field"])

	_, err = h.conn.SetSessionConfigOption(h.ctx(), wire.SetConfigOptionRequest(session.SessionId, configModel, "nomodel"))
	require.Equal(t, fieldValue, requestErrorData(t, err)["field"])

	require.Equal(t, "fake/text", storedRecord(t, store, session.SessionId).Model, "an applied value is committed")
}
