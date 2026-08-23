package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/observability"
	"github.com/Enterpr1se0/opsnerva/internal/security"
	"github.com/Enterpr1se0/opsnerva/internal/service"
	"github.com/Enterpr1se0/opsnerva/internal/store"

	"golang.org/x/net/websocket"
)

func TestApplicationWebSocketSubscribesThroughRequestLogger(t *testing.T) {
	server := &Server{}
	httpServer := httptest.NewServer(requestLogMiddleware(http.HandlerFunc(server.applicationWebSocket), nil))
	defer httpServer.Close()
	config, err := websocket.NewConfig("ws"+strings.TrimPrefix(httpServer.URL, "http"), httpServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := websocket.DialConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := websocket.JSON.Send(connection, applicationWebSocketCommand{Type: "subscribe", Topics: []string{"logs"}, Logs: applicationWebSocketLogFilter{Limit: 20}}); err != nil {
		t.Fatal(err)
	}
	var event applicationWebSocketEvent
	if err := websocket.JSON.Receive(connection, &event); err != nil {
		t.Fatal(err)
	}
	if event.Type != "event" || event.Topic != "logs" || event.Mode != "snapshot" || event.Sequence != 1 {
		t.Fatalf("unexpected subscription event: %#v", event)
	}
}

func TestApplicationWebSocketStreamsMCPActivityAfterSnapshot(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dataDir, "mcp-events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	encryptor, err := security.NewEncryptor("", dataDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	svc := service.New(st, nil, encryptor, security.NewRedactor(), cfg.Limits, cfg)
	t.Cleanup(func() { _ = svc.Shutdown(context.Background()) })
	server := &Server{service: svc}
	httpServer := httptest.NewServer(requestLogMiddleware(http.HandlerFunc(server.applicationWebSocket), nil))
	defer httpServer.Close()
	webSocketConfig, err := websocket.NewConfig("ws"+strings.TrimPrefix(httpServer.URL, "http"), httpServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := websocket.DialConfig(webSocketConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := websocket.JSON.Send(connection, applicationWebSocketCommand{Type: "subscribe", Topics: []string{"mcp_activity"}}); err != nil {
		t.Fatal(err)
	}
	var snapshot applicationWebSocketEvent
	if err := websocket.JSON.Receive(connection, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Topic != "mcp_activity" || snapshot.Mode != "snapshot" {
		t.Fatalf("unexpected MCP activity snapshot: %#v", snapshot)
	}
	_, call, err := svc.BeginMCPToolCall(ctx, domain.MCPClientSession{ID: "mcp_sess_ws", Transport: "streamable_http"}, "ssh_history", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	var delta applicationWebSocketEvent
	if err := websocket.JSON.Receive(connection, &delta); err != nil {
		t.Fatal(err)
	}
	var activity domain.MCPActivityEvent
	if err := json.Unmarshal(delta.Data, &activity); err != nil {
		t.Fatal(err)
	}
	if delta.Topic != "mcp_activity" || delta.Mode != "delta" || activity.Type != "call_started" || activity.CallID != call.ID {
		t.Fatalf("unexpected MCP activity delta: %#v, payload %#v", delta, activity)
	}
}

func TestApplicationLogUpdateUsesSnapshotThenDelta(t *testing.T) {
	now := time.Now().UTC()
	older := observability.LogEntry{Time: now.Add(-time.Second), Level: "info", Message: "older"}
	newer := observability.LogEntry{Time: now, Level: "warn", Message: "newer"}
	initial := applicationLogResponse{Entries: []observability.LogEntry{older}, Components: []string{"http"}, MinimumLevel: "debug"}
	mode, payload, changed, previous, err := applicationLogUpdate(nil, initial)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || mode != "snapshot" || len(previous) != 1 {
		t.Fatalf("initial update = mode %q, changed %t, previous %d", mode, changed, len(previous))
	}
	var snapshot applicationLogResponse
	if err := json.Unmarshal(payload, &snapshot); err != nil || len(snapshot.Entries) != 1 {
		t.Fatalf("snapshot payload = %#v, error %v", snapshot, err)
	}

	current := applicationLogResponse{Entries: []observability.LogEntry{newer, older}, Components: []string{"http"}, MinimumLevel: "debug"}
	mode, payload, changed, previous, err = applicationLogUpdate(previous, current)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || mode != "delta" || len(previous) != 2 {
		t.Fatalf("incremental update = mode %q, changed %t, previous %d", mode, changed, len(previous))
	}
	var delta applicationLogResponse
	if err := json.Unmarshal(payload, &delta); err != nil || len(delta.Entries) != 1 || delta.Entries[0].Message != "newer" {
		t.Fatalf("delta payload = %#v, error %v", delta, err)
	}

	mode, payload, changed, _, err = applicationLogUpdate(previous, current)
	if err != nil || changed || mode != "" || payload != nil {
		t.Fatalf("unchanged update = mode %q, changed %t, payload %q, error %v", mode, changed, payload, err)
	}
}

func TestApplicationLogUpdateFallsBackToSnapshotAfterCursorLoss(t *testing.T) {
	now := time.Now().UTC()
	previous := []observability.LogEntry{{Time: now.Add(-time.Minute), Level: "info", Message: "expired"}}
	current := applicationLogResponse{Entries: []observability.LogEntry{{Time: now, Level: "info", Message: "current"}}}
	mode, _, changed, next, err := applicationLogUpdate(previous, current)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || mode != "snapshot" || len(next) != 1 || next[0].Message != "current" {
		t.Fatalf("cursor-loss update = mode %q, changed %t, next %#v", mode, changed, next)
	}
}
