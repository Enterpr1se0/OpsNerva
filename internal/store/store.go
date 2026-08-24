package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/ids"

	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrAlreadyExists = errors.New("already exists")
	ErrInUse         = errors.New("in use")
)

type Store struct {
	db                *sql.DB
	shellEventEncoder *zstd.Encoder
	shellEventDecoder *zstd.Decoder
}

func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// DSN pragmas are applied to every pooled connection. WAL then permits
	// readers to proceed while the single SQLite writer is committing.
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	dsn := path + separator + "_pragma=foreign_keys%3Don&_pragma=busy_timeout%3D5000&_pragma=journal_mode%3DWAL&_pragma=synchronous%3DNORMAL"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	poolSize := 4
	if path == ":memory:" {
		// Each :memory: connection owns a different database.
		poolSize = 1
	}
	db.SetMaxOpenConns(poolSize)
	db.SetMaxIdleConns(poolSize)
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys = ON; PRAGMA journal_mode = WAL; PRAGMA synchronous = NORMAL; PRAGMA busy_timeout = 5000;"); err != nil {
		db.Close()
		return nil, err
	}
	shellEventEncoder, err := zstd.NewWriter(nil,
		zstd.WithEncoderConcurrency(1),
		zstd.WithEncoderLevel(zstd.SpeedFastest),
	)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize shell event compressor: %w", err)
	}
	shellEventDecoder, err := zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(maxSSHShellEventDecodedBytes),
	)
	if err != nil {
		shellEventEncoder.Close()
		db.Close()
		return nil, fmt.Errorf("initialize shell event decompressor: %w", err)
	}
	st := &Store{db: db, shellEventEncoder: shellEventEncoder, shellEventDecoder: shellEventDecoder}
	if err := st.initializeSchema(ctx); err != nil {
		shellEventDecoder.Close()
		shellEventEncoder.Close()
		db.Close()
		return nil, err
	}
	return st, nil
}

func (s *Store) Close() error {
	s.shellEventDecoder.Close()
	return errors.Join(s.shellEventEncoder.Close(), s.db.Close())
}

func (s *Store) UpsertTask(ctx context.Context, task domain.Task, result domain.ExecResult, taskError string) error {
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return err
	}
	var ended any
	if !task.EndedAt.IsZero() {
		ended = formatTime(task.EndedAt)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO tasks(id,run_id,host_id,status,result_json,error,started_at,ended_at) VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET run_id=excluded.run_id,status=excluded.status,result_json=excluded.result_json,error=excluded.error,ended_at=excluded.ended_at`,
		task.ID, task.RunID, task.HostID, task.Status, string(resultJSON), taskError, formatTime(task.StartedAt), ended)
	return err
}

func (s *Store) GetTask(ctx context.Context, id string) (domain.Task, domain.ExecResult, string, error) {
	var task domain.Task
	var resultJSON, taskError, started string
	var ended sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id,run_id,host_id,status,result_json,error,started_at,ended_at FROM tasks WHERE id=?`, id).Scan(
		&task.ID, &task.RunID, &task.HostID, &task.Status, &resultJSON, &taskError, &started, &ended,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Task{}, domain.ExecResult{}, "", ErrNotFound
	}
	if err != nil {
		return domain.Task{}, domain.ExecResult{}, "", err
	}
	task.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
	if ended.Valid {
		task.EndedAt, _ = time.Parse(time.RFC3339Nano, ended.String)
	}
	var result domain.ExecResult
	_ = json.Unmarshal([]byte(resultJSON), &result)
	return task, result, taskError, nil
}

func (s *Store) InterruptActiveTasks(ctx context.Context) error {
	now := formatTime(time.Now().UTC())
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET status='interrupted',error=CASE WHEN error='' THEN 'control plane restarted before the task completed' ELSE error END,ended_at=? WHERE status IN ('running','waiting_for_approval','approval_required')`, now)
	return err
}

func (s *Store) InterruptActiveRuns(ctx context.Context, reason string) error {
	now := formatTime(time.Now().UTC())
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET status='interrupted',
error=CASE WHEN error='' THEN ? ELSE error END,completed_at=COALESCE(completed_at,?)
WHERE status IN ('created','running')`, reason, now)
	return err
}

func (s *Store) FailPendingChatMessages(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE chat_messages SET status='failed'
WHERE role='user' AND status='pending' AND NOT EXISTS (
  SELECT 1 FROM chat_tool_calls
  JOIN approvals ON approvals.run_id=chat_tool_calls.run_id
  JOIN checkpoints ON checkpoints.id=approvals.checkpoint_id
  WHERE chat_tool_calls.user_message_id=chat_messages.id
    AND approvals.continuation_kind=? AND approvals.interrupt_id<>''
)`, domain.ApprovalContinuationAgent)
	return err
}

func (s *Store) AgentToolStates(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name,enabled FROM agent_tool_settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]bool)
	for rows.Next() {
		var name string
		var enabled int
		if err := rows.Scan(&name, &enabled); err != nil {
			return nil, err
		}
		result[name] = enabled != 0
	}
	return result, rows.Err()
}

func (s *Store) SetAgentToolEnabled(ctx context.Context, name string, enabled bool) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO agent_tool_settings(name,enabled,updated_at) VALUES(?,?,?)
ON CONFLICT(name) DO UPDATE SET enabled=excluded.enabled,updated_at=excluded.updated_at`, name, boolInt(enabled), formatTime(time.Now().UTC()))
	return err
}

func (s *Store) InitializeWorkspaces(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var initialized int
	err = tx.QueryRowContext(ctx, `SELECT initialized FROM workspace_state WHERE id=1`).Scan(&initialized)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) || initialized == 0 {
		now := formatTime(time.Now().UTC())
		if _, err := tx.ExecContext(ctx, `INSERT INTO workspaces(id,access,created_at,updated_at) VALUES('default','read_write',?,?)
ON CONFLICT(id) DO NOTHING`, now, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO workspace_state(id,initialized) VALUES(1,1)
ON CONFLICT(id) DO UPDATE SET initialized=1`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListWorkspaces(ctx context.Context) ([]domain.Workspace, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,access,created_at,updated_at FROM workspaces ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.Workspace, 0)
	for rows.Next() {
		workspace, err := scanWorkspace(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, workspace)
	}
	return result, rows.Err()
}

func (s *Store) CreateWorkspace(ctx context.Context, workspace domain.Workspace) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO workspaces(id,access,created_at,updated_at) VALUES(?,?,?,?)`,
		workspace.ID, workspace.Access, formatTime(workspace.CreatedAt), formatTime(workspace.UpdatedAt))
	return err
}

