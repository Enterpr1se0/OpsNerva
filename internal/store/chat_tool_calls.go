package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/ids"
)

func chatToolCallTerminal(status string) bool {
	switch status {
	case domain.ChatToolCallCompleted, domain.ChatToolCallPartial, domain.ChatToolCallFailed,
		domain.ChatToolCallInterrupted, domain.ChatToolCallRejected, domain.ChatToolCallExpired,
		domain.ChatToolCallUnknown:
		return true
	default:
		return false
	}
}

func validateChatToolCallStatus(status string) error {
	if status == domain.ChatToolCallRunning || chatToolCallTerminal(status) {
		return nil
	}
	return fmt.Errorf("invalid chat tool call status %q", status)
}

func runningToolContent(toolName, arguments string) string {
	var parsed any
	if json.Unmarshal([]byte(arguments), &parsed) != nil {
		parsed = arguments
	}
	payload, _ := json.Marshal(map[string]any{
		"status": domain.ChatToolCallRunning,
		"_display": map[string]any{
			"tool_name": toolName,
			"arguments": parsed,
		},
	})
	return string(payload)
}

func toolContentWithLifecycle(content, status, runID, errorText string) string {
	var payload map[string]any
	if json.Unmarshal([]byte(content), &payload) != nil || payload == nil {
		payload = map[string]any{"result": content}
	}
	payload["status"] = status
	if runID != "" {
		payload["run_id"] = runID
	}
	if errorText != "" {
		payload["error"] = errorText
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return content
	}
	return string(encoded)
}

func preserveToolDisplay(previous, content string) string {
	var oldPayload, payload map[string]any
	if json.Unmarshal([]byte(previous), &oldPayload) != nil || json.Unmarshal([]byte(content), &payload) != nil || payload == nil {
		return content
	}
	if _, exists := payload["_display"]; exists {
		return content
	}
	if display, exists := oldPayload["_display"]; exists {
		payload["_display"] = display
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return content
	}
	return string(encoded)
}

func toolContentWithRunID(content, runID string) string {
	if runID == "" {
		return content
	}
	var payload map[string]any
	if json.Unmarshal([]byte(content), &payload) != nil || payload == nil {
		return content
	}
	payload["run_id"] = runID
	encoded, err := json.Marshal(payload)
	if err != nil {
		return content
	}
	return string(encoded)
}

func unknownToolContent(runID string) string {
	return toolContentWithLifecycle(`{"ok":false,"code":"outcome_unknown"}`, domain.ChatToolCallUnknown, runID, "")
}

func (s *Store) StartChatToolCall(ctx context.Context, call domain.ChatToolCall) (domain.ChatToolCall, error) {
	call.SessionID = strings.TrimSpace(call.SessionID)
	call.UserMessageID = strings.TrimSpace(call.UserMessageID)
	call.ToolCallID = strings.TrimSpace(call.ToolCallID)
	call.ToolName = strings.TrimSpace(call.ToolName)
	if call.SessionID == "" || call.UserMessageID == "" || call.ToolCallID == "" || call.ToolName == "" {
		return domain.ChatToolCall{}, fmt.Errorf("session_id, user_message_id, tool_call_id, and tool_name are required")
	}
	if strings.TrimSpace(call.ArgumentsJSON) == "" {
		call.ArgumentsJSON = "{}"
	}
	now := time.Now().UTC()
	call.MessageID = ids.New("msg")
	call.Status = domain.ChatToolCallRunning
	if strings.TrimSpace(call.ResultJSON) == "" {
		call.ResultJSON = runningToolContent(call.ToolName, call.ArgumentsJSON)
	}
	call.StartedAt = now
	call.UpdatedAt = now

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.ChatToolCall{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO chat_sessions(session_id,workspace_id,created_at,updated_at) VALUES(?,?,?,?)`, call.SessionID, "", formatTime(now), formatTime(now)); err != nil {
		return domain.ChatToolCall{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO chat_messages(id,session_id,role,content,tool_name,status,created_at)
VALUES(?,?,?,?,?,'completed',?)`, call.MessageID, call.SessionID, "tool", call.ResultJSON, call.ToolName, formatTime(now)); err != nil {
		return domain.ChatToolCall{}, err
	}
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO chat_tool_calls(
session_id,user_message_id,message_id,tool_call_id,run_id,tool_name,arguments_json,status,result_json,error,started_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, call.SessionID, call.UserMessageID, call.MessageID, call.ToolCallID, "", call.ToolName,
		call.ArgumentsJSON, call.Status, call.ResultJSON, "", formatTime(now), formatTime(now))
	if err != nil {
		return domain.ChatToolCall{}, err
	}
	inserted, _ := result.RowsAffected()
	if inserted == 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM chat_messages WHERE id=?`, call.MessageID); err != nil {
			return domain.ChatToolCall{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chat_sessions SET updated_at=? WHERE session_id=?`, formatTime(now), call.SessionID); err != nil {
		return domain.ChatToolCall{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.ChatToolCall{}, err
	}
	return s.GetChatToolCall(ctx, call.SessionID, call.ToolCallID)
}

