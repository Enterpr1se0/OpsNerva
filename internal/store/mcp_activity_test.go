package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"eino-ops-agent/internal/domain"
)

func TestMCPActivityTracksSessionsAndCallLifecycle(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "mcp-activity.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	started := time.Now().UTC().Add(-time.Second)
	session := domain.MCPClientSession{
		ID: "mcp_sess_one", Transport: "streamable_http", ClientName: "codex", ClientVersion: "1",
		ProtocolVersion: "2025-11-25", StartedAt: started, LastSeenAt: started,
	}
	call := domain.MCPToolCall{
		ID: "mcp_call_one", SessionID: session.ID, ToolName: "ssh_exec", ArgumentsJSON: `{"host_id":"host_one"}`,
		Status: domain.MCPCallRunning, StartedAt: started, UpdatedAt: started,
	}
	if err := st.StartMCPToolCall(ctx, session, call); err != nil {
		t.Fatal(err)
	}

	call.Status = domain.MCPCallCompleted
	call.RunID = "run_one"
	call.OperationStatus = "completed"
	call.UpdatedAt = time.Now().UTC()
	call.CompletedAt = call.UpdatedAt
	if err := st.FinishMCPToolCall(ctx, call); err != nil {
		t.Fatal(err)
	}

	sessions, err := st.ListMCPClientSessions(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ID != session.ID || sessions[0].CallCount != 1 || sessions[0].RunningCalls != 0 {
		t.Fatalf("unexpected MCP sessions: %#v", sessions)
	}
	calls, err := st.ListMCPToolCalls(ctx, session.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Status != domain.MCPCallCompleted || calls[0].RunID != "run_one" || calls[0].CompletedAt.IsZero() {
		t.Fatalf("unexpected MCP calls: %#v", calls)
	}
}

func TestInterruptRunningMCPToolCallsLeavesCompletedCallsUntouched(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "mcp-recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now().UTC()
	session := domain.MCPClientSession{ID: "mcp_sess_recovery", Transport: "stdio", StartedAt: now, LastSeenAt: now}
	for _, status := range []string{domain.MCPCallRunning, domain.MCPCallCompleted} {
		call := domain.MCPToolCall{ID: "call_" + status, SessionID: session.ID, ToolName: "ssh_history", ArgumentsJSON: `{}`, Status: status, StartedAt: now, UpdatedAt: now}
		if err := st.StartMCPToolCall(ctx, session, call); err != nil {
			t.Fatal(err)
		}
		if status == domain.MCPCallCompleted {
			call.CompletedAt = now
			if err := st.FinishMCPToolCall(ctx, call); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := st.InterruptRunningMCPToolCalls(ctx); err != nil {
		t.Fatal(err)
	}
	calls, err := st.ListMCPToolCalls(ctx, session.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{}
	for _, call := range calls {
		statuses[call.ID] = call.Status
	}
	if statuses["call_running"] != domain.MCPCallInterrupted || statuses["call_completed"] != domain.MCPCallCompleted {
		t.Fatalf("unexpected recovered statuses: %#v", statuses)
	}
}
