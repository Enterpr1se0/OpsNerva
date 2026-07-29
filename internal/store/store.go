package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/ids"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound              = errors.New("not found")
	ErrAlreadyExists         = errors.New("already exists")
	ErrInUse                 = errors.New("in use")
	ErrInvalidPlanTransition = errors.New("invalid plan transition")
)

const persistedToolEvidenceLeakMarker = "[Persisted operational tool evidence from the previous turn."

type PlanTransitionError struct {
	StepNumber int
	Status     string
}

func (e *PlanTransitionError) Error() string {
	return fmt.Sprintf("invalid plan transition: step %d is %s, not in_progress", e.StepNumber, e.Status)
}

func (e *PlanTransitionError) Unwrap() error {
	return ErrInvalidPlanTransition
}

type Store struct {
	db *sql.DB
}

func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys = ON; PRAGMA journal_mode = WAL; PRAGMA busy_timeout = 5000;"); err != nil {
		db.Close()
		return nil, err
	}
	st := &Store{db: db}
	if err := st.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return st, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) AdminPasswordHash(ctx context.Context) (string, error) {
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT password_hash FROM admin_credentials WHERE id=1`).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return hash, err
}

func (s *Store) CreateAdminPasswordHash(ctx context.Context, hash string) error {
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO admin_credentials(id,password_hash,updated_at) VALUES(1,?,?)`, hash, formatTime(time.Now().UTC()))
	if err != nil {
		return err
	}
	created, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if created != 1 {
		return ErrAlreadyExists
	}
	return nil
}

func (s *Store) SetAdminPasswordHash(ctx context.Context, hash string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO admin_credentials(id,password_hash,updated_at) VALUES(1,?,?)
ON CONFLICT(id) DO UPDATE SET password_hash=excluded.password_hash,updated_at=excluded.updated_at`, hash, formatTime(time.Now().UTC()))
	return err
}

func (s *Store) CreateWebSession(ctx context.Context, session domain.WebSession) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO web_sessions(token_hash,csrf_token,created_at,expires_at) VALUES(?,?,?,?)`,
		session.TokenHash, session.CSRFToken, formatTime(session.CreatedAt), formatTime(session.ExpiresAt))
	return err
}

func (s *Store) GetWebSession(ctx context.Context, tokenHash string) (domain.WebSession, error) {
	var session domain.WebSession
	var created, expires string
	err := s.db.QueryRowContext(ctx, `SELECT token_hash,csrf_token,created_at,expires_at FROM web_sessions WHERE token_hash=?`, tokenHash).Scan(
		&session.TokenHash, &session.CSRFToken, &created, &expires,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.WebSession{}, ErrNotFound
	}
	if err != nil {
		return domain.WebSession{}, err
	}
	session.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	session.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expires)
	if time.Now().UTC().After(session.ExpiresAt) {
		_ = s.DeleteWebSession(ctx, tokenHash)
		return domain.WebSession{}, ErrNotFound
	}
	return session, nil
}

func (s *Store) DeleteWebSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM web_sessions WHERE token_hash=?`, tokenHash)
	return err
}

func (s *Store) DeleteAllWebSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM web_sessions`)
	return err
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

