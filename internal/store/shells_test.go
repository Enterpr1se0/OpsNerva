package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"eino-ops-agent/internal/domain"
)

func TestAppendSSHShellEventsCommitsBatchAndSessionCursor(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "shell-batch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	if err := st.CreateSSHShell(ctx, domain.SSHShell{ID: "shell-batch", RunID: "run-batch", Kind: domain.SSHShellKindSSH, Status: "running", Cols: 80, Rows: 24, StartedAt: now}); err != nil {
		t.Fatal(err)
	}
	readable := "second"
	events := []domain.SSHShellEvent{
		{ShellID: "shell-batch", Sequence: 1, Stream: "stdout", Content: "first", Status: "running", CreatedAt: now},
		{ShellID: "shell-batch", Sequence: 2, Stream: "stdout", Content: "second", ReadableContent: &readable, Status: "running", CreatedAt: now},
	}
	if err := st.AppendSSHShellEvents(ctx, events, "firstsecond"); err != nil {
		t.Fatal(err)
	}
	shell, err := st.GetSSHShell(ctx, "shell-batch")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := st.ListSSHShellEvents(ctx, "shell-batch", 0)
	if err != nil {
		t.Fatal(err)
	}
	recent, err := st.GetSSHShellRecentOutput(ctx, "shell-batch")
	if err != nil {
		t.Fatal(err)
	}
	if shell.LastSequence != 2 || len(stored) != 2 || stored[1].Content != "second" || recent != "firstsecond" {
		t.Fatalf("batch persistence mismatch: shell=%#v events=%#v recent=%q", shell, stored, recent)
	}
}

func TestOpenMigratesAddedColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shell-migration.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
CREATE TABLE ssh_shell_sessions (
  id TEXT PRIMARY KEY, run_id TEXT NOT NULL UNIQUE, session_id TEXT NOT NULL,
  kind TEXT NOT NULL DEFAULT 'ssh', surface TEXT NOT NULL, host_id TEXT NOT NULL,
  host_name TEXT NOT NULL, workspace_id TEXT NOT NULL DEFAULT '', backend TEXT NOT NULL DEFAULT '',
  username TEXT NOT NULL, elevated INTEGER NOT NULL DEFAULT 0, cwd TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL, cols INTEGER NOT NULL, rows INTEGER NOT NULL,
  last_sequence INTEGER NOT NULL DEFAULT 0, recent_output TEXT NOT NULL DEFAULT '',
  exit_code INTEGER, termination_reason TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL, ended_at TEXT
);
CREATE TABLE ssh_shell_events (
  shell_id TEXT NOT NULL, sequence INTEGER NOT NULL, stream TEXT NOT NULL,
  source TEXT NOT NULL DEFAULT '', content_redacted TEXT NOT NULL DEFAULT '',
  sensitive INTEGER NOT NULL DEFAULT 0, input_bytes INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL,
  PRIMARY KEY(shell_id,sequence)
);
CREATE TABLE model_providers (
  id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, kind TEXT NOT NULL,
  base_url TEXT NOT NULL DEFAULT '', model TEXT NOT NULL,
  api_key_cipher TEXT NOT NULL DEFAULT '', proxy_id TEXT NOT NULL DEFAULT '',
  user_agent TEXT NOT NULL DEFAULT '', active INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE chat_messages (
  id TEXT PRIMARY KEY, session_id TEXT NOT NULL, role TEXT NOT NULL,
  content TEXT NOT NULL, tool_name TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'completed', created_at TEXT NOT NULL
);`)
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}

	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for table, column := range map[string]string{
		"ssh_shell_sessions": "response_sequence",
		"ssh_shell_events":   "content_readable",
		"model_providers":    "reasoning_effort",
		"chat_messages":      "model_extra_json",
	} {
		rows, err := st.db.QueryContext(context.Background(), "PRAGMA table_info("+table+")")
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for rows.Next() {
			var cid int
			var name, columnType string
			var notNull, primaryKey int
			var defaultValue any
			if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			found = found || name == column
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if !found {
			t.Fatalf("%s.%s was not migrated", table, column)
		}
	}
}
