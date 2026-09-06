package opencodeacp

import (
	"testing"

	"github.com/coder/acp-go-sdk"
)

func TestValidationHelperBranches(t *testing.T) {
	// A start path that is not absolute takes one uniform verdict, and an empty
	// one is not absolute either: both name the field that carried the value.
	requireInvalidParamsData(t, validateSessionStartPaths("relative", nil),
		map[string]any{jsonFieldError: valUnsupported, jsonFieldField: jsonFieldCwd})
	requireInvalidParamsData(t, validateRequiredAbsolutePath(jsonFieldCwd, ""),
		map[string]any{jsonFieldError: valUnsupported, jsonFieldField: jsonFieldCwd})
	requireInvalidParamsData(t, validateSessionStartPaths(absTestPath("tmp", "project"), []string{"relative"}),
		map[string]any{jsonFieldError: valUnsupported, jsonFieldField: "additionalDirectories[0]"})
	value := absTestPath("tmp", "project")
	if err := validateOptionalAbsolutePath("cwd", &value); err != nil {
		t.Fatalf("validateOptionalAbsolutePath: %v", err)
	}
	// The MCP transport rejections keep their own shapes: the third `server`
	// key and the `no_transport` token are family-wide, not local spellings.
	requireInvalidParamsData(t, validateMCPServers([]acp.McpServer{{Sse: &acp.McpServerSseInline{Name: "sse"}}}),
		map[string]any{jsonFieldError: valUnsupported, jsonFieldField: "mcpServers[0]", jsonFieldServer: "sse"})
	requireInvalidParamsData(t, validateMCPServers([]acp.McpServer{{Acp: &acp.McpServerAcpInline{Name: "acp"}}}),
		map[string]any{jsonFieldError: valUnsupported, jsonFieldField: "mcpServers[0]", jsonFieldServer: "acp"})
	requireInvalidParamsData(t, validateMCPServers([]acp.McpServer{{}}),
		map[string]any{jsonFieldError: valNoTransport, jsonFieldField: "mcpServers[0]"})
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
