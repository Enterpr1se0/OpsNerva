package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestStoreChangesFollowSuccessfulWrites(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "changes.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	changes := make([]Change, 0, 4)
	unsubscribe := st.SubscribeChanges(func(change Change) {
		changes = append(changes, change)
	})

	event := domain.AuditEvent{ID: "event-change", Type: "test", Actor: "test"}
	if err := st.AppendAudit(ctx, event); err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Topic != ChangeAudit || changes[0].Audit == nil || changes[0].Audit.ID != event.ID {
		t.Fatalf("audit changes = %#v", changes)
	}
	if err := st.AppendAudit(ctx, event); err == nil {
		t.Fatal("duplicate audit event succeeded")
	}
	if len(changes) != 1 {
		t.Fatalf("failed write published changes: %#v", changes)
	}

	if _, err := st.CreateChatSession(ctx, "session-change", ""); err != nil {
		t.Fatal(err)
	}
	if len(changes) != 3 || changes[1] != (Change{Topic: ChangeSessions, SessionID: "session-change"}) ||
		changes[2] != (Change{Topic: ChangeChatState, SessionID: "session-change"}) {
		t.Fatalf("session changes = %#v", changes)
	}

	now := time.Now().UTC()
	host, err := st.UpsertHost(ctx, domain.Host{
		ID: "host-change", Name: "host-change", Address: "192.0.2.10", Port: 22,
		User: "ops", AuthType: "agent", SudoMode: "none", CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	run := domain.Run{ID: "run-change", SessionID: "session-change", HostID: host.ID, RequestJSON: `{}`, RequestDigest: "digest", Status: "approval_required", StartedAt: now}
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	approval := domain.Approval{ID: "approval-change", RunID: run.ID, HostID: host.ID, RequestJSON: `{}`, RequestDigest: run.RequestDigest, Status: domain.ApprovalStatusPending, CreatedAt: now}
	if err := st.CreateApproval(ctx, approval); err != nil {
		t.Fatal(err)
	}
	if len(changes) != 4 || changes[3].Topic != ChangeApprovals {
		t.Fatalf("approval changes = %#v", changes)
	}
	if err := st.CreateApproval(ctx, approval); err == nil {
		t.Fatal("duplicate approval succeeded")
	}
	if len(changes) != 4 {
		t.Fatalf("failed approval write published changes: %#v", changes)
	}

	unsubscribe()
	if err := st.AppendAudit(ctx, domain.AuditEvent{ID: "event-after-unsubscribe", Type: "test", Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if len(changes) != 4 {
		t.Fatalf("unsubscribed listener received changes: %#v", changes)
	}
}
