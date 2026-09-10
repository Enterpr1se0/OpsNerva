package store

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestAuditHistoryLargeDataset(t *testing.T) {
	st, ctx := newSearchStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	// Bulk fixture in the test-owned temporary database, not user data. Equal
	// timestamps stress the ID tie-breaker at the deepest end of a large group.
	_, err := st.db.ExecContext(ctx, `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<100000)
INSERT INTO runs(id,session_id,host_id,request_json,request_digest,status,search_text,started_at)
SELECT printf('run-%06d',x),CASE WHEN x<=50000 THEN 'large-session' ELSE printf('session-%03d',x%300) END,
'host-a','{"program":"echo"}','digest','completed',CASE WHEN x=1 THEN 'rare-needle' ELSE '' END,? FROM n`, formatTime(now))
	if err != nil {
		t.Fatal(err)
	}
	session := "large-session"
	measure := func(name string, read func()) {
		t.Helper()
		var samples []time.Duration
		for range 5 {
			start := time.Now()
			read()
			samples = append(samples, time.Since(start))
		}
		slices.Sort(samples)
		t.Logf("100000 runs / %s: median=%s max=%s", name, samples[2], samples[4])
	}
	groups := domain.AuditHistoryFilter{SnapshotAt: now, Limit: 20}
	measure("first 20 groups", func() {
		page, err := st.ListAuditHistoryGroups(ctx, groups)
		if err != nil || len(page.Groups) != 20 || !page.HasMore {
			t.Fatalf("first group page: %#v, %v", page, err)
		}
	})
	for _, item := range []struct {
		name, before, first, last string
	}{
		{"first 50 runs", "", "run-050000", "run-049951"},
		{"deep 50 runs", "run-000051", "run-000050", "run-000001"},
	} {
		filter := domain.AuditHistoryFilter{SessionID: &session, SnapshotAt: now, Limit: 50}
		if item.before != "" {
			filter.Before = &domain.AuditHistoryCursor{StartedAt: now, ID: item.before}
			statement, args, err := auditHistoryRunsQuery(filter)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := st.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+statement, args...)
			if err != nil {
				t.Fatal(err)
			}
			var steps []string
			for plan.Next() {
				var id, parent, unused int
				var detail string
				if err := plan.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				t.Log(detail)
				steps = append(steps, detail)
			}
			if err := plan.Err(); err != nil {
				t.Fatal(err)
			}
			plan.Close()
			joined := strings.Join(steps, "\n")
			if !strings.Contains(joined, "USING INDEX idx_runs_audit_time_session_id (<expr>=? AND session_id=? AND id<?)") ||
				!strings.Contains(joined, "USING INDEX idx_runs_audit_session_time_id (session_id=? AND <expr><?)") ||
				strings.Count(joined, "TEMP B-TREE") != 1 {
				t.Fatalf("deep pagination must seek both disjoint ranges and sort only the bounded merge: %s", joined)
			}
		}
		measure(item.name, func() {
			page, err := st.ListAuditHistoryRuns(ctx, filter)
			if err != nil || len(page.Runs) != 50 || page.Runs[0].ID != item.first || page.Runs[49].ID != item.last {
				t.Fatalf("%s: %#v, %v", item.name, page, err)
			}
			if item.before != "" && (page.HasMore || page.NextCursor != nil) {
				t.Fatal("the last page must terminate")
			}
		})
	}
	measure("search for an unloaded old run", func() {
		filter := domain.AuditHistoryFilter{SnapshotAt: now, Limit: 20, Query: "rare-needle"}
		page, err := st.ListAuditHistoryGroups(ctx, filter)
		if err != nil || len(page.Groups) != 1 || page.Groups[0].RunCount != 1 || page.Groups[0].SessionID != session || page.HasMore {
			t.Fatalf("search groups: %#v, %v", page, err)
		}
		filter.SessionID = &session
		runs, err := st.ListAuditHistoryRuns(ctx, filter)
		if err != nil || len(runs.Runs) != 1 || runs.Runs[0].ID != "run-000001" || runs.HasMore {
			t.Fatalf("search runs: %#v, %v", runs, err)
		}
	})
}
