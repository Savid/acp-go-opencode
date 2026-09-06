package opencodeacp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func simulateSessionEnvPlatform(t *testing.T, platform string) {
	t.Helper()

	previous := sessionEnvPlatform
	t.Cleanup(func() { sessionEnvPlatform = previous })

	sessionEnvPlatform = platform
}

func TestSessionEnvAcceptsEveryStructurallyValidName(t *testing.T) {
	simulateSessionEnvPlatform(t, "linux")

	env := map[string]any{
		"https_proxy":     "",
		"no_proxy":        "",
		"WAGIE_API_URL":   "http://127.0.0.1:1",
		"BASH_FUNC_x%%":   "() { :; }",
		"path":            "/not/the/search/path",
		"env":             "/not/the/shell/init",
		"ld_preload":      "/not/the/loader",
		"home":            "/not/the/managed/root",
		"opencode_config": "/not/the/managed/root",
	}

	meta, err := sessionMetaFromVendorOptions(carrierOptions(map[string]any{metaEnvKey: env}))
	require.NoError(t, err)
	require.Len(t, meta.Env, len(env))
	require.Equal(t, "", meta.Env["https_proxy"])
	require.NoError(t, ValidateOpenCodeSessionMeta(carrierOptions(map[string]any{metaEnvKey: env})))
}

func TestSessionEnvRefusesBlockedNamesUnderThePlatformIdentity(t *testing.T) {
	blocked := []string{
		envPathKey, "NODE_OPTIONS", "BASH_ENV", "ENV",
		"LD_PRELOAD", "DYLD_INSERT_LIBRARIES",
		"HOME", "XDG_CONFIG_HOME", envOpenCodeConfigDirKey, envOpenCodeDBKey,
	}

	for _, platform := range []string{"linux", platformWindows} {
		simulateSessionEnvPlatform(t, platform)

		for _, key := range blocked {
			_, err := sessionMetaFromVendorOptions(carrierOptions(map[string]any{metaEnvKey: map[string]any{key: "x"}}))
			require.Equal(t, unsupportedField(envOptionPath+"."+key), err)
		}

		for _, key := range []string{privateAdapterEnvPrefix + "TOKEN", "acp_go_opencode_internal_token"} {
			_, err := sessionMetaFromVendorOptions(carrierOptions(map[string]any{metaEnvKey: map[string]any{key: "x"}}))
			require.Equal(t, unsupportedField(envOptionPath+"."+key), err)
		}
	}

	simulateSessionEnvPlatform(t, platformWindows)

	for _, key := range []string{"Node_Options", "ld_preload", "home", "opencode_db", "xdg_state_home"} {
		_, err := sessionMetaFromVendorOptions(carrierOptions(map[string]any{metaEnvKey: map[string]any{key: "x"}}))
		require.Equal(t, unsupportedField(envOptionPath+"."+key), err)
	}
}

func TestSessionEnvRefusesAValueCarryingANUL(t *testing.T) {
	_, err := sessionMetaFromVendorOptions(carrierOptions(map[string]any{metaEnvKey: map[string]any{"A": "x\x00y"}}))
	require.Equal(t, unsupportedField(envOptionPath+".A"), err)
}

func TestSessionEnvReportsTheFirstKeyInSortedOrder(t *testing.T) {
	simulateSessionEnvPlatform(t, "linux")

	_, err := sessionMetaFromVendorOptions(carrierOptions(map[string]any{metaEnvKey: map[string]any{
		"ZZ_LAST":   "x\x00y",
		"AA_FIRST=": "x",
		"MM_MID":    "x",
	}}))
	require.Equal(t, unsupportedField(envOptionPath+".AA_FIRST="), err)
}

func TestSessionEnvRefusesTwoSpellingsOfOneWindowsVariable(t *testing.T) {
	env := map[string]any{"Https_Proxy": "a", "https_proxy": "b"}

	simulateSessionEnvPlatform(t, "linux")

	meta, err := sessionMetaFromVendorOptions(carrierOptions(map[string]any{metaEnvKey: env}))
	require.NoError(t, err)
	require.Len(t, meta.Env, 2)

	simulateSessionEnvPlatform(t, platformWindows)

	_, err = sessionMetaFromVendorOptions(carrierOptions(map[string]any{metaEnvKey: env}))
	require.Equal(t, ambiguousField(envOptionPath+".https_proxy"), err)
	require.Equal(t, ambiguousField(envOptionPath+".https_proxy"), ValidateOpenCodeSessionMeta(carrierOptions(map[string]any{metaEnvKey: env})))
}

func TestValidateOpenCodeSessionMetaMirrorsTheSessionParser(t *testing.T) {
	require.NoError(t, ValidateOpenCodeSessionMeta(nil))
	require.NoError(t, ValidateOpenCodeSessionMeta(NewOpenCodeOptions(
		WithOpenCodeEnv(map[string]string{"https_proxy": "", "WAGIE_API_TOKEN": "bearer"}),
		WithOpenCodeExtraPathDirs(absTestPath("session", "bin")),
	).Meta()))
	require.Equal(t, unsupportedField(envOptionPath+"."+envPathKey), ValidateOpenCodeSessionMeta(carrierOptions(map[string]any{metaEnvKey: map[string]any{envPathKey: "/bin"}})))
	require.Equal(t, unsupportedField(extraPathDirsOptionPath+"[0]"), ValidateOpenCodeSessionMeta(carrierOptions(map[string]any{metaExtraPathDirsKey: []any{"relative"}})))
}
