package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"eino-ops-agent/internal/domain"
)

func newSearchStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now().UTC()
	for _, id := range []string{"host-a", "host-b"} {
		host := domain.Host{ID: id, Name: id, Address: "127.0.0.1", Port: 22, User: "ops", AuthType: "agent", CreatedAt: now, UpdatedAt: now}
		if _, err := st.UpsertHost(ctx, host); err != nil {
			t.Fatal(err)
		}
	}
	return st, ctx
}

func insertRunForRequest(t *testing.T, st *Store, ctx context.Context, id, hostID string, req domain.ExecRequest, startedAt time.Time) {
	t.Helper()
	encoded, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	run := domain.Run{
		ID: id, HostID: hostID, RequestJSON: string(encoded), SearchText: req.SearchText(),
		RequestDigest: "digest-" + id, Status: "completed", StartedAt: startedAt,
	}
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
}

func TestSearchRunsMatchesCommandTextLiterally(t *testing.T) {
	st, ctx := newSearchStore(t)
	now := time.Now().UTC()
	insertRunForRequest(t, st, ctx, "run-script", "host-a", domain.ExecRequest{
		HostID: "host-a", Mode: "script",
		Script: `grep "error" /var/log/app.log > /tmp/out && echo C:\logs read_percent 100%`,
		Reason: "inspect app errors",
	}, now)

	for _, query := range []string{
		`grep "error"`,       // quotes exactly as typed
		`> /tmp/out`,         // redirection stored as > in request_json
		`&& echo`,            // ampersands stored as &&
		`C:\logs`,            // literal backslash must not act as ESCAPE
		`read_percent`,       // literal underscore must not act as wildcard
		`100%`,               // literal percent must not act as wildcard
		`inspect app errors`, // reason text
	} {
		runs, err := st.SearchRuns(ctx, query, "", "", 0)
		if err != nil {
			t.Fatalf("query %q: %v", query, err)
		}
		if len(runs) != 1 || runs[0].ID != "run-script" {
			t.Fatalf("query %q: expected run-script, got %d result(s)", query, len(runs))
		}
	}

	if runs, err := st.SearchRuns(ctx, `rm -rf`, "", "", 0); err != nil || len(runs) != 0 {
		t.Fatalf("unrelated query: expected no results, got %d (err=%v)", len(runs), err)
	}
	// A literal underscore query must not wildcard-match "readapercent".
	if runs, err := st.SearchRuns(ctx, `readapercent`, "", "", 0); err != nil || len(runs) != 0 {
		t.Fatalf("sanity: unexpected match for readapercent: %d (err=%v)", len(runs), err)
	}
}

