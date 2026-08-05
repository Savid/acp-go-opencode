package opencodeacp

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

func TestSessionMetaLifecycleBranches(t *testing.T) {
	if err := validateLifecycleMeta(map[string]any{opencodeMetaKey: "bad"}); err == nil {
		t.Fatal("bad opencode meta accepted")
	}
	if err := validateLifecycleMeta(map[string]any{"github.com/savid/acp-go-opencode": map[string]any{}}); err != nil {
		t.Fatalf("foreign module-path meta not ignored: %v", err)
	}
	if err := validateLifecycleMeta(map[string]any{opencodeMetaKey: map[string]any{metaOptionsKey: "bad"}}); err == nil {
		t.Fatal("bad options meta accepted")
	}
	if err := validateLifecycleMeta(map[string]any{opencodeMetaKey: map[string]any{rawEventKey: "bad"}}); err == nil {
		t.Fatal("bad raw event object accepted")
	}
	if err := validateLifecycleMeta(map[string]any{opencodeMetaKey: map[string]any{rawEventKey: map[string]any{"unknown": true}}}); err == nil {
		t.Fatal("unknown raw event key accepted")
	}
	if _, err := sessionMetaFromLifecycle(map[string]any{opencodeMetaKey: map[string]any{rawEventKey: map[string]any{rawEventEnabledKey: "bad"}}}); err == nil {
		t.Fatal("bad raw event meta accepted")
	}
	assertSessionMetaAndSchemaHelpers(t)
}

func assertSessionMetaAndSchemaHelpers(t *testing.T) {
	t.Helper()
	meta, err := sessionMetaFromLifecycle(map[string]any{opencodeMetaKey: map[string]any{metaOptionsKey: map[string]any{
		metaModelKey:      "p/m",
		metaModeKey:       "plan",
		metaPermissionKey: "ask",
	}}})
	if err != nil || meta.Model != "p/m" || meta.Mode != "plan" || meta.Permission != "ask" {
		t.Fatalf("session meta = %#v err=%v", meta, err)
	}
	meta, err = sessionMetaFromLifecycle(map[string]any{})
	if err != nil || meta.Permission != "ask" {
		t.Fatalf("default permission meta = %#v err=%v", meta, err)
	}
	if _, err := opencodeOptionsFromMeta(map[string]any{opencodeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaEnvKey: "bad"}}}); err == nil {
		t.Fatal("non-object per-session env meta accepted")
	}
	if _, err := opencodeOptionsFromMeta(map[string]any{opencodeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaPermissionKey: 1}}}); err == nil {
		t.Fatal("non-string permission meta accepted")
	}
	if _, err := sessionMetaFromLifecycle(map[string]any{opencodeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaPermissionKey: "maybe"}}}); err == nil {
		t.Fatal("unsupported permission meta accepted")
	}
	if _, err := sessionMetaFromLifecycle(map[string]any{opencodeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaEnvKey: "bad"}}}); err == nil {
		t.Fatal("bad env lifecycle meta accepted")
	}
	if _, err := opencodeOptionsFromMeta(map[string]any{opencodeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaOutputSchemaKey: map[string]any{"bad": func() {}}}}}); err == nil {
		t.Fatal("non-json output schema accepted")
	}
	if err := validateSchemaObject([]any{"bad"}); err == nil {
		t.Fatal("bad schema accepted")
	}
	if err := validateSchemaObject(map[string]any{}); err == nil {
		t.Fatal("empty schema accepted")
	}
	if got := cloneAny([]any{map[string]any{"a": "b"}}); !reflect.DeepEqual(got, []any{map[string]any{"a": "b"}}) {
		t.Fatalf("cloneAny slice = %#v", got)
	}
	if cloneAnySlice(nil) != nil {
		t.Fatal("nil cloneAnySlice returned non-nil")
	}
}

// Wrong-typed known option values are rejected with the uniform
// {"error":"unsupported","field":"<path>"} invalid-params data, never
// silently ignored.
func TestSessionMetaWrongTypedKnownOptionsRejected(t *testing.T) {
	for name, tc := range map[string]struct {
		value map[string]any
		field string
	}{
		"model":      {value: map[string]any{metaModelKey: 7}, field: "_meta.opencode.options.model"},
		"mode":       {value: map[string]any{metaModeKey: true}, field: "_meta.opencode.options.mode"},
		"permission": {value: map[string]any{metaPermissionKey: 1}, field: "_meta.opencode.options.permission"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := sessionMetaFromLifecycle(map[string]any{opencodeMetaKey: map[string]any{metaOptionsKey: tc.value}})
			if err == nil {
				t.Fatalf("wrong-typed %s option accepted", name)
			}
			requireInvalidParamsData(t, err, map[string]any{jsonFieldError: errValueUnsupported, jsonFieldField: tc.field})
		})
	}
}

