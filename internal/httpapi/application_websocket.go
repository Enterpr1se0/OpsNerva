package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"eino-ops-agent/internal/agent"
	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/observability"

	"golang.org/x/net/websocket"
)

const applicationWebSocketMaxMessage = 16 << 10
const applicationSnapshotCacheTTL = 200 * time.Millisecond
const applicationSnapshotCacheLimit = 256

var applicationEventIntervals = map[string]time.Duration{
	"connections": time.Second,
	"approvals":   time.Second,
	"sessions":    2 * time.Second,
	"chat_state":  2 * time.Second,
	"audit":       time.Second,
	"health":      30 * time.Second,
	"logs":        time.Second,
}

type applicationWebSocketLogFilter struct {
	Level     string `json:"level,omitempty"`
	Component string `json:"component,omitempty"`
	Query     string `json:"q,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

type applicationWebSocketCommand struct {
	Type      string                        `json:"type"`
	Topics    []string                      `json:"topics,omitempty"`
	Logs      applicationWebSocketLogFilter `json:"logs,omitempty"`
	SessionID string                        `json:"session_id,omitempty"`
}

type applicationWebSocketEvent struct {
	Type     string          `json:"type"`
	Topic    string          `json:"topic,omitempty"`
	Mode     string          `json:"mode,omitempty"`
	Sequence uint64          `json:"sequence,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
	Error    string          `json:"error,omitempty"`
}

type applicationWebSocketSubscription struct {
	topics    map[string]struct{}
	logs      applicationWebSocketLogFilter
	sessionID string
}

type applicationTopicState struct {
	payload []byte
	next    time.Time
}

type applicationSnapshotCacheEntry struct {
	payload []byte
	expires time.Time
}

type applicationLogResponse struct {
	Entries      []observability.LogEntry `json:"entries"`
	Components   []string                 `json:"components"`
	MinimumLevel string                   `json:"minimum_level"`
	File         string                   `json:"file"`
}

type applicationConnectionResponse struct {
	Tunnels []domain.SSHTunnel `json:"tunnels"`
	Shells  []domain.SSHShell  `json:"shells"`
}

func (s *Server) applicationWebSocket(w http.ResponseWriter, r *http.Request) {
	server := websocket.Server{
		Handshake: validateWebSocketOrigin,
		Handler: func(connection *websocket.Conn) {
			connection.MaxPayloadBytes = applicationWebSocketMaxMessage
			s.serveApplicationWebSocket(connection, r)
		},
	}
	server.ServeHTTP(w, r)
}

func (s *Server) serveApplicationWebSocket(connection *websocket.Conn, request *http.Request) {
	defer connection.Close()
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	updates := make(chan applicationWebSocketSubscription, 1)
	readDone := make(chan error, 1)
	go func() {
		readDone <- readApplicationWebSocket(ctx, connection, updates)
		cancel()
	}()

	subscription := applicationWebSocketSubscription{topics: map[string]struct{}{}}
	states := make(map[string]applicationTopicState)
	var previousLogs []observability.LogEntry
	var sequence uint64
	tick := time.NewTicker(250 * time.Millisecond)
	heartbeat := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	defer heartbeat.Stop()

	send := func(topic, mode string, payload []byte) error {
		sequence++
		return websocket.JSON.Send(connection, applicationWebSocketEvent{Type: "event", Topic: topic, Mode: mode, Sequence: sequence, Data: payload})
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-readDone:
			return
		case next := <-updates:
			subscription = next
			states = make(map[string]applicationTopicState)
			previousLogs = nil
		case now := <-tick.C:
			topics := make([]string, 0, len(subscription.topics))
			for topic := range subscription.topics {
				topics = append(topics, topic)
			}
			sort.Strings(topics)
			for _, topic := range topics {
				state := states[topic]
				if !state.next.IsZero() && now.Before(state.next) {
					continue
				}
				state.next = now.Add(applicationEventIntervals[topic])
				if topic == "logs" {
					current := recentApplicationLogs(subscription.logs)
					fullPayload, marshalErr := json.Marshal(current)
					if marshalErr != nil {
						_ = websocket.JSON.Send(connection, applicationWebSocketEvent{Type: "error", Topic: topic, Error: marshalErr.Error()})
						states[topic] = state
						continue
					}
					mode, payload, changed, nextPrevious, err := applicationLogUpdate(previousLogs, current)
					if err != nil {
						_ = websocket.JSON.Send(connection, applicationWebSocketEvent{Type: "error", Topic: topic, Error: err.Error()})
						states[topic] = state
						continue
					}
					previousLogs = nextPrevious
					if !changed && !bytes.Equal(state.payload, fullPayload) {
						mode, payload, changed = "snapshot", fullPayload, true
					}
					state.payload = fullPayload
					states[topic] = state
					if changed && send(topic, mode, payload) != nil {
						return
					}
					continue
				}
				payload, err := s.applicationTopicSnapshotCached(ctx, topic, subscription)
				if err != nil {
					_ = websocket.JSON.Send(connection, applicationWebSocketEvent{Type: "error", Topic: topic, Error: err.Error()})
					states[topic] = state
					continue
				}
				changed := !bytes.Equal(state.payload, payload)
				state.payload = payload
				states[topic] = state
				if changed && send(topic, "snapshot", payload) != nil {
					return
				}
			}
		case <-heartbeat.C:
			if err := websocket.JSON.Send(connection, applicationWebSocketEvent{Type: "heartbeat"}); err != nil {
				return
			}
		}
	}
}

