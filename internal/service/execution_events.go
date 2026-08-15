package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/observability"
	"eino-ops-agent/internal/store"
)

type ExecutionEvent struct {
	SessionID        string `json:"session_id"`
	RunID            string `json:"run_id"`
	ToolCallID       string `json:"tool_call_id,omitempty"`
	ToolName         string `json:"tool_name,omitempty"`
	Stream           string `json:"stream,omitempty"`
	Content          string `json:"content,omitempty"`
	Status           string `json:"status,omitempty"`
	TransferredBytes int64  `json:"transferred_bytes,omitempty"`
	TotalBytes       int64  `json:"total_bytes,omitempty"`
	Sequence         uint64 `json:"sequence"`
}

type executionSubscriber struct {
	events chan ExecutionEvent
	done   chan struct{}
	once   sync.Once
}

type executionOutputSink struct {
	service    *Service
	run        domain.Run
	downstream func(string, []byte)
	stdout     streamRedactor
	stderr     streamRedactor
}

type streamRedactor interface {
	Write([]byte) string
	Flush() string
}

func (s *Service) SubscribeExecutionEvents(sessionID string) (<-chan ExecutionEvent, func()) {
	subscriber := &executionSubscriber{
		events: make(chan ExecutionEvent, 256),
		done:   make(chan struct{}),
	}
	s.executionEventMu.Lock()
	if s.executionSubscribers == nil {
		s.executionSubscribers = make(map[string]map[uint64]*executionSubscriber)
	}
	s.executionSubscriberID++
	id := s.executionSubscriberID
	bySession := s.executionSubscribers[sessionID]
	if bySession == nil {
		bySession = make(map[uint64]*executionSubscriber)
		s.executionSubscribers[sessionID] = bySession
	}
	bySession[id] = subscriber
	s.executionEventMu.Unlock()

	cancel := func() {
		subscriber.once.Do(func() {
			s.executionEventMu.Lock()
			if current := s.executionSubscribers[sessionID]; current != nil {
				delete(current, id)
				if len(current) == 0 {
					delete(s.executionSubscribers, sessionID)
				}
			}
			s.executionEventMu.Unlock()
			close(subscriber.done)
		})
	}
	return subscriber.events, cancel
}

func (s *Service) hasExecutionSubscribers(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	s.executionEventMu.RLock()
	defer s.executionEventMu.RUnlock()
	return len(s.executionSubscribers[sessionID]) > 0
}

func (s *Service) publishExecutionEvent(event ExecutionEvent) {
	if event.SessionID == "" || event.RunID == "" {
		return
	}
	event.Sequence = s.executionEventSequence.Add(1)
	s.executionEventMu.RLock()
	owner, hasOwner := s.executionOwners[event.RunID]
	if hasOwner {
		if event.ToolCallID == "" {
			event.ToolCallID = owner.ToolCallID
		}
		if event.ToolName == "" {
			event.ToolName = owner.ToolName
		}
	}
	subscribers := make([]*executionSubscriber, 0, len(s.executionSubscribers[event.SessionID]))
	for _, subscriber := range s.executionSubscribers[event.SessionID] {
		subscribers = append(subscribers, subscriber)
	}
	s.executionEventMu.RUnlock()
	persistedStatus := persistedToolExecutionStatus(event.Status)
	var call domain.ChatToolCall
	callErr := store.ErrNotFound
	if persistedStatus != "" || (!hasOwner && (event.ToolCallID == "" || event.ToolName == "")) {
		call, callErr = s.store.GetChatToolCallByRun(context.Background(), event.RunID)
		if callErr == nil {
			if event.ToolCallID == "" {
				event.ToolCallID = call.ToolCallID
			}
			if event.ToolName == "" {
				event.ToolName = call.ToolName
			}
		}
	}
	if persistedStatus != "" && (callErr != nil || !detachedToolInvocation(call)) {
		content := ""
		if run, err := s.store.GetRun(context.Background(), event.RunID); err == nil {
			if encoded, err := json.Marshal(execResultFromRun(run, "", "")); err == nil {
				content = string(encoded)
			}
		}
		if _, err := s.store.UpdateChatToolCallByRun(context.Background(), event.RunID, persistedStatus, content, ""); err != nil && !errors.Is(err, store.ErrNotFound) {
			observability.FromContext(context.Background()).ErrorContext(context.Background(), "persist execution tool status failed", "run_id", event.RunID, "tool_call_id", event.ToolCallID, "status", persistedStatus, "error", err)
		}
	}

	var shutdown <-chan struct{}
	if s.executionCtx != nil {
		shutdown = s.executionCtx.Done()
	}
	for _, subscriber := range subscribers {
		select {
		case <-subscriber.done:
			continue
		default:
		}
		select {
		case subscriber.events <- event:
		case <-subscriber.done:
		case <-shutdown:
			return
		}
	}
	if terminalExecutionStatus(event.Status) {
		s.clearExecutionOwner(event.RunID)
	}
}

