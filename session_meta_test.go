package opencodeacp

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

func TestSessionMetaLifecycleBranches(t *testing.T) {
	if err := validateVendorOptionsMeta(map[string]any{opencodeMetaKey: "bad"}); err == nil {
		t.Fatal("bad opencode meta accepted")
	}
	if err := validateVendorOptionsMeta(map[string]any{"github.com/savid/acp-go-opencode": map[string]any{}}); err != nil {
		t.Fatalf("foreign module-path meta not ignored: %v", err)
	}
	if err := validateVendorOptionsMeta(map[string]any{opencodeMetaKey: map[string]any{metaOptionsKey: "bad"}}); err == nil {
		t.Fatal("bad options meta accepted")
	}
	if err := validateVendorOptionsMeta(map[string]any{opencodeMetaKey: map[string]any{rawEventKey: "bad"}}); err == nil {
		t.Fatal("bad raw event object accepted")
	}
	if err := validateVendorOptionsMeta(map[string]any{opencodeMetaKey: map[string]any{rawEventKey: map[string]any{"unknown": true}}}); err == nil {
		t.Fatal("unknown raw event key accepted")
	}
	if _, err := sessionMetaFromVendorOptions(map[string]any{opencodeMetaKey: map[string]any{rawEventKey: map[string]any{rawEventEnabledKey: "bad"}}}); err == nil {
		t.Fatal("bad raw event meta accepted")
	}
	assertSessionMetaAndSchemaHelpers(t)
}

func assertSessionMetaAndSchemaHelpers(t *testing.T) {
	t.Helper()
	meta, err := sessionMetaFromVendorOptions(map[string]any{opencodeMetaKey: map[string]any{metaOptionsKey: map[string]any{
		metaModelKey:      "p/m",
		metaModeKey:       "plan",
		metaPermissionKey: "ask",
	}}})
	if err != nil || meta.Model != "p/m" || meta.Mode != "plan" || meta.Permission != "ask" {
		t.Fatalf("session meta = %#v err=%v", meta, err)
	}
	meta, err = sessionMetaFromVendorOptions(map[string]any{})
	if err != nil || meta.Permission != "ask" {
		t.Fatalf("default permission meta = %#v err=%v", meta, err)
	}
	if _, err := opencodeOptionsFromMeta(map[string]any{opencodeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaPermissionKey: 1}}}); err == nil {
		t.Fatal("non-string permission meta accepted")
	}
	if _, err := sessionMetaFromVendorOptions(map[string]any{opencodeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaPermissionKey: "maybe"}}}); err == nil {
		t.Fatal("unsupported permission meta accepted")
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
			_, err := sessionMetaFromVendorOptions(map[string]any{opencodeMetaKey: map[string]any{metaOptionsKey: tc.value}})
			if err == nil {
				t.Fatalf("wrong-typed %s option accepted", name)
			}
			requireInvalidParamsData(t, err, map[string]any{jsonFieldError: valUnsupported, jsonFieldField: tc.field})
		})
	}
}

// A malformed member of the owned `_meta.opencode` namespace is refused with
// the same {"error":"unsupported","field":"<request path>"} data an unknown
// member gets. A host switching on `error` and reading `field` must never have
// to fall back to prose, and a wrong-typed container is exactly as nameable as
// a wrong-typed leaf.
func TestVendorMetaMalformedMembersNameTheirPath(t *testing.T) {
	for name, tc := range map[string]struct {
		meta  map[string]any
		field string
	}{
		"vendor namespace not an object": {
			meta:  map[string]any{opencodeMetaKey: 5},
			field: "_meta.opencode",
		},
		"options not an object": {
			meta:  map[string]any{opencodeMetaKey: map[string]any{metaOptionsKey: 5}},
			field: "_meta.opencode.options",
		},
		"options is a string": {
			meta:  map[string]any{opencodeMetaKey: map[string]any{metaOptionsKey: "bad"}},
			field: "_meta.opencode.options",
		},
		"rawEvent not an object": {
			meta:  map[string]any{opencodeMetaKey: map[string]any{rawEventKey: 5}},
			field: "_meta.opencode.rawEvent",
		},
		"rawEvent.enabled not a boolean": {
			meta:  map[string]any{opencodeMetaKey: map[string]any{rawEventKey: map[string]any{rawEventEnabledKey: "yes"}}},
			field: "_meta.opencode.rawEvent.enabled",
		},
		"unknown rawEvent member": {
			meta:  map[string]any{opencodeMetaKey: map[string]any{rawEventKey: map[string]any{"nope": true}}},
			field: "_meta.opencode.rawEvent.nope",
		},
		"unknown vendor member": {
			meta:  map[string]any{opencodeMetaKey: map[string]any{"nope": true}},
			field: "_meta.opencode.nope",
		},
		"unknown option": {
			meta:  map[string]any{opencodeMetaKey: map[string]any{metaOptionsKey: map[string]any{"bogus": 1}}},
			field: "_meta.opencode.options.bogus",
		},
		"output schema not an object": {
			meta:  map[string]any{opencodeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaOutputSchemaKey: 7}}},
			field: "_meta.opencode.options.outputSchema",
		},
		"output schema empty": {
			meta:  map[string]any{opencodeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaOutputSchemaKey: map[string]any{}}}},
			field: "_meta.opencode.options.outputSchema",
		},
		"output schema not serializable": {
			meta:  map[string]any{opencodeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaOutputSchemaKey: map[string]any{"bad": make(chan int)}}}},
			field: "_meta.opencode.options.outputSchema",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := sessionMetaFromVendorOptions(tc.meta)
			if err == nil {
				t.Fatalf("malformed %s accepted", name)
			}
			requireInvalidParamsData(t, err, map[string]any{
				jsonFieldError: valUnsupported,
				jsonFieldField: tc.field,
			})
		})
	}
}

