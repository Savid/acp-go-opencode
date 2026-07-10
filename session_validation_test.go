package opencodeacp

import (
	"testing"

	"github.com/coder/acp-go-sdk"
)

func TestValidationHelperBranches(t *testing.T) {
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
}
