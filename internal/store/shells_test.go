package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"eino-ops-agent/internal/domain"
)

func TestSSHShellExitMetadataMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shells.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE ssh_shell_sessions (
id TEXT PRIMARY KEY,
run_id TEXT NOT NULL UNIQUE,
session_id TEXT NOT NULL,
host_id TEXT NOT NULL,
host_name TEXT NOT NULL,
username TEXT NOT NULL,
elevated INTEGER NOT NULL DEFAULT 0,
cwd TEXT NOT NULL DEFAULT '',
status TEXT NOT NULL,
cols INTEGER NOT NULL,
rows INTEGER NOT NULL,
last_sequence INTEGER NOT NULL DEFAULT 0,
recent_output TEXT NOT NULL DEFAULT '',
exit_code INTEGER NOT NULL DEFAULT 0,
error TEXT NOT NULL DEFAULT '',
started_at TEXT NOT NULL,
expires_at TEXT NOT NULL,
ended_at TEXT
);
INSERT INTO ssh_shell_sessions VALUES
('shell_closed','run_closed','session','host','host','user',0,'','closed',120,32,0,'',-1,'','2026-01-01T00:00:00Z','2026-01-01T00:15:00Z','2026-01-01T00:01:00Z'),
('shell_failed','run_failed','session','host','host','user',0,'','failed',120,32,0,'',7,'failed','2026-01-01T00:00:00Z','2026-01-01T00:15:00Z','2026-01-01T00:01:00Z')`)
	if err != nil {
		db.Close()
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

	closed, err := st.GetSSHShell(ctx, "shell_closed")
	if err != nil {
		t.Fatal(err)
	}
	if closed.ExitCode != nil || closed.TerminationReason != "requested_close" || closed.Surface != domain.SSHShellSurfaceAgent {
		t.Fatalf("closed shell metadata was not normalized: %#v", closed)
	}
	failed, err := st.GetSSHShell(ctx, "shell_failed")
	if err != nil {
		t.Fatal(err)
	}
	if failed.ExitCode == nil || *failed.ExitCode != 7 || failed.TerminationReason != "remote_exit" {
		t.Fatalf("reported remote exit metadata was not preserved: %#v", failed)
	}

	var expiryColumns, exitCodeNotNull, surfaceColumns int
	if err := st.db.QueryRow(`SELECT count(*) FROM pragma_table_info('ssh_shell_sessions') WHERE name='expires_at'`).Scan(&expiryColumns); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow(`SELECT "notnull" FROM pragma_table_info('ssh_shell_sessions') WHERE name='exit_code'`).Scan(&exitCodeNotNull); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow(`SELECT count(*) FROM pragma_table_info('ssh_shell_sessions') WHERE name='surface'`).Scan(&surfaceColumns); err != nil {
		t.Fatal(err)
	}
	if expiryColumns != 0 || exitCodeNotNull != 0 || surfaceColumns != 1 {
		t.Fatalf("shell schema migration incomplete: expiry_columns=%d exit_code_not_null=%d surface_columns=%d", expiryColumns, exitCodeNotNull, surfaceColumns)
	}
}
