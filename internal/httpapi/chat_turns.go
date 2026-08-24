package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/Enterpr1se0/opsnerva/internal/agent"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

type chatTurnExecution func(context.Context, func(agent.Event)) (string, error)

// runChatTurns owns the conversation driver independently of the HTTP
// connection. It advances queued input only after the current turn reaches a
// terminal boundary or a steering checkpoint.
func (s *Server) runChatTurns(ctx context.Context, sessionID, message string, attachments []domain.ChatAttachment, emit func(agent.Event)) {
	s.runChatTurnLoop(ctx, sessionID, func(ctx context.Context, publish func(agent.Event)) (string, error) {
		return s.agent.QueryWithAttachments(ctx, sessionID, message, attachments, publish)
	}, emit)
}

func (s *Server) runChatTurnLoop(ctx context.Context, sessionID string, execute chatTurnExecution, emit func(agent.Event)) {
	defer s.chatQueue.finish(sessionID)
	ctx = agent.WithApprovalPauseRegistrar(ctx, func(pauses []agent.ApprovalPause) (agent.ApprovalWait, error) {
		approvalIDs := make([]string, 0, len(pauses))
		for _, pause := range pauses {
			if pause.SessionID != sessionID {
				return nil, errors.New("Agent approval pause belongs to a different conversation")
			}
			approvalIDs = append(approvalIDs, pause.ApprovalID)
		}
		resumed, err := s.chatQueue.pause(sessionID, approvalIDs)
		if err != nil {
			return nil, err
		}
		return func(context.Context) error {
			return s.chatQueue.waitForResume(sessionID, resumed)
		}, nil
	})
	var started atomic.Bool
	broadcast := func(event agent.Event) {
		if event.Type == "session" {
			started.Store(true)
		}
		event = s.chatEvents.publish(sessionID, event)
		emit(event)
	}
	for {
		var completed *agent.Event
		currentUserMessageID := ""
		publish := func(event agent.Event) {
			if event.UserMessageID != "" {
				currentUserMessageID = event.UserMessageID
			}
			if event.Type == "done" {
				copy := event
				completed = &copy
				return
			}
			broadcast(event)
		}
		_, err := execute(ctx, publish)
		if err != nil {
			if errors.Is(err, agent.ErrSteered) {
				next, ok := s.chatQueue.nextAfterTurn(sessionID)
				if !ok {
					broadcast(agent.Event{Type: "done", SessionID: sessionID, UserMessageID: currentUserMessageID})
					return
				}
				broadcast(agent.Event{Type: "turn_steered", SessionID: sessionID, UserMessageID: currentUserMessageID})
				broadcast(agent.Event{
					Type: "queue_started", MessageID: next.ID, SessionID: sessionID, Content: next.Message,
					Status: "in_progress", QueueMode: next.Mode, QueueCount: len(s.chatQueue.snapshot(sessionID)), AttachmentCount: len(next.Attachments),
				})
				execute = func(ctx context.Context, publish func(agent.Event)) (string, error) {
					return s.agent.QueryWithAttachments(ctx, sessionID, next.Message, next.Attachments, publish)
				}
				continue
			}
			_, _ = s.chatQueue.clear(sessionID)
			if !errors.Is(err, context.Canceled) {
				event := agent.Event{Type: "model_error", Error: err.Error(), SessionID: sessionID, UserMessageID: currentUserMessageID}
				if started.Load() {
					broadcast(event)
				} else {
					emit(event)
				}
			}
			return
		}

		next, ok := s.chatQueue.nextAfterTurn(sessionID)
		if !ok {
			if completed == nil {
				completed = &agent.Event{Type: "done", SessionID: sessionID, UserMessageID: currentUserMessageID}
			}
			broadcast(*completed)
			return
		}
		broadcast(agent.Event{Type: "turn_done", SessionID: sessionID, UserMessageID: currentUserMessageID, Content: completedContent(completed)})
		broadcast(agent.Event{
			Type: "queue_started", MessageID: next.ID, SessionID: sessionID, Content: next.Message,
			Status: "in_progress", QueueMode: next.Mode, QueueCount: len(s.chatQueue.snapshot(sessionID)), AttachmentCount: len(next.Attachments),
		})
		execute = func(ctx context.Context, publish func(agent.Event)) (string, error) {
			return s.agent.QueryWithAttachments(ctx, sessionID, next.Message, next.Attachments, publish)
		}
	}
}

