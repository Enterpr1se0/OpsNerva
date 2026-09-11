package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

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