func (s *Server) applicationTopicSnapshotCached(ctx context.Context, topic string, subscription applicationWebSocketSubscription) ([]byte, error) {
	key := topic
	if topic == "chat_state" {
		key += "\x00" + subscription.sessionID
	}
	now := time.Now()
	s.applicationSnapshotMu.RLock()
	cached, ok := s.applicationSnapshots[key]
	s.applicationSnapshotMu.RUnlock()
	if ok && now.Before(cached.expires) {
		return cached.payload, nil
	}
	payload, err := s.applicationTopicSnapshot(ctx, topic, subscription)
	if err != nil {
		return nil, err
	}
	s.applicationSnapshotMu.Lock()
	if s.applicationSnapshots == nil {
		s.applicationSnapshots = make(map[string]applicationSnapshotCacheEntry)
	}
	if len(s.applicationSnapshots) >= applicationSnapshotCacheLimit {
		oldestKey := ""
		var oldestExpiry time.Time
		for cachedKey, entry := range s.applicationSnapshots {
			if !now.Before(entry.expires) {
				delete(s.applicationSnapshots, cachedKey)
				continue
			}
			if oldestKey == "" || entry.expires.Before(oldestExpiry) {
				oldestKey, oldestExpiry = cachedKey, entry.expires
			}
		}
		if len(s.applicationSnapshots) >= applicationSnapshotCacheLimit && oldestKey != "" {
			delete(s.applicationSnapshots, oldestKey)
		}
	}
	s.applicationSnapshots[key] = applicationSnapshotCacheEntry{payload: payload, expires: now.Add(applicationSnapshotCacheTTL)}
	s.applicationSnapshotMu.Unlock()
	return payload, nil
}

func readApplicationWebSocket(ctx context.Context, connection *websocket.Conn, updates chan applicationWebSocketSubscription) error {
	for {
		var command applicationWebSocketCommand
		if err := websocket.JSON.Receive(connection, &command); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if command.Type != "subscribe" {
			return fmt.Errorf("unsupported application WebSocket command %q", command.Type)
		}
		topics := make(map[string]struct{}, len(command.Topics))
		for _, value := range command.Topics {
			topic := strings.ToLower(strings.TrimSpace(value))
			if _, ok := applicationEventIntervals[topic]; !ok {
				return fmt.Errorf("unsupported application event topic %q", value)
			}
			topics[topic] = struct{}{}
		}
		if command.Logs.Limit <= 0 {
			command.Logs.Limit = 500
		} else if command.Logs.Limit > 1000 {
			command.Logs.Limit = 1000
		}
		command.SessionID = strings.TrimSpace(command.SessionID)
		if _, subscribed := topics["chat_state"]; subscribed && command.SessionID == "" {
			return fmt.Errorf("session_id is required for application event topic chat_state")
		}
		if len(command.SessionID) > 256 {
			return fmt.Errorf("session_id must not exceed 256 bytes")
		}
		next := applicationWebSocketSubscription{topics: topics, logs: command.Logs, sessionID: command.SessionID}
		select {
		case updates <- next:
		default:
			select {
			case <-updates:
			default:
			}
			updates <- next
		}
	}
}