func (s *Store) migrate(ctx context.Context) error {
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
  risk TEXT NOT NULL,
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
CREATE INDEX IF NOT EXISTS idx_runs_digest ON runs(request_digest);
CREATE TABLE IF NOT EXISTS approvals (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL UNIQUE,
  host_id TEXT NOT NULL,
  request_json TEXT NOT NULL,
  request_cipher TEXT NOT NULL DEFAULT '',
  request_digest TEXT NOT NULL,
  risk TEXT NOT NULL,
  status TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
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
CREATE TABLE IF NOT EXISTS chat_sessions (
  session_id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS chat_messages (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  role TEXT NOT NULL,
  content TEXT NOT NULL,
	  tool_name TEXT NOT NULL DEFAULT '',
	  status TEXT NOT NULL DEFAULT 'completed',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_chat_session ON chat_messages(session_id, created_at);
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
CREATE TABLE IF NOT EXISTS agent_plans (
  session_id TEXT PRIMARY KEY,
  goal TEXT NOT NULL,
  status TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS agent_plan_steps (
  session_id TEXT NOT NULL,
  step_number INTEGER NOT NULL,
  title TEXT NOT NULL,
  status TEXT NOT NULL,
  evidence TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL,
  PRIMARY KEY(session_id,step_number),
  FOREIGN KEY(session_id) REFERENCES agent_plans(session_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_agent_plan_steps_session ON agent_plan_steps(session_id,step_number);
	CREATE TABLE IF NOT EXISTS admin_credentials (
	  id INTEGER PRIMARY KEY CHECK(id=1),
	  password_hash TEXT NOT NULL,
	  updated_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS web_sessions (
	  token_hash TEXT PRIMARY KEY,
	  csrf_token TEXT NOT NULL,
	  created_at TEXT NOT NULL,
	  expires_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_web_sessions_expires ON web_sessions(expires_at);
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
CREATE TABLE IF NOT EXISTS ssh_shell_sessions (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL UNIQUE,
  session_id TEXT NOT NULL,
  surface TEXT NOT NULL,
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
  sensitive INTEGER NOT NULL DEFAULT 0,
  input_bytes INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  PRIMARY KEY(shell_id,sequence),
  FOREIGN KEY(shell_id) REFERENCES ssh_shell_sessions(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_ssh_shell_events_sequence ON ssh_shell_events(shell_id,sequence);
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
  api_key_cipher TEXT NOT NULL DEFAULT '',
  proxy_id TEXT NOT NULL DEFAULT '',
  user_agent TEXT NOT NULL DEFAULT '',
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
  chat_image_allowed_types_json TEXT NOT NULL DEFAULT '["image/png","image/jpeg","image/webp","image/gif"]',
  workspace_shell_mode TEXT NOT NULL DEFAULT 'sandbox',
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
	if err := s.dropLegacySSHShellExpiry(ctx); err != nil {
		return err
	}
	if err := s.migrateSSHShellExitCode(ctx); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO chat_sessions(session_id,workspace_id,created_at,updated_at)
SELECT session_id,'',min(created_at),max(created_at) FROM chat_messages GROUP BY session_id`); err != nil {
		return err
	}
	if err := s.migrateManagedWorkspaces(ctx); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DROP TABLE IF EXISTS file_operations`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DROP TABLE IF EXISTS session_approval_grants`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM agent_tool_settings WHERE name IN ('ssh_approval_status','ssh_task_start','ssh_task_status','ssh_task_tail','ssh_task_list','ssh_file_restore','ssh_file_create','workspace_file_create','ssh_file_search','workspace_file_search','workspace_list')`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO system_settings(id,agent_max_iterations,updated_at) VALUES(1,?,?)`,
		domain.DefaultAgentMaxIterations, formatTime(time.Now().UTC())); err != nil {
		return err
	}
	for _, statement := range []string{
		`ALTER TABLE runs ADD COLUMN request_cipher TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE approvals ADD COLUMN request_cipher TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE hosts ADD COLUMN auth_type TEXT NOT NULL DEFAULT 'agent'`,
		`ALTER TABLE hosts ADD COLUMN agent_enabled INTEGER NOT NULL DEFAULT 1`,
		`ALTER TABLE hosts ADD COLUMN proxy_jump_host_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE hosts ADD COLUMN proxy_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE hosts ADD COLUMN password_cipher TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE hosts ADD COLUMN sudo_mode TEXT NOT NULL DEFAULT 'none'`,
		`ALTER TABLE hosts ADD COLUMN sudo_password_cipher TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE chat_messages ADD COLUMN tool_name TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE chat_messages ADD COLUMN status TEXT NOT NULL DEFAULT 'completed'`,
		`ALTER TABLE runs ADD COLUMN ai_review_json TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE runs ADD COLUMN tool_name TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE runs ADD COLUMN tool_arguments_json TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE model_providers ADD COLUMN proxy_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE model_providers ADD COLUMN user_agent TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE web_search_settings ADD COLUMN proxy_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE system_settings ADD COLUMN chat_image_allowed_types_json TEXT NOT NULL DEFAULT '["image/png","image/jpeg","image/webp","image/gif"]'`,
		`ALTER TABLE system_settings ADD COLUMN system_prompt TEXT DEFAULT NULL`,
		`ALTER TABLE system_settings ADD COLUMN approval_mode TEXT NOT NULL DEFAULT 'manual'`,
		`ALTER TABLE ssh_shell_sessions ADD COLUMN recent_output TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE ssh_shell_events ADD COLUMN source TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE ssh_shell_events ADD COLUMN sensitive INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE ssh_shell_events ADD COLUMN input_bytes INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE ssh_shell_sessions ADD COLUMN termination_reason TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE ssh_shell_sessions ADD COLUMN surface TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return err
		}
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM chat_messages
WHERE role='assistant' AND instr(content,?)>0`, persistedToolEvidenceLeakMarker); err != nil {
		return fmt.Errorf("remove leaked internal Agent context: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE ssh_shell_sessions SET termination_reason=
CASE
  WHEN status='closed' THEN 'requested_close'
  WHEN status='interrupted' THEN 'service_stopped'
  WHEN status='completed' THEN 'remote_exit'
  WHEN status='failed' AND exit_code IS NOT NULL THEN 'remote_exit'
  WHEN status='failed' THEN 'connection_lost'
  ELSE termination_reason
END
WHERE termination_reason=''`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE ssh_shell_sessions SET surface=
CASE WHEN session_id='' THEN 'quick' ELSE 'agent' END
WHERE surface=''`); err != nil {
		return err
	}
	// Migrate the former two-Agent toggles once. Existing installations keep an
	// explicit disable from either legacy setting; fresh databases already have
	// the new column and skip this compatibility copy.
	addedExplanationSetting := false
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE system_settings ADD COLUMN approval_explanations_enabled INTEGER NOT NULL DEFAULT 1`); err == nil {
		addedExplanationSetting = true
	} else if !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		return err
	}
	if addedExplanationSetting {
		_, err := s.db.ExecContext(ctx, `UPDATE system_settings SET approval_explanations_enabled=
CASE WHEN subagent_reviews_enabled<>0 AND beginner_explanations_enabled<>0 THEN 1 ELSE 0 END`)
		if err != nil && !strings.Contains(strings.ToLower(err.Error()), "no such column") {
			return err
		}
	}
	// Backfill the plain-text audit search column once when it is first added.
	// request_json is JSON-escaped, so quotes, redirections, and backslashes in
	// a command never matched a LIKE query against it; search_text stores the
	// redacted request text without JSON escaping.
	addedRunSearchText := false
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE runs ADD COLUMN search_text TEXT NOT NULL DEFAULT ''`); err == nil {
		addedRunSearchText = true
	} else if !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		return err
	}
	if addedRunSearchText {
		if err := s.backfillRunSearchText(ctx); err != nil {
			return err
		}
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE system_settings ADD COLUMN workspace_shell_mode TEXT NOT NULL DEFAULT 'sandbox'`); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE system_settings ADD COLUMN subagent_model_provider_id TEXT NOT NULL DEFAULT ''`); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE system_settings ADD COLUMN subagent_timeout_seconds INTEGER NOT NULL DEFAULT 30`); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE hosts ADD COLUMN private_key_cipher TEXT NOT NULL DEFAULT ''`); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		return err
	}
	if err := s.migrateSharedProxies(ctx); err != nil {
		return err
	}
	return s.migrateNativeOnlyHosts(ctx)
}

func (s *Store) dropLegacySSHShellExpiry(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(ssh_shell_sessions)`)
	if err != nil {
		return err
	}
	hasExpiry := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		hasExpiry = hasExpiry || name == "expires_at"
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !hasExpiry {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE ssh_shell_sessions DROP COLUMN expires_at`); err != nil {
		return fmt.Errorf("drop legacy SSH shell expiry: %w", err)
	}
	return nil
}

func (s *Store) migrateSSHShellExitCode(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(ssh_shell_sessions)`)
	if err != nil {
		return err
	}
	hasExitCode, exitCodeNotNull := false, false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		if name == "exit_code" {
			hasExitCode = true
			exitCodeNotNull = notNull != 0
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !hasExitCode {
		_, err := s.db.ExecContext(ctx, `ALTER TABLE ssh_shell_sessions ADD COLUMN exit_code INTEGER`)
		return err
	}
	if !exitCodeNotNull {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`ALTER TABLE ssh_shell_sessions ADD COLUMN nullable_exit_code INTEGER`,
		`UPDATE ssh_shell_sessions SET nullable_exit_code=CASE WHEN status IN ('completed','failed') AND exit_code>=0 THEN exit_code ELSE NULL END`,
		`ALTER TABLE ssh_shell_sessions DROP COLUMN exit_code`,
		`ALTER TABLE ssh_shell_sessions RENAME COLUMN nullable_exit_code TO exit_code`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate SSH shell exit code: %w", err)
		}
	}
	return tx.Commit()
}

func (s *Store) migrateSharedProxies(ctx context.Context) error {
	tableColumns := make(map[string]map[string]bool)
	for _, table := range []string{"hosts", "model_providers", "web_search_settings"} {
		rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
		if err != nil {
			return err
		}
		columns := make(map[string]bool)
		for rows.Next() {
			var cid, notNull, primaryKey int
			var name, columnType string
			var defaultValue any
			if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
				rows.Close()
				return err
			}
			columns[name] = true
		}
		if err := rows.Close(); err != nil {
			return err
		}
		tableColumns[table] = columns
	}
	hasLegacy := false
	for _, columns := range tableColumns {
		hasLegacy = hasLegacy || columns["proxy_url"] || columns["proxy_username"] || columns["proxy_password_cipher"]
	}
	if !hasLegacy {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := formatTime(time.Now().UTC())
	insertProxy := func(name, proxyURL, username, passwordCipher string) (string, error) {
		baseName := strings.TrimSpace(name)
		if baseName == "" {
			baseName = "Proxy"
		}
		candidate := baseName
		for suffix := 2; ; suffix++ {
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM proxies WHERE name=?`, candidate).Scan(&count); err != nil {
				return "", err
			}
			if count == 0 {
				break
			}
			candidate = fmt.Sprintf("%s (%d)", baseName, suffix)
		}
		proxyID := ids.New("proxy")
		if _, err := tx.ExecContext(ctx, `INSERT INTO proxies(id,name,url,username,password_cipher,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`,
			proxyID, candidate, proxyURL, username, passwordCipher, now, now); err != nil {
			return "", err
		}
		return proxyID, nil
	}

	if tableColumns["hosts"]["proxy_url"] {
		rows, err := tx.QueryContext(ctx, `SELECT id,name,proxy_id,proxy_url,proxy_username,proxy_password_cipher FROM hosts WHERE proxy_url<>''`)
		if err != nil {
			return err
		}
		type legacyHostProxy struct{ id, name, proxyID, url, username, passwordCipher string }
		values := make([]legacyHostProxy, 0)
		for rows.Next() {
			var value legacyHostProxy
			if err := rows.Scan(&value.id, &value.name, &value.proxyID, &value.url, &value.username, &value.passwordCipher); err != nil {
				rows.Close()
				return err
			}
			values = append(values, value)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, value := range values {
			if value.proxyID != "" {
				continue
			}
			proxyID, err := insertProxy("SSH · "+value.name, value.url, value.username, value.passwordCipher)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE hosts SET proxy_id=? WHERE id=?`, proxyID, value.id); err != nil {
				return err
			}
		}
	}
	if tableColumns["model_providers"]["proxy_url"] {
		rows, err := tx.QueryContext(ctx, `SELECT id,name,proxy_id,proxy_url,proxy_username,proxy_password_cipher FROM model_providers WHERE proxy_url<>''`)
		if err != nil {
			return err
		}
		type legacyModelProxy struct{ id, name, proxyID, url, username, passwordCipher string }
		values := make([]legacyModelProxy, 0)
		for rows.Next() {
			var value legacyModelProxy
			if err := rows.Scan(&value.id, &value.name, &value.proxyID, &value.url, &value.username, &value.passwordCipher); err != nil {
				rows.Close()
				return err
			}
			values = append(values, value)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, value := range values {
			if value.proxyID != "" {
				continue
			}
			proxyID, err := insertProxy("Model · "+value.name, value.url, value.username, value.passwordCipher)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE model_providers SET proxy_id=? WHERE id=?`, proxyID, value.id); err != nil {
				return err
			}
		}
	}
	if tableColumns["web_search_settings"]["proxy_url"] {
		var proxyID, proxyURL, username, passwordCipher string
		err := tx.QueryRowContext(ctx, `SELECT proxy_id,proxy_url,proxy_username,proxy_password_cipher FROM web_search_settings WHERE id=1 AND proxy_url<>''`).
			Scan(&proxyID, &proxyURL, &username, &passwordCipher)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && proxyID == "" {
			proxyID, err = insertProxy("Tavily", proxyURL, username, passwordCipher)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE web_search_settings SET proxy_id=? WHERE id=1`, proxyID); err != nil {
				return err
			}
		}
	}
	for table, columns := range tableColumns {
		for _, column := range []string{"proxy_url", "proxy_username", "proxy_password_cipher"} {
			if columns[column] {
				if _, err := tx.ExecContext(ctx, `ALTER TABLE `+table+` DROP COLUMN `+column); err != nil {
					return fmt.Errorf("drop legacy %s.%s: %w", table, column, err)
				}
			}
		}
	}
	return tx.Commit()
}

func (s *Store) migrateManagedWorkspaces(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(workspaces)`)
	if err != nil {
		return err
	}
	hasRoot := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		hasRoot = hasRoot || name == "root"
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !hasRoot {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`DROP TABLE workspaces`,
		`CREATE TABLE workspaces (
  id TEXT PRIMARY KEY,
  access TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
)`,
		`DELETE FROM workspace_state`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate managed workspaces: %w", err)
		}
	}
	return tx.Commit()
}

func (s *Store) migrateNativeOnlyHosts(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(hosts)`)
	if err != nil {
		return err
	}
	columns := make(map[string]bool)
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		columns[name] = true
	}
	if err := rows.Close(); err != nil {
		return err
	}
	obsolete := []string{"transport_backend", "config_alias", "proxy_jump", "identity_file"}
	needsMigration := false
	for _, column := range obsolete {
		needsMigration = needsMigration || columns[column]
	}
	if !needsMigration {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`DELETE FROM approvals`,
		`DELETE FROM tasks`,
		`DELETE FROM runs`,
		`DELETE FROM hosts`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("clear legacy SSH data: %w", err)
		}
	}
	for _, column := range obsolete {
		if columns[column] {
			if _, err := tx.ExecContext(ctx, `ALTER TABLE hosts DROP COLUMN `+column); err != nil {
				return fmt.Errorf("drop legacy hosts.%s: %w", column, err)
			}
		}
	}
	return tx.Commit()
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
INSERT INTO model_providers(id,name,kind,base_url,model,api_key_cipher,proxy_id,user_agent,active,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name,kind=excluded.kind,base_url=excluded.base_url,
model=excluded.model,api_key_cipher=excluded.api_key_cipher,proxy_id=excluded.proxy_id,
user_agent=excluded.user_agent,updated_at=excluded.updated_at`,
		provider.ID, provider.Name, provider.Kind, provider.BaseURL, provider.Model, provider.APIKeyCipher,
		provider.ProxyID, provider.UserAgent,
		boolInt(provider.Active), formatTime(provider.CreatedAt), formatTime(provider.UpdatedAt))
	if err != nil {
		return domain.ModelProvider{}, err
	}
	return s.GetModelProvider(ctx, provider.ID)
}

func (s *Store) GetModelProvider(ctx context.Context, id string) (domain.ModelProvider, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,name,kind,base_url,model,api_key_cipher,proxy_id,user_agent,active,created_at,updated_at
FROM model_providers WHERE id=?`, id)
	return scanModelProvider(row)
}

func (s *Store) ActiveModelProvider(ctx context.Context) (domain.ModelProvider, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,name,kind,base_url,model,api_key_cipher,proxy_id,user_agent,active,created_at,updated_at
FROM model_providers WHERE active=1 LIMIT 1`)
	return scanModelProvider(row)
}

func (s *Store) ListModelProviders(ctx context.Context) ([]domain.ModelProvider, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,kind,base_url,model,api_key_cipher,proxy_id,user_agent,active,created_at,updated_at
FROM model_providers ORDER BY active DESC,name`)
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
	var imageTypesJSON string
	var systemPrompt sql.NullString
	var updated string
	err := s.db.QueryRowContext(ctx, `SELECT agent_max_iterations,system_prompt,approval_mode,approval_explanations_enabled,subagent_model_provider_id,subagent_timeout_seconds,
chat_image_allowed_types_json,workspace_shell_mode,updated_at FROM system_settings WHERE id=1`).Scan(
		&settings.AgentMaxIterations, &systemPrompt, &settings.ApprovalMode, &explanationsEnabled, &settings.SubagentModelProviderID, &settings.SubagentTimeoutSeconds,
		&imageTypesJSON, &settings.WorkspaceShellMode, &updated,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.SystemSettings{
			AgentMaxIterations: domain.DefaultAgentMaxIterations, ApprovalExplanationsEnabled: true,
			ApprovalMode: domain.ApprovalModeManual,
			SystemPrompt: domain.DefaultSystemPrompt, DefaultSystemPrompt: domain.DefaultSystemPrompt,
			SubagentTimeoutSeconds: domain.DefaultSubagentTimeoutSeconds, WorkspaceShellMode: domain.WorkspaceShellModeSandbox,
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
	return settings, nil
}

func (s *Store) SaveSystemSettings(ctx context.Context, settings domain.SystemSettings) (domain.SystemSettings, error) {
	settings.UpdatedAt = time.Now().UTC()
	imageTypesJSON, err := json.Marshal(settings.ChatImageAllowedTypes)
	if err != nil {
		return domain.SystemSettings{}, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO system_settings(id,agent_max_iterations,system_prompt,approval_mode,approval_explanations_enabled,subagent_model_provider_id,subagent_timeout_seconds,chat_image_allowed_types_json,workspace_shell_mode,updated_at) VALUES(1,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET agent_max_iterations=excluded.agent_max_iterations,
system_prompt=excluded.system_prompt,
approval_mode=excluded.approval_mode,
approval_explanations_enabled=excluded.approval_explanations_enabled,
subagent_model_provider_id=excluded.subagent_model_provider_id,
subagent_timeout_seconds=excluded.subagent_timeout_seconds,
chat_image_allowed_types_json=excluded.chat_image_allowed_types_json,
workspace_shell_mode=excluded.workspace_shell_mode,updated_at=excluded.updated_at`,
		settings.AgentMaxIterations, settings.SystemPrompt, settings.ApprovalMode, boolInt(settings.ApprovalExplanationsEnabled), settings.SubagentModelProviderID,
		settings.SubagentTimeoutSeconds, string(imageTypesJSON), settings.WorkspaceShellMode, formatTime(settings.UpdatedAt))
	if err != nil {
		return domain.SystemSettings{}, err
	}
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
	err := row.Scan(&provider.ID, &provider.Name, &provider.Kind, &provider.BaseURL, &provider.Model,
		&provider.APIKeyCipher, &provider.ProxyID, &provider.UserAgent, &active, &created, &updated)
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

func (s *Store) backfillRunSearchText(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id, request_json FROM runs`)
	if err != nil {
		return err
	}
	defer rows.Close()
	updates := make(map[string]string)
	for rows.Next() {
		var id, requestJSON string
		if err := rows.Scan(&id, &requestJSON); err != nil {
			return err
		}
		var req domain.ExecRequest
		// Redaction can break the stored JSON; those rows keep the
		// request_json LIKE fallback in SearchRuns.
		if json.Unmarshal([]byte(requestJSON), &req) != nil {
			continue
		}
		if text := req.SearchText(); text != "" {
			updates[id] = text
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for id, text := range updates {
		if _, err := s.db.ExecContext(ctx, `UPDATE runs SET search_text=? WHERE id=?`, text, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) CreateRun(ctx context.Context, run domain.Run) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO runs(id,session_id,host_id,tool_name,tool_arguments_json,request_json,request_cipher,search_text,request_digest,risk,status,ai_review_json,
started_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, run.ID, run.SessionID, run.HostID, run.ToolName, run.ToolArgumentsJSON, run.RequestJSON, run.RequestCipher, run.SearchText, run.RequestDigest,
		run.Risk, run.Status, run.AIReviewJSON, formatTime(run.StartedAt))
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
	row := s.db.QueryRowContext(ctx, `SELECT id,session_id,host_id,tool_name,tool_arguments_json,request_json,request_cipher,request_digest,risk,status,
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
	pattern := likePattern(query)
	statement := `SELECT id,session_id,host_id,tool_name,tool_arguments_json,request_json,request_cipher,request_digest,risk,status,
exit_code,stdout_redacted,stderr_redacted,stdout_cipher,stderr_cipher,error,ai_review_json,started_at,completed_at
FROM runs WHERE (?='' OR session_id=?) AND (?='' OR host_id=?) AND (?='' OR search_text LIKE ? ESCAPE '\' OR request_json LIKE ? ESCAPE '\'
		OR tool_arguments_json LIKE ? ESCAPE '\' OR stdout_redacted LIKE ? ESCAPE '\' OR stderr_redacted LIKE ? ESCAPE '\') ORDER BY started_at DESC`
	arguments := []any{sessionID, sessionID, hostID, hostID, query, pattern, pattern, pattern, pattern, pattern}
	if limit > 0 {
		statement += " LIMIT ?"
		arguments = append(arguments, limit)
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

func scanRun(row scanner) (domain.Run, error) {
	var run domain.Run
	var started string
	var completed sql.NullString
	err := row.Scan(&run.ID, &run.SessionID, &run.HostID, &run.ToolName, &run.ToolArgumentsJSON, &run.RequestJSON, &run.RequestCipher, &run.RequestDigest, &run.Risk,
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

func (s *Store) CreateApproval(ctx context.Context, approval domain.Approval) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO approvals(id,run_id,host_id,request_json,request_cipher,request_digest,risk,
status,reason,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, approval.ID, approval.RunID,
		approval.HostID, approval.RequestJSON, approval.RequestCipher, approval.RequestDigest, approval.Risk, approval.Status,
		approval.Reason, formatTime(approval.CreatedAt), formatTime(approval.ExpiresAt))
	return err
}

func (s *Store) GetApproval(ctx context.Context, id string) (domain.Approval, error) {
	row := s.db.QueryRowContext(ctx, `SELECT approvals.id,approvals.run_id,runs.session_id,approvals.host_id,approvals.request_json,
approvals.request_cipher,approvals.request_digest,approvals.risk,approvals.status,approvals.reason,
approvals.created_at,approvals.expires_at,approvals.decided_at FROM approvals
JOIN runs ON runs.id=approvals.run_id WHERE approvals.id=?`, id)
	return scanApproval(row)
}

func (s *Store) ListApprovals(ctx context.Context, status string, limit int) ([]domain.Approval, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	now := formatTime(time.Now().UTC())
	_, _ = s.db.ExecContext(ctx, `UPDATE runs SET status='expired',error='approval expired',completed_at=?
WHERE id IN (SELECT run_id FROM approvals WHERE status='pending' AND expires_at < ?)`, now, now)
	_, _ = s.db.ExecContext(ctx, `UPDATE approvals SET status='expired',reason='approval expired',decided_at=?
WHERE status='pending' AND expires_at < ?`, now, now)
	rows, err := s.db.QueryContext(ctx, `SELECT approvals.id,approvals.run_id,runs.session_id,approvals.host_id,
approvals.request_json,approvals.request_cipher,approvals.request_digest,approvals.risk,approvals.status,
approvals.reason,approvals.created_at,approvals.expires_at,approvals.decided_at FROM approvals
JOIN runs ON runs.id=approvals.run_id WHERE (?='' OR approvals.status=?)
ORDER BY approvals.created_at DESC LIMIT ?`, status, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.Approval, 0)
	for rows.Next() {
		approval, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, approval)
	}
	return result, rows.Err()
}

func (s *Store) ListPendingApprovalsForSession(ctx context.Context, sessionID string) ([]domain.Approval, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT approvals.id,approvals.run_id,runs.session_id,approvals.host_id,
approvals.request_json,approvals.request_cipher,approvals.request_digest,approvals.risk,approvals.status,
approvals.reason,approvals.created_at,approvals.expires_at,approvals.decided_at FROM approvals
JOIN runs ON runs.id=approvals.run_id WHERE runs.session_id=? AND approvals.status='pending'
ORDER BY approvals.created_at`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.Approval, 0)
	for rows.Next() {
		approval, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, approval)
	}
	return result, rows.Err()
}

func (s *Store) DecideApproval(ctx context.Context, id, status, reason string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE approvals SET status=?,reason=?,decided_at=? WHERE id=? AND status='pending'`,
		status, reason, formatTime(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return fmt.Errorf("approval is missing or no longer pending")
	}
	return nil
}

func (s *Store) ApprovePendingAndStartRun(ctx context.Context, id, runID, reason string, expectedRisk domain.RiskLevel) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE approvals SET status='approved',reason=?,decided_at=?
WHERE id=? AND run_id=? AND status='pending' AND risk=?`, reason, formatTime(time.Now().UTC()), id, runID, expectedRisk)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return fmt.Errorf("approval changed or is no longer pending; refresh and review the latest risk")
	}
	result, err = tx.ExecContext(ctx, `UPDATE runs SET status='running' WHERE id=? AND status='approval_required'`, runID)
	if err != nil {
		return err
	}
	count, _ = result.RowsAffected()
	if count == 0 {
		return fmt.Errorf("approval run changed or is no longer awaiting approval")
	}
	return tx.Commit()
}

func (s *Store) UpdatePendingApprovalExplanation(ctx context.Context, approvalID, runID, reviewJSON string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE runs SET ai_review_json=?
WHERE id=? AND status='approval_required' AND EXISTS (
  SELECT 1 FROM approvals WHERE approvals.id=? AND approvals.run_id=runs.id AND approvals.status='pending'
)`, reviewJSON, runID, approvalID)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return fmt.Errorf("approval is no longer pending")
	}
	return nil
}

func (s *Store) UpdateRunAIReview(ctx context.Context, runID, reviewJSON string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE runs SET ai_review_json=? WHERE id=?`, reviewJSON, runID)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func scanApproval(row scanner) (domain.Approval, error) {
	var approval domain.Approval
	var created, expires string
	var decided sql.NullString
	err := row.Scan(&approval.ID, &approval.RunID, &approval.SessionID, &approval.HostID, &approval.RequestJSON, &approval.RequestCipher,
		&approval.RequestDigest, &approval.Risk, &approval.Status, &approval.Reason,
		&created, &expires, &decided)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Approval{}, ErrNotFound
	}
	if err != nil {
		return domain.Approval{}, err
	}
	approval.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	approval.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expires)
	if decided.Valid {
		approval.DecidedAt, _ = time.Parse(time.RFC3339Nano, decided.String)
	}
	return approval, nil
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
	rows, err := s.db.QueryContext(ctx, `SELECT id,run_id,event_type,actor,data_json,created_at
FROM audit_events WHERE (?='' OR run_id=?) ORDER BY created_at DESC LIMIT ?`, runID, runID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]domain.AuditEvent, 0)
	for rows.Next() {
		var event domain.AuditEvent
		var data, created string
		if err := rows.Scan(&event.ID, &event.RunID, &event.Type, &event.Actor, &data, &created); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(data), &event.Data)
		event.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) AppendChatMessage(ctx context.Context, sessionID, role, content string, toolName ...string) error {
	_, err := s.appendChatMessage(ctx, sessionID, role, content, "completed", toolName...)
	return err
}

func (s *Store) AppendPendingChatMessage(ctx context.Context, sessionID, role, content string, toolName ...string) (string, error) {
	return s.appendChatMessage(ctx, sessionID, role, content, "pending", toolName...)
}

func (s *Store) AppendPendingChatMessageWithAttachments(ctx context.Context, sessionID, role, content string, attachments []domain.ChatAttachment) (string, error) {
	name := ""
	return s.appendChatMessageWithAttachments(ctx, sessionID, role, content, "pending", name, attachments)
}

func (s *Store) appendChatMessage(ctx context.Context, sessionID, role, content, status string, toolName ...string) (string, error) {
	name := ""
	if len(toolName) > 0 {
		name = toolName[0]
	}
	return s.appendChatMessageWithAttachments(ctx, sessionID, role, content, status, name, nil)
}

func (s *Store) appendChatMessageWithAttachments(ctx context.Context, sessionID, role, content, status, toolName string, attachments []domain.ChatAttachment) (string, error) {
	id := ids.New("msg")
	now := formatTime(time.Now().UTC())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO chat_sessions(session_id,workspace_id,created_at,updated_at) VALUES(?,?,?,?)`, sessionID, "", now, now); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO chat_messages(id,session_id,role,content,tool_name,status,created_at)
VALUES(?,?,?,?,?,?,?)`, id, sessionID, role, content, toolName, status, now); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chat_sessions SET updated_at=? WHERE session_id=?`, now, sessionID); err != nil {
		return "", err
	}
	for _, attachment := range attachments {
		attachmentID := attachment.ID
		if attachmentID == "" {
			attachmentID = ids.New("image")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO chat_attachments(id,message_id,name,mime_type,size_bytes,data,created_at)
VALUES(?,?,?,?,?,?,?)`, attachmentID, id, attachment.Name, attachment.MIMEType, len(attachment.Data), attachment.Data, now); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) SetChatMessageStatus(ctx context.Context, id, status string) error {
	if status != "pending" && status != "completed" && status != "failed" {
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
// assistant response or Tool result. Reasoning and the transient interruption
// marker are not model context, so they are removed with the user message.
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
      OR (turn_message.role='assistant' AND trim(turn_message.content)<>'' AND trim(turn_message.content)<>?)
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

func (s *Store) ReplaceAgentPlan(ctx context.Context, plan domain.AgentPlan) (domain.AgentPlan, error) {
	now := time.Now().UTC()
	plan.CreatedAt = now
	plan.UpdatedAt = now
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.AgentPlan{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_plans WHERE session_id=?`, plan.SessionID); err != nil {
		return domain.AgentPlan{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_plans(session_id,goal,status,created_at,updated_at) VALUES(?,?,?,?,?)`,
		plan.SessionID, plan.Goal, plan.Status, formatTime(plan.CreatedAt), formatTime(plan.UpdatedAt)); err != nil {
		return domain.AgentPlan{}, err
	}
	for _, step := range plan.Steps {
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_plan_steps(session_id,step_number,title,status,evidence,updated_at) VALUES(?,?,?,?,?,?)`,
			plan.SessionID, step.Number, step.Title, step.Status, step.Evidence, formatTime(now)); err != nil {
			return domain.AgentPlan{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.AgentPlan{}, err
	}
	return s.GetAgentPlan(ctx, plan.SessionID)
}

func (s *Store) GetAgentPlan(ctx context.Context, sessionID string) (domain.AgentPlan, error) {
	var plan domain.AgentPlan
	var created, updated string
	err := s.db.QueryRowContext(ctx, `SELECT session_id,goal,status,created_at,updated_at FROM agent_plans WHERE session_id=?`, sessionID).Scan(
		&plan.SessionID, &plan.Goal, &plan.Status, &created, &updated,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AgentPlan{}, ErrNotFound
	}
	if err != nil {
		return domain.AgentPlan{}, err
	}
	plan.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	plan.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	rows, err := s.db.QueryContext(ctx, `SELECT step_number,title,status,evidence,updated_at FROM agent_plan_steps WHERE session_id=? ORDER BY step_number`, sessionID)
	if err != nil {
		return domain.AgentPlan{}, err
	}
	defer rows.Close()
	plan.Steps = make([]domain.AgentPlanStep, 0)
	for rows.Next() {
		var step domain.AgentPlanStep
		var stepUpdated string
		if err := rows.Scan(&step.Number, &step.Title, &step.Status, &step.Evidence, &stepUpdated); err != nil {
			return domain.AgentPlan{}, err
		}
		step.UpdatedAt, _ = time.Parse(time.RFC3339Nano, stepUpdated)
		plan.Steps = append(plan.Steps, step)
	}
	return plan, rows.Err()
}

func (s *Store) AdvanceAgentPlan(ctx context.Context, sessionID string, stepNumber int, status, evidence string) (domain.AgentPlan, error) {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.AgentPlan{}, err
	}
	defer tx.Rollback()
	var currentStatus string
	err = tx.QueryRowContext(ctx, `SELECT status FROM agent_plan_steps WHERE session_id=? AND step_number=?`, sessionID, stepNumber).Scan(&currentStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AgentPlan{}, ErrNotFound
	}
	if err != nil {
		return domain.AgentPlan{}, err
	}
	if currentStatus != "in_progress" {
		return domain.AgentPlan{}, &PlanTransitionError{StepNumber: stepNumber, Status: currentStatus}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_plan_steps SET status=?,evidence=?,updated_at=? WHERE session_id=? AND step_number=?`,
		status, evidence, formatTime(now), sessionID, stepNumber); err != nil {
		return domain.AgentPlan{}, err
	}
	planStatus := "blocked"
	if status == "completed" {
		var next int
		err := tx.QueryRowContext(ctx, `SELECT step_number FROM agent_plan_steps WHERE session_id=? AND step_number>? ORDER BY step_number LIMIT 1`, sessionID, stepNumber).Scan(&next)
		if errors.Is(err, sql.ErrNoRows) {
			planStatus = "completed"
		} else if err != nil {
			return domain.AgentPlan{}, err
		} else {
			planStatus = "active"
			if _, err := tx.ExecContext(ctx, `UPDATE agent_plan_steps SET status='in_progress',updated_at=? WHERE session_id=? AND step_number=? AND status='pending'`,
				formatTime(now), sessionID, next); err != nil {
				return domain.AgentPlan{}, err
			}
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE agent_plans SET status=?,updated_at=? WHERE session_id=?`, planStatus, formatTime(now), sessionID)
	if err != nil {
		return domain.AgentPlan{}, err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return domain.AgentPlan{}, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return domain.AgentPlan{}, err
	}
	return s.GetAgentPlan(ctx, sessionID)
}

func (s *Store) ListChatMessages(ctx context.Context, sessionID string, limit int) ([]domain.ChatMessage, error) {
	return s.listChatMessages(ctx, sessionID, limit, false)
}

func (s *Store) ListChatModelMessages(ctx context.Context, sessionID string, limit int) ([]domain.ChatMessage, error) {
	return s.listChatMessages(ctx, sessionID, limit, true)
}

// ListChatContextMessages returns the persisted, provider-relevant transcript.
// Reasoning is deliberately excluded, while tool evidence and failed turns are
// retained so the next model run can recover operational state.
func (s *Store) ListChatContextMessages(ctx context.Context, sessionID string) ([]domain.ChatMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,role,content,tool_name,status,created_at FROM chat_messages
WHERE session_id=? AND role IN ('user','assistant','tool') AND status IN ('completed','failed')
ORDER BY created_at`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.ChatMessage, 0)
	for rows.Next() {
		var message domain.ChatMessage
		var created string
		if err := rows.Scan(&message.ID, &message.Role, &message.Content, &message.ToolName, &message.Status, &created); err != nil {
			return nil, err
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
	return result, nil
}

func (s *Store) listChatMessages(ctx context.Context, sessionID string, limit int, modelOnly bool) ([]domain.ChatMessage, error) {
	filter := ""
	if modelOnly {
		filter = " AND role IN ('user','assistant') AND status='completed'"
	}
	query := `SELECT id,role,content,tool_name,status,created_at FROM chat_messages WHERE session_id=?` + filter + ` ORDER BY created_at`
	args := []any{sessionID}
	if limit > 0 {
		query = `SELECT id,role,content,tool_name,status,created_at FROM (
SELECT id,role,content,tool_name,status,created_at FROM chat_messages WHERE session_id=?` + filter + ` ORDER BY created_at DESC LIMIT ?)
ORDER BY created_at`
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
		var created string
		if err := rows.Scan(&message.ID, &message.Role, &message.Content, &message.ToolName, &message.Status, &created); err != nil {
			return nil, err
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
	if err := s.loadChatAttachments(ctx, result, false); err != nil {
		return nil, err
	}
	return result, nil
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
	var updated string
	err := s.db.QueryRowContext(ctx, `SELECT session_id,workspace_id,updated_at FROM chat_sessions WHERE session_id=?`, sessionID).Scan(&session.ID, &session.WorkspaceID, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ChatSession{}, ErrNotFound
	}
	if err != nil {
		return domain.ChatSession{}, err
	}
	session.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return session, nil
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

func (s *Store) ListChatSessions(ctx context.Context, limit int) ([]domain.ChatSession, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT sessions.session_id,
  COALESCE(NULLIF((SELECT trim(substr(first.content,1,80)) FROM chat_messages AS first
    WHERE first.session_id=sessions.session_id AND first.role='user'
    ORDER BY first.created_at ASC LIMIT 1),''),'New conversation') AS title,
  sessions.workspace_id,
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
		var updated string
		if err := rows.Scan(&session.ID, &session.Title, &session.WorkspaceID, &session.MessageCount, &updated); err != nil {
			return nil, err
		}
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM chat_messages WHERE session_id=?`, sessionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM checkpoints WHERE id=?`, sessionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ssh_shell_sessions WHERE session_id=?`, sessionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_plans WHERE session_id=?`, sessionID); err != nil {
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
