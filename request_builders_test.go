package opencodeacp

import (
	"reflect"
	"testing"

	"github.com/coder/acp-go-sdk"
)

func TestSessionRequestBuilders(t *testing.T) {
	t.Parallel()

	meta := map[string]any{
		"nested": map[string]any{"left": "old"},
		"list":   []any{"a"},
	}
	req := NewSessionRequest("/repo",
		WithSessionAdditionalDirectories("/one", "/two"),
		WithSessionMeta(meta),
		WithSessionMeta(map[string]any{
			"nested": map[string]any{"right": "new"},
		}),
	)

	if req.Cwd != "/repo" {
		t.Fatalf("cwd = %q", req.Cwd)
	}
	if req.McpServers == nil {
		t.Fatal("McpServers is nil")
	}
	if !reflect.DeepEqual(req.AdditionalDirectories, []string{"/one", "/two"}) {
		t.Fatalf("AdditionalDirectories = %#v", req.AdditionalDirectories)
	}
	wantMeta := map[string]any{
		"nested": map[string]any{"left": "old", "right": "new"},
		"list":   []any{"a"},
	}
	if !reflect.DeepEqual(req.Meta, wantMeta) {
		t.Fatalf("Meta = %#v, want %#v", req.Meta, wantMeta)
	}

	meta["nested"].(map[string]any)["left"] = "mutated"
	meta["list"].([]any)[0] = "mutated"
	if !reflect.DeepEqual(req.Meta, wantMeta) {
		t.Fatalf("Meta was not cloned: %#v", req.Meta)
	}

	sessionID := acp.SessionId("session-1")
	if got := LoadSessionRequest(sessionID, "/repo").SessionId; got != sessionID {
		t.Fatalf("load session id = %q", got)
	}
	if got := ResumeSessionRequest(sessionID, "/repo").SessionId; got != sessionID {
		t.Fatalf("resume session id = %q", got)
	}
	if got := ForkSessionRequest(sessionID, "/repo").SessionId; got != sessionID {
		t.Fatalf("fork session id = %q", got)
	}
}

func TestPromptAndListRequestBuilders(t *testing.T) {
	t.Parallel()

	prompt := TextPromptRequest("session-1", "hello")
	if prompt.SessionId != "session-1" || len(prompt.Prompt) != 1 || prompt.Prompt[0].Text.Text != "hello" {
		t.Fatalf("TextPromptRequest = %#v", prompt)
	}

	custom := PromptRequest("session-1")
	if custom.Prompt == nil {
		t.Fatal("PromptRequest prompt is nil")
	}

	list := ListSessionsRequest(
		WithListSessionsCwd("/repo"),
		WithListSessionsCursor("cursor"),
		WithListSessionsMeta(map[string]any{"k": "v"}),
	)
	if list.Cwd == nil || *list.Cwd != "/repo" {
		t.Fatalf("cwd = %#v", list.Cwd)
	}
	if list.Cursor == nil || *list.Cursor != "cursor" {
		t.Fatalf("cursor = %#v", list.Cursor)
	}
	if !reflect.DeepEqual(list.Meta, map[string]any{"k": "v"}) {
		t.Fatalf("meta = %#v", list.Meta)
	}
}

