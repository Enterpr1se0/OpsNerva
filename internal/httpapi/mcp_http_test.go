package httpapi

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/security"
	"github.com/Enterpr1se0/opsnerva/internal/service"
	"github.com/Enterpr1se0/opsnerva/internal/store"
)

func TestMCPHTTPServerRequiresEnabledModeAndBearerToken(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dataDir, "mcp-http.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	encryptor, err := security.NewEncryptor("", dataDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	svc := service.New(st, nil, encryptor, security.NewRedactor(), cfg.Limits, cfg)
	t.Cleanup(func() { _ = svc.Shutdown(context.Background()) })
	handler := New(svc, nil, Options{Version: "test"}).Handler()

	request := func(token string) *httptest.ResponseRecorder {
		body := bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"test","version":"1"}}}`)
		req := httptest.NewRequest(http.MethodPost, "/mcp", body)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}

	if response := request(""); response.Code != http.StatusNotFound {
		t.Fatalf("disabled MCP HTTP endpoint returned %d: %s", response.Code, response.Body.String())
	}
	enabled := true
	settings, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{
		AgentMaxIterations: domain.DefaultAgentMaxIterations,
		MCPHTTPEnabled:     &enabled,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if response := request(""); response.Code != http.StatusUnauthorized || response.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("unauthenticated MCP HTTP endpoint returned %d: %s", response.Code, response.Body.String())
	}
	if response := request("wrong-token"); response.Code != http.StatusUnauthorized {
		t.Fatalf("invalid MCP HTTP token returned %d: %s", response.Code, response.Body.String())
	}
	response := request(settings.MCPHTTPToken)
	result, err := io.ReadAll(response.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || !strings.Contains(string(result), `"serverInfo":{"name":"opsnerva","version":"test"}`) {
		t.Fatalf("authorized MCP initialize returned %d: %s", response.Code, result)
	}
	sessionID := response.Header().Get("Mcp-Session-Id")
	if !strings.HasPrefix(sessionID, "mcp_sess_") {
		t.Fatalf("authorized MCP initialize did not allocate a server session: %q", sessionID)
	}
	secondSessionID := request(settings.MCPHTTPToken).Header().Get("Mcp-Session-Id")
	if secondSessionID == "" || secondSessionID == sessionID {
		t.Fatalf("independent MCP clients shared a session: %q", secondSessionID)
	}

	callBody := bytes.NewBufferString(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ssh_history","arguments":{}}}`)
	callRequest := httptest.NewRequest(http.MethodPost, "/mcp", callBody)
	callRequest.Header.Set("Content-Type", "application/json")
	callRequest.Header.Set("Accept", "application/json, text/event-stream")
	callRequest.Header.Set("Authorization", "Bearer "+settings.MCPHTTPToken)
	callRequest.Header.Set("Mcp-Session-Id", sessionID)
	callRequest.Header.Set("Mcp-Protocol-Version", "2025-11-25")
	callResponse := httptest.NewRecorder()
	handler.ServeHTTP(callResponse, callRequest)
	if callResponse.Code != http.StatusOK || !strings.Contains(callResponse.Body.String(), `"result"`) || strings.Contains(callResponse.Body.String(), `"error":{"code"`) {
		t.Fatalf("stateful MCP tool call returned %d: %s", callResponse.Code, callResponse.Body.String())
	}
	activity, err := svc.ListMCPActivity(ctx, sessionID, 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(activity.Sessions) != 1 || len(activity.Calls) != 1 || activity.Calls[0].ToolName != "ssh_history" || activity.Calls[0].Status != domain.MCPCallCompleted {
		t.Fatalf("unexpected MCP activity: %#v", activity)
	}
	activityRequest := httptest.NewRequest(http.MethodGet, "/api/v1/mcp/activity?session_id="+sessionID, nil)
	activityResponse := httptest.NewRecorder()
	handler.ServeHTTP(activityResponse, activityRequest)
	if activityResponse.Code != http.StatusOK || !strings.Contains(activityResponse.Body.String(), activity.Calls[0].ID) {
		t.Fatalf("MCP activity endpoint returned %d: %s", activityResponse.Code, activityResponse.Body.String())
	}
}