func TestSearchRunsFiltersAndFallbacks(t *testing.T) {
	st, ctx := newSearchStore(t)
	now := time.Now().UTC()
	insertRunForRequest(t, st, ctx, "run-a", "host-a", domain.ExecRequest{
		HostID: "host-a", Mode: "program", Program: "systemctl", Args: []string{"status", "nginx"}, Reason: "check nginx",
	}, now.Add(-time.Minute))
	insertRunForRequest(t, st, ctx, "run-b", "host-b", domain.ExecRequest{
		HostID: "host-b", Mode: "program", Program: "systemctl", Args: []string{"status", "redis"}, Reason: "check redis",
	}, now)

	// Host filter narrows results.
	runs, err := st.SearchRuns(ctx, "systemctl", "host-a", "", 0)
	if err != nil || len(runs) != 1 || runs[0].ID != "run-a" {
		t.Fatalf("host filter: expected run-a, got %d result(s) (err=%v)", len(runs), err)
	}
	// Limit caps results, newest first.
	runs, err = st.SearchRuns(ctx, "systemctl", "", "", 1)
	if err != nil || len(runs) != 1 || runs[0].ID != "run-b" {
		t.Fatalf("limit: expected newest run-b, got %d result(s) (err=%v)", len(runs), err)
	}

	// Output search still works on the redacted columns.
	run := domain.Run{ID: "run-a", HostID: "host-a", Status: "completed", StdoutRedacted: "Active: active (running)", CompletedAt: now}
	if err := st.UpdateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	runs, err = st.SearchRuns(ctx, "active (running)", "", "", 0)
	if err != nil || len(runs) != 1 || runs[0].ID != "run-a" {
		t.Fatalf("stdout search: expected run-a, got %d result(s) (err=%v)", len(runs), err)
	}

	// Rows without search_text (legacy or hand-inserted) still match via request_json.
	legacy := domain.Run{
		ID: "run-legacy", HostID: "host-a", RequestJSON: `{"program":"nginx"}`,
		RequestDigest: "digest-legacy", Status: "completed", StartedAt: now,
	}
	if err := st.CreateRun(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	runs, err = st.SearchRuns(ctx, "nginx", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range runs {
		found = found || item.ID == "run-legacy"
	}
	if !found {
		t.Fatalf("request_json fallback: run-legacy missing from %d result(s)", len(runs))
	}
}

func TestSearchRunsFiltersBySession(t *testing.T) {
	st, ctx := newSearchStore(t)
	now := time.Now().UTC()
	for _, run := range []domain.Run{
		{
			ID: "run-session-a", SessionID: "session-a", HostID: "host-a",
			RequestJSON: `{"program":"uptime"}`, SearchText: "uptime",
			RequestDigest: "digest-session-a", Status: "completed", StartedAt: now.Add(-time.Minute),
		},
		{
			ID: "run-session-b", SessionID: "session-b", HostID: "host-a",
			RequestJSON: `{"program":"uptime"}`, SearchText: "uptime",
			RequestDigest: "digest-session-b", Status: "completed", StartedAt: now,
		},
	} {
		if err := st.CreateRun(ctx, run); err != nil {
			t.Fatal(err)
		}
	}

	runs, err := st.SearchRuns(ctx, "uptime", "", "session-a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != "run-session-a" {
		t.Fatalf("session-a search leaked other sessions: %#v", runs)
	}
	allRuns, err := st.SearchRuns(ctx, "uptime", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(allRuns) != 2 {
		t.Fatalf("global audit search lost runs: %#v", allRuns)
	}
}

func TestMigrationBackfillsRunSearchText(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`CREATE TABLE hosts (
  id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, address TEXT NOT NULL, port INTEGER NOT NULL DEFAULT 22,
  user TEXT NOT NULL, known_hosts_file TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE runs (
  id TEXT PRIMARY KEY, session_id TEXT NOT NULL DEFAULT '', host_id TEXT NOT NULL, request_json TEXT NOT NULL,
  request_digest TEXT NOT NULL, risk TEXT NOT NULL, status TEXT NOT NULL, exit_code INTEGER NOT NULL DEFAULT 0,
  stdout_redacted TEXT NOT NULL DEFAULT '', stderr_redacted TEXT NOT NULL DEFAULT '',
  stdout_cipher TEXT NOT NULL DEFAULT '', stderr_cipher TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL, completed_at TEXT, FOREIGN KEY(host_id) REFERENCES hosts(id))`,
		`INSERT INTO hosts(id,name,address,port,user,created_at,updated_at)
  VALUES('host-a','host-a','127.0.0.1',22,'ops','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`,
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	encoded, err := json.Marshal(domain.ExecRequest{
		HostID: "host-a", Mode: "script", Script: `tail -n 50 "/var/log/nginx/error.log" > /tmp/tail.out`, Reason: "collect errors",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO runs(id,host_id,request_json,request_digest,risk,status,started_at)
  VALUES('run-old','host-a',?,'digest-old','read_only','completed','2026-01-02T00:00:00Z')`, string(encoded)); err != nil {
		t.Fatal(err)
	}
	// A row whose redacted request is no longer valid JSON must not abort migration.
	if _, err := db.ExecContext(ctx, `INSERT INTO runs(id,host_id,request_json,request_digest,risk,status,started_at)
  VALUES('run-broken','host-a','{"script":[REDACTED]}','digest-broken','read_only','completed','2026-01-02T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	runs, err := st.SearchRuns(ctx, `> /tmp/tail.out`, "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != "run-old" {
		t.Fatalf("backfill: expected run-old for redirect query, got %d result(s)", len(runs))
	}
}
