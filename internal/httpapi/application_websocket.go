package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Enterpr1se0/opsnerva/internal/agent"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/observability"
	"github.com/Enterpr1se0/opsnerva/internal/service"
	"github.com/Enterpr1se0/opsnerva/internal/store"

	"golang.org/x/net/websocket"
)

const applicationWebSocketMaxMessage = 16 << 10
const applicationSnapshotCacheTTL = 200 * time.Millisecond
const applicationSnapshotCacheLimit = 256
const applicationTaskOutputLimit = 64 << 10
const applicationTaskSubscriptionLimit = 32
const applicationSessionLimit = 50

var applicationSampleIntervals = map[string]time.Duration{
	"connections": 5 * time.Second,
	"health":      30 * time.Second,
	"logs":        time.Second,
}

var applicationStateTopics = map[string]struct{}{
	service.StateTopicConnections: {},
	service.StateTopicApprovals:   {},
	service.StateTopicSessions:    {},
	service.StateTopicChatState:   {},
	service.StateTopicAudit:       {},
}

var applicationPushTopics = map[string]struct{}{
	"mcp_activity": {},
	"tasks":        {},
}

type applicationWebSocketLogFilter struct {
	Level     string `json:"level,omitempty"`
	Component string `json:"component,omitempty"`
	Query     string `json:"q,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

type applicationWebSocketCommand struct {
	Type         string                        `json:"type"`
	Topics       []string                      `json:"topics,omitempty"`
	Logs         applicationWebSocketLogFilter `json:"logs,omitempty"`
	SessionID    string                        `json:"session_id,omitempty"`
	MCPSessionID string                        `json:"mcp_session_id,omitempty"`
	TaskIDs      []string                      `json:"task_ids,omitempty"`
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
	topics       map[string]struct{}
	logs         applicationWebSocketLogFilter
	sessionID    string
	mcpSessionID string
	taskIDs      []string
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

type applicationSessionDelta struct {
	Sessions   []domain.ChatSession `json:"sessions,omitempty"`
	RemovedIDs []string             `json:"removed_ids,omitempty"`
}

func applicationSubscriptionTopicChanged(previous, next applicationWebSocketSubscription, topic string) bool {
	_, previouslySubscribed := previous.topics[topic]
	_, subscribed := next.topics[topic]
	if previouslySubscribed != subscribed {
		return true
	}
	if !subscribed {
		return false
	}
	switch topic {
	case "logs":
		return previous.logs != next.logs
	case "chat_state":
		return previous.sessionID != next.sessionID
	case "mcp_activity":
		return previous.mcpSessionID != next.mcpSessionID
	case "tasks":
		return previous.sessionID != next.sessionID || !slices.Equal(previous.taskIDs, next.taskIDs)
	default:
		return false
	}
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
	var mcpEvents <-chan domain.MCPActivityEvent
	var unsubscribeMCP func()
	var taskEvents <-chan domain.TaskEvent
	var unsubscribeTasks func()
	var stateEvents <-chan service.StateEvent
	var stateOverflow <-chan struct{}
	unsubscribeStateEvents := func() {}
	if s.service != nil {
		stateEvents, stateOverflow, unsubscribeStateEvents = s.service.SubscribeStateEvents()
	}
	defer func() {
		unsubscribeStateEvents()
		if unsubscribeMCP != nil {
			unsubscribeMCP()
		}
		if unsubscribeTasks != nil {
			unsubscribeTasks()
		}
	}()
	var sequence uint64
	tick := time.NewTicker(250 * time.Millisecond)
	heartbeat := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	defer heartbeat.Stop()

	send := func(topic, mode string, payload []byte) error {
		sequence++
		return websocket.JSON.Send(connection, applicationWebSocketEvent{Type: "event", Topic: topic, Mode: mode, Sequence: sequence, Data: payload})
	}
	sendSnapshot := func(topic string) error {
		var payload []byte
		if topic == "logs" {
			current := recentApplicationLogs(subscription.logs)
			encoded, err := json.Marshal(current)
			if err != nil {
				return websocket.JSON.Send(connection, applicationWebSocketEvent{Type: "error", Topic: topic, Error: err.Error()})
			}
			previousLogs = append([]observability.LogEntry(nil), current.Entries...)
			payload = encoded
		} else {
			var err error
			payload, err = s.applicationTopicSnapshot(ctx, topic, subscription)
			if err != nil {
				return websocket.JSON.Send(connection, applicationWebSocketEvent{Type: "error", Topic: topic, Error: err.Error()})
			}
		}
		state := states[topic]
		state.payload = payload
		if interval, sampled := applicationSampleIntervals[topic]; sampled {
			state.next = time.Now().Add(interval)
		}
		states[topic] = state
		return send(topic, "snapshot", payload)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-readDone:
			return
		case next := <-updates:
			previous := subscription
			mcpChanged := applicationSubscriptionTopicChanged(previous, next, "mcp_activity")
			tasksChanged := applicationSubscriptionTopicChanged(previous, next, "tasks")
			logsChanged := applicationSubscriptionTopicChanged(previous, next, "logs")
			if mcpChanged && unsubscribeMCP != nil {
				unsubscribeMCP()
				unsubscribeMCP = nil
				mcpEvents = nil
			}
			if tasksChanged && unsubscribeTasks != nil {
				unsubscribeTasks()
				unsubscribeTasks = nil
				taskEvents = nil
			}
			subscription = next
			for topic := range states {
				if _, subscribed := subscription.topics[topic]; !subscribed || applicationSubscriptionTopicChanged(previous, subscription, topic) {
					delete(states, topic)
				}
			}
			if logsChanged {
				previousLogs = nil
			}
			if _, subscribed := subscription.topics["mcp_activity"]; subscribed && mcpChanged {
				mcpEvents, unsubscribeMCP = s.service.SubscribeMCPActivity(subscription.mcpSessionID)
				payload, err := s.applicationTopicSnapshot(ctx, "mcp_activity", subscription)
				if err != nil {
					_ = websocket.JSON.Send(connection, applicationWebSocketEvent{Type: "error", Topic: "mcp_activity", Error: err.Error()})
				} else if send("mcp_activity", "snapshot", payload) != nil {
					return
				}
			}
			if _, subscribed := subscription.topics["tasks"]; subscribed && tasksChanged {
				var snapshots map[string]domain.TaskSnapshot
				snapshots, taskEvents, unsubscribeTasks = s.service.SubscribeTaskEvents(ctx, subscription.sessionID, subscription.taskIDs)
				payload, err := json.Marshal(applicationTaskSnapshots(snapshots))
				if err != nil {
					_ = websocket.JSON.Send(connection, applicationWebSocketEvent{Type: "error", Topic: "tasks", Error: err.Error()})
				} else if send("tasks", "snapshot", payload) != nil {
					return
				}
			}
			topics := make([]string, 0, len(subscription.topics))
			for topic := range subscription.topics {
				if _, pushed := applicationPushTopics[topic]; !pushed && applicationSubscriptionTopicChanged(previous, subscription, topic) {
					topics = append(topics, topic)
				}
			}
			sort.Strings(topics)
			for _, topic := range topics {
				if sendSnapshot(topic) != nil {
					return
				}
			}
		case event, ok := <-mcpEvents:
			if !ok {
				return
			}
			payload, err := json.Marshal(event)
			if err != nil {
				continue
			}
			if send("mcp_activity", "delta", payload) != nil {
				return
			}
		case event := <-taskEvents:
			if event.Type == "resync" {
				if unsubscribeTasks != nil {
					unsubscribeTasks()
				}
				var snapshots map[string]domain.TaskSnapshot
				snapshots, taskEvents, unsubscribeTasks = s.service.SubscribeTaskEvents(ctx, subscription.sessionID, subscription.taskIDs)
				payload, err := json.Marshal(applicationTaskSnapshots(snapshots))
				if err != nil {
					continue
				}
				if send("tasks", "snapshot", payload) != nil {
					return
				}
				continue
			}
			event = applicationTaskEvent(event)
			payload, err := json.Marshal(event)
			if err != nil {
				continue
			}
			if send("tasks", "delta", payload) != nil {
				return
			}
		case event := <-stateEvents:
			if _, subscribed := subscription.topics[event.Topic]; !subscribed {
				continue
			}
			if event.Topic == service.StateTopicChatState && event.SessionID != "" && event.SessionID != subscription.sessionID {
				continue
			}
			if event.Connection != nil {
				payload, err := json.Marshal(event.Connection)
				if err == nil && send(event.Topic, "delta", payload) != nil {
					return
				}
				continue
			}
			if event.Topic == service.StateTopicSessions {
				state := states[event.Topic]
				fullPayload, deltaPayload, changed, err := s.applicationSessionsUpdate(ctx, state.payload, event.SessionID)
				if err != nil {
					if websocket.JSON.Send(connection, applicationWebSocketEvent{Type: "error", Topic: event.Topic, Error: err.Error()}) != nil {
						return
					}
					continue
				}
				state.payload = fullPayload
				states[event.Topic] = state
				if changed && send(event.Topic, "delta", deltaPayload) != nil {
					return
				}
				continue
			}
			if event.Topic == service.StateTopicChatState {
				fullPayload, err := s.applicationTopicSnapshot(ctx, event.Topic, subscription)
				if err != nil {
					if websocket.JSON.Send(connection, applicationWebSocketEvent{Type: "error", Topic: event.Topic, Error: err.Error()}) != nil {
						return
					}
					continue
				}
				state := states[event.Topic]
				deltaPayload, changed, deltaErr := applicationObjectDelta(state.payload, fullPayload)
				if deltaErr != nil {
					if websocket.JSON.Send(connection, applicationWebSocketEvent{Type: "error", Topic: event.Topic, Error: deltaErr.Error()}) != nil {
						return
					}
					continue
				}
				state.payload = fullPayload
				states[event.Topic] = state
				if changed && send(event.Topic, "delta", deltaPayload) != nil {
					return
				}
				continue
			}
			if sendSnapshot(event.Topic) != nil {
				return
			}
		case <-stateOverflow:
			draining := true
			for draining {
				select {
				case <-stateEvents:
				default:
					draining = false
				}
			}
			for topic := range subscription.topics {
				if _, stateTopic := applicationStateTopics[topic]; !stateTopic {
					continue
				}
				if sendSnapshot(topic) != nil {
					return
				}
			}
		case now := <-tick.C:
			topics := make([]string, 0, len(subscription.topics))
			for topic := range subscription.topics {
				topics = append(topics, topic)
			}
			sort.Strings(topics)
			for _, topic := range topics {
				interval, sampled := applicationSampleIntervals[topic]
				if !sampled {
					continue
				}
				state := states[topic]
				if !state.next.IsZero() && now.Before(state.next) {
					continue
				}
				state.next = now.Add(interval)
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

func (s *Server) applicationSessions(ctx context.Context) ([]domain.ChatSession, error) {
	sessions, err := s.service.ListChatSessions(ctx, applicationSessionLimit)
	if err != nil {
		return nil, err
	}
	for index := range sessions {
		sessions[index].Active = s.chatSessionActive(sessions[index].ID)
	}
	return sessions, nil
}

func applicationSessionBefore(left, right domain.ChatSession) bool {
	if !left.UpdatedAt.Equal(right.UpdatedAt) {
		return left.UpdatedAt.After(right.UpdatedAt)
	}
	return left.ID > right.ID
}

func applicationSessionEqual(left, right domain.ChatSession) bool {
	return left.ID == right.ID &&
		left.Title == right.Title &&
		left.WorkspaceID == right.WorkspaceID &&
		left.ContextTokens == right.ContextTokens &&
		left.ContextWindow == right.ContextWindow &&
		left.MessageCount == right.MessageCount &&
		left.UpdatedAt.Equal(right.UpdatedAt) &&
		left.Active == right.Active
}

func applicationSessionWindow(current []domain.ChatSession, session domain.ChatSession) []domain.ChatSession {
	next := make([]domain.ChatSession, 0, min(applicationSessionLimit, len(current)+1))
	for _, item := range current {
		if item.ID != session.ID {
			next = append(next, item)
		}
	}
	next = append(next, session)
	sort.Slice(next, func(left, right int) bool { return applicationSessionBefore(next[left], next[right]) })
	if len(next) > applicationSessionLimit {
		next = next[:applicationSessionLimit]
	}
	return next
}

func applicationSessionsDelta(previous, current []domain.ChatSession) (applicationSessionDelta, bool) {
	previousByID := make(map[string]domain.ChatSession, len(previous))
	currentIDs := make(map[string]struct{}, len(current))
	for _, session := range previous {
		previousByID[session.ID] = session
	}
	delta := applicationSessionDelta{}
	for _, session := range current {
		currentIDs[session.ID] = struct{}{}
		if old, exists := previousByID[session.ID]; !exists || !applicationSessionEqual(old, session) {
			delta.Sessions = append(delta.Sessions, session)
		}
	}
	for _, session := range previous {
		if _, exists := currentIDs[session.ID]; !exists {
			delta.RemovedIDs = append(delta.RemovedIDs, session.ID)
		}
	}
	return delta, len(delta.Sessions) > 0 || len(delta.RemovedIDs) > 0
}

func (s *Server) applicationSessionsUpdate(ctx context.Context, previousPayload []byte, sessionID string) ([]byte, []byte, bool, error) {
	var previous []domain.ChatSession
	if err := json.Unmarshal(previousPayload, &previous); err != nil {
		return nil, nil, false, fmt.Errorf("decode previous sessions snapshot: %w", err)
	}
	var current []domain.ChatSession
	if sessionID == "" {
		var err error
		current, err = s.applicationSessions(ctx)
		if err != nil {
			return nil, nil, false, err
		}
	} else {
		session, err := s.service.GetChatSession(ctx, sessionID)
		switch {
		case err == nil:
			session.Active = s.chatSessionActive(session.ID)
			current = applicationSessionWindow(previous, session)
		case errors.Is(err, store.ErrNotFound):
			current, err = s.applicationSessions(ctx)
			if err != nil {
				return nil, nil, false, err
			}
		default:
			return nil, nil, false, err
		}
	}
	fullPayload, err := json.Marshal(current)
	if err != nil {
		return nil, nil, false, err
	}
	delta, changed := applicationSessionsDelta(previous, current)
	if !changed {
		return fullPayload, nil, false, nil
	}
	deltaPayload, err := json.Marshal(delta)
	return fullPayload, deltaPayload, err == nil, err
}

func applicationObjectDelta(previousPayload, currentPayload []byte) ([]byte, bool, error) {
	var previous, current map[string]json.RawMessage
	if err := json.Unmarshal(previousPayload, &previous); err != nil {
		return nil, false, err
	}
	if err := json.Unmarshal(currentPayload, &current); err != nil {
		return nil, false, err
	}
	delta := make(map[string]json.RawMessage)
	for key, value := range current {
		if !bytes.Equal(previous[key], value) {
			delta[key] = value
		}
	}
	for key := range previous {
		if _, exists := current[key]; !exists {
			delta[key] = json.RawMessage("null")
		}
	}
	if len(delta) == 0 {
		return nil, false, nil
	}
	payload, err := json.Marshal(delta)
	return payload, err == nil, err
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
			_, intervalTopic := applicationSampleIntervals[topic]
			_, stateTopic := applicationStateTopics[topic]
			_, pushTopic := applicationPushTopics[topic]
			if !intervalTopic && !stateTopic && !pushTopic {
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
		command.MCPSessionID = strings.TrimSpace(command.MCPSessionID)
		taskIDs := make([]string, 0, len(command.TaskIDs))
		seenTaskIDs := make(map[string]struct{}, len(command.TaskIDs))
		for _, value := range command.TaskIDs {
			taskID := strings.TrimSpace(value)
			if taskID == "" {
				continue
			}
			if len(taskID) > 256 {
				return fmt.Errorf("task_id must not exceed 256 bytes")
			}
			if _, exists := seenTaskIDs[taskID]; exists {
				continue
			}
			seenTaskIDs[taskID] = struct{}{}
			taskIDs = append(taskIDs, taskID)
		}
		if len(taskIDs) > applicationTaskSubscriptionLimit {
			return fmt.Errorf("task_ids must not contain more than %d entries", applicationTaskSubscriptionLimit)
		}
		sort.Strings(taskIDs)
		if _, subscribed := topics["chat_state"]; subscribed && command.SessionID == "" {
			return fmt.Errorf("session_id is required for application event topic chat_state")
		}
		if _, subscribed := topics["tasks"]; subscribed && len(taskIDs) == 0 {
			return fmt.Errorf("task_ids is required for application event topic tasks")
		}
		if _, subscribed := topics["tasks"]; subscribed && command.SessionID == "" {
			return fmt.Errorf("session_id is required for application event topic tasks")
		}
		if len(command.SessionID) > 256 {
			return fmt.Errorf("session_id must not exceed 256 bytes")
		}
		if len(command.MCPSessionID) > 256 {
			return fmt.Errorf("mcp_session_id must not exceed 256 bytes")
		}
		next := applicationWebSocketSubscription{topics: topics, logs: command.Logs, sessionID: command.SessionID, mcpSessionID: command.MCPSessionID, taskIDs: taskIDs}
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
		sessions, err := s.applicationSessions(ctx)
		if err != nil {
			return nil, err
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
	case "mcp_activity":
		snapshot, err := s.service.ListMCPActivity(ctx, subscription.mcpSessionID, 100, 200)
		if err != nil {
			return nil, err
		}
		value = snapshot
	default:
		return nil, fmt.Errorf("unsupported application event topic %q", topic)
	}
	return json.Marshal(value)
}

func applicationTaskSnapshots(snapshots map[string]domain.TaskSnapshot) map[string]domain.TaskSnapshot {
	for taskID, snapshot := range snapshots {
		snapshot.Result = applicationTaskResult(snapshot.Result)
		snapshots[taskID] = snapshot
	}
	return snapshots
}

func applicationTaskEvent(event domain.TaskEvent) domain.TaskEvent {
	if event.Snapshot != nil {
		snapshot := *event.Snapshot
		snapshot.Result = applicationTaskResult(snapshot.Result)
		event.Snapshot = &snapshot
	}
	return event
}

func applicationTaskResult(result domain.ExecResult) domain.ExecResult {
	stdoutTotal, stderrTotal := len(result.Stdout), len(result.Stderr)
	result.Stdout, result.StdoutOffsetBytes = applicationOutputTail(result.Stdout, applicationTaskOutputLimit)
	result.Stderr, result.StderrOffsetBytes = applicationOutputTail(result.Stderr, applicationTaskOutputLimit)
	result.StdoutTotalBytes, result.StderrTotalBytes = stdoutTotal, stderrTotal
	result.StdoutOmittedBytes, result.StderrOmittedBytes = result.StdoutOffsetBytes, result.StderrOffsetBytes
	result.OutputLimited = result.StdoutOmittedBytes > 0 || result.StderrOmittedBytes > 0
	if result.OutputLimited {
		result.OutputView = "tail"
	} else {
		result.OutputView = ""
	}
	return result
}

func applicationOutputTail(value string, limit int) (string, int) {
	if limit <= 0 || len(value) <= limit {
		return value, 0
	}
	start := len(value) - limit
	for start < len(value) && !utf8.RuneStart(value[start]) {
		start++
	}
	return value[start:], start
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