func (s *Store) UpdateWorkspace(ctx context.Context, workspace domain.Workspace) error {
	result, err := s.db.ExecContext(ctx, `UPDATE workspaces SET access=?,updated_at=? WHERE id=?`,
		workspace.Access, formatTime(workspace.UpdatedAt), workspace.ID)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteWorkspace(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE id=?`, id)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chat_sessions SET workspace_id='' WHERE workspace_id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func scanWorkspace(row scanner) (domain.Workspace, error) {
	var workspace domain.Workspace
	var created, updated string
	err := row.Scan(&workspace.ID, &workspace.Access, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Workspace{}, ErrNotFound
	}
	if err != nil {
		return domain.Workspace{}, err
	}
	workspace.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	workspace.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return workspace, nil
}

func (s *Store) initializeSchema(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS hosts (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  address TEXT NOT NULL,
  port INTEGER NOT NULL,
  username TEXT NOT NULL,
  agent_enabled INTEGER NOT NULL DEFAULT 1,
  auth_type TEXT NOT NULL DEFAULT 'agent',
	  private_key_cipher TEXT NOT NULL DEFAULT '',
  known_hosts_file TEXT NOT NULL DEFAULT '',
	  proxy_jump_host_id TEXT NOT NULL DEFAULT '',
	  proxy_id TEXT NOT NULL DEFAULT '',
	  password_cipher TEXT NOT NULL DEFAULT '',
	  sudo_mode TEXT NOT NULL DEFAULT 'none',
	  sudo_password_cipher TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS runs (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL DEFAULT '',
  host_id TEXT NOT NULL,
  tool_name TEXT NOT NULL DEFAULT '',
  tool_arguments_json TEXT NOT NULL DEFAULT '',
  request_json TEXT NOT NULL,
  request_cipher TEXT NOT NULL DEFAULT '',
  search_text TEXT NOT NULL DEFAULT '',
  request_digest TEXT NOT NULL,
  status TEXT NOT NULL,
  exit_code INTEGER NOT NULL DEFAULT 0,
  stdout_redacted TEXT NOT NULL DEFAULT '',
  stderr_redacted TEXT NOT NULL DEFAULT '',
  stdout_cipher TEXT NOT NULL DEFAULT '',
  stderr_cipher TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  ai_review_json TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL,
  completed_at TEXT,
  FOREIGN KEY(host_id) REFERENCES hosts(id)
);
CREATE INDEX IF NOT EXISTS idx_runs_host_started ON runs(host_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_runs_session_started_id ON runs(session_id, started_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_runs_started_id ON runs(started_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_runs_digest ON runs(request_digest);
CREATE TABLE IF NOT EXISTS approvals (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL UNIQUE,
  host_id TEXT NOT NULL,
  request_json TEXT NOT NULL,
  request_cipher TEXT NOT NULL DEFAULT '',
  request_digest TEXT NOT NULL,
  status TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '',
  continuation_kind TEXT NOT NULL DEFAULT '',
  checkpoint_id TEXT NOT NULL DEFAULT '',
  interrupt_id TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  decided_at TEXT,
  FOREIGN KEY(run_id) REFERENCES runs(id),
  FOREIGN KEY(host_id) REFERENCES hosts(id)
);
CREATE INDEX IF NOT EXISTS idx_approvals_status ON approvals(status, created_at DESC);
CREATE TABLE IF NOT EXISTS audit_events (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL DEFAULT '',
  event_type TEXT NOT NULL,
  actor TEXT NOT NULL,
  data_json TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_events(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_run_created ON audit_events(run_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_created_id ON audit_events(created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_audit_run_created_id ON audit_events(run_id, created_at DESC, id DESC);
CREATE TABLE IF NOT EXISTS chat_sessions (
  session_id TEXT PRIMARY KEY,
  title TEXT NOT NULL DEFAULT '',
  workspace_id TEXT NOT NULL DEFAULT '',
	context_tokens INTEGER NOT NULL DEFAULT 0,
	context_window INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_chat_sessions_updated ON chat_sessions(updated_at DESC, session_id DESC);
CREATE TABLE IF NOT EXISTS chat_messages (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  role TEXT NOT NULL,
  content TEXT NOT NULL,
	  model_extra_json TEXT NOT NULL DEFAULT '{}',
	  tool_name TEXT NOT NULL DEFAULT '',
	  status TEXT NOT NULL DEFAULT 'completed',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_chat_session ON chat_messages(session_id, created_at);
CREATE INDEX IF NOT EXISTS idx_chat_session_role_created ON chat_messages(session_id, role, created_at, id);
CREATE TABLE IF NOT EXISTS chat_context_summaries (
  session_id TEXT PRIMARY KEY,
  summary TEXT NOT NULL,
  through_message_id TEXT NOT NULL,
  revision INTEGER NOT NULL DEFAULT 1,
  trigger TEXT NOT NULL,
  source_tokens INTEGER NOT NULL DEFAULT 0,
  summary_tokens INTEGER NOT NULL DEFAULT 0,
  model TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(session_id) REFERENCES chat_sessions(session_id) ON DELETE CASCADE,
  FOREIGN KEY(through_message_id) REFERENCES chat_messages(id)
);
CREATE TABLE IF NOT EXISTS chat_message_context_usage (
  message_id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  total_tokens INTEGER NOT NULL DEFAULT 0,
  cached_tokens INTEGER NOT NULL DEFAULT 0,
  reasoning_tokens INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  FOREIGN KEY(message_id) REFERENCES chat_messages(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_chat_message_context_usage_session ON chat_message_context_usage(session_id,created_at);
CREATE TABLE IF NOT EXISTS chat_tool_calls (
  session_id TEXT NOT NULL,
  user_message_id TEXT NOT NULL,
  message_id TEXT NOT NULL UNIQUE,
  tool_call_id TEXT NOT NULL,
  run_id TEXT NOT NULL DEFAULT '',
  tool_name TEXT NOT NULL,
  arguments_json TEXT NOT NULL DEFAULT '{}',
  status TEXT NOT NULL,
  result_json TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  completed_at TEXT,
  PRIMARY KEY(session_id,tool_call_id),
  FOREIGN KEY(message_id) REFERENCES chat_messages(id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_chat_tool_calls_run ON chat_tool_calls(run_id) WHERE run_id<>'';
CREATE INDEX IF NOT EXISTS idx_chat_tool_calls_session_status ON chat_tool_calls(session_id,status,started_at);
CREATE TABLE IF NOT EXISTS chat_attachments (
  id TEXT PRIMARY KEY,
  message_id TEXT NOT NULL,
  name TEXT NOT NULL,
  mime_type TEXT NOT NULL,
  size_bytes INTEGER NOT NULL,
  data BLOB NOT NULL,
  created_at TEXT NOT NULL,
  FOREIGN KEY(message_id) REFERENCES chat_messages(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_chat_attachments_message ON chat_attachments(message_id, created_at);
CREATE TABLE IF NOT EXISTS agent_task_files (
  session_id TEXT NOT NULL,
  file_path TEXT NOT NULL,
  content TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(session_id,file_path)
);
DROP INDEX IF EXISTS idx_agent_task_files_session;
CREATE TABLE IF NOT EXISTS tasks (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL DEFAULT '',
  host_id TEXT NOT NULL,
  status TEXT NOT NULL,
  result_json TEXT NOT NULL DEFAULT '{}',
  error TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL,
  ended_at TEXT,
  FOREIGN KEY(host_id) REFERENCES hosts(id)
);
CREATE INDEX IF NOT EXISTS idx_tasks_started ON tasks(started_at DESC);
CREATE INDEX IF NOT EXISTS idx_tasks_run ON tasks(run_id);
CREATE TABLE IF NOT EXISTS ssh_shell_sessions (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL UNIQUE,
  session_id TEXT NOT NULL,
  kind TEXT NOT NULL DEFAULT 'ssh',
  surface TEXT NOT NULL,
  host_id TEXT NOT NULL,
  host_name TEXT NOT NULL,
  workspace_id TEXT NOT NULL DEFAULT '',
  backend TEXT NOT NULL DEFAULT '',
  username TEXT NOT NULL,
  elevated INTEGER NOT NULL DEFAULT 0,
  cwd TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,
  cols INTEGER NOT NULL,
  rows INTEGER NOT NULL,
  last_sequence INTEGER NOT NULL DEFAULT 0,
  response_sequence INTEGER NOT NULL DEFAULT 0,
  recent_output TEXT NOT NULL DEFAULT '',
  exit_code INTEGER,
  termination_reason TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL,
  ended_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_ssh_shell_sessions_conversation ON ssh_shell_sessions(session_id,started_at DESC);
CREATE INDEX IF NOT EXISTS idx_ssh_shell_sessions_status ON ssh_shell_sessions(status,started_at DESC);
CREATE TABLE IF NOT EXISTS ssh_shell_events (
  shell_id TEXT NOT NULL,
  sequence INTEGER NOT NULL,
  stream TEXT NOT NULL,
  source TEXT NOT NULL DEFAULT '',
  content_redacted TEXT NOT NULL DEFAULT '',
  content_readable TEXT,
  content_encoding TEXT NOT NULL DEFAULT '',
  sensitive INTEGER NOT NULL DEFAULT 0,
  input_bytes INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  PRIMARY KEY(shell_id,sequence),
  FOREIGN KEY(shell_id) REFERENCES ssh_shell_sessions(id) ON DELETE CASCADE
);
DROP INDEX IF EXISTS idx_ssh_shell_events_sequence;
CREATE TABLE IF NOT EXISTS checkpoints (
  id TEXT PRIMARY KEY,
  data BLOB NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS proxies (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  url TEXT NOT NULL,
  username TEXT NOT NULL DEFAULT '',
  password_cipher TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS model_providers (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  kind TEXT NOT NULL,
  base_url TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL,
	context_window INTEGER NOT NULL DEFAULT 0,
  api_key_cipher TEXT NOT NULL DEFAULT '',
  proxy_id TEXT NOT NULL DEFAULT '',
  user_agent TEXT NOT NULL DEFAULT '',
  reasoning_effort TEXT NOT NULL DEFAULT '',
  active INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_model_providers_active ON model_providers(active) WHERE active=1;
CREATE TABLE IF NOT EXISTS system_settings (
  id INTEGER PRIMARY KEY CHECK(id=1),
  agent_max_iterations INTEGER NOT NULL,
  system_prompt TEXT DEFAULT NULL,
  approval_mode TEXT NOT NULL DEFAULT 'manual',
  approval_explanations_enabled INTEGER NOT NULL DEFAULT 1,
  subagent_model_provider_id TEXT NOT NULL DEFAULT '',
  subagent_timeout_seconds INTEGER NOT NULL DEFAULT 30,
  context_compression_enabled INTEGER NOT NULL DEFAULT 1,
  context_compression_threshold_percent INTEGER NOT NULL DEFAULT 70,
  chat_image_allowed_types_json TEXT NOT NULL DEFAULT '["image/png","image/jpeg","image/webp","image/gif"]',
  workspace_shell_mode TEXT NOT NULL DEFAULT 'sandbox',
  mcp_http_enabled INTEGER NOT NULL DEFAULT 0,
  mcp_http_token_hash TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS automatic_approval_settings (
  id INTEGER PRIMARY KEY CHECK(id=1),
  model_provider_id TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS mcp_servers (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  transport TEXT NOT NULL,
  command TEXT NOT NULL DEFAULT '',
  args_json TEXT NOT NULL DEFAULT '[]',
  cwd TEXT NOT NULL DEFAULT '',
  url TEXT NOT NULL DEFAULT '',
  env_keys_json TEXT NOT NULL DEFAULT '[]',
  header_keys_json TEXT NOT NULL DEFAULT '[]',
  secrets_cipher TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_mcp_servers_enabled ON mcp_servers(enabled,name);
CREATE TABLE IF NOT EXISTS mcp_client_sessions (
  id TEXT PRIMARY KEY,
  transport TEXT NOT NULL,
  client_name TEXT NOT NULL DEFAULT '',
  client_version TEXT NOT NULL DEFAULT '',
  protocol_version TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL,
  last_seen_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_mcp_client_sessions_seen ON mcp_client_sessions(last_seen_at DESC,id DESC);
CREATE TABLE IF NOT EXISTS mcp_tool_calls (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  tool_name TEXT NOT NULL,
  arguments_json TEXT NOT NULL DEFAULT '{}',
  status TEXT NOT NULL,
  run_id TEXT NOT NULL DEFAULT '',
  approval_id TEXT NOT NULL DEFAULT '',
  task_id TEXT NOT NULL DEFAULT '',
  shell_id TEXT NOT NULL DEFAULT '',
  tunnel_id TEXT NOT NULL DEFAULT '',
  operation_status TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  completed_at TEXT,
  FOREIGN KEY(session_id) REFERENCES mcp_client_sessions(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_mcp_tool_calls_session_started ON mcp_tool_calls(session_id,started_at DESC,id DESC);
CREATE INDEX IF NOT EXISTS idx_mcp_tool_calls_session_status ON mcp_tool_calls(session_id,status);
CREATE INDEX IF NOT EXISTS idx_mcp_tool_calls_status_updated ON mcp_tool_calls(status,updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_mcp_tool_calls_run ON mcp_tool_calls(run_id) WHERE run_id<>'';
CREATE TABLE IF NOT EXISTS agent_tool_settings (
  name TEXT PRIMARY KEY,
  enabled INTEGER NOT NULL DEFAULT 1,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS workspaces (
  id TEXT PRIMARY KEY,
  access TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS workspace_state (
  id INTEGER PRIMARY KEY CHECK(id=1),
  initialized INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS web_search_settings (
  id INTEGER PRIMARY KEY CHECK(id=1),
  enabled INTEGER NOT NULL DEFAULT 0,
  provider TEXT NOT NULL DEFAULT 'tavily',
  base_url TEXT NOT NULL DEFAULT 'https://api.tavily.com',
  api_key_cipher TEXT NOT NULL DEFAULT '',
  proxy_id TEXT NOT NULL DEFAULT '',
  timeout_seconds INTEGER NOT NULL DEFAULT 20,
  max_results INTEGER NOT NULL DEFAULT 10,
  updated_at TEXT NOT NULL
);
`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "ssh_shell_events", "content_readable", "TEXT"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "ssh_shell_events", "content_encoding", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "ssh_shell_sessions", "response_sequence", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "model_providers", "reasoning_effort", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "model_providers", "context_window", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "chat_sessions", "context_tokens", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "chat_sessions", "context_window", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "chat_sessions", "title", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "chat_messages", "model_extra_json", "TEXT NOT NULL DEFAULT '{}'"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "approvals", "continuation_kind", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "approvals", "checkpoint_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "approvals", "interrupt_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_approvals_continuation_checkpoint
ON approvals(continuation_kind, checkpoint_id, created_at)`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "system_settings", "context_compression_enabled", "INTEGER NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "system_settings", "context_compression_threshold_percent", "INTEGER NOT NULL DEFAULT 70"); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO system_settings(
id,agent_max_iterations,workspace_shell_mode,updated_at) VALUES(1,?,?,?)`,
		domain.DefaultAgentMaxIterations, domain.DefaultWorkspaceShellMode(runtime.GOOS), formatTime(time.Now().UTC()))
	return err
}

func (s *Store) ensureColumn(ctx context.Context, table, column, definition string) error {
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = s.db.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN "+column+" "+definition)
	return err
}

func (s *Store) UpsertHost(ctx context.Context, host domain.Host) (domain.Host, error) {
	now := time.Now().UTC()
	if host.ID == "" {
		host.ID = ids.New("host")
		host.CreatedAt = now
	}
	if host.Port == 0 {
		host.Port = 22
	}
	host.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `
INSERT INTO hosts(id,name,address,port,username,agent_enabled,auth_type,private_key_cipher,known_hosts_file,proxy_jump_host_id,proxy_id,password_cipher,sudo_mode,sudo_password_cipher,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name,address=excluded.address,port=excluded.port,
username=excluded.username,agent_enabled=excluded.agent_enabled,auth_type=excluded.auth_type,private_key_cipher=excluded.private_key_cipher,
known_hosts_file=excluded.known_hosts_file,proxy_jump_host_id=excluded.proxy_jump_host_id,
proxy_id=excluded.proxy_id,password_cipher=excluded.password_cipher,
sudo_mode=excluded.sudo_mode,sudo_password_cipher=excluded.sudo_password_cipher,updated_at=excluded.updated_at`,
		host.ID, host.Name, host.Address, host.Port, host.User, boolInt(host.AgentEnabled), host.AuthType, host.PrivateKeyCipher,
		host.KnownHostsFile, host.ProxyJumpHostID, host.ProxyID,
		host.PasswordCipher, host.SudoMode, host.SudoCipher,
		formatTime(host.CreatedAt), formatTime(host.UpdatedAt))
	if err != nil {
		return domain.Host{}, err
	}
	return s.GetHost(ctx, host.ID)
}

