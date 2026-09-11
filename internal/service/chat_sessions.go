package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/store"
)

func (s *Service) ListChatSessions(ctx context.Context, limit int) ([]domain.ChatSession, error) {
	return s.store.ListChatSessions(ctx, limit)
}

func (s *Service) PrepareChatSession(ctx context.Context, sessionID, workspaceID, actor string) (domain.ChatSession, error) {
	sessionID = strings.TrimSpace(sessionID)
	workspaceID = strings.TrimSpace(workspaceID)
	if sessionID == "" {
		return domain.ChatSession{}, fmt.Errorf("session id is required")
	}
	if workspaceID != "" {
		if _, ok := s.workspaceByID(workspaceID); !ok {
			return domain.ChatSession{}, fmt.Errorf("workspace %q not found", workspaceID)
		}
	}
	session, err := s.store.GetChatSession(ctx, sessionID)
	if errors.Is(err, store.ErrNotFound) {
		session, err = s.store.CreateChatSession(ctx, sessionID, workspaceID)
		if err == nil {
			s.audit(ctx, "", "chat_session_created", actor, map[string]any{"session_id": sessionID, "workspace_id": workspaceID})
		}
		return session, err
	}
	if err != nil {
		return domain.ChatSession{}, err
	}
	if workspaceID == "" || session.WorkspaceID == workspaceID {
		return session, nil
	}
	if session.WorkspaceID != "" {
		return domain.ChatSession{}, fmt.Errorf("conversation is bound to workspace %q; switch it before sending a message", session.WorkspaceID)
	}
	return s.SetChatSessionWorkspace(ctx, sessionID, workspaceID, actor)
}

func (s *Service) GetChatSession(ctx context.Context, sessionID string) (domain.ChatSession, error) {
	return s.store.GetChatSession(ctx, strings.TrimSpace(sessionID))
}

func (s *Service) GetChatContextSummary(ctx context.Context, sessionID string) (domain.ChatContextSummary, error) {
	return s.store.GetChatContextSummary(ctx, strings.TrimSpace(sessionID))
}

const maxChatSessionTitleRunes = 80

func normalizeChatSessionTitle(title string) (string, error) {
	title = strings.Join(strings.Fields(title), " ")
	if title == "" {
		return "", fmt.Errorf("conversation title is required")
	}
	if len([]rune(title)) > maxChatSessionTitleRunes {
		return "", fmt.Errorf("conversation title must not exceed %d characters", maxChatSessionTitleRunes)
	}
	return title, nil
}

func (s *Service) RenameChatSession(ctx context.Context, sessionID, title, actor string) (domain.ChatSession, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return domain.ChatSession{}, fmt.Errorf("session id is required")
	}
	title, err := normalizeChatSessionTitle(title)
	if err != nil {
		return domain.ChatSession{}, err
	}
	current, err := s.store.GetChatSession(ctx, sessionID)
	if err != nil {
		return domain.ChatSession{}, err
	}
	if current.TitleSet && current.Title == title {
		return current, nil
	}
	session, err := s.store.SetChatSessionTitle(ctx, sessionID, title)
	if err != nil {
		return domain.ChatSession{}, err
	}
	s.audit(ctx, "", "chat_session_renamed", actor, map[string]any{"session_id": sessionID, "title": title})
	return session, nil
}

func (s *Service) SetGeneratedChatSessionTitle(ctx context.Context, sessionID, title string) (domain.ChatSession, bool, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return domain.ChatSession{}, false, fmt.Errorf("session id is required")
	}
	title, err := normalizeChatSessionTitle(title)
	if err != nil {
		return domain.ChatSession{}, false, err
	}
	session, changed, err := s.store.SetChatSessionTitleIfEmpty(ctx, sessionID, title)
	if err != nil || !changed {
		return session, changed, err
	}
	s.audit(ctx, "", "chat_session_title_generated", "agent", map[string]any{"session_id": sessionID, "title": title})
	return session, true, nil
}

