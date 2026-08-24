package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestHostAgentRootSettingMigratesExistingDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy-host.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `CREATE TABLE hosts (
id TEXT PRIMARY KEY,name TEXT NOT NULL UNIQUE,address TEXT NOT NULL,port INTEGER NOT NULL,username TEXT NOT NULL,
agent_enabled INTEGER NOT NULL DEFAULT 1,auth_type TEXT NOT NULL DEFAULT 'agent',private_key_cipher TEXT NOT NULL DEFAULT '',
known_hosts_file TEXT NOT NULL DEFAULT '',proxy_jump_host_id TEXT NOT NULL DEFAULT '',proxy_id TEXT NOT NULL DEFAULT '',
password_cipher TEXT NOT NULL DEFAULT '',sudo_mode TEXT NOT NULL DEFAULT 'none',sudo_password_cipher TEXT NOT NULL DEFAULT '',
created_at TEXT NOT NULL,updated_at TEXT NOT NULL)`)
	if closeErr := db.Close(); err != nil || closeErr != nil {
		t.Fatalf("prepare legacy database: create=%v close=%v", err, closeErr)
	}
	st, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var columns int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('hosts') WHERE name='agent_root_enabled'`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 1 {
		t.Fatalf("agent_root_enabled column count = %d", columns)
	}
}

func TestHostAgentRootSettingRoundTrips(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "host-root.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	host, err := st.UpsertHost(ctx, domain.Host{
		ID: "host-root", Name: "root", Address: "192.0.2.9", Port: 22, User: "root",
		AgentEnabled: true, AgentRootEnabled: true, AuthType: "agent", SudoMode: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !host.AgentRootEnabled {
		t.Fatal("enabled Agent root setting was not returned")
	}
	connectionUpdatedAt := host.UpdatedAt
	host, err = st.SetHostAgentRootEnabled(ctx, host.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if host.AgentRootEnabled {
		t.Fatal("disabled Agent root setting was not returned")
	}
	if !host.UpdatedAt.Equal(connectionUpdatedAt) {
		t.Fatalf("Agent root setting changed the SSH connection revision: %s -> %s", connectionUpdatedAt, host.UpdatedAt)
	}
	host, err = st.GetHost(ctx, host.ID)
	if err != nil || host.AgentRootEnabled {
		t.Fatalf("disabled Agent root setting did not persist: host=%#v err=%v", host, err)
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