func (s *Store) GetHost(ctx context.Context, id string) (domain.Host, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,name,address,port,username,agent_enabled,auth_type,private_key_cipher,
known_hosts_file,proxy_jump_host_id,proxy_id,password_cipher,
sudo_mode,sudo_password_cipher,created_at,updated_at FROM hosts WHERE id=? OR name=?`, id, id)
	return scanHost(row)
}

func (s *Store) UpsertProxy(ctx context.Context, proxy domain.Proxy) (domain.Proxy, error) {
	now := time.Now().UTC()
	if proxy.ID == "" {
		proxy.ID = ids.New("proxy")
		proxy.CreatedAt = now
	}
	proxy.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `INSERT INTO proxies(id,name,url,username,password_cipher,created_at,updated_at)
VALUES(?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name,url=excluded.url,username=excluded.username,
password_cipher=excluded.password_cipher,updated_at=excluded.updated_at`,
		proxy.ID, proxy.Name, proxy.URL, proxy.Username, proxy.PasswordCipher,
		formatTime(proxy.CreatedAt), formatTime(proxy.UpdatedAt))
	if err != nil {
		return domain.Proxy{}, err
	}
	return s.GetProxy(ctx, proxy.ID)
}

func (s *Store) GetProxy(ctx context.Context, id string) (domain.Proxy, error) {
	return scanProxy(s.db.QueryRowContext(ctx, `SELECT id,name,url,username,password_cipher,created_at,updated_at FROM proxies WHERE id=?`, id))
}

func (s *Store) ListProxies(ctx context.Context) ([]domain.Proxy, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,url,username,password_cipher,created_at,updated_at FROM proxies ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.Proxy, 0)
	for rows.Next() {
		proxy, err := scanProxy(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, proxy)
	}
	return result, rows.Err()
}

func (s *Store) DeleteProxy(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM proxies WHERE id=?
AND NOT EXISTS(SELECT 1 FROM model_providers WHERE proxy_id=?)
AND NOT EXISTS(SELECT 1 FROM hosts WHERE proxy_id=?)
AND NOT EXISTS(SELECT 1 FROM web_search_settings WHERE id=1 AND proxy_id=?)`, id, id, id, id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		var exists int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM proxies WHERE id=?`, id).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return ErrNotFound
		}
		return ErrInUse
	}
	return nil
}

func (s *Store) ProxyReferences(ctx context.Context, id string) ([]string, error) {
	result := make([]string, 0)
	rows, err := s.db.QueryContext(ctx, `SELECT 'model provider: '||name FROM model_providers WHERE proxy_id=?
UNION ALL SELECT 'SSH host: '||name FROM hosts WHERE proxy_id=?
UNION ALL SELECT 'Tavily Web' FROM web_search_settings WHERE id=1 AND proxy_id=?`, id, id, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var reference string
		if err := rows.Scan(&reference); err != nil {
			return nil, err
		}
		result = append(result, reference)
	}
	return result, rows.Err()
}

func (s *Store) UpsertModelProvider(ctx context.Context, provider domain.ModelProvider) (domain.ModelProvider, error) {
	now := time.Now().UTC()
	if provider.ID == "" {
		provider.ID = ids.New("model")
		provider.CreatedAt = now
	}
	provider.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `
INSERT INTO model_providers(id,name,kind,base_url,model,context_window,api_key_cipher,proxy_id,user_agent,reasoning_effort,active,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name,kind=excluded.kind,base_url=excluded.base_url,
model=excluded.model,context_window=excluded.context_window,api_key_cipher=excluded.api_key_cipher,proxy_id=excluded.proxy_id,
user_agent=excluded.user_agent,reasoning_effort=excluded.reasoning_effort,updated_at=excluded.updated_at`,
		provider.ID, provider.Name, provider.Kind, provider.BaseURL, provider.Model, provider.ContextWindow, provider.APIKeyCipher,
		provider.ProxyID, provider.UserAgent, provider.ReasoningEffort,
		boolInt(provider.Active), formatTime(provider.CreatedAt), formatTime(provider.UpdatedAt))
	if err != nil {
		return domain.ModelProvider{}, err
	}
	return s.GetModelProvider(ctx, provider.ID)
}

func (s *Store) GetModelProvider(ctx context.Context, id string) (domain.ModelProvider, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,name,kind,base_url,model,context_window,api_key_cipher,proxy_id,user_agent,reasoning_effort,active,created_at,updated_at
FROM model_providers WHERE id=?`, id)
	return scanModelProvider(row)
}

func (s *Store) ActiveModelProvider(ctx context.Context) (domain.ModelProvider, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,name,kind,base_url,model,context_window,api_key_cipher,proxy_id,user_agent,reasoning_effort,active,created_at,updated_at
FROM model_providers WHERE active=1 LIMIT 1`)
	return scanModelProvider(row)
}

func (s *Store) ListModelProviders(ctx context.Context) ([]domain.ModelProvider, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,kind,base_url,model,context_window,api_key_cipher,proxy_id,user_agent,reasoning_effort,active,created_at,updated_at
FROM model_providers ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.ModelProvider, 0)
	for rows.Next() {
		provider, err := scanModelProvider(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, provider)
	}
	return result, rows.Err()
}

func (s *Store) ActivateModelProvider(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE model_providers SET active=0 WHERE active=1`); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE model_providers SET active=1,updated_at=? WHERE id=?`, formatTime(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) DeleteModelProvider(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM model_providers WHERE id=?`, id)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) UpsertMCPServer(ctx context.Context, server domain.MCPServer) (domain.MCPServer, error) {
	now := time.Now().UTC()
	if server.ID == "" {
		server.ID = ids.New("mcp")
		server.CreatedAt = now
	}
	server.UpdatedAt = now
	argsJSON, err := json.Marshal(server.Args)
	if err != nil {
		return domain.MCPServer{}, err
	}
	envKeysJSON, err := json.Marshal(server.EnvKeys)
	if err != nil {
		return domain.MCPServer{}, err
	}
	headerKeysJSON, err := json.Marshal(server.HeaderKeys)
	if err != nil {
		return domain.MCPServer{}, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO mcp_servers(id,name,transport,command,args_json,cwd,url,env_keys_json,header_keys_json,secrets_cipher,enabled,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name,transport=excluded.transport,command=excluded.command,args_json=excluded.args_json,
cwd=excluded.cwd,url=excluded.url,env_keys_json=excluded.env_keys_json,header_keys_json=excluded.header_keys_json,
secrets_cipher=excluded.secrets_cipher,enabled=excluded.enabled,updated_at=excluded.updated_at`,
		server.ID, server.Name, server.Transport, server.Command, string(argsJSON), server.Cwd, server.URL, string(envKeysJSON),
		string(headerKeysJSON), server.SecretsCipher, boolInt(server.Enabled), formatTime(server.CreatedAt), formatTime(server.UpdatedAt))
	if err != nil {
		return domain.MCPServer{}, err
	}
	return s.GetMCPServer(ctx, server.ID)
}

func (s *Store) GetMCPServer(ctx context.Context, id string) (domain.MCPServer, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,name,transport,command,args_json,cwd,url,env_keys_json,header_keys_json,secrets_cipher,enabled,created_at,updated_at FROM mcp_servers WHERE id=?`, id)
	return scanMCPServer(row)
}

func (s *Store) ListMCPServers(ctx context.Context) ([]domain.MCPServer, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,transport,command,args_json,cwd,url,env_keys_json,header_keys_json,secrets_cipher,enabled,created_at,updated_at FROM mcp_servers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.MCPServer, 0)
	for rows.Next() {
		server, err := scanMCPServer(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, server)
	}
	return result, rows.Err()
}

