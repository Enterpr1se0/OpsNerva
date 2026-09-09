package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/store"
)

func TestAuditHistoryServicePreservesSessionIsolation(t *testing.T) {
	svc, _, host := newTestService(t)
	ctx := context.Background()
	now := time.Now().UTC().Add(-time.Minute)
	for _, session := range []string{"chat-a", "chat-b", "mcp_sess_a", ""} {
		if err := svc.store.CreateRun(ctx, domain.Run{ID: "run-" + session, SessionID: session, HostID: host.ID,
			RequestJSON: `{}`, RequestDigest: "digest", Status: "completed", StartedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := svc.ListAuditHistoryGroups(ctx, domain.AuditHistoryFilter{Limit: 20})
	if err != nil || len(page.Groups) != 4 || page.SnapshotAt.IsZero() {
		t.Fatalf("control-plane groups = %#v, err=%v", page, err)
	}
	for _, session := range []string{"chat-a", "mcp_sess_a"} {
		bound := WithSessionID(ctx, session)
		other := "chat-b"
		page, err := svc.ListAuditHistoryGroups(bound, domain.AuditHistoryFilter{SessionID: &other, Limit: 20})
		if err != nil || len(page.Groups) != 1 || page.Groups[0].SessionID != session {
			t.Fatalf("scoped groups = %#v, err=%v", page, err)
		}
		for _, requested := range []string{session, other, ""} {
			runs, err := svc.ListAuditHistoryRuns(bound, domain.AuditHistoryFilter{SessionID: &requested, Limit: 50})
			if requested == session {
				if err != nil || len(runs.Runs) != 1 || runs.Runs[0].SessionID != session {
					t.Fatalf("own runs = %#v, err=%v", runs, err)
				}
			} else if !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("cross-session access %q -> %q: %v", session, requested, err)
			}
		}
		// The original Agent/MCP history path must still impose context scope.
		legacy, err := svc.SearchRunSummariesMatchingPage(bound, domain.RunSearchFilter{SessionID: other, Limit: 20}, domain.FileSearchLiteral)
		if err != nil || len(legacy.Runs) != 1 || legacy.Runs[0].SessionID != session {
			t.Fatalf("original history isolation changed: %#v, err=%v", legacy, err)
		}
	}
	if _, err := svc.ListAuditHistoryGroups(ctx, domain.AuditHistoryFilter{Limit: 20, Before: &domain.AuditHistoryCursor{StartedAt: now, ID: "chat-a"}}); err == nil {
		t.Fatal("continuation without snapshot was accepted")
	}
}
