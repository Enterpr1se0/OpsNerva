package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"eino-ops-agent/internal/domain"

	"github.com/cloudwego/eino/components/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestManagedMCPServerInjectsNamespacedToolAndCanBeDisabled(t *testing.T) {
	svc, _, _ := newTestService(t)
	t.Cleanup(svc.CloseMCPServers)
	remote := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1.0.0"}, nil)
	type echoInput struct {
		Message string `json:"message" jsonschema:"message to echo"`
	}
	type echoOutput struct {
		Echo string `json:"echo"`
	}
	mcp.AddTool(remote, &mcp.Tool{Name: "echo.message", Description: "Echo one message."},
		func(_ context.Context, _ *mcp.CallToolRequest, input echoInput) (*mcp.CallToolResult, echoOutput, error) {
			return nil, echoOutput{Echo: input.Message}, nil
		})
	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remote }, nil)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		streamable.ServeHTTP(w, r)
	}))
	defer httpServer.Close()

	server, err := svc.SaveMCPServer(context.Background(), domain.MCPServerInput{
		Name: "Fixture MCP", Transport: domain.MCPTransportStreamableHTTP, URL: httpServer.URL,
		Headers: map[string]string{"Authorization": "Bearer fixture-secret"}, Enabled: true,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if server.Status != "ready" || server.ToolCount != 1 || !strings.HasPrefix(server.Tools[0].ExposedName, "mcp__") {
		t.Fatalf("unexpected MCP runtime: %#v", server)
	}
	encoded, _ := json.Marshal(server)
	if strings.Contains(string(encoded), "fixture-secret") || strings.Contains(string(encoded), "secrets_cipher") {
		t.Fatalf("MCP response exposed secret material: %s", encoded)
	}

	loaded := svc.MCPTools()
	if len(loaded) != 1 {
		t.Fatalf("loaded MCP tools=%d, want 1", len(loaded))
	}
	invokable, ok := loaded[0].(tool.InvokableTool)
	if !ok {
		t.Fatalf("MCP tool is not invokable: %T", loaded[0])
	}
	largeMessage := "mcp-start-" + strings.Repeat("m", 200<<10) + "-mcp-end"
	arguments, _ := json.Marshal(map[string]string{"message": largeMessage})
	output, err := invokable.InvokableRun(context.Background(), string(arguments))
	if err != nil || !strings.Contains(output, largeMessage) {
		t.Fatalf("complete MCP invocation output was not preserved: bytes=%d err=%v", len(output), err)
	}

	server, err = svc.SetMCPServerEnabled(context.Background(), server.ID, false, "test")
	if err != nil {
		t.Fatal(err)
	}
	if server.Status != "disabled" || len(svc.MCPTools()) != 0 {
		t.Fatalf("disabled MCP server remains loaded: %#v", server)
	}
	if _, err := invokable.InvokableRun(context.Background(), `{"message":"again"}`); err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("stale MCP wrapper remained callable: %v", err)
	}
}

func TestMCPToolErrorReturnsStructuredEnvelope(t *testing.T) {
	svc, _, _ := newTestService(t)
	t.Cleanup(svc.CloseMCPServers)
	remote := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1.0.0"}, nil)
	mcp.AddTool(remote, &mcp.Tool{Name: "always_fail", Description: "Return a tool-level error."},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: "fixture provider failure"}},
			}, nil, nil
		})
	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remote }, nil)
	httpServer := httptest.NewServer(streamable)
	defer httpServer.Close()

	server, err := svc.SaveMCPServer(context.Background(), domain.MCPServerInput{
		Name: "Failing MCP", Transport: domain.MCPTransportStreamableHTTP, URL: httpServer.URL, Enabled: true,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	tools := svc.MCPTools()
	if server.Status != "ready" || len(tools) != 1 {
		t.Fatalf("unexpected MCP runtime: server=%#v tools=%d", server, len(tools))
	}
	invokable, ok := tools[0].(tool.InvokableTool)
	if !ok {
		t.Fatalf("MCP tool is not invokable: %T", tools[0])
	}
	output, err := invokable.InvokableRun(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("MCP tool-level error escaped as a Go error: %v", err)
	}
	var envelope struct {
		ToolVersion        string             `json:"tool_version"`
		OK                 bool               `json:"ok"`
		Status             string             `json:"status"`
		Code               string             `json:"code"`
		Message            string             `json:"message"`
		NextAction         string             `json:"next_action"`
		ContentIsUntrusted bool               `json:"content_is_untrusted"`
		Result             mcp.CallToolResult `json:"result"`
	}
	if err := json.Unmarshal([]byte(output), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.ToolVersion != "1.1" || envelope.OK || envelope.Status != "failed" || envelope.Code != "provider_failed" || !envelope.ContentIsUntrusted {
		t.Fatalf("unexpected MCP failure envelope: %#v", envelope)
	}
	if envelope.Message == "" || envelope.NextAction == "" || !envelope.Result.IsError || len(envelope.Result.Content) != 1 {
		t.Fatalf("MCP failure details were not preserved: %#v", envelope)
	}
}

func TestMCPServerEditPreservesEncryptedSecretsWhenOmitted(t *testing.T) {
	svc, _, _ := newTestService(t)
	created, err := svc.SaveMCPServer(context.Background(), domain.MCPServerInput{
		Name: "Local package tools", Transport: domain.MCPTransportStdio, Command: "npx", Args: []string{"-y", "fixture"},
		Env: map[string]string{"TOKEN": "top-secret"}, Enabled: false,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	updated, err := svc.SaveMCPServer(context.Background(), domain.MCPServerInput{
		ID: created.ID, Name: "Renamed package tools", Transport: domain.MCPTransportStdio,
		Command: "npx", Args: []string{"-y", "fixture"}, Enabled: false,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.EnvKeys) != 1 || updated.EnvKeys[0] != "TOKEN" {
		t.Fatalf("blank edit erased MCP secrets: %#v", updated)
	}
	stored, err := svc.store.GetMCPServer(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := svc.decryptMCPSecrets(stored.SecretsCipher)
	if err != nil || secrets.Env["TOKEN"] != "top-secret" {
		t.Fatalf("encrypted MCP secret did not round-trip: %#v err=%v", secrets, err)
	}
}
