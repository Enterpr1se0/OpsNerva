package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/ids"
)

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
	s.publishSessionChange(sessionID, false)
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
	var cursorRowID int64
	if beforeCreatedAt != "" && beforeID != "" {
		if _, err := time.Parse(time.RFC3339Nano, beforeCreatedAt); err != nil {
			return domain.ChatMessagePage{}, fmt.Errorf("invalid chat message cursor time: %w", err)
		}
		err := s.db.QueryRowContext(ctx, `SELECT rowid FROM chat_messages WHERE session_id=? AND id=? AND created_at=?`, sessionID, beforeID, beforeCreatedAt).Scan(&cursorRowID)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ChatMessagePage{}, ErrNotFound
		}
		if err != nil {
			return domain.ChatMessagePage{}, err
		}
	}
	statement, args := chatMessagesPageQuery(sessionID, limit, beforeCreatedAt, cursorRowID)
	rows, err := s.db.QueryContext(ctx, statement, args...)
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

// chatMessagesPageQuery keeps both cursor ranges index-seekable. SQLite does
// not seek the implicit rowid tie-breaker for a (created_at,rowid) tuple.
// Merge at most two pages of rowids, then load only the selected messages.
func chatMessagesPageQuery(sessionID string, limit int, beforeCreatedAt string, cursorRowID int64) (string, []any) {
	where := "session_id=?"
	whereArgs := []any{sessionID}
	if beforeCreatedAt != "" {
		where = `rowid IN (
SELECT rowid FROM (
  SELECT rowid FROM chat_messages WHERE session_id=? AND created_at=? AND rowid<?
  ORDER BY rowid DESC LIMIT ?
)
UNION ALL
SELECT rowid FROM (
  SELECT rowid FROM chat_messages WHERE session_id=? AND created_at<?
  ORDER BY created_at DESC,rowid DESC LIMIT ?
))`
		whereArgs = []any{sessionID, beforeCreatedAt, cursorRowID, limit + 1, sessionID, beforeCreatedAt, limit + 1}
	}
	args := []any{maxChatToolMessagePreviewChars, maxChatToolMessagePreviewChars}
	args = append(args, whereArgs...)
	args = append(args, limit+1)
	return `SELECT id,role,
CASE WHEN role='tool' AND length(content)>? THEN substr(content,1,?) ELSE content END,
length(content),model_extra_json,tool_name,status,created_at
FROM chat_messages WHERE ` + where + ` ORDER BY created_at DESC,rowid DESC LIMIT ?`, args
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
	if message.ToolName == "ssh_task" {
		var arguments struct {
			Action string `json:"action"`
			TaskID string `json:"task_id"`
		}
		if json.Unmarshal([]byte(message.ToolArguments), &arguments) == nil && arguments.Action == "status" && arguments.TaskID != "" {
			value["task_id"] = arguments.TaskID
			value["_display"] = map[string]any{"arguments": map[string]string{"action": arguments.Action, "task_id": arguments.TaskID}}
		}
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

func (s *Store) listChatMessages(ctx context.Context, sessionID string, limit int, modelOnly bool) ([]domain.ChatMessage, error) {
	filter := ""
	if modelOnly {
		filter = " AND role IN ('user','assistant') AND status='completed'"
	}
	query := `SELECT id,role,content,model_extra_json,tool_name,status,created_at FROM chat_messages WHERE session_id=?` + filter + ` ORDER BY rowid`
	args := []any{sessionID}
	if limit > 0 {
		query = `SELECT id,role,content,model_extra_json,tool_name,status,created_at FROM (
SELECT id,role,content,model_extra_json,tool_name,status,created_at,rowid AS message_sequence FROM chat_messages WHERE session_id=?` + filter + ` ORDER BY rowid DESC LIMIT ?)
ORDER BY message_sequence`
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
