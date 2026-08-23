package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"

	officialmcp "github.com/cloudwego/eino-ext/components/tool/mcp/officialmcp"
	"github.com/cloudwego/eino/components/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type staticMCPClientSession struct {
	tools []*mcp.Tool
	pages map[string]*mcp.ListToolsResult
}

func (s *staticMCPClientSession) ListTools(_ context.Context, params *mcp.ListToolsParams) (*mcp.ListToolsResult, error) {
	if s.pages != nil {
		return s.pages[params.Cursor], nil
	}
	return &mcp.ListToolsResult{Tools: s.tools}, nil
}

func (s *staticMCPClientSession) CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	return nil, nil
}

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
	info, err := loaded[0].Info(context.Background())
	if err != nil || info.Extra[officialmcp.ExtraMCPRawToolName] != "echo.message" || !strings.HasPrefix(info.Desc, "Fixture MCP: ") {
		t.Fatalf("official MCP tool metadata was not preserved: info=%#v err=%v", info, err)
	}
	if err := svc.ReconnectMCPServer(context.Background(), server.ID); err != nil {
		t.Fatal(err)
	}
	afterReconnect, err := invokable.InvokableRun(context.Background(), `{"message":"after-reconnect"}`)
	if err != nil || !strings.Contains(afterReconnect, "after-reconnect") {
		t.Fatalf("stale MCP tool did not resolve the current ready session: output=%s err=%v", afterReconnect, err)
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

func TestMCPToolErrorReturnsOfficialResultWithSecurityMetadata(t *testing.T) {
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
	var result mcp.CallToolResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	security, ok := result.Meta["opsnerva"].(map[string]any)
	if !ok || security["ok"] != false || security["status"] != "failed" || security["code"] != "provider_failed" || security["content_is_untrusted"] != true {
		t.Fatalf("unexpected MCP security metadata: result=%#v metadata=%#v", result, security)
	}
	if security["message"] == "" || security["next_action"] == "" || !result.IsError || len(result.Content) != 1 {
		t.Fatalf("MCP failure details were not preserved: result=%#v metadata=%#v", result, security)
	}
	audit, err := svc.ListAudit(context.Background(), "", 100)
	if err != nil {
		t.Fatal(err)
	}
	foundAudit := false
	for _, event := range audit {
		if event.Type == "mcp_tool_called" && event.Data["tool_name"] == "always_fail" && event.Data["status"] == "tool_error" {
			foundAudit = true
			break
		}
	}
	if !foundAudit {
		t.Fatalf("MCP tool call audit was not recorded: %#v", audit)
	}
}

func TestOfficialMCPToolDiscoveryKeepsSafetyLimitsAndUniqueNames(t *testing.T) {
	svc, _, _ := newTestService(t)
	tools := make([]*mcp.Tool, 0, mcpMaxTools+2)
	tools = append(tools,
		&mcp.Tool{Name: "same.name", Description: "first", InputSchema: map[string]any{"type": "object"}},
		&mcp.Tool{Name: "same name", Description: "second", InputSchema: map[string]any{"type": "object"}},
	)
	for index := 2; index < mcpMaxTools+2; index++ {
		tools = append(tools, &mcp.Tool{Name: fmt.Sprintf("tool_%03d", index), InputSchema: map[string]any{"type": "object"}})
	}
	resolved, err := svc.resolveMCPTools(context.Background(), &staticMCPClientSession{tools: tools}, domain.MCPServer{ID: "server-limits", Name: "Limits"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != mcpMaxTools {
		t.Fatalf("resolved MCP tools=%d, want %d", len(resolved), mcpMaxTools)
	}
	first, err := resolved[0].Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := resolved[1].Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Name == second.Name || len(first.Name) > 64 || len(second.Name) > 64 {
		t.Fatalf("colliding MCP names were not mapped safely: %q %q", first.Name, second.Name)
	}
	if first.Extra[officialmcp.ExtraMCPServerName] != "Limits" || first.Extra[officialmcp.ExtraMCPRawToolName] != "same.name" {
		t.Fatalf("official MCP metadata is incomplete: %#v", first.Extra)
	}
}

func TestOfficialMCPToolDiscoveryLoadsAllPages(t *testing.T) {
	svc, _, _ := newTestService(t)
	session := &staticMCPClientSession{pages: map[string]*mcp.ListToolsResult{
		"": {
			Tools:      []*mcp.Tool{{Name: "first", InputSchema: map[string]any{"type": "object"}}},
			NextCursor: "next",
		},
		"next": {
			Tools: []*mcp.Tool{{Name: "second", InputSchema: map[string]any{"type": "object"}}},
		},
	}}
	resolved, err := svc.resolveMCPTools(context.Background(), session, domain.MCPServer{ID: "server-pages", Name: "Pages"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 2 {
		t.Fatalf("resolved paginated MCP tools=%d, want 2", len(resolved))
	}
}

func TestOfficialMCPToolDiscoveryRejectsOversizedSchema(t *testing.T) {
	svc, _, _ := newTestService(t)
	session := &staticMCPClientSession{tools: []*mcp.Tool{{
		Name: "oversized", InputSchema: map[string]any{"type": "object", "description": strings.Repeat("x", mcpMaxSchemaBytes)},
	}}}
	_, err := svc.resolveMCPTools(context.Background(), session, domain.MCPServer{ID: "server-schema", Name: "Schema"})
	if err == nil || !strings.Contains(err.Error(), "input schema exceeds") {
		t.Fatalf("oversized MCP schema error=%v", err)
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

func TestManagedMCPServerOAuthAuthorizationPersistsAndClearsSession(t *testing.T) {
	svc, _, _ := newTestService(t)
	t.Cleanup(svc.CloseMCPServers)
	remote := mcp.NewServer(&mcp.Implementation{Name: "oauth-fixture", Version: "1.0.0"}, nil)
	mcp.AddTool(remote, &mcp.Tool{Name: "oauth_echo", Description: "OAuth fixture."},
		func(_ context.Context, _ *mcp.CallToolRequest, input struct {
			Value string `json:"value"`
		}) (*mcp.CallToolResult, map[string]string, error) {
			return nil, map[string]string{"value": input.Value}, nil
		})
	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remote }, nil)
	var serverURL string
	var registrations atomic.Int32
	var authorizedRequests atomic.Int32
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/.well-known/oauth-protected-resource"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"resource":"` + serverURL + `/mcp","authorization_servers":["` + serverURL + `"]}`))
		case strings.HasPrefix(r.URL.Path, "/.well-known/oauth-authorization-server"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"issuer":"` + serverURL + `","authorization_endpoint":"` + serverURL + `/authorize","token_endpoint":"` + serverURL + `/token","registration_endpoint":"` + serverURL + `/register","code_challenge_methods_supported":["S256"],"token_endpoint_auth_methods_supported":["none"]}`))
		case r.URL.Path == "/register":
			registrations.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"client_id":"opsnerva-fixture","redirect_uris":["http://127.0.0.1:8080/api/v1/mcp/oauth/callback"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"],"response_types":["code"]}`))
		case r.URL.Path == "/token":
			if err := r.ParseForm(); err != nil || r.Form.Get("code") != "fixture-code" || r.Form.Get("code_verifier") == "" {
				http.Error(w, "invalid token request", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"oauth-access-token","token_type":"Bearer","refresh_token":"oauth-refresh-token","expires_in":3600}`))
		case r.URL.Path == "/mcp":
			if r.Header.Get("Authorization") != "Bearer oauth-access-token" {
				w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+serverURL+`/.well-known/oauth-protected-resource"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			authorizedRequests.Add(1)
			streamable.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer httpServer.Close()
	serverURL = httpServer.URL

	server, err := svc.SaveMCPServer(context.Background(), domain.MCPServerInput{
		Name: "OAuth MCP", Transport: domain.MCPTransportStreamableHTTP, URL: serverURL + "/mcp", Enabled: true,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if server.Status != "error" || server.OAuthConfigured {
		t.Fatalf("unauthorized MCP server state = %#v", server)
	}

	start, err := svc.BeginMCPOAuth(context.Background(), server.ID, "http://127.0.0.1:8080/api/v1/mcp/oauth/callback", "test")
	if err != nil {
		t.Fatal(err)
	}
	authorizationURL, err := url.Parse(start.AuthorizationURL)
	if err != nil || authorizationURL.Path != "/authorize" || authorizationURL.Query().Get("code_challenge") == "" {
		t.Fatalf("authorization URL = %q, err=%v", start.AuthorizationURL, err)
	}
	callbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := svc.CompleteMCPOAuth(callbackCtx, authorizationURL.Query().Get("state"), "fixture-code", "", ""); err != nil {
		t.Fatal(err)
	}
	authorized, err := svc.GetMCPServer(context.Background(), server.ID)
	if err != nil {
		t.Fatal(err)
	}
	if authorized.Status != "ready" || !authorized.OAuthConfigured || authorized.OAuthExpiresAt == nil || authorized.ToolCount != 1 {
		t.Fatalf("authorized MCP state = %#v", authorized)
	}
	if registrations.Load() != 1 || authorizedRequests.Load() == 0 {
		t.Fatalf("OAuth requests: registrations=%d authorized=%d", registrations.Load(), authorizedRequests.Load())
	}
	stored, err := svc.store.GetMCPServer(context.Background(), server.ID)
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := svc.decryptMCPSecrets(stored.SecretsCipher)
	if err != nil || secrets.OAuth == nil || secrets.OAuth.AccessToken != "oauth-access-token" || secrets.OAuth.RefreshToken != "oauth-refresh-token" {
		t.Fatalf("OAuth session was not encrypted and restored: %#v err=%v", secrets.OAuth, err)
	}

	svc.disconnectMCPServer(server.ID, "disconnected")
	if err := svc.ReconnectMCPServer(context.Background(), server.ID); err != nil {
		t.Fatal(err)
	}
	reconnected, err := svc.GetMCPServer(context.Background(), server.ID)
	if err != nil || reconnected.Status != "ready" || registrations.Load() != 1 {
		t.Fatalf("stored OAuth reconnect = %#v, registrations=%d err=%v", reconnected, registrations.Load(), err)
	}

	cleared, err := svc.ClearMCPOAuth(context.Background(), server.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if cleared.OAuthConfigured || cleared.Status != "error" {
		t.Fatalf("cleared OAuth state = %#v", cleared)
	}
}
