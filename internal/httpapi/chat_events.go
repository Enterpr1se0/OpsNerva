package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"eino-ops-agent/internal/agent"
	"eino-ops-agent/internal/observability"
)

const chatEventSubscriberBuffer = 128

type chatEventSession struct {
	nextID      uint64
	events      []agent.Event
	done        bool
	subscribers map[uint64]chan agent.Event
}

type chatEventHub struct {
	mu               sync.Mutex
	sessions         map[string]*chatEventSession
	nextSubscriberID uint64
}

func newChatEventHub() *chatEventHub {
	return &chatEventHub{sessions: make(map[string]*chatEventSession)}
}

func (h *chatEventHub) publish(sessionID string, event agent.Event) agent.Event {
	sessionID = strings.TrimSpace(sessionID)
	h.mu.Lock()
	defer h.mu.Unlock()
	state := h.sessions[sessionID]
	if state == nil || state.done {
		state = &chatEventSession{subscribers: make(map[uint64]chan agent.Event)}
		h.sessions[sessionID] = state
	}
	state.nextID++
	event.EventID = state.nextID
	if event.SessionID == "" {
		event.SessionID = sessionID
	}
	state.events = append(state.events, event)
	terminal := chatEventTerminal(event.Type)
	for id, subscriber := range state.subscribers {
		select {
		case subscriber <- event:
		default:
			close(subscriber)
			delete(state.subscribers, id)
		}
	}
	if terminal {
		state.done = true
		for id, subscriber := range state.subscribers {
			close(subscriber)
			delete(state.subscribers, id)
		}
	}
	return event
}

func (h *chatEventHub) subscribe(sessionID string, after uint64) ([]agent.Event, <-chan agent.Event, bool, func()) {
	sessionID = strings.TrimSpace(sessionID)
	h.mu.Lock()
	state := h.sessions[sessionID]
	if state == nil {
		state = &chatEventSession{subscribers: make(map[uint64]chan agent.Event)}
		h.sessions[sessionID] = state
	}
	replay := make([]agent.Event, 0, len(state.events))
	for _, event := range state.events {
		if event.EventID > after {
			replay = append(replay, event)
		}
	}
	if state.done {
		h.mu.Unlock()
		return replay, nil, true, func() {}
	}
	h.nextSubscriberID++
	id := h.nextSubscriberID
	events := make(chan agent.Event, chatEventSubscriberBuffer)
	state.subscribers[id] = events
	h.mu.Unlock()
	var once sync.Once
	return replay, events, false, func() {
		once.Do(func() {
			h.mu.Lock()
			if current := h.sessions[sessionID]; current != nil {
				delete(current.subscribers, id)
			}
			h.mu.Unlock()
		})
	}
}

func (h *chatEventHub) delete(sessionID string) {
	h.mu.Lock()
	state := h.sessions[strings.TrimSpace(sessionID)]
	delete(h.sessions, strings.TrimSpace(sessionID))
	if state != nil {
		for id, subscriber := range state.subscribers {
			close(subscriber)
			delete(state.subscribers, id)
		}
	}
	h.mu.Unlock()
}

func chatEventTerminal(eventType string) bool {
	switch eventType {
	case "done", "error", "interrupted":
		return true
	default:
		return false
	}
}

func (s *Server) chatEventsStream(w http.ResponseWriter, r *http.Request) {
	if s.agent == nil {
		writeErrorStatus(w, agent.ErrUnavailable, http.StatusServiceUnavailable)
		return
	}
	sessionID := strings.TrimSpace(r.PathValue("id"))
	if sessionID == "" {
		writeErrorStatus(w, fmt.Errorf("session id is required"), http.StatusBadRequest)
		return
	}
	after, err := strconv.ParseUint(strings.TrimSpace(r.URL.Query().Get("after")), 10, 64)
	if err != nil && strings.TrimSpace(r.URL.Query().Get("after")) != "" {
		writeErrorStatus(w, fmt.Errorf("after must be a non-negative event id"), http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, fmt.Errorf("streaming is unavailable"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Connection", "keep-alive")
	write := func(event agent.Event) error {
		data, marshalErr := json.Marshal(event)
		if marshalErr != nil {
			return marshalErr
		}
		if _, writeErr := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.EventID, event.Type, data); writeErr != nil {
			return writeErr
		}
		flusher.Flush()
		return nil
	}
	if _, err := fmt.Fprint(w, "retry: 1000\n: connected\n\n"); err != nil {
		return
	}
	flusher.Flush()
	replay, events, done, unsubscribe := s.chatEvents.subscribe(sessionID, after)
	defer unsubscribe()
	for _, event := range replay {
		if err := write(event); err != nil {
			return
		}
	}
	if done || !s.agent.IsSessionActive(sessionID) {
		return
	}
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()
	logger := observability.FromContext(r.Context())
	for {
		select {
		case <-r.Context().Done():
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			if err := write(event); err != nil {
				logger.DebugContext(r.Context(), "reconnected chat stream closed", "component", "agent", "session_id", sessionID, "error", err)
				return
			}
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