func (s *Store) SetMCPServerEnabled(ctx context.Context, id string, enabled bool) error {
	result, err := s.db.ExecContext(ctx, `UPDATE mcp_servers SET enabled=?,updated_at=? WHERE id=?`, boolInt(enabled), formatTime(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) UpdateMCPServerSecrets(ctx context.Context, id, secretsCipher string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE mcp_servers SET secrets_cipher=?,updated_at=? WHERE id=?`,
		secretsCipher, formatTime(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteMCPServer(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM mcp_servers WHERE id=?`, id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return ErrNotFound
	}
	return nil
}

func scanMCPServer(row scanner) (domain.MCPServer, error) {
	var server domain.MCPServer
	var argsJSON, envKeysJSON, headerKeysJSON, created, updated string
	var enabled int
	err := row.Scan(&server.ID, &server.Name, &server.Transport, &server.Command, &argsJSON, &server.Cwd, &server.URL,
		&envKeysJSON, &headerKeysJSON, &server.SecretsCipher, &enabled, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.MCPServer{}, ErrNotFound
	}
	if err != nil {
		return domain.MCPServer{}, err
	}
	if err := json.Unmarshal([]byte(argsJSON), &server.Args); err != nil {
		return domain.MCPServer{}, err
	}
	if err := json.Unmarshal([]byte(envKeysJSON), &server.EnvKeys); err != nil {
		return domain.MCPServer{}, err
	}
	if err := json.Unmarshal([]byte(headerKeysJSON), &server.HeaderKeys); err != nil {
		return domain.MCPServer{}, err
	}
	server.Enabled = enabled != 0
	server.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	server.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return server, nil
}

func (s *Store) GetSystemSettings(ctx context.Context) (domain.SystemSettings, error) {
	var settings domain.SystemSettings
	var explanationsEnabled int
	var contextCompressionEnabled int
	var mcpHTTPEnabled int
	var imageTypesJSON string
	var systemPrompt sql.NullString
	var updated string
	err := s.db.QueryRowContext(ctx, `SELECT agent_max_iterations,system_prompt,approval_mode,approval_explanations_enabled,subagent_model_provider_id,subagent_timeout_seconds,
context_compression_enabled,context_compression_threshold_percent,chat_image_allowed_types_json,workspace_shell_mode,mcp_http_enabled,mcp_http_token_hash,updated_at FROM system_settings WHERE id=1`).Scan(
		&settings.AgentMaxIterations, &systemPrompt, &settings.ApprovalMode, &explanationsEnabled, &settings.SubagentModelProviderID, &settings.SubagentTimeoutSeconds,
		&contextCompressionEnabled, &settings.ContextCompressionPercent, &imageTypesJSON, &settings.WorkspaceShellMode, &mcpHTTPEnabled, &settings.MCPHTTPTokenHash, &updated,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.SystemSettings{
			AgentMaxIterations: domain.DefaultAgentMaxIterations, ApprovalExplanationsEnabled: true,
			ApprovalMode: domain.ApprovalModeManual,
			SystemPrompt: domain.DefaultSystemPrompt, DefaultSystemPrompt: domain.DefaultSystemPrompt,
			SubagentTimeoutSeconds: domain.DefaultSubagentTimeoutSeconds, WorkspaceShellMode: domain.DefaultWorkspaceShellMode(runtime.GOOS),
			ContextCompressionEnabled: true, ContextCompressionPercent: domain.DefaultContextCompressionPercent,
			ChatImageAllowedTypes: append([]string(nil), domain.DefaultChatImageAllowedTypes...),
		}, nil
	}
	if err != nil {
		return domain.SystemSettings{}, err
	}
	settings.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	settings.DefaultSystemPrompt = domain.DefaultSystemPrompt
	if systemPrompt.Valid {
		settings.SystemPrompt = systemPrompt.String
	} else {
		settings.SystemPrompt = domain.DefaultSystemPrompt
	}
	settings.ApprovalExplanationsEnabled = explanationsEnabled != 0
	settings.ContextCompressionEnabled = contextCompressionEnabled != 0
	if settings.ContextCompressionPercent < domain.MinContextCompressionPercent || settings.ContextCompressionPercent > domain.MaxContextCompressionPercent {
		settings.ContextCompressionPercent = domain.DefaultContextCompressionPercent
	}
	settings.MCPHTTPEnabled = mcpHTTPEnabled != 0
	settings.MCPHTTPTokenConfigured = settings.MCPHTTPTokenHash != ""
	switch settings.ApprovalMode {
	case domain.ApprovalModeManual, domain.ApprovalModeAuto, domain.ApprovalModeFullAccess:
	default:
		settings.ApprovalMode = domain.ApprovalModeManual
	}
	if err := json.Unmarshal([]byte(imageTypesJSON), &settings.ChatImageAllowedTypes); err != nil || len(settings.ChatImageAllowedTypes) == 0 {
		settings.ChatImageAllowedTypes = append([]string(nil), domain.DefaultChatImageAllowedTypes...)
	}
	if settings.SubagentTimeoutSeconds < domain.MinSubagentTimeoutSeconds || settings.SubagentTimeoutSeconds > domain.MaxSubagentTimeoutSeconds {
		settings.SubagentTimeoutSeconds = domain.DefaultSubagentTimeoutSeconds
	}
	err = s.db.QueryRowContext(ctx, `SELECT model_provider_id FROM automatic_approval_settings WHERE id=1`).Scan(&settings.AutomaticApprovalModelProviderID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return domain.SystemSettings{}, err
	}
	return settings, nil
}

func (s *Store) SaveSystemSettings(ctx context.Context, settings domain.SystemSettings) (domain.SystemSettings, error) {
	settings.UpdatedAt = time.Now().UTC()
	imageTypesJSON, err := json.Marshal(settings.ChatImageAllowedTypes)
	if err != nil {
		return domain.SystemSettings{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.SystemSettings{}, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO system_settings(id,agent_max_iterations,system_prompt,approval_mode,approval_explanations_enabled,subagent_model_provider_id,subagent_timeout_seconds,context_compression_enabled,context_compression_threshold_percent,chat_image_allowed_types_json,workspace_shell_mode,mcp_http_enabled,mcp_http_token_hash,updated_at) VALUES(1,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET agent_max_iterations=excluded.agent_max_iterations,
system_prompt=excluded.system_prompt,
approval_mode=excluded.approval_mode,
approval_explanations_enabled=excluded.approval_explanations_enabled,
subagent_model_provider_id=excluded.subagent_model_provider_id,
subagent_timeout_seconds=excluded.subagent_timeout_seconds,
context_compression_enabled=excluded.context_compression_enabled,
context_compression_threshold_percent=excluded.context_compression_threshold_percent,
chat_image_allowed_types_json=excluded.chat_image_allowed_types_json,
workspace_shell_mode=excluded.workspace_shell_mode,
mcp_http_enabled=excluded.mcp_http_enabled,
mcp_http_token_hash=excluded.mcp_http_token_hash,
updated_at=excluded.updated_at`,
		settings.AgentMaxIterations, settings.SystemPrompt, settings.ApprovalMode, boolInt(settings.ApprovalExplanationsEnabled), settings.SubagentModelProviderID,
		settings.SubagentTimeoutSeconds, boolInt(settings.ContextCompressionEnabled), settings.ContextCompressionPercent, string(imageTypesJSON), settings.WorkspaceShellMode, boolInt(settings.MCPHTTPEnabled), settings.MCPHTTPTokenHash, formatTime(settings.UpdatedAt))
	if err != nil {
		return domain.SystemSettings{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO automatic_approval_settings(id,model_provider_id,updated_at) VALUES(1,?,?)
ON CONFLICT(id) DO UPDATE SET model_provider_id=excluded.model_provider_id,updated_at=excluded.updated_at`,
		settings.AutomaticApprovalModelProviderID, formatTime(settings.UpdatedAt))
	if err != nil {
		return domain.SystemSettings{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.SystemSettings{}, err
	}
	settings.MCPHTTPTokenConfigured = settings.MCPHTTPTokenHash != ""
	return settings, nil
}

func (s *Store) GetWebSearchSettings(ctx context.Context) (domain.WebSearchSettings, error) {
	var settings domain.WebSearchSettings
	var enabled int
	var updated string
	err := s.db.QueryRowContext(ctx, `SELECT enabled,provider,base_url,api_key_cipher,proxy_id,timeout_seconds,max_results,updated_at
FROM web_search_settings WHERE id=1`).Scan(
		&enabled, &settings.Provider, &settings.BaseURL, &settings.APIKeyCipher, &settings.ProxyID,
		&settings.TimeoutSeconds, &settings.MaxResults, &updated,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.WebSearchSettings{
			Provider: "tavily", BaseURL: domain.DefaultWebSearchBaseURL,
			TimeoutSeconds: domain.DefaultWebSearchTimeoutSeconds, MaxResults: domain.DefaultWebSearchMaxResults,
		}, nil
	}
	if err != nil {
		return domain.WebSearchSettings{}, err
	}
	settings.Enabled = enabled != 0
	settings.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return settings, nil
}

func (s *Store) SaveWebSearchSettings(ctx context.Context, settings domain.WebSearchSettings) (domain.WebSearchSettings, error) {
	settings.UpdatedAt = time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `INSERT INTO web_search_settings(id,enabled,provider,base_url,api_key_cipher,proxy_id,timeout_seconds,max_results,updated_at)
VALUES(1,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET enabled=excluded.enabled,provider=excluded.provider,base_url=excluded.base_url,
api_key_cipher=excluded.api_key_cipher,proxy_id=excluded.proxy_id,
timeout_seconds=excluded.timeout_seconds,max_results=excluded.max_results,
updated_at=excluded.updated_at`,
		boolInt(settings.Enabled), settings.Provider, settings.BaseURL, settings.APIKeyCipher, settings.ProxyID,
		settings.TimeoutSeconds, settings.MaxResults, formatTime(settings.UpdatedAt))
	if err != nil {
		return domain.WebSearchSettings{}, err
	}
	return settings, nil
}

func scanModelProvider(row scanner) (domain.ModelProvider, error) {
	var provider domain.ModelProvider
	var active int
	var created, updated string
	err := row.Scan(&provider.ID, &provider.Name, &provider.Kind, &provider.BaseURL, &provider.Model, &provider.ContextWindow,
		&provider.APIKeyCipher, &provider.ProxyID, &provider.UserAgent, &provider.ReasoningEffort, &active, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ModelProvider{}, ErrNotFound
	}
	if err != nil {
		return domain.ModelProvider{}, err
	}
	provider.HasAPIKey = provider.APIKeyCipher != ""
	provider.Active = active != 0
	provider.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	provider.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return provider, nil
}

func (s *Store) ListHosts(ctx context.Context) ([]domain.Host, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,address,port,username,agent_enabled,auth_type,private_key_cipher,
known_hosts_file,proxy_jump_host_id,proxy_id,password_cipher,
sudo_mode,sudo_password_cipher,created_at,updated_at FROM hosts WHERE auth_type<>'workspace' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.Host, 0)
	for rows.Next() {
		host, err := scanHost(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, host)
	}
	return result, rows.Err()
}

func (s *Store) DeleteHost(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	deletions := []struct {
		query string
		args  []any
	}{
		{`DELETE FROM ssh_shell_sessions WHERE host_id=? OR run_id IN (SELECT id FROM runs WHERE host_id=?)`, []any{id, id}},
		{`DELETE FROM approvals WHERE host_id=? OR run_id IN (SELECT id FROM runs WHERE host_id=?)`, []any{id, id}},
		{`DELETE FROM tasks WHERE host_id=? OR run_id IN (SELECT id FROM runs WHERE host_id=?)`, []any{id, id}},
		{`DELETE FROM runs WHERE host_id=?`, []any{id}},
	}
	for _, deletion := range deletions {
		if _, err := tx.ExecContext(ctx, deletion.query, deletion.args...); err != nil {
			tx.Rollback()
			return fmt.Errorf("delete host records: %w", err)
		}
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM hosts WHERE id=?`, id)
	if err != nil {
		tx.Rollback()
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		tx.Rollback()
		return ErrNotFound
	}
	return tx.Commit()
}

type scanner interface{ Scan(...any) error }

func scanHost(row scanner) (domain.Host, error) {
	var host domain.Host
	var agentEnabled int
	var created, updated string
	err := row.Scan(&host.ID, &host.Name, &host.Address, &host.Port, &host.User, &agentEnabled, &host.AuthType,
		&host.PrivateKeyCipher, &host.KnownHostsFile, &host.ProxyJumpHostID, &host.ProxyID,
		&host.PasswordCipher, &host.SudoMode, &host.SudoCipher, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Host{}, ErrNotFound
	}
	host.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	host.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	host.AgentEnabled = agentEnabled != 0
	host.HasPassword = host.PasswordCipher != ""
	host.HasSudoPassword = host.SudoCipher != ""
	host.HasPrivateKey = host.PrivateKeyCipher != ""
	return host, err
}

func scanProxy(row scanner) (domain.Proxy, error) {
	var proxy domain.Proxy
	var created, updated string
	err := row.Scan(&proxy.ID, &proxy.Name, &proxy.URL, &proxy.Username, &proxy.PasswordCipher, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Proxy{}, ErrNotFound
	}
	if err != nil {
		return domain.Proxy{}, err
	}
	proxy.HasPassword = proxy.PasswordCipher != ""
	proxy.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	proxy.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return proxy, nil
}

func (s *Store) CreateRun(ctx context.Context, run domain.Run) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO runs(id,session_id,host_id,tool_name,tool_arguments_json,request_json,request_cipher,search_text,request_digest,status,ai_review_json,
started_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, run.ID, run.SessionID, run.HostID, run.ToolName, run.ToolArgumentsJSON, run.RequestJSON, run.RequestCipher, run.SearchText, run.RequestDigest,
		run.Status, run.AIReviewJSON, formatTime(run.StartedAt))
	return err
}

func (s *Store) UpdateRun(ctx context.Context, run domain.Run) error {
	var completed any
	if !run.CompletedAt.IsZero() {
		completed = formatTime(run.CompletedAt)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET status=?,exit_code=?,stdout_redacted=?,stderr_redacted=?,
stdout_cipher=?,stderr_cipher=?,error=?,completed_at=? WHERE id=?`, run.Status, run.ExitCode,
		run.StdoutRedacted, run.StderrRedacted, run.StdoutCipher, run.StderrCipher, run.Error, completed, run.ID)
	return err
}

func (s *Store) GetRun(ctx context.Context, id string) (domain.Run, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,session_id,host_id,tool_name,tool_arguments_json,request_json,request_cipher,request_digest,status,
exit_code,stdout_redacted,stderr_redacted,stdout_cipher,stderr_cipher,error,ai_review_json,started_at,completed_at
FROM runs WHERE id=?`, id)
	return scanRun(row)
}

// likePattern wraps query in wildcards for a substring LIKE match, escaping
// every character LIKE treats specially so the query only matches literally.
func likePattern(query string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query)
	return "%" + escaped + "%"
}

func (s *Store) SearchRuns(ctx context.Context, query, hostID, sessionID string, limit int) ([]domain.Run, error) {
	return s.SearchRunsFiltered(ctx, domain.RunSearchFilter{
		Query: query, QueryScope: "all", HostID: hostID, SessionID: sessionID, Limit: limit,
	})
}

func (s *Store) SearchRunsFiltered(ctx context.Context, filter domain.RunSearchFilter) ([]domain.Run, error) {
	where, arguments, err := runSearchWhere(filter, true)
	if err != nil {
		return nil, err
	}
	statement := `SELECT id,session_id,host_id,tool_name,tool_arguments_json,request_json,request_cipher,request_digest,status,
exit_code,stdout_redacted,stderr_redacted,stdout_cipher,stderr_cipher,error,ai_review_json,started_at,completed_at
FROM runs` + where + " ORDER BY started_at DESC,id DESC"
	if filter.Limit > 0 {
		statement += " LIMIT ?"
		arguments = append(arguments, filter.Limit)
	}
	rows, err := s.db.QueryContext(ctx, statement, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.Run, 0)
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, run)
	}
	return result, rows.Err()
}

func runSearchWhere(filter domain.RunSearchFilter, literalQuery bool) (string, []any, error) {
	statement := " WHERE 1=1"
	arguments := make([]any, 0, 16)
	if filter.SessionID != "" {
		statement += " AND session_id=?"
		arguments = append(arguments, filter.SessionID)
	}
	if filter.HostID != "" {
		statement += " AND host_id=?"
		arguments = append(arguments, filter.HostID)
	}
	if filter.ToolName != "" {
		statement += " AND tool_name=?"
		arguments = append(arguments, filter.ToolName)
	}
	if filter.Status != "" {
		statement += " AND status=?"
		arguments = append(arguments, filter.Status)
	}
	if !filter.StartedAfter.IsZero() {
		statement += " AND started_at>=?"
		arguments = append(arguments, formatTime(filter.StartedAfter.UTC()))
	}
	if !filter.StartedBefore.IsZero() {
		statement += " AND started_at<=?"
		arguments = append(arguments, formatTime(filter.StartedBefore.UTC()))
	}
	if filter.CursorStarted.IsZero() != (filter.CursorID == "") {
		return "", nil, fmt.Errorf("invalid history cursor boundary")
	}
	if !filter.CursorStarted.IsZero() {
		cursorTime := formatTime(filter.CursorStarted.UTC())
		statement += " AND (started_at<? OR (started_at=? AND id<?))"
		arguments = append(arguments, cursorTime, cursorTime, filter.CursorID)
	}
	if literalQuery && filter.Query != "" {
		pattern := likePattern(filter.Query)
		switch filter.QueryScope {
		case "", "all":
			statement += ` AND (search_text LIKE ? ESCAPE '\' OR request_json LIKE ? ESCAPE '\' OR tool_arguments_json LIKE ? ESCAPE '\'
				OR stdout_redacted LIKE ? ESCAPE '\' OR stderr_redacted LIKE ? ESCAPE '\')`
			arguments = append(arguments, pattern, pattern, pattern, pattern, pattern)
		case "request":
			statement += ` AND (search_text LIKE ? ESCAPE '\' OR request_json LIKE ? ESCAPE '\' OR tool_arguments_json LIKE ? ESCAPE '\')`
			arguments = append(arguments, pattern, pattern, pattern)
		case "output":
			statement += ` AND (stdout_redacted LIKE ? ESCAPE '\' OR stderr_redacted LIKE ? ESCAPE '\')`
			arguments = append(arguments, pattern, pattern)
		default:
			return "", nil, fmt.Errorf("invalid history query_scope: use all, request, or output")
		}
	}
	return statement, arguments, nil
}

// SearchRunSummariesFilteredPage is the bounded literal history search.
// It only selects fields needed for summaries and always requires a bounded
// page size; detail and legacy CLI callers continue to use SearchRunsFiltered.
func (s *Store) SearchRunSummariesFilteredPage(ctx context.Context, filter domain.RunSearchFilter) (domain.RunSearchPage, error) {
	if filter.Limit <= 0 {
		return domain.RunSearchPage{}, fmt.Errorf("history page limit must be positive")
	}
	where, arguments, err := runSearchWhere(filter, true)
	if err != nil {
		return domain.RunSearchPage{}, err
	}
	statement := `SELECT id,session_id,host_id,tool_name,request_json,status,exit_code,ai_review_json,started_at,completed_at
FROM runs` + where + " ORDER BY started_at DESC,id DESC LIMIT ?"
	arguments = append(arguments, filter.Limit+1)
	rows, err := s.db.QueryContext(ctx, statement, arguments...)
	if err != nil {
		return domain.RunSearchPage{}, err
	}
	defer rows.Close()
	runs := make([]domain.Run, 0, filter.Limit+1)
	for rows.Next() {
		run, err := scanRunSummary(rows)
		if err != nil {
			return domain.RunSearchPage{}, err
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return domain.RunSearchPage{}, err
	}
	page := domain.RunSearchPage{Runs: runs}
	if len(page.Runs) > filter.Limit {
		page.HasMore = true
		page.Runs = page.Runs[:filter.Limit]
	}
	if page.HasMore && len(page.Runs) > 0 {
		last := page.Runs[len(page.Runs)-1]
		page.NextStartedAt, page.NextID = last.StartedAt, last.ID
	}
	return page, nil
}

func scanRunSummary(row scanner) (domain.Run, error) {
	var run domain.Run
	var reviewJSON string
	var started string
	var completed sql.NullString
	err := row.Scan(&run.ID, &run.SessionID, &run.HostID, &run.ToolName, &run.RequestJSON,
		&run.Status, &run.ExitCode, &reviewJSON, &started, &completed)
	if err != nil {
		return domain.Run{}, err
	}
	if reviewJSON != "" {
		var review domain.CommandReview
		if json.Unmarshal([]byte(reviewJSON), &review) == nil {
			run.AIReview = &review
		}
	}
	run.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
	if completed.Valid {
		run.CompletedAt, _ = time.Parse(time.RFC3339Nano, completed.String)
	}
	return run, nil
}

func scanRunRegexCandidate(row scanner) (domain.Run, error) {
	var run domain.Run
	var started string
	var completed sql.NullString
	err := row.Scan(&run.ID, &run.SessionID, &run.HostID, &run.ToolName, &run.ToolArgumentsJSON,
		&run.RequestJSON, &run.Status, &run.ExitCode, &run.StdoutRedacted, &run.StderrRedacted,
		&started, &completed)
	if err != nil {
		return domain.Run{}, err
	}
	run.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
	if completed.Valid {
		run.CompletedAt, _ = time.Parse(time.RFC3339Nano, completed.String)
	}
	return run, nil
}

func runMatchesHistoryRegex(expression *regexp.Regexp, run domain.Run, queryScope string) (bool, error) {
	var parts []string
	switch queryScope {
	case "", "all":
		parts = []string{run.RequestJSON, run.ToolArgumentsJSON, run.StdoutRedacted, run.StderrRedacted}
	case "request":
		parts = []string{run.RequestJSON, run.ToolArgumentsJSON}
	case "output":
		parts = []string{run.StdoutRedacted, run.StderrRedacted}
	default:
		return false, fmt.Errorf("invalid history query_scope: use all, request, or output")
	}
	if queryScope != "output" {
		var request domain.ExecRequest
		if json.Unmarshal([]byte(run.RequestJSON), &request) == nil {
			parts = append(parts, request.SearchText())
		}
	}
	return expression.MatchString(strings.Join(parts, "\n")), nil
}

// SearchRunSummariesRegexFilteredPage streams regex candidates instead of
// materializing every complete Run. ScanLimit bounds work even when matches
// are rare; NextStartedAt/NextID then point after the last inspected row.
func (s *Store) SearchRunSummariesRegexFilteredPage(ctx context.Context, pattern string, filter domain.RunSearchFilter) (domain.RunSearchPage, error) {
	if filter.Limit <= 0 {
		return domain.RunSearchPage{}, fmt.Errorf("history page limit must be positive")
	}
	expression, err := regexp.CompilePOSIX(pattern)
	if err != nil {
		return domain.RunSearchPage{}, fmt.Errorf("invalid POSIX history regex: %w", err)
	}
	where, arguments, err := runSearchWhere(filter, false)
	if err != nil {
		return domain.RunSearchPage{}, err
	}
	statement := `SELECT id,session_id,host_id,tool_name,tool_arguments_json,request_json,status,exit_code,
stdout_redacted,stderr_redacted,started_at,completed_at FROM runs` + where + " ORDER BY started_at DESC,id DESC"
	rows, err := s.db.QueryContext(ctx, statement, arguments...)
	if err != nil {
		return domain.RunSearchPage{}, err
	}
	defer rows.Close()
	page := domain.RunSearchPage{Runs: make([]domain.Run, 0, filter.Limit+1)}
	var lastScanned domain.Run
	scanned := 0
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return domain.RunSearchPage{}, err
		}
		run, err := scanRunRegexCandidate(rows)
		if err != nil {
			return domain.RunSearchPage{}, err
		}
		lastScanned = run
		scanned++
		matched, err := runMatchesHistoryRegex(expression, run, filter.QueryScope)
		if err != nil {
			return domain.RunSearchPage{}, err
		}
		if matched {
			// Search output is a summary; discard candidate-only large fields.
			run.ToolArgumentsJSON = ""
			run.StdoutRedacted = ""
			run.StderrRedacted = ""
			page.Runs = append(page.Runs, run)
			if len(page.Runs) > filter.Limit {
				page.HasMore = true
				page.Runs = page.Runs[:filter.Limit]
				last := page.Runs[len(page.Runs)-1]
				page.NextStartedAt, page.NextID = last.StartedAt, last.ID
				return page, nil
			}
		}
		if filter.ScanLimit > 0 && scanned >= filter.ScanLimit {
			if rows.Next() {
				page.HasMore = true
				page.ScanLimited = true
				page.NextStartedAt, page.NextID = lastScanned.StartedAt, lastScanned.ID
			} else if err := rows.Err(); err != nil {
				return domain.RunSearchPage{}, err
			}
			return page, nil
		}
	}
	if err := rows.Err(); err != nil {
		return domain.RunSearchPage{}, err
	}
	return page, nil
}

