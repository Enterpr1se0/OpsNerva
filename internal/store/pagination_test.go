package store

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func paginationStore(t testing.TB) (*Store, context.Context, time.Time) {
	t.Helper()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "pagination.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if _, err := st.UpsertHost(ctx, domain.Host{ID: "host", Name: "host", Address: "localhost", User: "ops", AuthType: "agent", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	// Equal timestamps exercise the tie-breaker: a deep cursor must not scan
	// the entire timestamp group before returning a small page.
	for _, statement := range []string{
		`INSERT INTO runs(id,session_id,host_id,request_json,request_digest,status,started_at)
SELECT printf('row-%06d',x),'session','host','{}','digest','completed',? FROM n`,
		`INSERT INTO audit_events(id,run_id,event_type,actor,data_json,created_at)
SELECT printf('row-%06d',x),'run','test','test','{}',? FROM n`,
		`INSERT INTO chat_messages(id,session_id,role,content,created_at)
SELECT printf('row-%06d',x),'session','user','hello',? FROM n`,
	} {
		_, err := st.db.ExecContext(ctx, `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<100000) `+statement, formatTime(now))
		if err != nil {
			t.Fatal(err)
		}
	}
	return st, ctx, now
}

func queryPlan(t *testing.T, st *Store, statement string, args ...any) string {
	t.Helper()
	rows, err := st.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+statement, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var steps []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		steps = append(steps, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(steps, "\n")
}

func TestPaginationQueryPlans(t *testing.T) {
	st, _ := newSearchStore(t)
	now := time.Now().UTC()
	for _, scope := range []struct {
		name, sessionID, hostID, index string
	}{
		{"global", "", "", "idx_runs_started_id"},
		{"session", "session", "", "idx_runs_session_started_id"},
		{"host", "", "host-a", "idx_runs_host_started_id"},
	} {
		t.Run("runs/"+scope.name, func(t *testing.T) {
			where, args, err := runSearchWhere(domain.RunSearchFilter{
				SessionID: scope.sessionID, HostID: scope.hostID, CursorStarted: now, CursorID: "row-000051",
			}, true)
			if err != nil {
				t.Fatal(err)
			}
			plan := queryPlan(t, st, "SELECT id FROM runs"+where+" ORDER BY started_at DESC,id DESC LIMIT 50", args...)
			if !strings.Contains(plan, scope.index) || !strings.Contains(plan, "(started_at,id)<(?,?)") || strings.Contains(plan, "TEMP B-TREE") {
				t.Fatalf("run cursor must seek the time and ID boundary without sorting: %s", plan)
			}
		})
	}
	for _, runID := range []string{"", "run"} {
		statement, args := auditEventsPageQuery(runID, 50, now, "row-000051")
		plan := queryPlan(t, st, statement, args...)
		if !strings.Contains(plan, "(created_at,id)<(?,?)") || strings.Contains(plan, "TEMP B-TREE") {
			t.Fatalf("audit cursor %q must seek the time and ID boundary: %s", runID, plan)
		}
	}
	statement, args := chatMessagesPageQuery("session", 50, formatTime(now), 51)
	plan := queryPlan(t, st, statement, args...)
	if !strings.Contains(plan, "(session_id=? AND created_at=? AND rowid<?)") ||
		!strings.Contains(plan, "(session_id=? AND created_at<?)") || strings.Count(plan, "TEMP B-TREE") != 1 {
		t.Fatalf("chat cursor must seek both ranges and sort only the bounded merge: %s", plan)
	}
	statement, args = excludedChatTurnsQuery("session")
	plan = queryPlan(t, st, statement, args...)
	if !strings.Contains(plan, "SEARCH users USING INDEX idx_chat_session_role_created (session_id=? AND role=?)") {
		t.Fatalf("scoped cleanup must not scan other conversations: %s", plan)
	}
}

func TestPaginationAcrossTimestampGroups(t *testing.T) {
	st, ctx := newSearchStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	// IDs deliberately differ from insertion order. Chat uses rowid; run and
	// audit pages use ID. Pages must span both timestamps without duplicates.
	for index, id := range []string{"z", "a", "y", "b", "x", "c"} {
		created := now.Add(time.Duration(index/3) * time.Minute)
		if err := st.CreateRun(ctx, domain.Run{ID: id, SessionID: "session", HostID: "host-a", RequestJSON: "{}", Status: "completed", StartedAt: created}); err != nil {
			t.Fatal(err)
		}
		if err := st.AppendAudit(ctx, domain.AuditEvent{ID: id, RunID: "run", Type: "test", CreatedAt: created}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.ExecContext(ctx, `INSERT INTO chat_messages(id,session_id,role,content,created_at) VALUES(?,'session','user','hello',?)`, id, formatTime(created)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO chat_messages(id,session_id,role,content,created_at) VALUES('other','other','user','unrelated',?)`, formatTime(now)); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"runs", "audit", "chat"} {
		t.Run(kind, func(t *testing.T) {
			var cursorTime time.Time
			var cursorID string
			var got []string
			for pageIndex := 0; ; pageIndex++ {
				if pageIndex > 3 {
					t.Fatal("cursor failed to terminate")
				}
				var more bool
				switch kind {
				case "runs":
					page, err := st.SearchRunSummariesFilteredPage(ctx, domain.RunSearchFilter{SessionID: "session", Limit: 2, CursorStarted: cursorTime, CursorID: cursorID})
					if err != nil {
						t.Fatal(err)
					}
					for _, run := range page.Runs {
						got = append(got, run.ID)
					}
					cursorTime, cursorID, more = page.NextStartedAt, page.NextID, page.HasMore
				case "audit":
					page, err := st.ListAuditPage(ctx, "run", 2, cursorTime, cursorID)
					if err != nil {
						t.Fatal(err)
					}
					for _, event := range page.Events {
						got = append(got, event.ID)
					}
					cursorTime, cursorID, more = page.NextCreatedAt, page.NextID, page.HasMore
				case "chat":
					before := ""
					if cursorID != "" {
						before = formatTime(cursorTime)
					}
					page, err := st.ListChatMessagesPage(ctx, "session", 2, before, cursorID)
					if err != nil {
						t.Fatal(err)
					}
					for _, message := range slices.Backward(page.Messages) {
						got = append(got, message.ID)
					}
					cursorTime, _ = time.Parse(time.RFC3339Nano, page.NextCreatedAt)
					cursorID, more = page.NextID, page.HasMore
				}
				if !more {
					break
				}
			}
			want := []string{"x", "c", "b", "z", "y", "a"}
			if kind == "chat" {
				want = []string{"c", "x", "b", "y", "a", "z"}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("pages = %v, want %v", got, want)
			}
		})
	}
}

func TestPaginationIndexMigration(t *testing.T) {
	st, ctx := newSearchStore(t)
	// Recreate the previous index layout, then run startup migration twice.
	if _, err := st.db.ExecContext(ctx, `DROP INDEX idx_runs_host_started_id;
CREATE INDEX idx_runs_host_started ON runs(host_id,started_at DESC);
CREATE INDEX idx_audit_created ON audit_events(created_at DESC);
CREATE INDEX idx_audit_run_created ON audit_events(run_id,created_at DESC);`); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendAudit(ctx, domain.AuditEvent{ID: "keep-event", Type: "test"}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := st.initializeSchema(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var oldIndexes int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema WHERE type='index' AND name IN ('idx_runs_host_started','idx_audit_created','idx_audit_run_created')`).Scan(&oldIndexes); err != nil || oldIndexes != 0 {
		t.Fatalf("obsolete indexes remained: %d, %v", oldIndexes, err)
	}
	plan := queryPlan(t, st, `SELECT id FROM runs WHERE host_id=? AND (started_at,id)<(?,?) ORDER BY started_at DESC,id DESC LIMIT 50`, "host-a", formatTime(time.Now()), "run")
	if !strings.Contains(plan, "idx_runs_host_started_id") || !strings.Contains(plan, "(started_at,id)<(?,?)") {
		t.Fatalf("migration did not install the seek index: %s", plan)
	}
	if page, err := st.ListAuditPage(ctx, "", 10, time.Time{}, ""); err != nil || len(page.Events) != 1 || page.Events[0].ID != "keep-event" {
		t.Fatalf("index migration changed audit data: %#v, %v", page, err)
	}
}

func BenchmarkStoreDeepPagination(b *testing.B) {
	st, ctx, now := paginationStore(b)
	b.Run("runs", func(b *testing.B) {
		for b.Loop() {
			page, err := st.SearchRunSummariesFilteredPage(ctx, domain.RunSearchFilter{SessionID: "session", CursorStarted: now, CursorID: "row-000051", Limit: 50})
			if err != nil || len(page.Runs) != 50 || page.Runs[0].ID != "row-000050" || page.Runs[49].ID != "row-000001" || page.HasMore {
				b.Fatalf("run page: %#v, %v", page, err)
			}
		}
	})
	b.Run("audit", func(b *testing.B) {
		for b.Loop() {
			page, err := st.ListAuditPage(ctx, "run", 50, now, "row-000051")
			if err != nil || len(page.Events) != 50 || page.Events[0].ID != "row-000050" || page.Events[49].ID != "row-000001" || page.HasMore {
				b.Fatalf("audit page: %#v, %v", page, err)
			}
		}
	})
	b.Run("chat", func(b *testing.B) {
		for b.Loop() {
			page, err := st.ListChatMessagesPage(ctx, "session", 50, formatTime(now), "row-000051")
			if err != nil || len(page.Messages) != 50 || page.Messages[0].ID != "row-000001" || page.Messages[49].ID != "row-000050" || page.HasMore {
				b.Fatalf("chat page: %#v, %v", page, err)
			}
		}
	})
}
