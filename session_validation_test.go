package opencodeacp

import (
	"testing"

	"github.com/coder/acp-go-sdk"
)

func TestValidationHelperBranches(t *testing.T) {
	// A malformed start path is not an unsupported field: an absent path is
	// `required` and a relative one names the format it failed.
	requireInvalidParamsData(t, validateSessionStartPaths("relative", nil),
		map[string]any{jsonFieldError: errValueAbsolutePathRequired, jsonFieldField: jsonFieldCwd})
	requireInvalidParamsData(t, validateRequiredAbsolutePath(jsonFieldCwd, ""),
		map[string]any{jsonFieldCwd: validationRequired})
	requireInvalidParamsData(t, validateSessionStartPaths(absTestPath("tmp", "project"), []string{"relative"}),
		map[string]any{jsonFieldError: errValueAbsolutePathRequired, jsonFieldField: "additionalDirectories[0]"})
	value := absTestPath("tmp", "project")
	if err := validateOptionalAbsolutePath("cwd", &value); err != nil {
		t.Fatalf("validateOptionalAbsolutePath: %v", err)
	}
	// The MCP transport rejections keep their own shapes: the third `server`
	// key and the `no_transport` token are family-wide, not local spellings.
	requireInvalidParamsData(t, validateMCPServers([]acp.McpServer{{Sse: &acp.McpServerSseInline{Name: "sse"}}}),
		map[string]any{jsonFieldError: errValueUnsupported, jsonFieldField: "mcpServers[0]", jsonFieldServer: "sse"})
	requireInvalidParamsData(t, validateMCPServers([]acp.McpServer{{Acp: &acp.McpServerAcpInline{Name: "acp"}}}),
		map[string]any{jsonFieldError: errValueUnsupported, jsonFieldField: "mcpServers[0]", jsonFieldServer: "acp"})
	requireInvalidParamsData(t, validateMCPServers([]acp.McpServer{{}}),
		map[string]any{jsonFieldError: errValueNoTransport, jsonFieldField: "mcpServers[0]"})
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
}
