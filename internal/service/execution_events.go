package service

import (
	"sync"

	"eino-ops-agent/internal/domain"
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
	if owner, ok := s.executionOwners[event.RunID]; ok {
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

func (s *Service) bindExecutionOwner(runID string, owner executionOwner) {
	if runID == "" || (owner.ToolCallID == "" && owner.ToolName == "") {
		return
	}
	s.executionEventMu.Lock()
	if s.executionOwners == nil {
		s.executionOwners = make(map[string]executionOwner)
	}
	s.executionOwners[runID] = owner
	s.executionEventMu.Unlock()
}

func (s *Service) clearExecutionOwner(runID string) {
	if runID == "" {
		return
	}
	s.executionEventMu.Lock()
	delete(s.executionOwners, runID)
	s.executionEventMu.Unlock()
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
