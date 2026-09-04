package store

import "github.com/Enterpr1se0/opsnerva/internal/domain"

const (
	ChangeApprovals = "approvals"
	ChangeAudit     = "audit"
	ChangeSessions  = "sessions"
	ChangeChatState = "chat_state"
)

// Change identifies a committed control-plane mutation. SessionID narrows
// conversation-scoped invalidations; Audit carries an exact committed audit
// event when one exists. An empty value invalidates every session.
type Change struct {
	Topic     string
	SessionID string
	Audit     *domain.AuditEvent
}

// SubscribeChanges registers a listener for changes published after a
// successful database write or transaction commit. Listeners run synchronously
// and must remain lightweight.
func (s *Store) SubscribeChanges(listener func(Change)) func() {
	if listener == nil {
		return func() {}
	}
	s.changeMu.Lock()
	if s.changeListeners == nil {
		s.changeListeners = make(map[uint64]func(Change))
	}
	s.changeListenerID++
	id := s.changeListenerID
	s.changeListeners[id] = listener
	s.changeMu.Unlock()
	return func() {
		s.changeMu.Lock()
		delete(s.changeListeners, id)
		s.changeMu.Unlock()
	}
}

func (s *Store) publishChange(change Change) {
	if change.Topic == "" {
		return
	}
	s.changeMu.RLock()
	listeners := make([]func(Change), 0, len(s.changeListeners))
	for _, listener := range s.changeListeners {
		listeners = append(listeners, listener)
	}
	s.changeMu.RUnlock()
	for _, listener := range listeners {
		listener(change)
	}
}

func (s *Store) publishSessionChange(sessionID string, chatState bool) {
	s.publishChange(Change{Topic: ChangeSessions, SessionID: sessionID})
	if chatState {
		s.publishChange(Change{Topic: ChangeChatState, SessionID: sessionID})
	}
}
