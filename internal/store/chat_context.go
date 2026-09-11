package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

// PruneChatTurnsExcludedFromContext removes failed user turns that have no
// visible assistant output or Tool result. Reasoning and the transient
// interruption marker are removed with the user message.
func (s *Store) PruneChatTurnsExcludedFromContext(ctx context.Context, sessionID string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	statement, arguments := excludedChatTurnsQuery(sessionID)
	rows, err := tx.QueryContext(ctx, statement, arguments...)
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
	changedSessions := make(map[string]struct{}, len(turns))
	for _, turn := range turns {
		changedSessions[turn.sessionID] = struct{}{}
	}
	for changedSessionID := range changedSessions {
		s.publishSessionChange(changedSessionID, false)
	}
	return len(turns), nil
}

func excludedChatTurnsQuery(sessionID string) (string, []any) {
	filter := "users.role='user' AND users.status='failed'"
	var arguments []any
	if sessionID != "" {
		filter += " AND users.session_id=?"
		arguments = append(arguments, sessionID)
	}
	return `
SELECT users.session_id, users.rowid,
  COALESCE((SELECT min(next_user.rowid) FROM chat_messages AS next_user
    WHERE next_user.session_id=users.session_id AND next_user.role='user' AND next_user.rowid>users.rowid),0)
FROM chat_messages AS users
WHERE ` + filter + `
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
)`, append(arguments, domain.AgentInterruptedMessage)
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
	s.publishChange(Change{Topic: ChangeChatState, SessionID: summary.SessionID})
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
	s.publishChange(Change{Topic: ChangeChatState, SessionID: sessionID})
	return nil
}
