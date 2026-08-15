package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"eino-ops-agent/internal/domain"
)

const maxSSHShellModelPageEvents = 512

func (s *Store) CreateSSHShell(ctx context.Context, shell domain.SSHShell) error {
	if shell.Kind == "" {
		shell.Kind = domain.SSHShellKindSSH
	}
	elevated := 0
	if shell.Elevated {
		elevated = 1
	}
	var exitCode any
	if shell.ExitCode != nil {
		exitCode = *shell.ExitCode
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO ssh_shell_sessions(
id,run_id,session_id,kind,surface,host_id,host_name,workspace_id,backend,username,elevated,cwd,status,cols,rows,last_sequence,
exit_code,termination_reason,error,started_at,ended_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,NULL)`,
		shell.ID, shell.RunID, shell.SessionID, shell.Kind, shell.Surface, shell.HostID, shell.HostName,
		shell.WorkspaceID, shell.Backend, shell.User,
		elevated, shell.Cwd, shell.Status, shell.Cols, shell.Rows, shell.LastSequence,
		exitCode, shell.TerminationReason, shell.Error, formatTime(shell.StartedAt))
	return err
}

func (s *Store) UpdateSSHShell(ctx context.Context, shell domain.SSHShell) error {
	elevated := 0
	if shell.Elevated {
		elevated = 1
	}
	var ended any
	if !shell.EndedAt.IsZero() {
		ended = formatTime(shell.EndedAt)
	}
	var exitCode any
	if shell.ExitCode != nil {
		exitCode = *shell.ExitCode
	}
	result, err := s.db.ExecContext(ctx, `UPDATE ssh_shell_sessions SET
kind=?,surface=?,host_name=?,workspace_id=?,backend=?,username=?,elevated=?,cwd=?,status=?,cols=?,rows=?,last_sequence=?,exit_code=?,termination_reason=?,error=?,ended_at=?
WHERE id=?`, shell.Kind, shell.Surface, shell.HostName, shell.WorkspaceID, shell.Backend, shell.User, elevated, shell.Cwd, shell.Status, shell.Cols, shell.Rows,
		shell.LastSequence, exitCode, shell.TerminationReason, shell.Error, ended, shell.ID)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) AppendSSHShellEvent(ctx context.Context, event domain.SSHShellEvent, recentOutput string) error {
	return s.AppendSSHShellEvents(ctx, []domain.SSHShellEvent{event}, recentOutput)
}

func (s *Store) AppendSSHShellEvents(ctx context.Context, events []domain.SSHShellEvent, recentOutput string) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, event := range events {
		var readable any
		if event.ReadableContent != nil {
			readable = *event.ReadableContent
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ssh_shell_events(
shell_id,sequence,stream,source,content_redacted,content_readable,sensitive,input_bytes,status,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			event.ShellID, event.Sequence, event.Stream, event.Source, event.Content, readable, event.Sensitive,
			event.InputBytes, event.Status, formatTime(event.CreatedAt)); err != nil {
			return err
		}
	}
	last := events[len(events)-1]
	if _, err := tx.ExecContext(ctx, `UPDATE ssh_shell_sessions SET last_sequence=?,recent_output=? WHERE id=?`,
		last.Sequence, recentOutput, last.ShellID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetSSHShell(ctx context.Context, id string) (domain.SSHShell, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,run_id,session_id,kind,surface,host_id,host_name,workspace_id,backend,username,elevated,cwd,
status,cols,rows,last_sequence,response_sequence,exit_code,termination_reason,error,started_at,ended_at
FROM ssh_shell_sessions WHERE id=?`, id)
	return scanSSHShell(row)
}