// resumeAgentApprovals starts a new conversation driver only when the original
// in-memory waiter no longer exists (for example after a process restart).
func (s *Server) resumeAgentApprovals(requestCtx context.Context, approvals []domain.Approval) error {
	if s.agent == nil {
		return agent.ErrUnavailable
	}
	if len(approvals) == 0 {
		return errors.New("Agent approval continuation is empty")
	}
	sessionID := approvals[0].SessionID
	queueCtx, started := s.chatQueue.begin(sessionID)
	if !started {
		return fmt.Errorf("conversation is already active")
	}
	runCtx, cancel := newChatRunContext(requestCtx, queueCtx)
	go func() {
		defer cancel()
		s.runChatTurnLoop(runCtx, sessionID, func(ctx context.Context, publish func(agent.Event)) (string, error) {
			return s.agent.ResumeAgentApprovals(ctx, approvals, publish)
		}, func(agent.Event) {})
	}()
	return nil
}

func (s *Server) decidedAgentApprovalGroup(ctx context.Context, approval domain.Approval) ([]domain.Approval, error) {
	group, err := s.service.ListAgentApprovalsByCheckpoint(ctx, approval.CheckpointID)
	if err != nil {
		return nil, err
	}
	decided := make([]domain.Approval, 0, len(group))
	active := 0
	for _, item := range group {
		if item.InterruptID == "" {
			continue
		}
		active++
		if item.Status == domain.ApprovalStatusPending {
			return nil, nil
		}
		if item.Status == domain.ApprovalStatusApproved || item.Status == domain.ApprovalStatusRejected {
			decided = append(decided, item)
			continue
		}
		return nil, fmt.Errorf("Agent approval %q is %s", item.ID, item.Status)
	}
	if active == 0 || len(decided) != active {
		return nil, errors.New("Agent approval continuation is no longer resumable")
	}
	return decided, nil
}

func (s *Server) recoverAgentApprovals() {
	if s == nil || s.agent == nil || !s.agent.Available() {
		return
	}
	approvals, err := s.service.ListDecidedAgentApprovals(context.Background())
	if err != nil {
		slog.Error("load resumable Agent approvals failed", "component", "approval", "error", err)
		return
	}
	groups := make(map[string][]domain.Approval)
	order := make([]string, 0)
	for _, approval := range approvals {
		if _, exists := groups[approval.CheckpointID]; !exists {
			order = append(order, approval.CheckpointID)
		}
		groups[approval.CheckpointID] = append(groups[approval.CheckpointID], approval)
	}
	for _, checkpointID := range order {
		group := groups[checkpointID]
		if len(group) == 0 || s.chatSessionActive(group[0].SessionID) {
			continue
		}
		decided, reconcileErr := s.decidedAgentApprovalGroup(context.Background(), group[0])
		if reconcileErr != nil {
			slog.Error("load persisted Agent approval group failed", "component", "approval",
				"checkpoint_id", checkpointID, "session_id", group[0].SessionID, "error", reconcileErr)
			continue
		}
		if len(decided) == 0 {
			continue
		}
		if err := s.resumeAgentApprovals(context.Background(), decided); err != nil {
			slog.Error("resume persisted Agent approval failed", "component", "approval",
				"checkpoint_id", checkpointID, "session_id", group[0].SessionID, "error", err)
		}
	}
}

// newChatRunContext keeps an Agent turn alive across browser disconnects and
// gives model and tool calls enough time to enforce their own retryable
// timeouts. The conversation queue remains the owner of explicit cancellation.
func newChatRunContext(requestCtx, queueCtx context.Context) (context.Context, context.CancelFunc) {
	runCtx, cancel := context.WithCancel(context.WithoutCancel(requestCtx))
	stopQueueCancel := context.AfterFunc(queueCtx, cancel)
	return runCtx, func() {
		stopQueueCancel()
		cancel()
	}
}

func completedContent(event *agent.Event) string {
	if event == nil {
		return ""
	}
	return event.Content
}
