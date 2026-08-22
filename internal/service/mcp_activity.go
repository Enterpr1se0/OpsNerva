package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/ids"
	"eino-ops-agent/internal/observability"
	"eino-ops-agent/internal/store"
)

const maxMCPAuditArgumentsBytes = 64 << 10

type mcpActivitySubscriber struct {
	sessionID string
	events    chan domain.MCPActivityEvent
}

func (s *Service) BeginMCPToolCall(ctx context.Context, session domain.MCPClientSession, toolName, arguments string) (context.Context, domain.MCPToolCall, error) {
	now := time.Now().UTC()
	session.ID = strings.TrimSpace(session.ID)
	session.Transport = strings.TrimSpace(session.Transport)
	session.ClientName = boundedMCPMetadata(session.ClientName, 256)
	session.ClientVersion = boundedMCPMetadata(session.ClientVersion, 256)
	session.ProtocolVersion = boundedMCPMetadata(session.ProtocolVersion, 64)
	if session.ID == "" || session.Transport == "" {
		return ctx, domain.MCPToolCall{}, errors.New("MCP session id and transport are required")
	}
	session.StartedAt, session.LastSeenAt = now, now
	argumentsJSON, err := s.mcpAuditArguments(arguments)
	if err != nil {
		return ctx, domain.MCPToolCall{}, err
	}
	call := domain.MCPToolCall{
		ID: ids.New("mcp_call"), SessionID: session.ID, ToolName: strings.TrimSpace(toolName),
		ArgumentsJSON: argumentsJSON, Status: domain.MCPCallRunning,
		StartedAt: now, UpdatedAt: now,
	}
	if call.ToolName == "" {
		return ctx, domain.MCPToolCall{}, errors.New("MCP tool name is required")
	}
	if err := s.store.StartMCPToolCall(ctx, session, call); err != nil {
		return ctx, domain.MCPToolCall{}, err
	}
	s.audit(ctx, "", "mcp_tool_call_started", "mcp-client", map[string]any{
		"session_id": session.ID, "call_id": call.ID, "tool_name": call.ToolName,
		"transport": session.Transport, "client_name": session.ClientName,
	})
	s.publishMCPActivity(domain.MCPActivityEvent{
		Type: "call_started", SessionID: session.ID, CallID: call.ID, Session: &session, Call: &call, Status: call.Status,
	})
	return WithMCPToolCall(ctx, session.ID, call.ID, call.ToolName, call.ArgumentsJSON), call, nil
}

func (s *Service) FinishMCPToolCall(ctx context.Context, call domain.MCPToolCall) error {
	call.ID = strings.TrimSpace(call.ID)
	call.SessionID = strings.TrimSpace(call.SessionID)
	call.Error = boundedMCPMetadata(s.redactor.Redact(call.Error), 4096)
	if call.Status == "" {
		call.Status = domain.MCPCallCompleted
	}
	now := time.Now().UTC()
	call.UpdatedAt, call.CompletedAt = now, now
	if err := s.store.FinishMCPToolCall(context.WithoutCancel(ctx), call); err != nil {
		return err
	}
	s.audit(ctx, call.RunID, "mcp_tool_call_"+call.Status, "mcp-client", map[string]any{
		"session_id": call.SessionID, "call_id": call.ID, "tool_name": call.ToolName,
		"run_id": call.RunID, "approval_id": call.ApprovalID, "task_id": call.TaskID,
		"shell_id": call.ShellID, "tunnel_id": call.TunnelID, "operation_status": call.OperationStatus,
	})
	s.publishMCPActivity(domain.MCPActivityEvent{
		Type: "call_finished", SessionID: call.SessionID, CallID: call.ID, Call: &call, Status: call.Status,
	})
	return nil
}

func (s *Service) ListMCPActivity(ctx context.Context, sessionID string, sessionLimit, callLimit int) (domain.MCPActivitySnapshot, error) {
	sessions, err := s.store.ListMCPClientSessions(ctx, sessionLimit)
	if err != nil {
		return domain.MCPActivitySnapshot{}, err
	}
	snapshot := domain.MCPActivitySnapshot{Sessions: sessions, Calls: []domain.MCPToolCall{}}
	if strings.TrimSpace(sessionID) == "" {
		return snapshot, nil
	}
	calls, err := s.store.ListMCPToolCalls(ctx, sessionID, callLimit)
	if err != nil {
		return domain.MCPActivitySnapshot{}, err
	}
	snapshot.Calls = calls
	return snapshot, nil
}

