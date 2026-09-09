package store

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func createAuditHistoryRun(t *testing.T, st *Store, ctx context.Context, id, session string, started time.Time) domain.Run {
	t.Helper()
	run := domain.Run{ID: id, SessionID: session, HostID: "host-a", RequestJSON: `{"program":"echo"}`,
		RequestDigest: id, Status: "completed", StartedAt: started}
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestAuditHistoryGroupsPageBySessionWithOldTitles(t *testing.T) {
	st, ctx := newSearchStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	for i := range 65 {
		id := fmt.Sprintf("session-%02d", i)
		if _, err := st.CreateChatSession(ctx, id, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetChatSessionTitle(ctx, id, "Title "+id); err != nil {
			t.Fatal(err)
		}
		createAuditHistoryRun(t, st, ctx, "run-"+id, id, now)
	}
	// A large session still occupies exactly one outer page slot.
	for i := range 120 {
		createAuditHistoryRun(t, st, ctx, fmt.Sprintf("extra-%03d", i), "session-64", now.Add(-time.Second))
	}
	filter := domain.AuditHistoryFilter{SnapshotAt: now, Limit: 20}
	var got []string
	for pageNumber := 0; ; pageNumber++ {
		if pageNumber > 65 {
			t.Fatal("pagination did not terminate")
		}
		page, err := st.ListAuditHistoryGroups(ctx, filter)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Groups) > 20 || !page.SnapshotAt.Equal(now) {
			t.Fatalf("invalid page: %#v", page)
		}
		for _, group := range page.Groups {
			if group.Kind != "chat" || !group.SessionExists || group.Title != "Title "+group.SessionID {
				t.Fatalf("missing old session title: %#v", group)
			}
			if group.SessionID == "session-64" && group.RunCount != 121 {
				t.Fatalf("group count = %d", group.RunCount)
			}
			got = append(got, group.SessionID)
		}
		if !page.HasMore {
			if page.NextCursor != nil {
				t.Fatal("terminal page has a cursor")
			}
			break
		}
		filter.Before = page.NextCursor
		if len(got) > 65 {
			t.Fatal("pagination did not terminate")
		}
	}
	var want []string
	for i := 64; i >= 0; i-- {
		want = append(want, fmt.Sprintf("session-%02d", i))
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("groups = %v, want %v", got, want)
	}
}

func TestAuditHistoryGroupKindsAndDirectCursor(t *testing.T) {
	st, ctx := newSearchStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := st.CreateChatSession(ctx, "chat", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendChatMessage(ctx, "chat", "user", "First user title"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateChatSession(ctx, "deleted", ""); err != nil {
		t.Fatal(err)
	}
	createAuditHistoryRun(t, st, ctx, "deleted-run", "deleted", now.Add(-time.Second))
	if err := st.DeleteChatSession(ctx, "deleted"); err != nil {
		t.Fatal(err)
	}
	if err := st.StartMCPToolCall(ctx, domain.MCPClientSession{ID: "mcp_sess_a", Transport: "stdio", ClientName: "Client A", StartedAt: now, LastSeenAt: now},
		domain.MCPToolCall{ID: "call-a", SessionID: "mcp_sess_a", ToolName: "ssh_exec", ArgumentsJSON: `{}`, Status: "completed", StartedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"chat", "mcp_sess_a", "mcp_sess_missing", ""} {
		createAuditHistoryRun(t, st, ctx, "run-"+id, id, now)
	}
	filter := domain.AuditHistoryFilter{SnapshotAt: now, Limit: 1}
	seen := map[string]domain.AuditHistoryGroup{}
	for pageNumber := 0; ; pageNumber++ {
		if pageNumber > 5 {
			t.Fatal("pagination did not terminate")
		}
		page, err := st.ListAuditHistoryGroups(ctx, filter)
		if err != nil || len(page.Groups) != 1 {
			t.Fatalf("page = %#v, err = %v", page, err)
		}
		group := page.Groups[0]
		seen[group.SessionID] = group
		if group.SessionID == "" && (!page.HasMore || page.NextCursor.ID != "") {
			t.Fatalf("direct group lost its empty cursor ID: %#v", page)
		}
		if !page.HasMore {
			break
		}
		filter.Before = page.NextCursor
		if len(seen) > 5 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != 5 || seen["chat"].Title != "First user title" || !seen["chat"].SessionExists || seen["deleted"].SessionExists || seen[""].Kind != "direct" ||
		seen["mcp_sess_a"].Title != "Client A" || seen["mcp_sess_a"].Kind != "mcp" || !seen["mcp_sess_a"].SessionExists || seen["mcp_sess_missing"].Kind != "mcp" || seen["mcp_sess_missing"].SessionExists {
		t.Fatalf("groups = %#v", seen)
	}
}

func TestAuditHistoryRunsStablePrecisionAndSnapshot(t *testing.T) {
	st, ctx := newSearchStore(t)
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
	session := "session"
	for i, offset := range []time.Duration{0, 100 * time.Millisecond, 120 * time.Millisecond, 123456789 * time.Nanosecond, 123456789 * time.Nanosecond} {
		run := createAuditHistoryRun(t, st, ctx, fmt.Sprintf("run-%d", i), session, base.Add(offset))
		run.StdoutRedacted = strings.Repeat("output", 1000)
		run.StdoutCipher = "cipher"
		if err := st.UpdateRun(ctx, run); err != nil {
			t.Fatal(err)
		}
	}
	filter := domain.AuditHistoryFilter{SessionID: &session, SnapshotAt: base.Add(time.Second), Limit: 2}
	var ids []string
	for {
		page, err := st.ListAuditHistoryRuns(ctx, filter)
		if err != nil {
			t.Fatal(err)
		}
		for _, run := range page.Runs {
			if run.StdoutRedacted != "" || run.StdoutCipher != "" || run.RequestCipher != "" {
				t.Fatal("page fetched full outputs or credentials")
			}
			ids = append(ids, run.ID)
		}
		if !page.HasMore {
			break
		}
		if len(ids) == 2 {
			for i := range 150 {
				createAuditHistoryRun(t, st, ctx, fmt.Sprintf("new-%d", i), session, base.Add(2*time.Second))
			}
		}
		filter.Before = page.NextCursor
		if len(ids) > 5 {
			t.Fatal("new rows leaked into the historical window")
		}
	}
	if !reflect.DeepEqual(ids, []string{"run-4", "run-3", "run-2", "run-1", "run-0"}) {
		t.Fatalf("run order = %v", ids)
	}
	filter.Before = nil
	filter.SnapshotAt = base.Add(120 * time.Millisecond)
	page, err := st.ListAuditHistoryRuns(ctx, filter)
	if err != nil || len(page.Runs) != 2 || page.Runs[0].ID != "run-2" || page.Runs[1].ID != "run-1" || !page.HasMore {
		t.Fatalf("precise upper boundary = %#v, err = %v", page, err)
	}
}

func TestAuditHistorySearchAndExactSessionScope(t *testing.T) {
	st, ctx := newSearchStore(t)
	now := time.Now().UTC()
	for _, session := range []string{"", "chat", "mcp_sess_a"} {
		for i := range 3 {
			run := domain.Run{ID: fmt.Sprintf("%s-%d", session, i), SessionID: session, HostID: "host-a", RequestDigest: "digest", StartedAt: now, Status: "completed", RequestJSON: `{"program":"echo"}`}
			if i == 0 {
				run.SearchText = `read_percent 100% C:\logs`
				run.Status = "approval_required"
			} else if i == 1 {
				run.ToolArgumentsJSON = `{"script":"unique-script"}`
			}
			if err := st.CreateRun(ctx, run); err != nil {
				t.Fatal(err)
			}
			run.StdoutRedacted = "output-only"
			if err := st.UpdateRun(ctx, run); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, query := range []string{`read_percent`, `100%`, `C:\logs`, "unique-script", "echo", "readXpercent", "output-only"} {
		filter := domain.AuditHistoryFilter{Query: query, SnapshotAt: now, Limit: 20}
		groups, err := st.ListAuditHistoryGroups(ctx, filter)
		if err != nil {
			t.Fatal(err)
		}
		wantGroups := 3
		if query == "readXpercent" || query == "output-only" {
			wantGroups = 0
		}
		if len(groups.Groups) != wantGroups {
			t.Fatalf("search %q groups = %#v", query, groups)
		}
		for _, group := range groups.Groups {
			filter.SessionID = &group.SessionID
			page, err := st.ListAuditHistoryRuns(ctx, filter)
			if err != nil || len(page.Runs) != group.RunCount {
				t.Fatalf("query %q disagrees between groups and runs: %#v, %#v, %v", query, group, page, err)
			}
			pending := 0
			for _, run := range page.Runs {
				if run.SessionID != group.SessionID {
					t.Fatalf("query %q crossed session boundary: %#v", query, run)
				}
				if run.Status == "approval_required" {
					pending++
				}
			}
			if pending != group.PendingCount {
				t.Fatalf("pending count = %d, want %d", group.PendingCount, pending)
			}
		}
	}
}

func TestAuditHistoryRevalidationReflectsDeletionAndStatus(t *testing.T) {
	st, ctx := newSearchStore(t)
	now := time.Now().UTC()
	run := createAuditHistoryRun(t, st, ctx, "run", "session", now)
	filter := domain.AuditHistoryFilter{SnapshotAt: now, Limit: 20}
	run.Status = "approval_required"
	if err := st.UpdateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	page, err := st.ListAuditHistoryGroups(ctx, filter)
	if err != nil || len(page.Groups) != 1 || page.Groups[0].PendingCount != 1 {
		t.Fatalf("updated group = %#v, err = %v", page, err)
	}
	run.Status = "completed"
	if err := st.UpdateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteAuditRuns(ctx, &run.SessionID, "test"); err != nil {
		t.Fatal(err)
	}
	page, err = st.ListAuditHistoryGroups(ctx, filter)
	if err != nil || len(page.Groups) != 0 || page.HasMore || page.NextCursor != nil {
		t.Fatalf("deleted groups = %#v, err = %v", page, err)
	}
	filter.SessionID = &run.SessionID
	runs, err := st.ListAuditHistoryRuns(ctx, filter)
	if err != nil || len(runs.Runs) != 0 || runs.HasMore || runs.NextCursor != nil {
		t.Fatalf("deleted runs = %#v, err = %v", runs, err)
	}
}

func TestAuditHistorySessionMetadataInvalidation(t *testing.T) {
	st, ctx := newSearchStore(t)
	if _, err := st.CreateChatSession(ctx, "title-session", ""); err != nil {
		t.Fatal(err)
	}
	createAuditHistoryRun(t, st, ctx, "title-run", "title-session", time.Now().UTC())
	var updates []domain.AuditHistoryGroup
	unsubscribe := st.SubscribeChanges(func(change Change) {
		if change.Topic != ChangeAudit {
			return
		}
		if change.Audit != nil {
			t.Fatal("session metadata is an invalidation, not a logged audit operation")
		}
		page, err := st.ListAuditHistoryGroups(ctx, domain.AuditHistoryFilter{SnapshotAt: time.Now().UTC(), Limit: 20})
		if err != nil || len(page.Groups) != 1 {
			t.Fatalf("post-commit history: %#v, %v", page, err)
		}
		updates = append(updates, page.Groups[0])
	})
	defer unsubscribe()
	if _, changed, err := st.SetChatSessionTitleIfEmpty(ctx, "title-session", "Generated title"); err != nil || !changed {
		t.Fatalf("set empty title: changed=%v err=%v", changed, err)
	}
	if _, changed, err := st.SetChatSessionTitleIfEmpty(ctx, "title-session", "Ignored"); err != nil || changed {
		t.Fatalf("unchanged title: changed=%v err=%v", changed, err)
	}
	if _, err := st.SetChatSessionTitle(ctx, "missing", "Ignored"); err != ErrNotFound {
		t.Fatal(err)
	}
	if len(updates) != 1 || updates[0].Title != "Generated title" {
		t.Fatalf("initial updates: %#v", updates)
	}
	if _, err := st.SetChatSessionTitle(ctx, "title-session", "Renamed"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteChatSession(ctx, "title-session"); err != nil {
		t.Fatal(err)
	}
	if len(updates) != 3 || updates[1].Title != "Renamed" || updates[2].SessionExists || updates[2].RunCount != 1 {
		t.Fatalf("title/deletion invalidations: %#v", updates)
	}
	var events int
	if err := st.db.QueryRowContext(ctx, "SELECT count(*) FROM audit_events").Scan(&events); err != nil || events != 0 {
		t.Fatalf("metadata changes must not create operation history: count=%d err=%v", events, err)
	}
}

func TestAuditHistoryGroupsKeepSnapshotWhenNewRunsArrive(t *testing.T) {
	st, ctx := newSearchStore(t)
	now := time.Now().UTC()
	createAuditHistoryRun(t, st, ctx, "run-a", "a", now)
	createAuditHistoryRun(t, st, ctx, "run-b", "b", now.Add(-time.Second))
	filter := domain.AuditHistoryFilter{SnapshotAt: now, Limit: 1}
	first, err := st.ListAuditHistoryGroups(ctx, filter)
	if err != nil || len(first.Groups) != 1 || first.Groups[0].SessionID != "a" || !first.HasMore {
		t.Fatalf("first page = %#v, err=%v", first, err)
	}
	// New runs in an old group must not move it across the historical cursor.
	for i := range 150 {
		createAuditHistoryRun(t, st, ctx, fmt.Sprintf("new-%d", i), "b", now.Add(time.Second))
	}
	createAuditHistoryRun(t, st, ctx, "new-group", "c", now.Add(time.Second))
	// The boundary record need not still exist to continue keyset pagination.
	a := "a"
	if _, err := st.DeleteAuditRuns(ctx, &a, "test"); err != nil {
		t.Fatal(err)
	}
	filter.Before = first.NextCursor
	second, err := st.ListAuditHistoryGroups(ctx, filter)
	if err != nil || len(second.Groups) != 1 || second.Groups[0].SessionID != "b" || second.Groups[0].RunCount != 1 || second.HasMore {
		t.Fatalf("historical page moved after new runs: %#v, err=%v", second, err)
	}
}

func TestAuditHistoryStoreRejectsInvalidFilters(t *testing.T) {
	st, ctx := newSearchStore(t)
	now := time.Now().UTC()
	direct := ""
	for _, filter := range []domain.AuditHistoryFilter{
		{Limit: 20},
		{SnapshotAt: now, Limit: 0},
		{SnapshotAt: now, Limit: 201},
		{SnapshotAt: now, Limit: 20, Before: &domain.AuditHistoryCursor{}},
		{SnapshotAt: now, Limit: 20, Before: &domain.AuditHistoryCursor{StartedAt: now.Add(time.Second), ID: "one"}},
	} {
		if _, err := st.ListAuditHistoryGroups(ctx, filter); err == nil {
			t.Fatalf("accepted invalid group filter: %#v", filter)
		}
		filter.SessionID = &direct
		if _, err := st.ListAuditHistoryRuns(ctx, filter); err == nil {
			t.Fatalf("accepted invalid run filter: %#v", filter)
		}
	}
	if _, err := st.ListAuditHistoryRuns(ctx, domain.AuditHistoryFilter{SnapshotAt: now, Limit: 20}); err == nil {
		t.Fatal("missing session ID became an unscoped query")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := st.ListAuditHistoryGroups(cancelled, domain.AuditHistoryFilter{SnapshotAt: now, Limit: 20}); err == nil {
		t.Fatal("cancelled query succeeded")
	}
}

func TestAuditHistoryIndexUpgradePreservesExistingRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "upgrade.db")
	// First create a database without the new index, as on the previous version.
	func() {
		st, err := Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if _, err := st.db.ExecContext(ctx, "DROP INDEX idx_runs_audit_session_time_id"); err != nil {
			t.Fatal(err)
		}
		now := time.Date(2026, 1, 1, 0, 0, 0, 100000000, time.UTC)
		if _, err := st.UpsertHost(ctx, domain.Host{ID: "host-a", Name: "host-a", Address: "127.0.0.1", Port: 22, User: "ops", AuthType: "agent", CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		createAuditHistoryRun(t, st, ctx, "run", "session", now)
	}()
	st, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var started string
	if err := st.db.QueryRowContext(ctx, "SELECT started_at FROM runs WHERE id='run'").Scan(&started); err != nil || started != "2026-01-01T00:00:00.1Z" {
		t.Fatalf("upgrade rewrote stored timestamp: %q, err=%v", started, err)
	}
	var indexes int
	if err := st.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_runs_audit_session_time_id'").Scan(&indexes); err != nil || indexes != 1 {
		t.Fatalf("upgrade index count=%d, err=%v", indexes, err)
	}
	page, err := st.ListAuditHistoryGroups(ctx, domain.AuditHistoryFilter{SnapshotAt: time.Now().UTC(), Limit: 20})
	if err != nil || len(page.Groups) != 1 {
		t.Fatalf("upgraded history = %#v, err=%v", page, err)
	}
}

func TestAuditHistoryQueryPlans(t *testing.T) {
	st, ctx := newSearchStore(t)
	now := time.Now().UTC()
	// 30,000 runs: one 15,000-run group and 300 smaller groups. Insert in one
	// transaction and omit output blobs to keep this diagnostic inexpensive.
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<30000)
INSERT INTO runs(id,session_id,host_id,request_json,request_digest,status,started_at)
SELECT printf('run-%05d',x),CASE WHEN x<=15000 THEN 'large-session' ELSE printf('session-%03d',x%300) END,
'host-a','{}','digest','completed',? FROM n`, formatTime(now))
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	session := "large-session"
	filter := domain.AuditHistoryFilter{SnapshotAt: now, Limit: 20}
	for _, kind := range []string{"groups", "runs"} {
		var statement string
		var args []any
		if kind == "groups" {
			statement, args, err = auditHistoryGroupsQuery(filter)
		} else {
			filter.SessionID = &session
			statement, args, err = auditHistoryRunsQuery(filter)
		}
		if err != nil {
			t.Fatal(err)
		}
		rows, err := st.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+statement, args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: %s", kind, detail)
			plan = append(plan, detail)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		joined := strings.Join(plan, "\n")
		if !strings.Contains(joined, "USING INDEX idx_runs_audit_session_time_id") {
			t.Fatalf("%s no longer uses the audit index: %s", kind, joined)
		}
		if kind == "runs" && (!strings.Contains(joined, "SEARCH runs") || strings.Contains(joined, "TEMP B-TREE")) {
			t.Fatalf("run pagination scans or sorts the whole group: %s", joined)
		}
		started := time.Now()
		if kind == "groups" {
			_, err = st.ListAuditHistoryGroups(ctx, filter)
		} else {
			_, err = st.ListAuditHistoryRuns(ctx, filter)
		}
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: 30000-run fixture took %s", kind, time.Since(started))
	}
}
