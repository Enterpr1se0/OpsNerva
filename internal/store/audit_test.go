package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestAuditPagesUseStableCursor(t *testing.T) {
	st, err := Open(context.Background(), t.TempDir()+"/audit.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now().UTC().Truncate(time.Second)
	for _, id := range []string{"evt-a", "evt-b", "evt-c"} {
		if err := st.AppendAudit(context.Background(), domain.AuditEvent{ID: id, Type: "test", Actor: "test", CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := st.ListAuditPage(context.Background(), "", 2, time.Time{}, "")
	if err != nil || len(first.Events) != 2 || !first.HasMore || first.Events[0].ID != "evt-c" || first.Events[1].ID != "evt-b" {
		t.Fatalf("first audit page = %#v, err=%v", first, err)
	}
	second, err := st.ListAuditPage(context.Background(), "", 2, first.NextCreatedAt, first.NextID)
	if err != nil || len(second.Events) != 1 || second.HasMore || second.Events[0].ID != "evt-a" {
		t.Fatalf("second audit page = %#v, err=%v", second, err)
	}
	if _, err := st.ListAuditPage(context.Background(), "", 2, now, ""); err == nil {
		t.Fatal("incomplete audit cursor was accepted")
	}
}

func TestDeleteAuditRunsIsTransactionalAndPreservesChat(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/audit-delete.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now().UTC()
	host, err := st.UpsertHost(ctx, domain.Host{ID: "host-audit", Name: "audit", Address: "192.0.2.50", Port: 22, User: "ops", AuthType: "agent", SudoMode: "none", CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateChatSession(ctx, "session-audit", ""); err != nil {
		t.Fatal(err)
	}
	userMessageID, err := st.AppendPendingChatMessage(ctx, "session-audit", "user", "run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartChatToolCall(ctx, domain.ChatToolCall{SessionID: "session-audit", UserMessageID: userMessageID, ToolCallID: "call-audit", ToolName: "ssh_exec"}); err != nil {
		t.Fatal(err)
	}
	run := domain.Run{ID: "run-audit", SessionID: "session-audit", HostID: host.ID, RequestJSON: `{}`, RequestDigest: "digest", Status: "completed", StartedAt: now, CompletedAt: now}
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := st.BindChatToolCallRun(ctx, "session-audit", "call-audit", run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FinishChatToolCall(ctx, "session-audit", "call-audit", run.ID, domain.ChatToolCallCompleted, `{"status":"completed"}`, ""); err != nil {
		t.Fatal(err)
	}
	approval := domain.Approval{ID: "approval-audit", RunID: run.ID, HostID: host.ID, RequestJSON: `{}`, RequestDigest: run.RequestDigest, Status: "approved", CreatedAt: now, DecidedAt: now}
	if err := st.CreateApproval(ctx, approval); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertTask(ctx, domain.Task{ID: "task-audit", RunID: run.ID, HostID: host.ID, Status: "completed", StartedAt: now, EndedAt: now}, domain.ExecResult{RunID: run.ID, Status: "completed"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendAudit(ctx, domain.AuditEvent{ID: "event-audit", RunID: run.ID, Type: "completed", Actor: "test", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	sessionID := run.SessionID
	result, err := st.DeleteAuditRuns(ctx, &sessionID, "test")
	if err != nil || result.Deleted != 1 || result.Retained != 0 || result.Scope != "session" || result.SessionID != sessionID || len(result.RetainedRunIDs) != 0 {
		t.Fatalf("delete result=%#v err=%v", result, err)
	}
	if _, err := st.GetRun(ctx, run.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("run was not deleted: %v", err)
	}
	if _, err := st.GetApproval(ctx, approval.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("approval was not deleted: %v", err)
	}
	if _, _, _, err := st.GetTask(ctx, "task-audit"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("task was not deleted: %v", err)
	}
	call, err := st.GetChatToolCall(ctx, "session-audit", "call-audit")
	if err != nil || call.RunID != "" || call.Status != domain.ChatToolCallCompleted {
		t.Fatalf("chat tool call was not preserved and detached: %#v err=%v", call, err)
	}
	var linkedEvents, deletionEvents int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE run_id=?`, run.ID).Scan(&linkedEvents); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE event_type='audit_records_deleted'`).Scan(&deletionEvents); err != nil {
		t.Fatal(err)
	}
	if linkedEvents != 0 || deletionEvents != 1 {
		t.Fatalf("linked events=%d deletion events=%d", linkedEvents, deletionEvents)
	}
}

func TestDeleteAuditRunsRetainsActiveRecords(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/audit-bulk-delete.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now().UTC()
	host, err := st.UpsertHost(ctx, domain.Host{ID: "host-bulk", Name: "bulk", Address: "192.0.2.51", Port: 22, User: "ops", AuthType: "agent", SudoMode: "none", CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range []domain.Run{
		{ID: "run-finished", SessionID: "session-bulk", HostID: host.ID, RequestJSON: `{}`, RequestDigest: "finished", Status: "completed", StartedAt: now, CompletedAt: now},
		{ID: "run-active", SessionID: "session-bulk", HostID: host.ID, RequestJSON: `{}`, RequestDigest: "active", Status: "running", StartedAt: now},
		{ID: "run-active-shell", SessionID: "session-bulk", HostID: host.ID, RequestJSON: `{}`, RequestDigest: "shell", Status: "completed", StartedAt: now, CompletedAt: now},
	} {
		if err := st.CreateRun(ctx, run); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateSSHShell(ctx, domain.SSHShell{ID: "shell-active", RunID: "run-active-shell", SessionID: "session-bulk", Kind: domain.SSHShellKindSSH, Surface: domain.SSHShellSurfaceAgent, HostID: host.ID, HostName: host.Name, User: host.User, Status: "running", Cols: 80, Rows: 24, StartedAt: now}); err != nil {
		t.Fatal(err)
	}
	sessionID := "session-bulk"
	result, err := st.DeleteAuditRuns(ctx, &sessionID, "test")
	if err != nil || result.Deleted != 1 || result.Retained != 2 || result.Scope != "session" || result.SessionID != sessionID || !slices.Equal(result.RetainedRunIDs, []string{"run-active-shell", "run-active"}) {
		t.Fatalf("delete result=%#v err=%v", result, err)
	}
	if _, err := st.GetRun(ctx, "run-finished"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("completed run was retained: %v", err)
	}
	if _, err := st.GetRun(ctx, "run-active"); err != nil {
		t.Fatalf("active run was deleted: %v", err)
	}
	if _, err := st.GetRun(ctx, "run-active-shell"); err != nil {
		t.Fatalf("run with an active shell was deleted: %v", err)
	}
}