func TestRequestBuilderMCPClone(t *testing.T) {
	t.Parallel()

	servers := []acp.McpServer{
		{
			Stdio: &acp.McpServerStdio{
				Meta:    map[string]any{"nested": map[string]any{"kind": "stdio"}},
				Name:    "tools",
				Command: "tool",
				Args:    []string{"one"},
				Env: []acp.EnvVariable{{
					Meta:  map[string]any{"nested": map[string]any{"kind": "stdio-env"}},
					Name:  "A",
					Value: "1",
				}},
			},
		},
		{
			Http: &acp.McpServerHttpInline{
				Meta: map[string]any{"nested": map[string]any{"kind": "http"}},
				Headers: []acp.HttpHeader{{
					Meta:  map[string]any{"nested": map[string]any{"kind": "http-header"}},
					Name:  "X-HTTP",
					Value: "1",
				}},
				Name: "remote",
				Type: "http",
				Url:  "https://mcp.example.test",
			},
		},
		{
			Sse: &acp.McpServerSseInline{
				Meta: map[string]any{"nested": map[string]any{"kind": "sse"}},
				Headers: []acp.HttpHeader{{
					Meta:  map[string]any{"nested": map[string]any{"kind": "sse-header"}},
					Name:  "X-SSE",
					Value: "1",
				}},
				Name: "events",
				Type: "sse",
				Url:  "https://mcp.example.test/sse",
			},
		},
		{
			Acp: &acp.McpServerAcpInline{
				Meta: map[string]any{"nested": map[string]any{"kind": "acp"}},
				Id:   acp.McpServerAcpId("bridge-1"),
				Name: "bridge",
				Type: "acp",
			},
		},
	}

	req := NewSessionRequest("/repo", WithSessionMCPServers(servers...))
	servers[0].Stdio.Args[0] = "mutated"
	servers[0].Stdio.Env[0].Value = "mutated"
	servers[0].Stdio.Env[0].Meta["nested"].(map[string]any)["kind"] = "mutated"
	servers[0].Stdio.Meta["nested"].(map[string]any)["kind"] = "mutated"
	servers[1].Http.Headers[0].Value = "mutated"
	servers[1].Http.Headers[0].Meta["nested"].(map[string]any)["kind"] = "mutated"
	servers[1].Http.Meta["nested"].(map[string]any)["kind"] = "mutated"
	servers[2].Sse.Headers[0].Value = "mutated"
	servers[2].Sse.Headers[0].Meta["nested"].(map[string]any)["kind"] = "mutated"
	servers[2].Sse.Meta["nested"].(map[string]any)["kind"] = "mutated"
	servers[3].Acp.Meta["nested"].(map[string]any)["kind"] = "mutated"

	if got := req.McpServers[0].Stdio.Args[0]; got != "one" {
		t.Fatalf("args were not cloned: %q", got)
	}
	if got := req.McpServers[0].Stdio.Env[0].Value; got != "1" {
		t.Fatalf("env was not cloned: %q", got)
	}
	if got := req.McpServers[0].Stdio.Env[0].Meta["nested"].(map[string]any)["kind"]; got != "stdio-env" {
		t.Fatalf("env meta was not cloned: %q", got)
	}
	if got := req.McpServers[0].Stdio.Meta["nested"].(map[string]any)["kind"]; got != "stdio" {
		t.Fatalf("stdio meta was not cloned: %q", got)
	}
	if got := req.McpServers[1].Http.Headers[0].Value; got != "1" {
		t.Fatalf("http headers were not cloned: %q", got)
	}
	if got := req.McpServers[1].Http.Headers[0].Meta["nested"].(map[string]any)["kind"]; got != "http-header" {
		t.Fatalf("http header meta was not cloned: %q", got)
	}
	if got := req.McpServers[1].Http.Meta["nested"].(map[string]any)["kind"]; got != "http" {
		t.Fatalf("http meta was not cloned: %q", got)
	}
	if got := req.McpServers[2].Sse.Headers[0].Value; got != "1" {
		t.Fatalf("sse headers were not cloned: %q", got)
	}
	if got := req.McpServers[2].Sse.Headers[0].Meta["nested"].(map[string]any)["kind"]; got != "sse-header" {
		t.Fatalf("sse header meta was not cloned: %q", got)
	}
	if got := req.McpServers[2].Sse.Meta["nested"].(map[string]any)["kind"]; got != "sse" {
		t.Fatalf("sse meta was not cloned: %q", got)
	}
	if got := req.McpServers[3].Acp.Meta["nested"].(map[string]any)["kind"]; got != "acp" {
		t.Fatalf("acp meta was not cloned: %q", got)
	}

	fork := ForkSessionRequest("session-1", "/repo", WithSessionMCPServers(req.McpServers...))
	if len(fork.McpServers) != 4 ||
		fork.McpServers[0].Stdio == nil ||
		fork.McpServers[0].Stdio.Name != "tools" ||
		fork.McpServers[1].Http == nil ||
		fork.McpServers[1].Http.Name != "remote" ||
		fork.McpServers[2].Sse == nil ||
		fork.McpServers[2].Sse.Name != "events" ||
		fork.McpServers[3].Acp == nil ||
		fork.McpServers[3].Acp.Name != "bridge" {
		t.Fatalf("fork MCP servers = %#v", fork.McpServers)
	}

	req.McpServers[0].Stdio.Env[0].Meta["nested"].(map[string]any)["kind"] = "mutated again"
	req.McpServers[1].Http.Headers[0].Meta["nested"].(map[string]any)["kind"] = "mutated again"
	req.McpServers[2].Sse.Headers[0].Meta["nested"].(map[string]any)["kind"] = "mutated again"
	if got := fork.McpServers[0].Stdio.Env[0].Meta["nested"].(map[string]any)["kind"]; got != "stdio-env" {
		t.Fatalf("fork env meta was not cloned: %q", got)
	}
	if got := fork.McpServers[1].Http.Headers[0].Meta["nested"].(map[string]any)["kind"]; got != "http-header" {
		t.Fatalf("fork http header meta was not cloned: %q", got)
	}
	if got := fork.McpServers[2].Sse.Headers[0].Meta["nested"].(map[string]any)["kind"]; got != "sse-header" {
		t.Fatalf("fork sse header meta was not cloned: %q", got)
	}
}