func (s *Store) ListSSHShells(ctx context.Context, sessionID string, activeOnly bool) ([]domain.SSHShell, error) {
	active := 0
	if activeOnly {
		active = 1
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,run_id,session_id,kind,surface,host_id,host_name,workspace_id,backend,username,elevated,cwd,
status,cols,rows,last_sequence,response_sequence,exit_code,termination_reason,error,started_at,ended_at
FROM ssh_shell_sessions
WHERE (?='' OR session_id=?) AND (?=0 OR status IN ('starting','running','stopping'))
ORDER BY started_at`, sessionID, sessionID, active)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.SSHShell, 0)
	for rows.Next() {
		shell, err := scanSSHShell(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, shell)
	}
	return result, rows.Err()
}

func (s *Store) ListSSHShellEvents(ctx context.Context, shellID string, after uint64) ([]domain.SSHShellEvent, error) {
	result, _, err := s.ListSSHShellEventsPage(ctx, shellID, after, 0)
	return result, err
}

func (s *Store) ListSSHShellEventsPage(ctx context.Context, shellID string, after uint64, maxOutputBytes int) ([]domain.SSHShellEvent, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT shell_id,sequence,stream,source,content_redacted,content_readable,sensitive,input_bytes,status,created_at
FROM ssh_shell_events WHERE shell_id=? AND sequence>? ORDER BY sequence`, shellID, after)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	result := make([]domain.SSHShellEvent, 0)
	outputBytes := 0
	hasMore := false
	for rows.Next() {
		if maxOutputBytes > 0 && len(result) >= maxSSHShellModelPageEvents {
			hasMore = true
			break
		}
		var event domain.SSHShellEvent
		var sensitive int
		var readable sql.NullString
		var created string
		if err := rows.Scan(&event.ShellID, &event.Sequence, &event.Stream, &event.Source,
			&event.Content, &readable, &sensitive, &event.InputBytes, &event.Status, &created); err != nil {
			return nil, false, err
		}
		if readable.Valid {
			value := readable.String
			event.ReadableContent = &value
		}
		eventBytes := 0
		if event.Stream == "stdout" || event.Stream == "stderr" {
			eventBytes = len(event.Content)
			if event.ReadableContent != nil {
				eventBytes = len(*event.ReadableContent)
			}
		}
		if maxOutputBytes > 0 && outputBytes > 0 && outputBytes+eventBytes > maxOutputBytes {
			hasMore = true
			break
		}
		event.Sensitive = sensitive != 0
		event.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		result = append(result, event)
		outputBytes += eventBytes
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	return result, hasMore, nil
}

func (s *Store) GetSSHShellRecentOutput(ctx context.Context, shellID string) (string, error) {
	var output string
	err := s.db.QueryRowContext(ctx, `SELECT recent_output FROM ssh_shell_sessions WHERE id=?`, shellID).Scan(&output)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return output, err
}

func (s *Store) LastSSHShellAgentInputSequence(ctx context.Context, shellID string) (uint64, error) {
	var sequence uint64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0) FROM ssh_shell_events
WHERE shell_id=? AND stream='input' AND source='agent'`, shellID).Scan(&sequence)
	return sequence, err
}

func (s *Store) AdvanceSSHShellResponseSequence(ctx context.Context, shellID, expectedSessionID string, sequence uint64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE ssh_shell_sessions SET response_sequence=CASE
WHEN response_sequence<? THEN ? ELSE response_sequence END
WHERE id=? AND (?='' OR session_id=?)`, sequence, sequence, shellID, expectedSessionID, expectedSessionID)
	return err
}

func (s *Store) InterruptActiveSSHShells(ctx context.Context) error {
	now := formatTime(time.Now().UTC())
	_, err := s.db.ExecContext(ctx, `UPDATE ssh_shell_sessions
SET status='interrupted',exit_code=NULL,termination_reason='service_stopped',
error=CASE WHEN error='' THEN 'control plane restarted while the shell was active' ELSE error END,ended_at=?
WHERE status IN ('starting','running','stopping')`, now)
	return err
}

func scanSSHShell(row scanner) (domain.SSHShell, error) {
	var shell domain.SSHShell
	var elevated int
	var exitCode sql.NullInt64
	var started string
	var ended sql.NullString
	err := row.Scan(&shell.ID, &shell.RunID, &shell.SessionID, &shell.Kind, &shell.Surface, &shell.HostID, &shell.HostName,
		&shell.WorkspaceID, &shell.Backend, &shell.User, &elevated, &shell.Cwd, &shell.Status, &shell.Cols, &shell.Rows,
		&shell.LastSequence, &shell.ResponseSequence, &exitCode, &shell.TerminationReason, &shell.Error, &started, &ended)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.SSHShell{}, ErrNotFound
	}
	if err != nil {
		return domain.SSHShell{}, err
	}
	shell.Elevated = elevated != 0
	if exitCode.Valid {
		code := int(exitCode.Int64)
		shell.ExitCode = &code
	}
	shell.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
	if ended.Valid {
		shell.EndedAt, _ = time.Parse(time.RFC3339Nano, ended.String)
	}
	return shell, nil
}