func (s *Store) SearchRunsRegex(ctx context.Context, pattern, hostID, sessionID string, limit int) ([]domain.Run, error) {
	return s.SearchRunsRegexFiltered(ctx, pattern, domain.RunSearchFilter{
		QueryScope: "all", HostID: hostID, SessionID: sessionID, Limit: limit,
	})
}

func (s *Store) SearchRunsRegexFiltered(ctx context.Context, pattern string, filter domain.RunSearchFilter) ([]domain.Run, error) {
	expression, err := regexp.CompilePOSIX(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid POSIX history regex: %w", err)
	}
	limit := filter.Limit
	filter.Query = ""
	filter.Limit = 0
	runs, err := s.SearchRunsFiltered(ctx, filter)
	if err != nil {
		return nil, err
	}
	matched := make([]domain.Run, 0)
	for _, run := range runs {
		matches, err := runMatchesHistoryRegex(expression, run, filter.QueryScope)
		if err != nil {
			return nil, err
		}
		if !matches {
			continue
		}
		matched = append(matched, run)
		if limit > 0 && len(matched) >= limit {
			break
		}
	}
	return matched, nil
}

func scanRun(row scanner) (domain.Run, error) {
	var run domain.Run
	var started string
	var completed sql.NullString
	err := row.Scan(&run.ID, &run.SessionID, &run.HostID, &run.ToolName, &run.ToolArgumentsJSON, &run.RequestJSON, &run.RequestCipher, &run.RequestDigest,
		&run.Status, &run.ExitCode, &run.StdoutRedacted, &run.StderrRedacted, &run.StdoutCipher,
		&run.StderrCipher, &run.Error, &run.AIReviewJSON, &started, &completed)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Run{}, ErrNotFound
	}
	if err != nil {
		return domain.Run{}, err
	}
	if run.AIReviewJSON != "" {
		var review domain.CommandReview
		if json.Unmarshal([]byte(run.AIReviewJSON), &review) == nil {
			run.AIReview = &review
		}
	}
	run.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
	if completed.Valid {
		run.CompletedAt, _ = time.Parse(time.RFC3339Nano, completed.String)
	}
	return run, nil
}

