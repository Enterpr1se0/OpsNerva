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

	"eino-ops-agent/internal/config"
	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/security"
	"eino-ops-agent/internal/service"
	"eino-ops-agent/internal/store"
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
}