func detachedToolInvocation(call domain.ChatToolCall) bool {
	var arguments struct {
		Background bool `json:"background"`
	}
	return json.Unmarshal([]byte(call.ArgumentsJSON), &arguments) == nil && arguments.Background
}

func (s *Service) bindExecutionOwner(ctx context.Context, runID, sessionID string, owner executionOwner) {
	if runID == "" || (owner.ToolCallID == "" && owner.ToolName == "") {
		return
	}
	if sessionID != "" && owner.ToolCallID != "" {
		if err := s.store.BindChatToolCallRun(ctx, sessionID, owner.ToolCallID, runID); err != nil && !errors.Is(err, store.ErrNotFound) {
			observability.FromContext(ctx).ErrorContext(ctx, "persist execution owner failed", "run_id", runID, "tool_call_id", owner.ToolCallID, "error", err)
		}
	}
	s.executionEventMu.Lock()
	if s.executionOwners == nil {
		s.executionOwners = make(map[string]executionOwner)
	}
	s.executionOwners[runID] = owner
	s.executionEventMu.Unlock()
}

func persistedToolExecutionStatus(status string) string {
	switch status {
	case "completed":
		return domain.ChatToolCallCompleted
	case "partial":
		return domain.ChatToolCallPartial
	case "failed":
		return domain.ChatToolCallFailed
	case "interrupted", "cancelled":
		return domain.ChatToolCallInterrupted
	case "rejected", "denied":
		return domain.ChatToolCallRejected
	case "expired", "timeout":
		return domain.ChatToolCallExpired
	default:
		return ""
	}
}

func (s *Service) clearExecutionOwner(runID string) {
	if runID == "" {
		return
	}
	s.executionEventMu.Lock()
	delete(s.executionOwners, runID)
	s.executionEventMu.Unlock()
}

// CancelSessionToolExecutions stops only active, turn-scoped executions.
// Background tasks and already-started shells or tunnels have their own
// lifecycle and are not owned by the model turn after their start call ends.
func (s *Service) CancelSessionToolExecutions(ctx context.Context, sessionID string) (int, error) {
	s.executionMu.Lock()
	runIDs := make([]string, 0, len(s.executionCancels))
	for runID := range s.executionCancels {
		runIDs = append(runIDs, runID)
	}
	s.executionMu.Unlock()
	cancelled := 0
	for _, runID := range runIDs {
		run, err := s.store.GetRun(ctx, runID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return cancelled, err
		}
		if run.SessionID != sessionID {
			continue
		}
		var arguments struct {
			Background bool `json:"background"`
		}
		if json.Unmarshal([]byte(run.ToolArgumentsJSON), &arguments) == nil && arguments.Background {
			continue
		}
		if s.cancelApprovedExecution(runID) {
			cancelled++
		}
	}
	return cancelled, nil
}

func terminalExecutionStatus(status string) bool {
	switch status {
	case "completed", "partial", "failed", "interrupted", "rejected", "denied", "expired":
		return true
	default:
		return false
	}
}

func (s *Service) newExecutionOutputSink(run domain.Run, downstream func(string, []byte)) *executionOutputSink {
	return &executionOutputSink{
		service: s, run: run, downstream: downstream,
		stdout: s.redactor.NewStreamRedactor(),
		stderr: s.redactor.NewStreamRedactor(),
	}
}

func (s *executionOutputSink) Write(stream string, data []byte) {
	redactor := s.stdout
	if stream == "stderr" {
		redactor = s.stderr
	}
	s.emit(stream, redactor.Write(data))
}

func (s *executionOutputSink) Flush() {
	s.emit("stdout", s.stdout.Flush())
	s.emit("stderr", s.stderr.Flush())
}

func (s *executionOutputSink) emit(stream, content string) {
	if content == "" {
		return
	}
	if s.downstream != nil {
		s.downstream(stream, []byte(content))
	}
	s.service.publishExecutionEvent(ExecutionEvent{
		SessionID: s.run.SessionID,
		RunID:     s.run.ID,
		Stream:    stream,
		Content:   content,
		Status:    "running",
	})
}
