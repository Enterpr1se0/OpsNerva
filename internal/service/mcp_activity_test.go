package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"eino-ops-agent/internal/domain"
)

func TestMCPActivityPublishesRedactedLifecycle(t *testing.T) {
	svc, _, _ := newTestService(t)
	events, unsubscribe := svc.SubscribeMCPActivity("mcp_sess_test")
	defer unsubscribe()

	callCtx, call, err := svc.BeginMCPToolCall(context.Background(), domain.MCPClientSession{
		ID: "mcp_sess_test", Transport: "streamable_http", ClientName: "codex",
	}, "ssh_exec", `{"host_id":"host_one","password":"secret-value"}`)
	if err != nil {
		t.Fatal(err)
	}
	if SessionIDFromContext(callCtx) != "mcp_sess_test" {
		t.Fatalf("MCP session was not bound to the call context")
	}
	owner, ok := executionOwnerFromContext(callCtx)
	if !ok || owner.Source != "mcp" || owner.ToolCallID != call.ID {
		t.Fatalf("unexpected MCP execution owner: %#v", owner)
	}
	if strings.Contains(call.ArgumentsJSON, "secret-value") || !strings.Contains(call.ArgumentsJSON, `"password":"[REDACTED]"`) || !json.Valid([]byte(call.ArgumentsJSON)) {
		t.Fatalf("MCP arguments were not redacted: %s", call.ArgumentsJSON)
	}
	started := <-events
	if started.Type != "call_started" || started.Call == nil || started.Call.ID != call.ID {
		t.Fatalf("unexpected start event: %#v", started)
	}

	call.Status = domain.MCPCallCompleted
	call.RunID = "run_test"
	if err := svc.FinishMCPToolCall(callCtx, call); err != nil {
		t.Fatal(err)
	}
	finished := <-events
	if finished.Type != "call_finished" || finished.Call == nil || finished.Call.RunID != "run_test" {
		t.Fatalf("unexpected finish event: %#v", finished)
	}
	snapshot, err := svc.ListMCPActivity(context.Background(), "mcp_sess_test", 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Sessions) != 1 || len(snapshot.Calls) != 1 || snapshot.Calls[0].Status != domain.MCPCallCompleted {
		t.Fatalf("unexpected MCP snapshot: %#v", snapshot)
	}
}

func TestMCPActivitySubscriptionIsSessionScoped(t *testing.T) {
	svc, _, _ := newTestService(t)
	events, unsubscribe := svc.SubscribeMCPActivity("mcp_sess_one")
	defer unsubscribe()

	_, other, err := svc.BeginMCPToolCall(context.Background(), domain.MCPClientSession{ID: "mcp_sess_two", Transport: "stdio"}, "ssh_history", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	_, selected, err := svc.BeginMCPToolCall(context.Background(), domain.MCPClientSession{ID: "mcp_sess_one", Transport: "stdio"}, "ssh_history", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	event := <-events
	if event.CallID != selected.ID || event.CallID == other.ID {
		t.Fatalf("session-scoped subscription received the wrong call: %#v", event)
	}
}