func (s *Store) AppendAudit(ctx context.Context, event domain.AuditEvent) error {
	data, err := json.Marshal(event.Data)
	if err != nil {
		return err
	}
	if event.ID == "" {
		event.ID = ids.New("evt")
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO audit_events(id,run_id,event_type,actor,data_json,created_at)
VALUES(?,?,?,?,?,?)`, event.ID, event.RunID, event.Type, event.Actor, string(data), formatTime(event.CreatedAt))
	return err
}

func (s *Store) ListAudit(ctx context.Context, runID string, limit int) ([]domain.AuditEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	page, err := s.ListAuditPage(ctx, runID, limit, time.Time{}, "")
	return page.Events, err
}

func (s *Store) ListAuditPage(ctx context.Context, runID string, limit int, cursorCreated time.Time, cursorID string) (domain.AuditEventPage, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	if cursorCreated.IsZero() != (strings.TrimSpace(cursorID) == "") {
		return domain.AuditEventPage{}, fmt.Errorf("invalid audit cursor boundary")
	}
	statement := `SELECT id,run_id,event_type,actor,data_json,created_at FROM audit_events`
	arguments := make([]any, 0, 6)
	conditions := make([]string, 0, 2)
	if runID != "" {
		conditions = append(conditions, "run_id=?")
		arguments = append(arguments, runID)
	}
	if !cursorCreated.IsZero() {
		conditions = append(conditions, "(created_at<? OR (created_at=? AND id<?))")
		created := formatTime(cursorCreated.UTC())
		arguments = append(arguments, created, created, strings.TrimSpace(cursorID))
	}
	if len(conditions) > 0 {
		statement += " WHERE " + strings.Join(conditions, " AND ")
	}
	statement += " ORDER BY created_at DESC,id DESC LIMIT ?"
	arguments = append(arguments, limit+1)
	rows, err := s.db.QueryContext(ctx, statement, arguments...)
	if err != nil {
		return domain.AuditEventPage{}, err
	}
	defer rows.Close()
	page := domain.AuditEventPage{Events: make([]domain.AuditEvent, 0, limit+1)}
	for rows.Next() {
		var event domain.AuditEvent
		var data, created string
		if err := rows.Scan(&event.ID, &event.RunID, &event.Type, &event.Actor, &data, &created); err != nil {
			return domain.AuditEventPage{}, err
		}
		_ = json.Unmarshal([]byte(data), &event.Data)
		event.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		page.Events = append(page.Events, event)
	}
	if err := rows.Err(); err != nil {
		return domain.AuditEventPage{}, err
	}
	if len(page.Events) > limit {
		page.HasMore = true
		page.Events = page.Events[:limit]
	}
	if page.HasMore && len(page.Events) > 0 {
		last := page.Events[len(page.Events)-1]
		page.NextCreatedAt, page.NextID = last.CreatedAt, last.ID
	}
	return page, nil
}

const deletableAuditRunSQL = `runs.status IN ('completed','failed','partial','interrupted','rejected','denied','expired','stopped','closed','skipped','cancelled','canceled','unavailable')
AND NOT EXISTS (SELECT 1 FROM approvals WHERE approvals.run_id=runs.id AND approvals.status='pending')
AND NOT EXISTS (SELECT 1 FROM tasks WHERE tasks.run_id=runs.id AND tasks.status IN ('created','pending','active','running','retrying','stopping','waiting_for_approval','approval_required'))
AND NOT EXISTS (SELECT 1 FROM ssh_shell_sessions WHERE ssh_shell_sessions.run_id=runs.id AND ssh_shell_sessions.status IN ('starting','running','stopping'))`

// DeleteAuditRuns removes completed audit runs in one conversation, direct
// operations (an empty session ID), or all scopes when sessionID is nil.
func (s *Store) DeleteAuditRuns(ctx context.Context, sessionID *string, actor string) (domain.AuditRunDeleteResult, error) {
	where := "1=1"
	var arguments []any
	scope := "all"
	if sessionID != nil {
		where = "runs.session_id=?"
		arguments = []any{strings.TrimSpace(*sessionID)}
		scope = "session"
		if strings.TrimSpace(*sessionID) == "" {
			scope = "direct"
		}
	}
	result, _, err := s.deleteAuditRuns(ctx, where, arguments, actor, scope)
	return result, err
}

func (s *Store) deleteAuditRuns(ctx context.Context, where string, arguments []any, actor, scope string) (domain.AuditRunDeleteResult, int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.AuditRunDeleteResult{}, 0, err
	}
	defer tx.Rollback()
	var total, deletable int
	countStatement := `SELECT COUNT(*),COALESCE(SUM(CASE WHEN ` + deletableAuditRunSQL + ` THEN 1 ELSE 0 END),0) FROM runs WHERE ` + where
	if err := tx.QueryRowContext(ctx, countStatement, arguments...).Scan(&total, &deletable); err != nil {
		return domain.AuditRunDeleteResult{}, 0, err
	}
	result := domain.AuditRunDeleteResult{Deleted: deletable, Retained: total - deletable}
	if deletable == 0 {
		return result, total, tx.Commit()
	}
	selection := `SELECT runs.id FROM runs WHERE (` + where + `) AND (` + deletableAuditRunSQL + `)`
	statements := []string{
		`UPDATE chat_tool_calls SET run_id='' WHERE run_id IN (` + selection + `)`,
		`DELETE FROM ssh_shell_sessions WHERE run_id IN (` + selection + `)`,
		`DELETE FROM approvals WHERE run_id IN (` + selection + `)`,
		`DELETE FROM tasks WHERE run_id IN (` + selection + `)`,
		`DELETE FROM audit_events WHERE run_id IN (` + selection + `)`,
		`DELETE FROM runs WHERE id IN (` + selection + `)`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement, arguments...); err != nil {
			return domain.AuditRunDeleteResult{}, total, err
		}
	}
	if actor == "" {
		actor = "local-user"
	}
	data, err := json.Marshal(map[string]any{"deleted": result.Deleted, "retained": result.Retained, "scope": scope})
	if err != nil {
		return domain.AuditRunDeleteResult{}, total, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_events(id,run_id,event_type,actor,data_json,created_at) VALUES(?,?,?,?,?,?)`,
		ids.New("evt"), "", "audit_records_deleted", actor, string(data), formatTime(time.Now().UTC())); err != nil {
		return domain.AuditRunDeleteResult{}, total, err
	}
	if err := tx.Commit(); err != nil {
		return domain.AuditRunDeleteResult{}, total, err
	}
	return result, total, nil
}

func (s *Store) AppendChatMessage(ctx context.Context, sessionID, role, content string, toolName ...string) error {
	_, err := s.appendChatMessage(ctx, sessionID, role, content, "completed", toolName...)
	return err
}

// AppendChatReasoning persists provider metadata that must be replayed with
// reasoning content, such as an Anthropic thinking signature.
func (s *Store) AppendChatReasoning(ctx context.Context, sessionID, content string, modelExtra map[string]any) error {
	_, err := s.appendChatMessageWithAttachments(ctx, sessionID, "reasoning", content, "completed", "", nil, modelExtra)
	return err
}

// AppendChatMessageWithID lets a streamed message keep its lifecycle ID after persistence.
func (s *Store) AppendChatMessageWithID(ctx context.Context, id, sessionID, role, content string, toolName ...string) error {
	name := ""
	if len(toolName) > 0 {
		name = toolName[0]
	}
	return s.appendChatMessageWithAttachmentsID(ctx, id, sessionID, role, content, "completed", name, nil, nil)
}

func (s *Store) AppendChatAssistantMessageWithUsage(ctx context.Context, id, sessionID, content string, usage domain.ChatTokenUsage) error {
	if usage.TotalTokens <= 0 {
		return fmt.Errorf("chat token total must be positive")
	}
	return s.appendChatMessageWithAttachmentsID(ctx, id, sessionID, "assistant", content, "completed", "", nil, nil, &usage)
}

func (s *Store) AppendPendingChatMessage(ctx context.Context, sessionID, role, content string, toolName ...string) (string, error) {
	return s.appendChatMessage(ctx, sessionID, role, content, "pending", toolName...)
}

func (s *Store) AppendPendingChatMessageWithAttachments(ctx context.Context, sessionID, role, content string, attachments []domain.ChatAttachment) (string, error) {
	name := ""
	return s.appendChatMessageWithAttachments(ctx, sessionID, role, content, "pending", name, attachments, nil)
}

func (s *Store) appendChatMessage(ctx context.Context, sessionID, role, content, status string, toolName ...string) (string, error) {
	name := ""
	if len(toolName) > 0 {
		name = toolName[0]
	}
	return s.appendChatMessageWithAttachments(ctx, sessionID, role, content, status, name, nil, nil)
}

func (s *Store) appendChatMessageWithAttachments(ctx context.Context, sessionID, role, content, status, toolName string, attachments []domain.ChatAttachment, modelExtra map[string]any) (string, error) {
	id := ids.New("msg")
	if err := s.appendChatMessageWithAttachmentsID(ctx, id, sessionID, role, content, status, toolName, attachments, modelExtra); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) appendChatMessageWithAttachmentsID(ctx context.Context, id, sessionID, role, content, status, toolName string, attachments []domain.ChatAttachment, modelExtra map[string]any, tokenUsage ...*domain.ChatTokenUsage) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("chat message id is required")
	}
	modelExtraJSON, err := json.Marshal(modelExtra)
	if err != nil {
		return fmt.Errorf("encode chat message model metadata: %w", err)
	}
	now := formatTime(time.Now().UTC())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO chat_sessions(session_id,workspace_id,created_at,updated_at) VALUES(?,?,?,?)`, sessionID, "", now, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO chat_messages(id,session_id,role,content,model_extra_json,tool_name,status,created_at)
VALUES(?,?,?,?,?,?,?,?)`, id, sessionID, role, content, string(modelExtraJSON), toolName, status, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chat_sessions SET updated_at=? WHERE session_id=?`, now, sessionID); err != nil {
		return err
	}
	if len(tokenUsage) > 0 && tokenUsage[0] != nil {
		usage := tokenUsage[0]
		if usage.TotalTokens <= 0 {
			return fmt.Errorf("chat token total must be positive")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO chat_message_context_usage(
message_id,session_id,input_tokens,output_tokens,total_tokens,cached_tokens,reasoning_tokens,created_at)
VALUES(?,?,?,?,?,?,?,?)`, id, sessionID, usage.InputTokens, usage.OutputTokens, usage.TotalTokens,
			usage.CachedTokens, usage.ReasoningTokens, now); err != nil {
			return err
		}
	}
	for _, attachment := range attachments {
		attachmentID := attachment.ID
		if attachmentID == "" {
			attachmentID = ids.New("image")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO chat_attachments(id,message_id,name,mime_type,size_bytes,data,created_at)
VALUES(?,?,?,?,?,?,?)`, attachmentID, id, attachment.Name, attachment.MIMEType, len(attachment.Data), attachment.Data, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func (s *Store) SetChatMessageStatus(ctx context.Context, id, status string) error {
	if status != "pending" && status != "waiting_for_approval" && status != "completed" && status != "failed" {
		return fmt.Errorf("invalid chat message status %q", status)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE chat_messages SET status=? WHERE id=?`, status, id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return ErrNotFound
	}
	return nil
}

// PruneChatTurnsExcludedFromContext removes failed user turns that have no
// visible assistant output or Tool result. Reasoning and the transient
// interruption marker are removed with the user message.
func (s *Store) PruneChatTurnsExcludedFromContext(ctx context.Context, sessionID string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
SELECT users.session_id, users.rowid,
  COALESCE((SELECT min(next_user.rowid) FROM chat_messages AS next_user
    WHERE next_user.session_id=users.session_id AND next_user.role='user' AND next_user.rowid>users.rowid),0)
FROM chat_messages AS users
WHERE users.role='user' AND users.status='failed' AND (?='' OR users.session_id=?)
AND NOT EXISTS (
  SELECT 1 FROM chat_messages AS turn_message
  WHERE turn_message.session_id=users.session_id
    AND turn_message.rowid>users.rowid
    AND turn_message.rowid<COALESCE((SELECT min(next_user.rowid) FROM chat_messages AS next_user
      WHERE next_user.session_id=users.session_id AND next_user.role='user' AND next_user.rowid>users.rowid),9223372036854775807)
    AND (
		turn_message.role='tool'
		OR (turn_message.role IN ('assistant','assistant_progress') AND trim(turn_message.content)<>'' AND trim(turn_message.content)<>?)
    )
)`, sessionID, sessionID, domain.AgentInterruptedMessage)
	if err != nil {
		return 0, err
	}
	type excludedTurn struct {
		sessionID string
		firstRow  int64
		nextRow   int64
	}
	turns := make([]excludedTurn, 0)
	for rows.Next() {
		var turn excludedTurn
		if err := rows.Scan(&turn.sessionID, &turn.firstRow, &turn.nextRow); err != nil {
			rows.Close()
			return 0, err
		}
		turns = append(turns, turn)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, turn := range turns {
		if turn.nextRow > 0 {
			_, err = tx.ExecContext(ctx, `DELETE FROM chat_messages WHERE session_id=? AND rowid>=? AND rowid<?`,
				turn.sessionID, turn.firstRow, turn.nextRow)
		} else {
			_, err = tx.ExecContext(ctx, `DELETE FROM chat_messages WHERE session_id=? AND rowid>=?`,
				turn.sessionID, turn.firstRow)
		}
		if err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(turns), nil
}

type AgentTaskFile struct {
	Path      string
	Content   string
	UpdatedAt time.Time
}

func (s *Store) ListAgentTaskFiles(ctx context.Context, sessionID string) ([]AgentTaskFile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT file_path,content,updated_at FROM agent_task_files WHERE session_id=? ORDER BY file_path`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	files := make([]AgentTaskFile, 0)
	for rows.Next() {
		var file AgentTaskFile
		var updated string
		if err := rows.Scan(&file.Path, &file.Content, &updated); err != nil {
			return nil, err
		}
		file.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		files = append(files, file)
	}
	return files, rows.Err()
}