// Malformed lifecycle _meta surfaces as invalid params (-32602) on every
// lifecycle route, never as an internal error, and every route names the same
// field path.
func TestLifecycleMetaErrorsAreInvalidParams(t *testing.T) {
	var reqErr *acp.RequestError

	ctx := context.Background()
	badMeta := map[string]any{opencodeMetaKey: map[string]any{metaOptionsKey: "bad"}}

	agent := NewAgent()
	if _, err := agent.NewSession(ctx, NewSessionRequest(t.TempDir(), WithSessionMeta(badMeta))); err == nil {
		t.Fatal("new session with malformed meta succeeded")
	} else if !errors.As(err, &reqErr) || reqErr.Code != -32602 {
		t.Fatalf("new session malformed meta err = %#v, want -32602", err)
	} else {
		requireInvalidParamsData(t, err, uniformOptionsRefusal)
	}

	if _, err := agent.LoadSession(ctx, LoadSessionRequest("11111111-1111-4111-8111-111111111111", t.TempDir(), WithSessionMeta(badMeta))); err == nil {
		t.Fatal("load session with malformed meta succeeded")
	} else if !errors.As(err, &reqErr) || reqErr.Code != -32602 {
		t.Fatalf("load session malformed meta err = %#v, want -32602", err)
	} else {
		requireInvalidParamsData(t, err, uniformOptionsRefusal)
	}

	if _, err := agent.forkSession(ctx, acp.UnstableForkSessionRequest{SessionId: "parent", Cwd: t.TempDir(), Meta: badMeta}); err == nil {
		t.Fatal("fork session with malformed meta succeeded")
	} else if !errors.As(err, &reqErr) || reqErr.Code != -32602 {
		t.Fatalf("fork session malformed meta err = %#v, want -32602", err)
	} else {
		requireInvalidParamsData(t, err, uniformOptionsRefusal)
	}
}

// uniformOptionsRefusal is the data every lifecycle route answers for the same
// malformed `_meta.opencode.options`.
var uniformOptionsRefusal = map[string]any{
	jsonFieldError: valUnsupported,
	jsonFieldField: "_meta.opencode.options",
}

func TestSessionExtraPathDirsMeta(t *testing.T) {
	options := func(values map[string]any) map[string]any {
		return map[string]any{opencodeMetaKey: map[string]any{metaOptionsKey: values}}
	}

	meta, err := sessionMetaFromVendorOptions(options(map[string]any{
		metaExtraPathDirsKey: []any{absTestPath("session", "bin"), absTestPath("tools", "bin")},
	}))
	require.NoError(t, err)
	require.Equal(t, []string{absTestPath("session", "bin"), absTestPath("tools", "bin")}, meta.ExtraPathDirs)
	require.True(t, meta.ExtraPathDirsSet)

	meta, err = sessionMetaFromVendorOptions(options(map[string]any{
		metaExtraPathDirsKey: []string{absTestPath("session", "bin")},
	}))
	require.NoError(t, err)
	require.Equal(t, []string{absTestPath("session", "bin")}, meta.ExtraPathDirs)

	rejected := []struct {
		name   string
		values map[string]any
		field  string
	}{
		{"extra path dirs is not an array", map[string]any{metaExtraPathDirsKey: absTestPath("session", "bin")}, extraPathDirsOptionPath},
		{"extra path dir is not a string", map[string]any{metaExtraPathDirsKey: []any{1}}, extraPathDirsOptionPath + "[0]"},
	}

	for _, test := range rejected {
		t.Run(test.name, func(t *testing.T) {
			_, rejectErr := sessionMetaFromVendorOptions(options(test.values))
			require.Equal(t, unsupportedField(test.field), rejectErr)
		})
	}

	_, err = sessionMetaFromVendorOptions(options(map[string]any{
		metaExtraPathDirsKey: []any{absTestPath("session", "bin"), "tools/bin"},
	}))
	require.Equal(t, unsupportedField(extraPathDirsOptionPath+"[1]"), err)

	for _, value := range []string{"", absTestPath("session", "bin") + string(os.PathListSeparator) + absTestPath("tools", "bin")} {
		_, err = sessionMetaFromVendorOptions(options(map[string]any{metaExtraPathDirsKey: []any{value}}))
		require.Equal(t, unsupportedField(extraPathDirsOptionPath+"[0]"), err)
	}
}
