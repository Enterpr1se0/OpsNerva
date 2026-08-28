package service

import (
	"sync"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/store"
)

const (
	StateTopicConnections = "connections"
	StateTopicApprovals   = "approvals"
	StateTopicSessions    = "sessions"
	StateTopicChatState   = "chat_state"
	StateTopicAudit       = "audit"
)

type ConnectionStateDelta struct {
	Tunnel  *domain.SSHTunnel `json:"tunnel,omitempty"`
	Shell   *domain.SSHShell  `json:"shell,omitempty"`
	Removed bool              `json:"removed,omitempty"`
}

type StateEvent struct {
	Topic      string                `json:"topic"`
	SessionID  string                `json:"session_id,omitempty"`
	Connection *ConnectionStateDelta `json:"connection,omitempty"`
}

type stateSubscriber struct {
	events   chan StateEvent
	overflow chan struct{}
	done     chan struct{}
	once     sync.Once
}

func (s *Service) SubscribeStateEvents() (<-chan StateEvent, <-chan struct{}, func()) {
	subscriber := &stateSubscriber{events: make(chan StateEvent, 256), overflow: make(chan struct{}, 1), done: make(chan struct{})}
	s.stateEventMu.Lock()
	if s.stateSubscribers == nil {
		s.stateSubscribers = make(map[uint64]*stateSubscriber)
	}
	s.stateSubscriberID++
	id := s.stateSubscriberID
	s.stateSubscribers[id] = subscriber
	s.stateEventMu.Unlock()
	return subscriber.events, subscriber.overflow, func() {
		subscriber.once.Do(func() {
			s.stateEventMu.Lock()
			delete(s.stateSubscribers, id)
			s.stateEventMu.Unlock()
			close(subscriber.done)
		})
	}
}

func (s *Service) publishStateEvent(event StateEvent) {
	if event.Topic == "" {
		return
	}
	s.stateEventMu.RLock()
	subscribers := make([]*stateSubscriber, 0, len(s.stateSubscribers))
	for _, subscriber := range s.stateSubscribers {
		subscribers = append(subscribers, subscriber)
	}
	s.stateEventMu.RUnlock()
	for _, subscriber := range subscribers {
		select {
		case <-subscriber.done:
			continue
		default:
		}
		select {
		case subscriber.events <- event:
		default:
			select {
			case subscriber.overflow <- struct{}{}:
			default:
			}
		}
	}
}

func (s *Service) publishTunnelState(tunnel domain.SSHTunnel, removed bool) {
	s.publishStateEvent(StateEvent{
		Topic:      StateTopicConnections,
		Connection: &ConnectionStateDelta{Tunnel: &tunnel, Removed: removed},
	})
}

func (s *Service) publishShellState(shell domain.SSHShell, removed bool) {
	s.publishStateEvent(StateEvent{
		Topic:      StateTopicConnections,
		Connection: &ConnectionStateDelta{Shell: &shell, Removed: removed},
	})
}

func (s *Service) PublishChatState(sessionID string) {
	if sessionID == "" {
		return
	}
	s.publishStateEvent(StateEvent{Topic: StateTopicChatState, SessionID: sessionID})
	s.publishStateEvent(StateEvent{Topic: StateTopicSessions})
}

func (s *Service) publishStoreChange(change store.Change) {
	switch change.Topic {
	case store.ChangeApprovals:
		s.publishStateEvent(StateEvent{Topic: StateTopicApprovals})
	case store.ChangeAudit:
		s.publishStateEvent(StateEvent{Topic: StateTopicAudit})
	case store.ChangeSessions:
		s.publishStateEvent(StateEvent{Topic: StateTopicSessions})
	case store.ChangeChatState:
		s.publishStateEvent(StateEvent{Topic: StateTopicChatState, SessionID: change.SessionID})
	}
}
