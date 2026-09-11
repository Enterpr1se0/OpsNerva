package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

// The caller aliases chat_sessions as sessions. Keep sidebar, session detail,
// and audit history titles identical without loading a whole chat session.
const chatSessionDisplayTitleSQL = `COALESCE(NULLIF(trim(sessions.title),''),NULLIF((SELECT trim(substr(first.content,1,80)) FROM chat_messages AS first
    WHERE first.session_id=sessions.session_id AND first.role='user'
    ORDER BY first.created_at ASC LIMIT 1),''),'New conversation')`

func (s *Store) GetChatSession(ctx context.Context, sessionID string) (domain.ChatSession, error) {
	var session domain.ChatSession
	var storedTitle, updated string
	err := s.db.QueryRowContext(ctx, `SELECT sessions.session_id,sessions.title,
	  `+chatSessionDisplayTitleSQL+`,
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
	s.publishSessionChange(sessionID, false)
	s.publishChange(Change{Topic: ChangeAudit})
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
	if changed == 1 {
		s.publishSessionChange(sessionID, false)
		s.publishChange(Change{Topic: ChangeAudit})
	}
	session, err := s.GetChatSession(ctx, sessionID)
	if err != nil {
		return domain.ChatSession{}, false, err
	}
	return session, changed == 1, nil
}

func (s *Store) ListChatSessions(ctx context.Context, limit int) ([]domain.ChatSession, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT sessions.session_id,
  sessions.title,
  `+chatSessionDisplayTitleSQL+` AS display_title,
  sessions.workspace_id,
	sessions.context_tokens,
	sessions.context_window,
  (SELECT count(*) FROM chat_messages AS messages WHERE messages.session_id=sessions.session_id),
  sessions.updated_at
FROM chat_sessions AS sessions
ORDER BY sessions.updated_at DESC,sessions.session_id DESC
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
	s.publishSessionChange(sessionID, true)
	return s.GetChatSession(ctx, sessionID)
}

func (s *Store) SetChatSessionWorkspace(ctx context.Context, sessionID, workspaceID string) (domain.ChatSession, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE chat_sessions SET workspace_id=?,updated_at=? WHERE session_id=?`, workspaceID, formatTime(time.Now().UTC()), sessionID)
	if err != nil {
		return domain.ChatSession{}, err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return domain.ChatSession{}, ErrNotFound
	}
	s.publishSessionChange(sessionID, true)
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
	s.publishSessionChange(sessionID, true)
	return nil
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_plans WHERE session_id=?`, sessionID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.publishChange(Change{Topic: ChangeSessions, SessionID: sessionID})
	s.publishChange(Change{Topic: ChangeApprovals})
	s.publishChange(Change{Topic: ChangeAudit})
	return nil
}