func (s *Store) BindChatToolCallRun(ctx context.Context, sessionID, toolCallID, runID string) error {
	sessionID, toolCallID, runID = strings.TrimSpace(sessionID), strings.TrimSpace(toolCallID), strings.TrimSpace(runID)
	if sessionID == "" || toolCallID == "" || runID == "" {
		return fmt.Errorf("session_id, tool_call_id, and run_id are required")
	}
	call, err := s.GetChatToolCall(ctx, sessionID, toolCallID)
	if err != nil {
		return err
	}
	content := toolContentWithRunID(call.ResultJSON, runID)
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE chat_tool_calls SET run_id=?,result_json=?,updated_at=?
WHERE session_id=? AND tool_call_id=? AND (run_id='' OR run_id=?)`, runID, content, formatTime(now), sessionID, toolCallID, runID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chat_messages SET content=? WHERE id=?`, content, call.MessageID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FinishChatToolCall(ctx context.Context, sessionID, toolCallID, runID, status, content, errorText string) (domain.ChatToolCall, error) {
	if err := validateChatToolCallStatus(status); err != nil || !chatToolCallTerminal(status) {
		if err != nil {
			return domain.ChatToolCall{}, err
		}
		return domain.ChatToolCall{}, fmt.Errorf("chat tool call status %q is not terminal", status)
	}
	call, err := s.GetChatToolCall(ctx, sessionID, toolCallID)
	if err != nil {
		return domain.ChatToolCall{}, err
	}
	if chatToolCallTerminal(call.Status) && call.Status != status {
		return call, nil
	}
	if runID != "" && call.RunID != "" && call.RunID != runID {
		return domain.ChatToolCall{}, fmt.Errorf("tool_call_id %q is already bound to run_id %q", toolCallID, call.RunID)
	}
	if call.RunID == "" {
		call.RunID = runID
	}
	if strings.TrimSpace(content) == "" {
		if status == domain.ChatToolCallUnknown {
			content = unknownToolContent(call.RunID)
		} else {
			content = call.ResultJSON
		}
	}
	content = preserveToolDisplay(call.ResultJSON, content)
	content = toolContentWithLifecycle(content, status, call.RunID, errorText)
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.ChatToolCall{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE chat_tool_calls SET run_id=?,status=?,result_json=?,error=?,updated_at=?,completed_at=?
WHERE session_id=? AND tool_call_id=? AND (run_id='' OR run_id=?)`, call.RunID, status, content, errorText,
		formatTime(now), formatTime(now), sessionID, toolCallID, call.RunID)
	if err != nil {
		return domain.ChatToolCall{}, err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return domain.ChatToolCall{}, ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chat_messages SET content=?,tool_name=?,status='completed' WHERE id=?`, content, call.ToolName, call.MessageID); err != nil {
		return domain.ChatToolCall{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chat_sessions SET updated_at=? WHERE session_id=?`, formatTime(now), sessionID); err != nil {
		return domain.ChatToolCall{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.ChatToolCall{}, err
	}
	return s.GetChatToolCall(ctx, sessionID, toolCallID)
}

func (s *Store) UpdateChatToolCallByRun(ctx context.Context, runID, status, content, errorText string) (domain.ChatToolCall, error) {
	call, err := s.GetChatToolCallByRun(ctx, runID)
	if err != nil {
		return domain.ChatToolCall{}, err
	}
	if chatToolCallTerminal(call.Status) {
		return call, nil
	}
	if !chatToolCallTerminal(status) {
		return call, nil
	}
	return s.FinishChatToolCall(ctx, call.SessionID, call.ToolCallID, runID, status, content, errorText)
}

func (s *Store) MarkChatToolCallUnknown(ctx context.Context, sessionID, toolCallID string) (domain.ChatToolCall, error) {
	return s.FinishChatToolCall(ctx, sessionID, toolCallID, "", domain.ChatToolCallUnknown, "", "")
}

func (s *Store) GetChatToolCall(ctx context.Context, sessionID, toolCallID string) (domain.ChatToolCall, error) {
	return scanChatToolCall(s.db.QueryRowContext(ctx, `SELECT session_id,user_message_id,message_id,tool_call_id,run_id,tool_name,
arguments_json,status,result_json,error,started_at,updated_at,completed_at
FROM chat_tool_calls WHERE session_id=? AND tool_call_id=?`, sessionID, toolCallID))
}

func (s *Store) GetChatToolCallByRun(ctx context.Context, runID string) (domain.ChatToolCall, error) {
	return scanChatToolCall(s.db.QueryRowContext(ctx, `SELECT session_id,user_message_id,message_id,tool_call_id,run_id,tool_name,
arguments_json,status,result_json,error,started_at,updated_at,completed_at
FROM chat_tool_calls WHERE run_id=?`, runID))
}

func (s *Store) ListChatToolCalls(ctx context.Context, sessionID string) ([]domain.ChatToolCall, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session_id,user_message_id,message_id,tool_call_id,run_id,tool_name,
arguments_json,status,result_json,error,started_at,updated_at,completed_at
FROM chat_tool_calls WHERE session_id=? ORDER BY started_at,message_id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.ChatToolCall, 0)
	for rows.Next() {
		call, err := scanChatToolCall(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, call)
	}
	return result, rows.Err()
}

// CountRunningChatToolCalls keeps the frequently-polled chat state response
// independent from potentially very large persisted tool results.
func (s *Store) CountRunningChatToolCalls(ctx context.Context, sessionID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chat_tool_calls WHERE session_id=? AND status=?`,
		sessionID, domain.ChatToolCallRunning).Scan(&count)
	return count, err
}

func (s *Store) ListRunningChatToolCalls(ctx context.Context) ([]domain.ChatToolCall, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session_id,user_message_id,message_id,tool_call_id,run_id,tool_name,
arguments_json,status,result_json,error,started_at,updated_at,completed_at
FROM chat_tool_calls WHERE status=? ORDER BY started_at,message_id`, domain.ChatToolCallRunning)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.ChatToolCall, 0)
	for rows.Next() {
		call, err := scanChatToolCall(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, call)
	}
	return result, rows.Err()
}

func scanChatToolCall(row scanner) (domain.ChatToolCall, error) {
	var call domain.ChatToolCall
	var started, updated string
	var completed sql.NullString
	err := row.Scan(&call.SessionID, &call.UserMessageID, &call.MessageID, &call.ToolCallID, &call.RunID,
		&call.ToolName, &call.ArgumentsJSON, &call.Status, &call.ResultJSON, &call.Error, &started, &updated, &completed)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ChatToolCall{}, ErrNotFound
	}
	if err != nil {
		return domain.ChatToolCall{}, err
	}
	call.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
	call.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	if completed.Valid {
		call.CompletedAt, _ = time.Parse(time.RFC3339Nano, completed.String)
	}
	return call, nil
}

func (s *Store) loadChatToolMessageState(ctx context.Context, messages []domain.ChatMessage) error {
	if len(messages) == 0 {
		return nil
	}
	byID := make(map[string]*domain.ChatMessage, len(messages))
	for index := range messages {
		if messages[index].Role == "tool" {
			byID[messages[index].ID] = &messages[index]
		}
	}
	if len(byID) == 0 {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT message_id,tool_call_id,run_id,status,arguments_json FROM chat_tool_calls WHERE message_id IN (`+sqlPlaceholders(len(byID))+`)`, mapKeys(byID)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var messageID, toolCallID, runID, status, argumentsJSON string
		if err := rows.Scan(&messageID, &toolCallID, &runID, &status, &argumentsJSON); err != nil {
			return err
		}
		if message := byID[messageID]; message != nil {
			message.ToolCallID = toolCallID
			message.ToolArguments = argumentsJSON
			message.RunID = runID
			message.ToolStatus = status
		}
	}
	return rows.Err()
}

func sqlPlaceholders(count int) string {
	if count <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func mapKeys(values map[string]*domain.ChatMessage) []any {
	result := make([]any, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	return result
}
