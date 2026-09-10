package httpapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/agent"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/store"
)

func (s *Server) chatSessionActive(sessionID string) bool {
	return s.chatQueue.active(sessionID) || s.agent != nil && s.agent.IsSessionActive(sessionID)
}

func (s *Server) chatMessages(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	result, err := s.service.ListChatMessagesPage(r.Context(), r.PathValue("id"), limit,
		r.URL.Query().Get("before_created_at"), r.URL.Query().Get("before_id"))
	respond(w, result, err)
}

func (s *Server) chatMessage(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.GetChatMessage(r.Context(), r.PathValue("id"), r.PathValue("message_id"))
	if errors.Is(err, store.ErrNotFound) {
		writeErrorStatus(w, err, http.StatusNotFound)
		return
	}
	respond(w, result, err)
}

func (s *Server) chatAttachment(w http.ResponseWriter, r *http.Request) {
	attachment, err := s.service.GetChatAttachment(r.Context(), r.PathValue("id"), r.PathValue("attachment_id"))
	if errors.Is(err, store.ErrNotFound) {
		writeErrorStatus(w, err, http.StatusNotFound)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", attachment.MIMEType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": attachment.Name}))
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, attachment.Name, time.Time{}, bytes.NewReader(attachment.Data))
}

func (s *Server) chatState(w http.ResponseWriter, r *http.Request) {
	state, err := s.applicationChatState(r.Context(), r.PathValue("id"), true)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) applicationChatState(ctx context.Context, sessionID string, includeMessages bool) (map[string]any, error) {
	session, err := s.service.GetChatSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	runningToolCalls, err := s.service.CountRunningChatToolCalls(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	plan, planErr := s.service.GetAgentPlan(ctx, sessionID)
	var currentPlan *domain.AgentPlan
	if planErr == nil {
		currentPlan = &plan
	}
	if planErr != nil && !errors.Is(planErr, store.ErrNotFound) {
		return nil, planErr
	}
	active := s.chatSessionActive(sessionID)
	contextSummary, summaryErr := s.service.GetChatContextSummary(ctx, sessionID)
	if summaryErr != nil && !errors.Is(summaryErr, store.ErrNotFound) {
		return nil, summaryErr
	}
	state := map[string]any{
		"active": active, "workspace_id": session.WorkspaceID,
		"context_tokens": session.ContextTokens, "context_window": session.ContextWindow,
		"running_tool_calls": runningToolCalls, "plan": currentPlan,
		"queued_messages": s.chatQueue.snapshot(sessionID),
	}
	if includeMessages {
		messagePage, messageErr := s.service.ListChatMessagesPage(ctx, sessionID, 100, "", "")
		if messageErr != nil {
			return nil, messageErr
		}
		state["messages"] = messagePage.Messages
		state["messages_has_more"] = messagePage.HasMore
		state["messages_next_created_at"] = messagePage.NextCreatedAt
		state["messages_next_id"] = messagePage.NextID
	}
	if summaryErr == nil {
		state["context_summary"] = contextSummary
	}
	return state, nil
}

func (s *Server) compressChatContext(w http.ResponseWriter, r *http.Request) {
	if s.agent == nil || !s.agent.Available() {
		writeErrorStatus(w, agent.ErrUnavailable, http.StatusServiceUnavailable)
		return
	}
	if s.chatSessionActive(r.PathValue("id")) {
		writeErrorStatus(w, agent.ErrSessionBusy, http.StatusConflict)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	result, err := s.agent.CompressContext(ctx, strings.TrimSpace(r.PathValue("id")))
	switch {
	case errors.Is(err, agent.ErrSessionBusy), errors.Is(err, agent.ErrNothingToCompress):
		writeErrorStatus(w, err, http.StatusConflict)
	case errors.Is(err, store.ErrNotFound):
		writeErrorStatus(w, err, http.StatusNotFound)
	case err != nil:
		writeError(w, err)
	default:
		writeJSON(w, http.StatusOK, result)
	}
}

func (s *Server) chatSessions(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	result, err := s.service.ListChatSessions(r.Context(), limit)
	if err != nil {
		writeError(w, err)
		return
	}
	if s.agent != nil {
		for index := range result {
			result[index].Active = s.chatSessionActive(result[index].ID)
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) cancelChatSession(w http.ResponseWriter, r *http.Request) {
	if s.agent == nil {
		writeErrorStatus(w, agent.ErrUnavailable, http.StatusServiceUnavailable)
		return
	}
	sessionID := strings.TrimSpace(r.PathValue("id"))
	if sessionID == "" {
		writeErrorStatus(w, fmt.Errorf("session id is required"), http.StatusBadRequest)
		return
	}
	cancelled := s.agent.CancelSession(sessionID)
	cancelledQueued, cancelledDriver := s.chatQueue.clear(sessionID)
	cancelledTools := 0
	rejectedApprovals := 0
	if s.service != nil {
		var err error
		cancelledTools, err = s.service.CancelSessionToolExecutions(r.Context(), sessionID)
		if err != nil {
			writeError(w, fmt.Errorf("cancel Agent session tools: %w", err))
			return
		}
		rejectedApprovals, err = s.service.AbortApprovalsForSession(r.Context(), sessionID, "Agent run stopped by the operator", actor(r))
		if err != nil {
			writeError(w, fmt.Errorf("cancel Agent session approvals: %w", err))
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": cancelled || cancelledDriver || cancelledTools > 0 || cancelledQueued > 0, "cancelled_tools": cancelledTools, "cancelled_queued": cancelledQueued, "rejected_approvals": rejectedApprovals})
}

func (s *Server) setChatSessionWorkspace(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimSpace(r.PathValue("id"))
	if sessionID == "" {
		writeErrorStatus(w, fmt.Errorf("session id is required"), http.StatusBadRequest)
		return
	}
	if s.chatSessionActive(sessionID) {
		writeErrorStatus(w, fmt.Errorf("cannot switch Workspace while this conversation's Agent run is active"), http.StatusConflict)
		return
	}
	var input struct {
		WorkspaceID string `json:"workspace_id"`
	}
	if !decode(w, r, &input) {
		return
	}
	session, err := s.service.SetChatSessionWorkspace(r.Context(), sessionID, input.WorkspaceID, actor(r))
	respond(w, session, err)
}

func (s *Server) renameChatSession(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Title string `json:"title"`
	}
	if !decode(w, r, &input) {
		return
	}
	session, err := s.service.RenameChatSession(r.Context(), r.PathValue("id"), input.Title, actor(r))
	respond(w, session, err)
}

func (s *Server) deleteChatSession(w http.ResponseWriter, r *http.Request) {
	if s.chatSessionActive(r.PathValue("id")) {
		writeErrorStatus(w, fmt.Errorf("cannot delete a conversation while its Agent run is active"), http.StatusConflict)
		return
	}
	if err := s.service.DeleteChatSession(r.Context(), r.PathValue("id"), actor(r)); err != nil {
		writeError(w, err)
		return
	}
	s.chatEvents.delete(r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}