func (s *Service) SetChatSessionWorkspace(ctx context.Context, sessionID, workspaceID, actor string) (domain.ChatSession, error) {
	sessionID = strings.TrimSpace(sessionID)
	workspaceID = strings.TrimSpace(workspaceID)
	if sessionID == "" {
		return domain.ChatSession{}, fmt.Errorf("session id is required")
	}
	if workspaceID != "" {
		if _, ok := s.workspaceByID(workspaceID); !ok {
			return domain.ChatSession{}, fmt.Errorf("workspace %q not found", workspaceID)
		}
	}
	current, err := s.store.GetChatSession(ctx, sessionID)
	if err != nil {
		return domain.ChatSession{}, err
	}
	if current.WorkspaceID == workspaceID {
		return current, nil
	}
	if s.hasActiveWorkspaceShellForSession(sessionID) {
		return domain.ChatSession{}, fmt.Errorf("conversation %q has an active Workspace terminal", sessionID)
	}
	session, err := s.store.SetChatSessionWorkspace(ctx, sessionID, workspaceID)
	if err != nil {
		return domain.ChatSession{}, err
	}
	s.audit(ctx, "", "chat_session_workspace_changed", actor, map[string]any{
		"session_id": sessionID, "previous_workspace_id": current.WorkspaceID, "workspace_id": workspaceID,
	})
	return session, nil
}

func (s *Service) SessionWorkspace(ctx context.Context) (WorkspaceCapability, error) {
	sessionID := SessionIDFromContext(ctx)
	if sessionID == "" {
		return WorkspaceCapability{}, fmt.Errorf("Workspace tools require an Agent conversation")
	}
	session, err := s.store.GetChatSession(ctx, sessionID)
	if err != nil {
		return WorkspaceCapability{}, fmt.Errorf("load conversation Workspace: %w", err)
	}
	if session.WorkspaceID == "" {
		return WorkspaceCapability{}, fmt.Errorf("no Workspace is bound to this conversation; select one in the chat interface")
	}
	workspace, ok := s.workspaceByID(session.WorkspaceID)
	if !ok {
		return WorkspaceCapability{}, fmt.Errorf("the conversation Workspace %q is no longer available; select another Workspace", session.WorkspaceID)
	}
	return s.adminWorkspaceCapability(workspace).WorkspaceCapability, nil
}

func (s *Service) ListChatMessages(ctx context.Context, sessionID string, limit int) ([]domain.ChatMessage, error) {
	return s.store.ListChatMessages(ctx, sessionID, limit)
}

func (s *Service) ListChatMessagesPage(ctx context.Context, sessionID string, limit int, beforeCreatedAt, beforeID string) (domain.ChatMessagePage, error) {
	return s.store.ListChatMessagesPage(ctx, strings.TrimSpace(sessionID), limit, strings.TrimSpace(beforeCreatedAt), strings.TrimSpace(beforeID))
}

func (s *Service) GetChatMessage(ctx context.Context, sessionID, messageID string) (domain.ChatMessage, error) {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(messageID) == "" {
		return domain.ChatMessage{}, store.ErrNotFound
	}
	return s.store.GetChatMessage(ctx, strings.TrimSpace(sessionID), strings.TrimSpace(messageID))
}

func (s *Service) ListChatToolCalls(ctx context.Context, sessionID string) ([]domain.ChatToolCall, error) {
	return s.store.ListChatToolCalls(ctx, strings.TrimSpace(sessionID))
}

func (s *Service) CountRunningChatToolCalls(ctx context.Context, sessionID string) (int, error) {
	return s.store.CountRunningChatToolCalls(ctx, strings.TrimSpace(sessionID))
}

func (s *Service) GetChatAttachment(ctx context.Context, sessionID, attachmentID string) (domain.ChatAttachment, error) {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(attachmentID) == "" {
		return domain.ChatAttachment{}, store.ErrNotFound
	}
	return s.store.GetChatAttachment(ctx, sessionID, attachmentID)
}

func (s *Service) DeleteChatSession(ctx context.Context, sessionID string, actors ...string) error {
	if calls, err := s.store.ListChatToolCalls(ctx, sessionID); err != nil {
		return err
	} else {
		for _, call := range calls {
			if call.Status == domain.ChatToolCallRunning || call.Status == domain.ChatToolCallApprovalRequired {
				return fmt.Errorf("conversation %q has an active function tool; stop it before deleting the conversation", sessionID)
			}
		}
	}
	if s.hasActiveSSHShellForSession(sessionID) {
		return fmt.Errorf("conversation %q has an active terminal; close it before deleting the conversation", sessionID)
	}
	if s.hasActiveTaskForSession(sessionID) {
		return fmt.Errorf("conversation %q has an active background task; cancel it before deleting the conversation", sessionID)
	}
	if err := s.store.DeleteChatSession(ctx, sessionID); err != nil {
		return err
	}
	actor := ""
	if len(actors) > 0 {
		actor = actors[0]
	}
	s.audit(ctx, "", "chat_session_deleted", actor, map[string]any{"session_id": sessionID})
	return nil
}