// Malformed lifecycle _meta surfaces as invalid params (-32602) on every
// lifecycle route, never as an internal error.
func TestLifecycleMetaErrorsAreInvalidParams(t *testing.T) {
	if got := lifecycleMetaError(unsupportedField("_meta.opencode.x")); !reflect.DeepEqual(got, unsupportedField("_meta.opencode.x")) {
		t.Fatalf("request error not passed through: %#v", got)
	}

	wrapped := lifecycleMetaError(errors.New("_meta.opencode must be an object"))

	var reqErr *acp.RequestError
	if !errors.As(wrapped, &reqErr) || reqErr.Code != -32602 {
		t.Fatalf("wrapped error = %#v, want -32602", wrapped)
	}

	ctx := context.Background()
	badMeta := map[string]any{opencodeMetaKey: map[string]any{metaOptionsKey: "bad"}}

	agent := NewAgent()
	if _, err := agent.NewSession(ctx, NewSessionRequest(t.TempDir(), WithSessionMeta(badMeta))); err == nil {
		t.Fatal("new session with malformed meta succeeded")
	} else if !errors.As(err, &reqErr) || reqErr.Code != -32602 {
		t.Fatalf("new session malformed meta err = %#v, want -32602", err)
	}

	if _, err := agent.LoadSession(ctx, LoadSessionRequest("11111111-1111-4111-8111-111111111111", t.TempDir(), WithSessionMeta(badMeta))); err == nil {
		t.Fatal("load session with malformed meta succeeded")
	} else if !errors.As(err, &reqErr) || reqErr.Code != -32602 {
		t.Fatalf("load session malformed meta err = %#v, want -32602", err)
	}

	if _, err := agent.forkSession(ctx, acp.UnstableForkSessionRequest{SessionId: "parent", Cwd: t.TempDir(), Meta: badMeta}); err == nil {
		t.Fatal("fork session with malformed meta succeeded")
	} else if !errors.As(err, &reqErr) || reqErr.Code != -32602 {
		t.Fatalf("fork session malformed meta err = %#v, want -32602", err)
	}
}

// Per-session environment and search-path directories are parsed from either
// the JSON shape a stdio host sends or the Go shape an in-process host hands
// over, and every value a native process could not carry is refused.
func TestSessionEnvironmentMeta(t *testing.T) {
	options := func(values map[string]any) map[string]any {
		return map[string]any{opencodeMetaKey: map[string]any{metaOptionsKey: values}}
	}

	meta, err := sessionMetaFromLifecycle(options(map[string]any{
		metaEnvKey:           map[string]any{"HOST_API_URL": "http://127.0.0.1:9", "EMPTY": ""},
		metaExtraPathDirsKey: []any{"/session/bin", "/tools/bin"},
	}))
	require.NoError(t, err)
	require.Equal(t, map[string]string{"HOST_API_URL": "http://127.0.0.1:9", "EMPTY": ""}, meta.Env)
	require.Equal(t, []string{"/session/bin", "/tools/bin"}, meta.ExtraPathDirs)

	meta, err = sessionMetaFromLifecycle(options(map[string]any{
		metaEnvKey:           map[string]string{"HOST_API_TOKEN": "secret"},
		metaExtraPathDirsKey: []string{"/session/bin"},
	}))
	require.NoError(t, err)
	require.Equal(t, map[string]string{"HOST_API_TOKEN": "secret"}, meta.Env)
	require.Equal(t, []string{"/session/bin"}, meta.ExtraPathDirs)

	rejected := []struct {
		name   string
		values map[string]any
		field  string
	}{
		{"env is not an object", map[string]any{metaEnvKey: []any{"A=1"}}, envOptionPath},
		{"env value is not a string", map[string]any{metaEnvKey: map[string]any{"A": 1}}, envOptionPath + ".A"},
		{"env carries a search path", map[string]any{metaEnvKey: map[string]any{envPathKey: "/session/bin"}}, envOptionPath + ".PATH"},
		{"env carries a managed data root", map[string]any{metaEnvKey: map[string]any{managedEnvXDGDataHome: "/hostile"}}, envOptionPath + "." + managedEnvXDGDataHome},
		{"env carries a custom config root", map[string]any{metaEnvKey: map[string]any{managedEnvOpenCodeConfigDir: "/hostile"}}, envOptionPath + "." + managedEnvOpenCodeConfigDir},
		{"env carries a private adapter key", map[string]any{metaEnvKey: map[string]any{"ACP_GO_OPENCODE_INTERNAL_HELPER": "1"}}, envOptionPath + ".ACP_GO_OPENCODE_INTERNAL_HELPER"},
		{"env name is empty", map[string]any{metaEnvKey: map[string]any{"": "1"}}, envOptionPath + "."},
		{"env name carries a separator", map[string]any{metaEnvKey: map[string]any{"A=B": "1"}}, envOptionPath + ".A=B"},
		{"extra path dirs is not an array", map[string]any{metaExtraPathDirsKey: "/session/bin"}, extraPathDirsOptionPath},
		{"extra path dir is not a string", map[string]any{metaExtraPathDirsKey: []any{1}}, extraPathDirsOptionPath + "[0]"},
	}

	for _, test := range rejected {
		t.Run(test.name, func(t *testing.T) {
			_, rejectErr := sessionMetaFromLifecycle(options(test.values))
			require.Equal(t, unsupportedField(test.field), rejectErr)
		})
	}

	_, err = sessionMetaFromLifecycle(options(map[string]any{
		metaExtraPathDirsKey: []any{"/session/bin", "tools/bin"},
	}))
	require.Equal(t, acp.NewInvalidParams(map[string]any{
		jsonFieldError: errValueAbsolutePathRequired,
		jsonFieldField: extraPathDirsOptionPath + "[1]",
	}), err)
}
