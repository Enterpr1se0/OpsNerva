package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func (s *Store) StartMCPToolCall(ctx context.Context, session domain.MCPClientSession, call domain.MCPToolCall) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO mcp_client_sessions(
id,transport,client_name,client_version,protocol_version,started_at,last_seen_at) VALUES(?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET transport=excluded.transport,
client_name=CASE WHEN excluded.client_name='' THEN mcp_client_sessions.client_name ELSE excluded.client_name END,
client_version=CASE WHEN excluded.client_version='' THEN mcp_client_sessions.client_version ELSE excluded.client_version END,
protocol_version=CASE WHEN excluded.protocol_version='' THEN mcp_client_sessions.protocol_version ELSE excluded.protocol_version END,
last_seen_at=excluded.last_seen_at`,
		session.ID, session.Transport, session.ClientName, session.ClientVersion, session.ProtocolVersion,
		formatTime(session.StartedAt), formatTime(session.LastSeenAt)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mcp_tool_calls(
id,session_id,tool_name,arguments_json,status,started_at,updated_at) VALUES(?,?,?,?,?,?,?)`,
		call.ID, call.SessionID, call.ToolName, call.ArgumentsJSON, call.Status,
		formatTime(call.StartedAt), formatTime(call.UpdatedAt)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FinishMCPToolCall(ctx context.Context, call domain.MCPToolCall) error {
	var completed any
	if !call.CompletedAt.IsZero() {
		completed = formatTime(call.CompletedAt)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE mcp_tool_calls SET status=?,run_id=?,approval_id=?,task_id=?,shell_id=?,tunnel_id=?,
operation_status=?,error=?,updated_at=?,completed_at=? WHERE id=?`,
		call.Status, call.RunID, call.ApprovalID, call.TaskID, call.ShellID, call.TunnelID,
		call.OperationStatus, call.Error, formatTime(call.UpdatedAt), completed, call.ID)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) BindMCPToolCallOperation(ctx context.Context, callID, runID, shellID, operationStatus string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE mcp_tool_calls SET
run_id=CASE WHEN run_id='' THEN ? ELSE run_id END,
shell_id=CASE WHEN shell_id='' THEN ? ELSE shell_id END,
operation_status=CASE WHEN ?='' THEN operation_status ELSE ? END,
updated_at=? WHERE id=?`, strings.TrimSpace(runID), strings.TrimSpace(shellID), operationStatus, operationStatus,
		formatTime(time.Now().UTC()), strings.TrimSpace(callID))
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) InterruptRunningMCPToolCalls(ctx context.Context) error {
	now := formatTime(time.Now().UTC())
	_, err := s.db.ExecContext(ctx, `UPDATE mcp_tool_calls SET status=?,operation_status=CASE WHEN operation_status='' THEN ? ELSE operation_status END,
error=CASE WHEN error='' THEN 'control plane restarted before the MCP call completed' ELSE error END,updated_at=?,completed_at=? WHERE status=?`,
		domain.MCPCallInterrupted, domain.MCPCallInterrupted, now, now, domain.MCPCallRunning)
	return err
}

func (s *Store) ListMCPClientSessions(ctx context.Context, limit int) ([]domain.MCPClientSession, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT s.id,s.transport,s.client_name,s.client_version,s.protocol_version,s.started_at,s.last_seen_at,
(SELECT COUNT(*) FROM mcp_tool_calls c WHERE c.session_id=s.id),
(SELECT COUNT(*) FROM mcp_tool_calls c WHERE c.session_id=s.id AND c.status=?)
FROM mcp_client_sessions s ORDER BY s.last_seen_at DESC,s.id DESC LIMIT ?`, domain.MCPCallRunning, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sessions := make([]domain.MCPClientSession, 0)
	for rows.Next() {
		var session domain.MCPClientSession
		var started, seen string
		if err := rows.Scan(&session.ID, &session.Transport, &session.ClientName, &session.ClientVersion,
			&session.ProtocolVersion, &started, &seen, &session.CallCount, &session.RunningCalls); err != nil {
			return nil, err
		}
		session.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
		session.LastSeenAt, _ = time.Parse(time.RFC3339Nano, seen)
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

func (s *Store) ListMCPToolCalls(ctx context.Context, sessionID string, limit int) ([]domain.MCPToolCall, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,session_id,tool_name,arguments_json,status,run_id,approval_id,task_id,shell_id,tunnel_id,
operation_status,error,started_at,updated_at,completed_at FROM mcp_tool_calls WHERE session_id=?
ORDER BY started_at DESC,id DESC LIMIT ?`, strings.TrimSpace(sessionID), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	calls := make([]domain.MCPToolCall, 0)
	for rows.Next() {
		call, err := scanMCPToolCall(rows)
		if err != nil {
			return nil, err
		}
		calls = append(calls, call)
	}
	return calls, rows.Err()
}

func scanMCPToolCall(row scanner) (domain.MCPToolCall, error) {
	var call domain.MCPToolCall
	var started, updated string
	var completed sql.NullString
	err := row.Scan(&call.ID, &call.SessionID, &call.ToolName, &call.ArgumentsJSON, &call.Status, &call.RunID,
		&call.ApprovalID, &call.TaskID, &call.ShellID, &call.TunnelID, &call.OperationStatus, &call.Error,
		&started, &updated, &completed)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.MCPToolCall{}, ErrNotFound
	}
	if err != nil {
		return domain.MCPToolCall{}, err
	}
	call.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
	call.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	if completed.Valid {
		call.CompletedAt, _ = time.Parse(time.RFC3339Nano, completed.String)
	}
	return call, nil
}