func TestRequestBuilderCloneEdges(t *testing.T) {
	t.Parallel()

	source := map[string]any{
		"strings": []string{"a"},
		"ints":    []int{1},
		"floats":  []float64{1.5},
		"bools":   []bool{true},
	}
	cloned := cloneAnyMap(source)
	source["strings"].([]string)[0] = "changed"
	source["ints"].([]int)[0] = 2
	source["floats"].([]float64)[0] = 2.5
	source["bools"].([]bool)[0] = false

	want := map[string]any{
		"strings": []string{"a"},
		"ints":    []int{1},
		"floats":  []float64{1.5},
		"bools":   []bool{true},
	}
	if !reflect.DeepEqual(cloned, want) {
		t.Fatalf("cloned = %#v, want %#v", cloned, want)
	}
	if mergeAnyMap(nil, nil) != nil {
		t.Fatal("mergeAnyMap(nil, nil) returned non-nil")
	}
	if env := envVariables(nil); env == nil || len(env) != 0 {
		t.Fatalf("envVariables(nil) = %#v", env)
	}
	if headers := cloneHTTPHeaders(nil); headers != nil {
		t.Fatalf("cloneHTTPHeaders(nil) = %#v", headers)
	}
	if env := cloneEnvVariables(nil); env != nil {
		t.Fatalf("cloneEnvVariables(nil) = %#v", env)
	}
	if server := cloneMCPServerStdio(nil); server != nil {
		t.Fatalf("cloneMCPServerStdio(nil) = %#v", server)
	}
	if servers := cloneMCPServers(nil); servers != nil {
		t.Fatalf("cloneMCPServers(nil) = %#v", servers)
	}
	if servers := unstableMCPServersFromStable(nil); servers != nil {
		t.Fatalf("unstableMCPServersFromStable(nil) = %#v", servers)
	}
	if got := cloneMCPServer(acp.McpServer{}); !reflect.DeepEqual(got, acp.McpServer{}) {
		t.Fatalf("cloneMCPServer(empty) = %#v", got)
	}
	if got := unstableMCPServerFromStable(acp.McpServer{}); !reflect.DeepEqual(got, acp.UnstableMcpServer{}) {
		t.Fatalf("unstableMCPServerFromStable(empty) = %#v", got)
	}
}

func TestMCPServerBuilders(t *testing.T) {
	t.Parallel()

	stdio := StdioMCPServer("tools", "tool", []string{"one"}, map[string]string{
		"B": "2",
		"A": "1",
	})
	if stdio.Stdio == nil {
		t.Fatalf("stdio server = %#v", stdio)
	}
	if !reflect.DeepEqual(stdio.Stdio.Args, []string{"one"}) {
		t.Fatalf("stdio args = %#v", stdio.Stdio.Args)
	}
	if !reflect.DeepEqual(stdio.Stdio.Env, []acp.EnvVariable{
		{Name: "A", Value: "1"},
		{Name: "B", Value: "2"},
	}) {
		t.Fatalf("stdio env = %#v", stdio.Stdio.Env)
	}

	http := HTTPMCPServer("remote", "https://mcp.example.test", map[string]string{
		"X-Two": "2",
		"X-One": "1",
	})
	if http.Http == nil || http.Http.Name != "remote" || http.Http.Url != "https://mcp.example.test" {
		t.Fatalf("http server = %#v", http)
	}
	if !reflect.DeepEqual(http.Http.Headers, []acp.HttpHeader{
		{Name: "X-One", Value: "1"},
		{Name: "X-Two", Value: "2"},
	}) {
		t.Fatalf("http headers = %#v", http.Http.Headers)
	}

	sse := SSEMCPServer("events", "https://mcp.example.test/sse", nil)
	if sse.Sse == nil || sse.Sse.Name != "events" || sse.Sse.Headers == nil {
		t.Fatalf("sse server = %#v", sse)
	}
}

func TestOpenCodeConfigRequestBuilders(t *testing.T) {
	t.Parallel()

	sessionID := acp.SessionId("session-1")
	model := SetModelRequest(sessionID, "opencode/big-pickle")
	if model.ValueId == nil {
		t.Fatalf("model request = %#v", model)
	}
	if model.ValueId.SessionId != sessionID || model.ValueId.ConfigId != OpenCodeConfigModel {
		t.Fatalf("model request = %#v", model.ValueId)
	}
	if model.ValueId.Value != "opencode/big-pickle" {
		t.Fatalf("model value = %q", model.ValueId.Value)
	}

	mode := SetModeConfigRequest(sessionID, OpenCodeModePlan)
	if mode.ValueId == nil || mode.ValueId.ConfigId != OpenCodeConfigMode || mode.ValueId.Value != OpenCodeModePlan {
		t.Fatalf("mode config request = %#v", mode)
	}
}
