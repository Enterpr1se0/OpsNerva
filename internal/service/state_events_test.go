package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/store"
)

func TestCommittedStoreChangesReachStateSubscribers(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state-events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.Default()
	svc := New(st, nil, nil, nil, cfg.Limits, cfg)
	t.Cleanup(func() { _ = svc.Shutdown(context.Background()) })
	events, _, unsubscribe := svc.SubscribeStateEvents()
	defer unsubscribe()

	if err := st.AppendAudit(ctx, domain.AuditEvent{ID: "audit-state", Type: "test", Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	assertStateTopic(t, events, StateTopicAudit, "")

	if _, err := st.CreateChatSession(ctx, "session-state", ""); err != nil {
		t.Fatal(err)
	}
	assertStateTopic(t, events, StateTopicSessions, "session-state")
	assertStateTopic(t, events, StateTopicChatState, "session-state")

	now := time.Now().UTC()
	host, err := st.UpsertHost(ctx, domain.Host{
		ID: "host-state", Name: "host-state", Address: "192.0.2.11", Port: 22,
		User: "ops", AuthType: "agent", SudoMode: "none", CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	run := domain.Run{ID: "run-state", SessionID: "session-state", HostID: host.ID, RequestJSON: `{}`, RequestDigest: "digest", Status: "approval_required", StartedAt: now}
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	approval := domain.Approval{ID: "approval-state", RunID: run.ID, HostID: host.ID, RequestJSON: `{}`, RequestDigest: run.RequestDigest, Status: domain.ApprovalStatusPending, CreatedAt: now}
	if err := st.CreateApproval(ctx, approval); err != nil {
		t.Fatal(err)
	}
	assertStateTopic(t, events, StateTopicApprovals, "")
}

func assertStateTopic(t *testing.T, events <-chan StateEvent, topic, sessionID string) {
	t.Helper()
	select {
	case event := <-events:
		if event.Topic != topic || event.SessionID != sessionID {
			t.Fatalf("state event = %#v, want topic=%q session=%q", event, topic, sessionID)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for topic %q", topic)
	}
}