func (s *Store) ReadAgentTaskFile(ctx context.Context, sessionID, filePath string) (AgentTaskFile, error) {
	var file AgentTaskFile
	var updated string
	err := s.db.QueryRowContext(ctx, `SELECT file_path,content,updated_at FROM agent_task_files WHERE session_id=? AND file_path=?`, sessionID, filePath).Scan(
		&file.Path, &file.Content, &updated,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentTaskFile{}, ErrNotFound
	}
	if err != nil {
		return AgentTaskFile{}, err
	}
	file.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return file, nil
}

func (s *Store) WriteAgentTaskFile(ctx context.Context, sessionID, filePath, content string) error {
	now := formatTime(time.Now().UTC())
	_, err := s.db.ExecContext(ctx, `INSERT INTO agent_task_files(session_id,file_path,content,created_at,updated_at) VALUES(?,?,?,?,?)
ON CONFLICT(session_id,file_path) DO UPDATE SET content=excluded.content,updated_at=excluded.updated_at`,
		sessionID, filePath, content, now, now)
	return err
}

func (s *Store) DeleteAgentTaskFile(ctx context.Context, sessionID, filePath string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM agent_task_files WHERE session_id=? AND file_path=?`, sessionID, filePath)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListAgentTasks(ctx context.Context, sessionID string) (domain.AgentTaskList, error) {
	files, err := s.ListAgentTaskFiles(ctx, sessionID)
	if err != nil {
		return domain.AgentTaskList{}, err
	}
	result := domain.AgentTaskList{SessionID: sessionID, Items: make([]domain.AgentTask, 0)}
	for _, file := range files {
		name := filepath.Base(file.Path)
		if filepath.Ext(name) != ".json" {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if _, err := strconv.Atoi(id); err != nil {
			continue
		}
		var stored struct {
			Subject     string         `json:"subject"`
			Description string         `json:"description"`
			Status      string         `json:"status"`
			Blocks      []string       `json:"blocks"`
			BlockedBy   []string       `json:"blockedBy"`
			ActiveForm  string         `json:"activeForm"`
			Owner       string         `json:"owner"`
			Metadata    map[string]any `json:"metadata"`
		}
		if err := json.Unmarshal([]byte(file.Content), &stored); err != nil {
			return domain.AgentTaskList{}, fmt.Errorf("decode agent task %s: %w", id, err)
		}
		result.Items = append(result.Items, domain.AgentTask{
			ID: id, Subject: stored.Subject, Description: stored.Description, Status: stored.Status,
			Blocks: stored.Blocks, BlockedBy: stored.BlockedBy, ActiveForm: stored.ActiveForm,
			Owner: stored.Owner, Metadata: stored.Metadata, UpdatedAt: file.UpdatedAt,
		})
		if file.UpdatedAt.After(result.UpdatedAt) {
			result.UpdatedAt = file.UpdatedAt
		}
	}
	sort.Slice(result.Items, func(i, j int) bool {
		left, _ := strconv.Atoi(result.Items[i].ID)
		right, _ := strconv.Atoi(result.Items[j].ID)
		return left < right
	})
	return result, nil
}

func (s *Store) ListChatMessages(ctx context.Context, sessionID string, limit int) ([]domain.ChatMessage, error) {
	return s.listChatMessages(ctx, sessionID, limit, false)
}

const maxChatToolMessagePreviewChars = 64 << 10

func (s *Store) ListChatMessagesPage(ctx context.Context, sessionID string, limit int, beforeCreatedAt, beforeID string) (domain.ChatMessagePage, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 200 {
		limit = 200
	}
	if (beforeCreatedAt == "") != (beforeID == "") {
		return domain.ChatMessagePage{}, fmt.Errorf("chat message cursor requires both before_created_at and before_id")
	}
	where := "session_id=?"
	whereArgs := []any{sessionID}
	if beforeCreatedAt != "" && beforeID != "" {
		if _, err := time.Parse(time.RFC3339Nano, beforeCreatedAt); err != nil {
			return domain.ChatMessagePage{}, fmt.Errorf("invalid chat message cursor time: %w", err)
		}
		var cursorRowID int64
		err := s.db.QueryRowContext(ctx, `SELECT rowid FROM chat_messages WHERE session_id=? AND id=? AND created_at=?`, sessionID, beforeID, beforeCreatedAt).Scan(&cursorRowID)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ChatMessagePage{}, ErrNotFound
		}
		if err != nil {
			return domain.ChatMessagePage{}, err
		}
		where += " AND (created_at<? OR (created_at=? AND rowid<?))"
		whereArgs = append(whereArgs, beforeCreatedAt, beforeCreatedAt, cursorRowID)
	}
	args := []any{maxChatToolMessagePreviewChars, maxChatToolMessagePreviewChars}
	args = append(args, whereArgs...)
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT id,role,
CASE WHEN role='tool' AND length(content)>? THEN substr(content,1,?) ELSE content END,
length(content),model_extra_json,tool_name,status,created_at
FROM chat_messages WHERE `+where+` ORDER BY created_at DESC,rowid DESC LIMIT ?`, args...)
	if err != nil {
		return domain.ChatMessagePage{}, err
	}
	defer rows.Close()
	messages := make([]domain.ChatMessage, 0, limit+1)
	for rows.Next() {
		var message domain.ChatMessage
		var created, modelExtraJSON string
		if err := rows.Scan(&message.ID, &message.Role, &message.Content, &message.ContentChars, &modelExtraJSON, &message.ToolName, &message.Status, &created); err != nil {
			return domain.ChatMessagePage{}, err
		}
		if err := json.Unmarshal([]byte(modelExtraJSON), &message.ModelExtra); err != nil {
			return domain.ChatMessagePage{}, fmt.Errorf("decode chat message model metadata: %w", err)
		}
		message.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		message.ContentTruncated = message.Role == "tool" && message.ContentChars > maxChatToolMessagePreviewChars
		if !message.ContentTruncated {
			message.ContentChars = 0
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return domain.ChatMessagePage{}, err
	}
	hasMore := len(messages) > limit
	if hasMore {
		messages = messages[:limit]
	}
	slices.Reverse(messages)
	if err := s.enrichChatMessages(ctx, sessionID, messages, false); err != nil {
		return domain.ChatMessagePage{}, err
	}
	for index := range messages {
		if messages[index].ContentTruncated {
			messages[index].Content = projectedChatToolContent(messages[index])
		}
	}
	page := domain.ChatMessagePage{Messages: messages, HasMore: hasMore}
	if hasMore && len(messages) > 0 {
		page.NextCreatedAt = formatTime(messages[0].CreatedAt)
		page.NextID = messages[0].ID
	}
	return page, nil
}