func (s *Service) SubscribeMCPActivity(sessionID string) (<-chan domain.MCPActivityEvent, func()) {
	subscriber := &mcpActivitySubscriber{sessionID: strings.TrimSpace(sessionID), events: make(chan domain.MCPActivityEvent, 512)}
	s.mcpActivityMu.Lock()
	s.mcpActivitySubscriberID++
	id := s.mcpActivitySubscriberID
	s.mcpActivitySubscribers[id] = subscriber
	s.mcpActivityMu.Unlock()
	return subscriber.events, func() {
		s.mcpActivityMu.Lock()
		if current, ok := s.mcpActivitySubscribers[id]; ok {
			delete(s.mcpActivitySubscribers, id)
			close(current.events)
		}
		s.mcpActivityMu.Unlock()
	}
}

func (s *Service) publishMCPActivity(event domain.MCPActivityEvent) {
	if event.SessionID == "" || event.CallID == "" {
		return
	}
	event.Sequence = s.mcpActivitySequence.Add(1)
	s.mcpActivityMu.Lock()
	for id, subscriber := range s.mcpActivitySubscribers {
		if subscriber.sessionID != "" && subscriber.sessionID != event.SessionID {
			continue
		}
		select {
		case subscriber.events <- event:
		default:
			delete(s.mcpActivitySubscribers, id)
			close(subscriber.events)
		}
	}
	s.mcpActivityMu.Unlock()
}

func (s *Service) publishMCPExecutionEvent(event ExecutionEvent) {
	if event.ToolCallID == "" {
		return
	}
	if event.Content == "" && event.Stream == "" {
		runID, shellID := event.RunID, ""
		if strings.HasPrefix(runID, "shell_") {
			shellID, runID = runID, ""
		}
		persistCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := s.store.BindMCPToolCallOperation(persistCtx, event.ToolCallID, runID, shellID, event.Status)
		cancel()
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			observability.FromContext(context.Background()).ErrorContext(context.Background(), "persist MCP execution link failed",
				"component", "mcp_server", "call_id", event.ToolCallID, "run_id", event.RunID, "error", err)
		}
	}
	eventType := "call_output"
	if event.Stream == "progress" {
		eventType = "call_progress"
	} else if event.Content == "" {
		eventType = "operation_status"
	}
	s.publishMCPActivity(domain.MCPActivityEvent{
		Type: eventType, SessionID: event.SessionID, CallID: event.ToolCallID, RunID: event.RunID,
		Stream: event.Stream, Content: event.Content, Status: event.Status,
		TransferredBytes: event.TransferredBytes, TotalBytes: event.TotalBytes,
	})
}

func (s *Service) mcpAuditArguments(arguments string) (string, error) {
	arguments = strings.TrimSpace(arguments)
	if arguments == "" {
		return "{}", nil
	}
	var value any
	if err := json.Unmarshal([]byte(arguments), &value); err != nil {
		return "", errors.New("MCP tool arguments must be valid JSON")
	}
	redacted, err := json.Marshal(s.redactMCPArgumentValue("", value))
	if err != nil {
		return "", err
	}
	if len(redacted) <= maxMCPAuditArgumentsBytes {
		return string(redacted), nil
	}
	preview := strings.ToValidUTF8(string(redacted[:maxMCPAuditArgumentsBytes]), "�")
	encoded, _ := json.Marshal(map[string]any{"preview": preview, "truncated": true})
	return string(encoded), nil
}

func (s *Service) redactMCPArgumentValue(key string, value any) any {
	if sensitiveMCPArgumentKey(key) {
		return "[REDACTED]"
	}
	switch typed := value.(type) {
	case string:
		return s.redactor.Redact(typed)
	case []any:
		for index := range typed {
			typed[index] = s.redactMCPArgumentValue("", typed[index])
		}
	case map[string]any:
		for childKey, childValue := range typed {
			typed[childKey] = s.redactMCPArgumentValue(childKey, childValue)
		}
	}
	return value
}

func sensitiveMCPArgumentKey(key string) bool {
	normalized := strings.Map(func(value rune) rune {
		if value >= 'A' && value <= 'Z' {
			return value + ('a' - 'A')
		}
		if value >= 'a' && value <= 'z' || value >= '0' && value <= '9' {
			return value
		}
		return -1
	}, key)
	for _, marker := range []string{"authorization", "cookie", "credential", "passphrase", "password", "passwd", "privatekey", "apikey", "accesstoken", "clientsecret", "secret"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func boundedMCPMetadata(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) > limit {
		return value[:limit]
	}
	return value
}
