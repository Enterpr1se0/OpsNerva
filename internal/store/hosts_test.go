package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"eino-ops-agent/internal/domain"
)

func TestLegacySystemSSHHostsAreRemovedAndSchemaIsSimplified(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/legacy-hosts.db"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `CREATE TABLE hosts (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  address TEXT NOT NULL,
  port INTEGER NOT NULL,
	username TEXT NOT NULL,
	transport_backend TEXT NOT NULL DEFAULT 'system',
	auth_type TEXT NOT NULL DEFAULT 'agent',
  config_alias TEXT NOT NULL DEFAULT '',
  identity_file TEXT NOT NULL DEFAULT '',
  known_hosts_file TEXT NOT NULL DEFAULT '',
  proxy_jump TEXT NOT NULL DEFAULT '',
  password_cipher TEXT NOT NULL DEFAULT '',
  sudo_mode TEXT NOT NULL DEFAULT 'none',
  sudo_password_cipher TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
INSERT INTO hosts(id,name,address,port,username,auth_type,identity_file,created_at,updated_at)
VALUES('legacy-host','legacy','192.0.2.10',22,'ops','key','/legacy/id_ed25519','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	if err != nil {
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
	if _, err := st.GetHost(ctx, "legacy-host"); err != ErrNotFound {
		t.Fatalf("legacy System SSH host survived breaking migration: %v", err)
	}
	rows, err := st.db.QueryContext(ctx, `PRAGMA table_info(hosts)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		switch name {
		case "transport_backend", "config_alias", "proxy_jump", "identity_file":
			t.Fatalf("obsolete System SSH column %q survived migration", name)
		}
	}
	created, err := st.UpsertHost(ctx, domain.Host{Name: "native", Address: "192.0.2.11", Port: 22, User: "ops", AuthType: "agent", SudoMode: "none"})
	if err != nil || created.Name != "native" {
		t.Fatalf("Native SSH host did not persist after migration: host=%#v err=%v", created, err)
	}
}

func TestDeprecatedFileOperationsTableIsDropped(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy-file-operations.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE file_operations (id TEXT PRIMARY KEY); INSERT INTO file_operations(id) VALUES('legacy')`); err != nil {
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
	var count int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='file_operations'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("deprecated file_operations table survived migration")
	}
}

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
		RequestDigest: "digest", Risk: domain.RiskChange, Status: "approval_required", StartedAt: now,
	}
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	approval := domain.Approval{
		ID: "approval-delete", RunID: run.ID, HostID: host.ID, RequestJSON: `{}`,
		RequestDigest: run.RequestDigest, Risk: run.Risk, Status: "pending", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
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
	if err := st.ApprovePendingWithSessionGrantAndStartRun(ctx, approval.ID, run.ID, "reviewed", run.SessionID, "fingerprint", now.Add(time.Hour), approval.Risk); err != nil {
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
	if granted, err := st.HasSessionApprovalGrant(ctx, run.SessionID, "fingerprint"); err != nil || granted {
		t.Fatalf("related session grant was not deleted: granted=%v err=%v", granted, err)
	}
	var auditCount int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE run_id=?`, run.ID).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Fatalf("audit trail was not retained: %d", auditCount)
	}
}
