package opencodeacp

import (
	"reflect"
	"testing"

	"github.com/coder/acp-go-sdk"
)

func TestValidationMetaAndHelperBranches(t *testing.T) {
	if err := validateSessionStartPaths("relative", nil); err == nil {
		t.Fatal("relative cwd accepted")
	}
	if err := validateRequiredAbsolutePath("cwd", ""); err == nil {
		t.Fatal("empty required absolute path accepted")
	}
	if err := validateSessionStartPaths("/tmp/project", []string{"relative"}); err == nil {
		t.Fatal("relative additional directory accepted")
	}
	value := "/tmp/project"
	if err := validateOptionalAbsolutePath("cwd", &value); err != nil {
		t.Fatalf("validateOptionalAbsolutePath: %v", err)
	}
	if err := validateMCPServers([]acp.McpServer{{Sse: &acp.McpServerSseInline{Name: "sse"}}}); err == nil {
		t.Fatal("unsupported MCP servers accepted")
	}
	if err := validateMCPServers([]acp.McpServer{{Acp: &acp.McpServerAcpInline{Name: "acp"}}}); err == nil {
		t.Fatal("unsupported ACP MCP server accepted")
	}
	if err := validateMCPServers([]acp.McpServer{{}}); err == nil {
		t.Fatal("empty MCP server accepted")
	}
	if err := validateMCPServers([]acp.McpServer{
		{Http: &acp.McpServerHttpInline{Name: "http", Url: "https://mcp.example"}},
		{Stdio: &acp.McpServerStdio{Name: "stdio", Command: "server"}},
	}); err != nil {
		t.Fatalf("valid MCP servers rejected: %v", err)
	}
	if err := validateMCPServers([]acp.McpServer{{Http: &acp.McpServerHttpInline{Name: "", Url: "https://mcp.example"}}}); err == nil {
		t.Fatal("empty-name MCP server accepted")
	} else {
		requireInvalidParamsData(t, err, map[string]any{"mcpServers[0].name": "required"})
	}
	if err := validateMCPServers([]acp.McpServer{{Http: &acp.McpServerHttpInline{Name: "   ", Url: "https://mcp.example"}}}); err == nil {
		t.Fatal("whitespace-only-name MCP server accepted")
	} else {
		requireInvalidParamsData(t, err, map[string]any{"mcpServers[0].name": "required"})
	}
	if err := validateMCPServers([]acp.McpServer{
		{Http: &acp.McpServerHttpInline{Name: "dup", Url: "https://mcp.example"}},
		{Stdio: &acp.McpServerStdio{Name: "dup", Command: "server"}},
	}); err == nil {
		t.Fatal("duplicate-name MCP servers accepted")
	} else {
		requireInvalidParamsData(t, err, map[string]any{"mcpServers[1].name": "duplicate"})
	}
	if _, err := normalizeConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: -1}); err == nil {
		t.Fatal("negative concurrency accepted")
	}
	if _, err := stringMapFromMeta(map[string]any{"A": 1}); err == nil {
		t.Fatal("non-string env accepted")
	}
	if env, err := stringMapFromMeta(map[string]string{"A": "1"}); err != nil || env["A"] != "1" {
		t.Fatalf("stringMapFromMeta map[string]string = %#v err=%v", env, err)
	}
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
		metaEnvKey:        map[string]any{"A": "1"},
		metaModeKey:       "plan",
		metaPermissionKey: "ask",
	}}})
	if err != nil || meta.Model != "p/m" || meta.Env["A"] != "1" || meta.Mode != "plan" || meta.Permission != "ask" {
		t.Fatalf("session meta = %#v err=%v", meta, err)
	}
	meta, err = sessionMetaFromLifecycle(map[string]any{})
	if err != nil || meta.Permission != "ask" {
		t.Fatalf("default permission meta = %#v err=%v", meta, err)
	}
	if _, err := opencodeOptionsFromMeta(map[string]any{opencodeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaEnvKey: "bad"}}}); err == nil {
		t.Fatal("bad env meta accepted")
	}
	if _, err := opencodeOptionsFromMeta(map[string]any{opencodeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaPermissionKey: 1}}}); err == nil {
		t.Fatal("non-string permission meta accepted")
	}
	if _, err := sessionMetaFromLifecycle(map[string]any{opencodeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaPermissionKey: "deny"}}}); err == nil {
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
	if splitProvider, splitModel := splitModelValue("model-only", "p", "m"); splitProvider != "p" || splitModel != "model-only" {
		t.Fatalf("split fallback = %q %q", splitProvider, splitModel)
	}
	if joinModelValue("", "m") != "m" || joinModelValue("p", "") != "p" {
		t.Fatal("joinModelValue fallback mismatch")
	}
	if titleASCII("") != "" || titleASCII("plan") != "Plan" {
		t.Fatal("titleASCII mismatch")
	}
}
