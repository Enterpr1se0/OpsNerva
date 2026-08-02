package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"eino-ops-agent/internal/domain"
)

func TestDeleteHostRemovesRelatedRecords(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "hosts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().UTC()
	host, err := st.UpsertHost(ctx, domain.Host{
		ID: "host-delete", Name: "delete-me", Address: "192.0.2.10", Port: 22,
		User: "ops", AuthType: "agent", SudoMode: "none", CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	run := domain.Run{
		ID: "run-delete", SessionID: "session-delete", HostID: host.ID, RequestJSON: `{}`,
		RequestDigest: "digest", Status: "approval_required", StartedAt: now,
	}
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	approval := domain.Approval{
		ID: "approval-delete", RunID: run.ID, HostID: host.ID, RequestJSON: `{}`,
		RequestDigest: run.RequestDigest, Status: "pending", CreatedAt: now,
	}
	if err := st.CreateApproval(ctx, approval); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertTask(ctx, domain.Task{
		ID: "task-delete", RunID: run.ID, HostID: host.ID, Status: "pending", StartedAt: now,
	}, domain.ExecResult{RunID: run.ID, Status: "pending"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendAudit(ctx, domain.AuditEvent{
		ID: "audit-delete", RunID: run.ID, Type: "test", Actor: "test", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.ApprovePendingAndStartRun(ctx, approval.ID, run.ID, "reviewed"); err != nil {
		t.Fatal(err)
	}

	if err := st.DeleteHost(ctx, host.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetHost(ctx, host.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted host is still available: %v", err)
	}
	if _, err := st.GetRun(ctx, run.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("related run was not deleted: %v", err)
	}
	if _, err := st.GetApproval(ctx, approval.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("related approval was not deleted: %v", err)
	}
	if _, _, _, err := st.GetTask(ctx, "task-delete"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("related task was not deleted: %v", err)
	}
	var auditCount int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE run_id=?`, run.ID).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Fatalf("audit trail was not retained: %d", auditCount)
	}
}
