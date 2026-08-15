package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"eino-ops-agent/internal/domain"
)

func TestPendingApprovalDoesNotExpire(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "approvals.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	created := time.Now().UTC().Add(-24 * time.Hour)
	host, err := st.UpsertHost(ctx, domain.Host{
		ID: "host-approval", Name: "approval-host", Address: "192.0.2.20", Port: 22,
		User: "ops", AuthType: "agent", SudoMode: "none", CreatedAt: created,
	})
	if err != nil {
		t.Fatal(err)
	}
	run := domain.Run{
		ID: "run-approval", HostID: host.ID, RequestJSON: `{}`, RequestDigest: "digest",
		Status: "approval_required", AIReviewJSON: `{"status":"completed","decision":"manual_review"}`, StartedAt: created,
	}
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	approval := domain.Approval{
		ID: "approval-pending", RunID: run.ID, HostID: host.ID, RequestJSON: `{}`,
		RequestDigest: run.RequestDigest, Status: "pending", CreatedAt: created,
	}
	if err := st.CreateApproval(ctx, approval); err != nil {
		t.Fatal(err)
	}

	pending, err := st.ListApprovals(ctx, "pending", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != approval.ID || pending[0].Status != "pending" || pending[0].AIReview == nil || pending[0].AIReview.Decision != "manual_review" {
		t.Fatalf("old pending approval changed unexpectedly: %#v", pending)
	}
}
