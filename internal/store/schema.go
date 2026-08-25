package store

import (
	"context"
	"runtime"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func (s *Store) initializeSchema(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS hosts (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  address TEXT NOT NULL,
  port INTEGER NOT NULL,
  username TEXT NOT NULL,
  agent_enabled INTEGER NOT NULL DEFAULT 1,
  agent_root_enabled INTEGER NOT NULL DEFAULT 0,
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
  session_id TEXT NOT NULL DEFAULT '',
  host_id TEXT NOT NULL,
  status TEXT NOT NULL,
  revision INTEGER NOT NULL DEFAULT 0,
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
	if err := s.ensureColumn(ctx, "hosts", "agent_root_enabled", "INTEGER NOT NULL DEFAULT 0"); err != nil {
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
	if err := s.ensureColumn(ctx, "tasks", "session_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "tasks", "revision", "INTEGER NOT NULL DEFAULT 0"); err != nil {
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