func projectedChatToolContent(message domain.ChatMessage) string {
	status := message.ToolStatus
	if status == "" {
		status = message.Status
	}
	value := map[string]any{
		"status": status, "output_limited": true, "original_chars": message.ContentChars,
		"preview": message.Content,
	}
	if message.RunID != "" {
		value["run_id"] = message.RunID
	}
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func (s *Store) GetChatMessage(ctx context.Context, sessionID, messageID string) (domain.ChatMessage, error) {
	var message domain.ChatMessage
	var created, modelExtraJSON string
	err := s.db.QueryRowContext(ctx, `SELECT id,role,content,length(content),model_extra_json,tool_name,status,created_at
FROM chat_messages WHERE session_id=? AND id=?`, sessionID, messageID).Scan(
		&message.ID, &message.Role, &message.Content, &message.ContentChars, &modelExtraJSON, &message.ToolName, &message.Status, &created,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ChatMessage{}, ErrNotFound
	}
	if err != nil {
		return domain.ChatMessage{}, err
	}
	if err := json.Unmarshal([]byte(modelExtraJSON), &message.ModelExtra); err != nil {
		return domain.ChatMessage{}, fmt.Errorf("decode chat message model metadata: %w", err)
	}
	message.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	messages := []domain.ChatMessage{message}
	if err := s.enrichChatMessages(ctx, sessionID, messages, false); err != nil {
		return domain.ChatMessage{}, err
	}
	return messages[0], nil
}

func (s *Store) ListChatModelMessages(ctx context.Context, sessionID string, limit int) ([]domain.ChatMessage, error) {
	return s.listChatMessages(ctx, sessionID, limit, true)
}

// ListChatContextMessages returns the complete persisted transcript used to
// rebuild prior model turns, including reasoning and visible tool preambles.
func (s *Store) ListChatContextMessages(ctx context.Context, sessionID string) ([]domain.ChatMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,role,content,model_extra_json,tool_name,status,created_at FROM chat_messages
WHERE session_id=? AND role IN ('user','assistant','assistant_progress','tool','reasoning') AND status IN ('completed','failed')
ORDER BY created_at,rowid`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.ChatMessage, 0)
	for rows.Next() {
		var message domain.ChatMessage
		var created, modelExtraJSON string
		if err := rows.Scan(&message.ID, &message.Role, &message.Content, &modelExtraJSON, &message.ToolName, &message.Status, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(modelExtraJSON), &message.ModelExtra); err != nil {
			return nil, fmt.Errorf("decode chat message model metadata: %w", err)
		}
		message.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		result = append(result, message)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := s.loadChatAttachments(ctx, result, true); err != nil {
		return nil, err
	}
	if err := s.loadChatToolMessageState(ctx, result); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) GetChatContextSummary(ctx context.Context, sessionID string) (domain.ChatContextSummary, error) {
	var summary domain.ChatContextSummary
	var created, updated string
	err := s.db.QueryRowContext(ctx, `SELECT session_id,summary,through_message_id,revision,trigger,source_tokens,summary_tokens,model,created_at,updated_at
FROM chat_context_summaries WHERE session_id=?`, sessionID).Scan(
		&summary.SessionID, &summary.Summary, &summary.ThroughMessageID, &summary.Revision, &summary.Trigger,
		&summary.SourceTokens, &summary.SummaryTokens, &summary.Model, &created, &updated,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ChatContextSummary{}, ErrNotFound
	}
	if err != nil {
		return domain.ChatContextSummary{}, err
	}
	summary.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	summary.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return summary, nil
}

func (s *Store) SaveChatContextSummary(ctx context.Context, summary domain.ChatContextSummary) (domain.ChatContextSummary, error) {
	if strings.TrimSpace(summary.SessionID) == "" || strings.TrimSpace(summary.Summary) == "" || strings.TrimSpace(summary.ThroughMessageID) == "" {
		return domain.ChatContextSummary{}, fmt.Errorf("context summary session, content, and boundary are required")
	}
	if summary.Trigger != "auto" && summary.Trigger != "manual" {
		return domain.ChatContextSummary{}, fmt.Errorf("invalid context summary trigger %q", summary.Trigger)
	}
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `INSERT INTO chat_context_summaries(
session_id,summary,through_message_id,revision,trigger,source_tokens,summary_tokens,model,created_at,updated_at)
VALUES(?,?,?,1,?,?,?,?,?,?)
ON CONFLICT(session_id) DO UPDATE SET summary=excluded.summary,through_message_id=excluded.through_message_id,
revision=chat_context_summaries.revision+1,trigger=excluded.trigger,source_tokens=excluded.source_tokens,
summary_tokens=excluded.summary_tokens,model=excluded.model,updated_at=excluded.updated_at`,
		summary.SessionID, summary.Summary, summary.ThroughMessageID, summary.Trigger, max(summary.SourceTokens, 0),
		max(summary.SummaryTokens, 0), summary.Model, formatTime(now), formatTime(now))
	if err != nil {
		return domain.ChatContextSummary{}, err
	}
	return s.GetChatContextSummary(ctx, summary.SessionID)
}

func (s *Store) DeleteChatContextSummary(ctx context.Context, sessionID string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM chat_context_summaries WHERE session_id=?`, sessionID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) listChatMessages(ctx context.Context, sessionID string, limit int, modelOnly bool) ([]domain.ChatMessage, error) {
	filter := ""
	if modelOnly {
		filter = " AND role IN ('user','assistant') AND status='completed'"
	}
	query := `SELECT id,role,content,model_extra_json,tool_name,status,created_at FROM chat_messages WHERE session_id=?` + filter + ` ORDER BY created_at,rowid`
	args := []any{sessionID}
	if limit > 0 {
		query = `SELECT id,role,content,model_extra_json,tool_name,status,created_at FROM (
SELECT id,role,content,model_extra_json,tool_name,status,created_at,rowid AS message_sequence FROM chat_messages WHERE session_id=?` + filter + ` ORDER BY created_at DESC,rowid DESC LIMIT ?)
ORDER BY created_at,message_sequence`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.ChatMessage, 0)
	for rows.Next() {
		var message domain.ChatMessage
		var created, modelExtraJSON string
		if err := rows.Scan(&message.ID, &message.Role, &message.Content, &modelExtraJSON, &message.ToolName, &message.Status, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(modelExtraJSON), &message.ModelExtra); err != nil {
			return nil, fmt.Errorf("decode chat message model metadata: %w", err)
		}
		message.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		result = append(result, message)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := s.enrichChatMessages(ctx, sessionID, result, false); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) enrichChatMessages(ctx context.Context, sessionID string, messages []domain.ChatMessage, includeAttachmentData bool) error {
	if err := s.loadChatAttachments(ctx, messages, includeAttachmentData); err != nil {
		return err
	}
	if err := s.loadChatToolMessageState(ctx, messages); err != nil {
		return err
	}
	return s.loadChatTokenUsage(ctx, sessionID, messages)
}

func (s *Store) loadChatTokenUsage(ctx context.Context, sessionID string, messages []domain.ChatMessage) error {
	if len(messages) == 0 {
		return nil
	}
	byID := make(map[string]*domain.ChatMessage, len(messages))
	placeholders := make([]string, 0, len(messages))
	arguments := make([]any, 0, len(messages))
	for index := range messages {
		byID[messages[index].ID] = &messages[index]
		placeholders = append(placeholders, "?")
		arguments = append(arguments, messages[index].ID)
	}
	query := `SELECT message_id,input_tokens,output_tokens,total_tokens,cached_tokens,reasoning_tokens FROM chat_message_context_usage WHERE session_id=? ORDER BY created_at`
	arguments = []any{sessionID}
	if len(messages) <= 500 {
		query = `SELECT message_id,input_tokens,output_tokens,total_tokens,cached_tokens,reasoning_tokens FROM chat_message_context_usage WHERE message_id IN (` + strings.Join(placeholders, ",") + `)`
		arguments = arguments[:0]
		for index := range messages {
			arguments = append(arguments, messages[index].ID)
		}
	}
	rows, err := s.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var messageID string
		var usage domain.ChatTokenUsage
		if err := rows.Scan(&messageID, &usage.InputTokens, &usage.OutputTokens, &usage.TotalTokens,
			&usage.CachedTokens, &usage.ReasoningTokens); err != nil {
			return err
		}
		if message := byID[messageID]; message != nil {
			message.TokenUsage = &usage
		}
	}
	return rows.Err()
}

func (s *Store) loadChatAttachments(ctx context.Context, messages []domain.ChatMessage, includeData bool) error {
	if len(messages) == 0 {
		return nil
	}
	messageIndex := make(map[string]int, len(messages))
	placeholders := make([]string, 0, len(messages))
	args := make([]any, 0, len(messages))
	for index := range messages {
		messageIndex[messages[index].ID] = index
		placeholders = append(placeholders, "?")
		args = append(args, messages[index].ID)
	}
	dataColumn := "NULL"
	if includeData {
		dataColumn = "data"
	}
	query := `SELECT id,message_id,name,mime_type,size_bytes,` + dataColumn + ` FROM chat_attachments WHERE message_id IN (` + strings.Join(placeholders, ",") + `) ORDER BY created_at`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var attachment domain.ChatAttachment
		if err := rows.Scan(&attachment.ID, &attachment.MessageID, &attachment.Name, &attachment.MIMEType, &attachment.SizeBytes, &attachment.Data); err != nil {
			return err
		}
		if index, ok := messageIndex[attachment.MessageID]; ok {
			messages[index].Attachments = append(messages[index].Attachments, attachment)
		}
	}
	return rows.Err()
}

func (s *Store) GetChatAttachment(ctx context.Context, sessionID, attachmentID string) (domain.ChatAttachment, error) {
	var attachment domain.ChatAttachment
	err := s.db.QueryRowContext(ctx, `SELECT attachments.id,attachments.message_id,attachments.name,attachments.mime_type,attachments.size_bytes,attachments.data
FROM chat_attachments AS attachments
JOIN chat_messages AS messages ON messages.id=attachments.message_id
WHERE attachments.id=? AND messages.session_id=?`, attachmentID, sessionID).Scan(
		&attachment.ID, &attachment.MessageID, &attachment.Name, &attachment.MIMEType, &attachment.SizeBytes, &attachment.Data,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ChatAttachment{}, ErrNotFound
	}
	return attachment, err
}

func (s *Store) CreateChatSession(ctx context.Context, sessionID, workspaceID string) (domain.ChatSession, error) {
	now := formatTime(time.Now().UTC())
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO chat_sessions(session_id,workspace_id,created_at,updated_at) VALUES(?,?,?,?)`, sessionID, workspaceID, now, now)
	if err != nil {
		return domain.ChatSession{}, err
	}
	created, err := result.RowsAffected()
	if err != nil {
		return domain.ChatSession{}, err
	}
	if created != 1 {
		return domain.ChatSession{}, ErrAlreadyExists
	}
	return s.GetChatSession(ctx, sessionID)
}

func (s *Store) GetChatSession(ctx context.Context, sessionID string) (domain.ChatSession, error) {
	var session domain.ChatSession
	var storedTitle, updated string
	err := s.db.QueryRowContext(ctx, `SELECT sessions.session_id,sessions.title,
  COALESCE(NULLIF(trim(sessions.title),''),NULLIF((SELECT trim(substr(first.content,1,80)) FROM chat_messages AS first
    WHERE first.session_id=sessions.session_id AND first.role='user'
    ORDER BY first.created_at ASC LIMIT 1),''),'New conversation'),
  sessions.workspace_id,sessions.context_tokens,sessions.context_window,
  (SELECT count(*) FROM chat_messages AS messages WHERE messages.session_id=sessions.session_id),sessions.updated_at
FROM chat_sessions AS sessions WHERE sessions.session_id=?`, sessionID).Scan(
		&session.ID, &storedTitle, &session.Title, &session.WorkspaceID, &session.ContextTokens, &session.ContextWindow, &session.MessageCount, &updated,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ChatSession{}, ErrNotFound
	}
	if err != nil {
		return domain.ChatSession{}, err
	}
	session.TitleSet = strings.TrimSpace(storedTitle) != ""
	session.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return session, nil
}

func (s *Store) SetChatSessionTitle(ctx context.Context, sessionID, title string) (domain.ChatSession, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE chat_sessions SET title=? WHERE session_id=?`, title, sessionID)
	if err != nil {
		return domain.ChatSession{}, err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return domain.ChatSession{}, ErrNotFound
	}
	return s.GetChatSession(ctx, sessionID)
}

func (s *Store) SetChatSessionTitleIfEmpty(ctx context.Context, sessionID, title string) (domain.ChatSession, bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE chat_sessions SET title=? WHERE session_id=? AND trim(title)=''`, title, sessionID)
	if err != nil {
		return domain.ChatSession{}, false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return domain.ChatSession{}, false, err
	}
	session, err := s.GetChatSession(ctx, sessionID)
	if err != nil {
		return domain.ChatSession{}, false, err
	}
	return session, changed == 1, nil
}

func (s *Store) SetChatSessionWorkspace(ctx context.Context, sessionID, workspaceID string) (domain.ChatSession, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE chat_sessions SET workspace_id=?,updated_at=? WHERE session_id=?`, workspaceID, formatTime(time.Now().UTC()), sessionID)
	if err != nil {
		return domain.ChatSession{}, err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return domain.ChatSession{}, ErrNotFound
	}
	return s.GetChatSession(ctx, sessionID)
}

func (s *Store) SetChatSessionContextUsage(ctx context.Context, sessionID string, tokens, window int) error {
	result, err := s.db.ExecContext(ctx, `UPDATE chat_sessions SET context_tokens=?,context_window=? WHERE session_id=?`, tokens, window, sessionID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListChatSessions(ctx context.Context, limit int) ([]domain.ChatSession, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT sessions.session_id,
  sessions.title,
  COALESCE(NULLIF(trim(sessions.title),''),NULLIF((SELECT trim(substr(first.content,1,80)) FROM chat_messages AS first
    WHERE first.session_id=sessions.session_id AND first.role='user'
    ORDER BY first.created_at ASC LIMIT 1),''),'New conversation') AS display_title,
  sessions.workspace_id,
	sessions.context_tokens,
	sessions.context_window,
  (SELECT count(*) FROM chat_messages AS messages WHERE messages.session_id=sessions.session_id),
  sessions.updated_at
FROM chat_sessions AS sessions
ORDER BY sessions.updated_at DESC
LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.ChatSession, 0)
	for rows.Next() {
		var session domain.ChatSession
		var storedTitle, updated string
		if err := rows.Scan(&session.ID, &storedTitle, &session.Title, &session.WorkspaceID, &session.ContextTokens, &session.ContextWindow, &session.MessageCount, &updated); err != nil {
			return nil, err
		}
		session.TitleSet = strings.TrimSpace(storedTitle) != ""
		session.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		result = append(result, session)
	}
	return result, rows.Err()
}

func (s *Store) DeleteChatSession(ctx context.Context, sessionID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM chat_sessions WHERE session_id=?`, sessionID)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM checkpoints WHERE id IN (
SELECT approvals.checkpoint_id FROM approvals JOIN runs ON runs.id=approvals.run_id
WHERE runs.session_id=? AND approvals.continuation_kind=? AND approvals.checkpoint_id<>''
)`, sessionID, domain.ApprovalContinuationAgent); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM approvals WHERE run_id IN (SELECT id FROM runs WHERE session_id=?)`, sessionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tasks WHERE run_id IN (SELECT id FROM runs WHERE session_id=?)`, sessionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM chat_context_summaries WHERE session_id=?`, sessionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM chat_messages WHERE session_id=?`, sessionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM chat_tool_calls WHERE session_id=?`, sessionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ssh_shell_sessions
WHERE session_id=? OR run_id IN (SELECT id FROM runs WHERE session_id=?)`, sessionID, sessionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_task_files WHERE session_id=?`, sessionID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Get(ctx context.Context, id string) ([]byte, bool, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM checkpoints WHERE id=?`, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

func (s *Store) Set(ctx context.Context, id string, data []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO checkpoints(id,data,updated_at) VALUES(?,?,?)
ON CONFLICT(id) DO UPDATE SET data=excluded.data,updated_at=excluded.updated_at`, id, data, formatTime(time.Now().UTC()))
	return err
}

func (s *Store) Delete(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM checkpoints WHERE id=?`, id)
	return err
}

func formatTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }
func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