func (s *Server) applicationTopicSnapshot(ctx context.Context, topic string, subscription applicationWebSocketSubscription) ([]byte, error) {
	var value any
	switch topic {
	case "connections":
		tunnels := s.service.ListSSHTunnels()
		shells, err := s.service.ListSSHShells(ctx, "", true, "", "")
		if err != nil {
			return nil, err
		}
		value = applicationConnectionResponse{Tunnels: tunnels.Tunnels, Shells: shells.Shells}
	case "approvals":
		approvals, err := s.service.ListApprovals(ctx, "pending", 100)
		if err != nil {
			return nil, err
		}
		value = approvals
	case "sessions":
		sessions, err := s.service.ListChatSessions(ctx, 50)
		if err != nil {
			return nil, err
		}
		if s.agent != nil {
			for index := range sessions {
				sessions[index].Active = s.chatSessionActive(sessions[index].ID)
			}
		}
		value = sessions
	case "health":
		value = s.applicationHealth()
	case "chat_state":
		state, err := s.applicationChatState(ctx, subscription.sessionID, false)
		if err != nil {
			return nil, err
		}
		value = state
	case "audit":
		page, err := s.service.ListAuditPage(ctx, "", 1, time.Time{}, "")
		if err != nil {
			return nil, err
		}
		if len(page.Events) == 0 {
			value = map[string]any{}
		} else {
			latest := page.Events[0]
			value = map[string]any{"id": latest.ID, "type": latest.Type, "created_at": latest.CreatedAt}
		}
	default:
		return nil, fmt.Errorf("unsupported application event topic %q", topic)
	}
	return json.Marshal(value)
}

func (s *Server) applicationHealth() map[string]any {
	model := agent.Status{Source: "none"}
	if s.agent != nil {
		model = s.agent.Status()
	}
	return map[string]any{"status": "ok", "agent_available": model.Available, "model": model, "time": time.Now().UTC()}
}

func recentApplicationLogs(filter applicationWebSocketLogFilter) applicationLogResponse {
	return applicationLogResponse{
		Entries:    observability.Recent(observability.LogFilter{Level: filter.Level, Component: filter.Component, Query: filter.Query, Limit: filter.Limit}),
		Components: observability.Components(), MinimumLevel: observability.MinimumLevel(), File: observability.File(),
	}
}

func applicationLogUpdate(previous []observability.LogEntry, current applicationLogResponse) (string, []byte, bool, []observability.LogEntry, error) {
	mode := "snapshot"
	nextPrevious := append([]observability.LogEntry(nil), current.Entries...)
	entries := current.Entries
	if previous != nil {
		if len(previous) == 0 && len(current.Entries) == 0 {
			return "", nil, false, nextPrevious, nil
		}
		if len(previous) > 0 {
			previousHead, err := json.Marshal(previous[0])
			if err != nil {
				return "", nil, false, previous, err
			}
			for index, entry := range current.Entries {
				encoded, marshalErr := json.Marshal(entry)
				if marshalErr != nil {
					return "", nil, false, previous, marshalErr
				}
				if bytes.Equal(encoded, previousHead) {
					if index == 0 {
						return "", nil, false, nextPrevious, nil
					}
					mode = "delta"
					entries = current.Entries[:index]
					break
				}
			}
		}
	}
	current.Entries = entries
	payload, err := json.Marshal(current)
	return mode, payload, err == nil, nextPrevious, err
}
