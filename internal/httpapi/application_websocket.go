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
	"time"
	"unicode/utf8"

	"github.com/Enterpr1se0/opsnerva/internal/agent"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/observability"
	"github.com/Enterpr1se0/opsnerva/internal/service"
	"github.com/Enterpr1se0/opsnerva/internal/store"

	"golang.org/x/net/websocket"
)

const applicationSnapshotCacheTTL = 200 * time.Millisecond
const applicationSnapshotCacheLimit = 256
const applicationTaskOutputLimit = 64 << 10
const applicationSessionLimit = 50

type applicationTopicState struct {
	payload []byte
	next    time.Time
}

type applicationSnapshotCacheEntry struct {
	payload []byte
	expires time.Time
}

type applicationConnectionResponse struct {
	Tunnels []domain.SSHTunnel `json:"tunnels"`
	Shells  []domain.SSHShell  `json:"shells"`
}

type applicationSessionDelta struct {
	Sessions   []domain.ChatSession `json:"sessions,omitempty"`
	RemovedIDs []string             `json:"removed_ids,omitempty"`
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
	var mcpEvents <-chan domain.MCPActivityEvent
	var unsubscribeMCP func()
	var taskEvents <-chan domain.TaskEvent
	var unsubscribeTasks func()
	var logEvents <-chan observability.LogEntry
	var logOverflow <-chan struct{}
	unsubscribeLogs := func() {}
	var modelTestEvents <-chan modelTestJob
	var modelTestOverflow <-chan struct{}
	unsubscribeModelTests := func() {}
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
		unsubscribeLogs()
		unsubscribeModelTests()
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
		payload, err := s.applicationTopicSnapshot(ctx, topic, subscription)
		if err != nil {
			return websocket.JSON.Send(connection, applicationWebSocketEvent{Type: "error", Topic: topic, Error: err.Error()})
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
			modelTestsChanged := applicationSubscriptionTopicChanged(previous, next, "model_tests")
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
			if logsChanged {
				unsubscribeLogs()
				unsubscribeLogs = func() {}
				logEvents = nil
				logOverflow = nil
			}
			if modelTestsChanged {
				unsubscribeModelTests()
				unsubscribeModelTests = func() {}
				modelTestEvents = nil
				modelTestOverflow = nil
			}
			subscription = next
			for topic := range states {
				if _, subscribed := subscription.topics[topic]; !subscribed || applicationSubscriptionTopicChanged(previous, subscription, topic) {
					delete(states, topic)
				}
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
			if _, subscribed := subscription.topics["logs"]; subscribed && logsChanged {
				logSubscription := observability.Subscribe(applicationLogFilter(subscription.logs))
				logEvents, logOverflow, unsubscribeLogs = logSubscription.Events, logSubscription.Overflow, logSubscription.Unsubscribe
				payload, err := json.Marshal(applicationLogSnapshot(logSubscription.Entries))
				if err != nil {
					_ = websocket.JSON.Send(connection, applicationWebSocketEvent{Type: "error", Topic: "logs", Error: err.Error()})
				} else if send("logs", "snapshot", payload) != nil {
					return
				}
			}
			if _, subscribed := subscription.topics["model_tests"]; subscribed && modelTestsChanged {
				if s.modelTests == nil {
					_ = websocket.JSON.Send(connection, applicationWebSocketEvent{Type: "error", Topic: "model_tests", Error: "model tests are unavailable"})
				} else {
					modelTestSubscription := s.modelTests.subscribe(subscription.modelTestIDs)
					modelTestEvents, modelTestOverflow, unsubscribeModelTests = modelTestSubscription.Events, modelTestSubscription.Overflow, modelTestSubscription.Unsubscribe
					payload, err := json.Marshal(modelTestSubscription.Jobs)
					if err != nil {
						_ = websocket.JSON.Send(connection, applicationWebSocketEvent{Type: "error", Topic: "model_tests", Error: err.Error()})
					} else if send("model_tests", "snapshot", payload) != nil {
						return
					}
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
		case entry := <-logEvents:
			entries := []observability.LogEntry{entry}
			draining := true
			for draining && len(entries) < 128 {
				select {
				case entry = <-logEvents:
					entries = append(entries, entry)
				default:
					draining = false
				}
			}
			slices.Reverse(entries)
			payload, err := json.Marshal(applicationLogResponse{Entries: entries})
			if err == nil && send("logs", "delta", payload) != nil {
				return
			}
		case <-logOverflow:
			unsubscribeLogs()
			logSubscription := observability.Subscribe(applicationLogFilter(subscription.logs))
			logEvents, logOverflow, unsubscribeLogs = logSubscription.Events, logSubscription.Overflow, logSubscription.Unsubscribe
			payload, err := json.Marshal(applicationLogSnapshot(logSubscription.Entries))
			if err == nil && send("logs", "snapshot", payload) != nil {
				return
			}
		case job := <-modelTestEvents:
			payload, err := json.Marshal(job)
			if err == nil && send("model_tests", "delta", payload) != nil {
				return
			}
		case <-modelTestOverflow:
			unsubscribeModelTests()
			modelTestSubscription := s.modelTests.subscribe(subscription.modelTestIDs)
			modelTestEvents, modelTestOverflow, unsubscribeModelTests = modelTestSubscription.Events, modelTestSubscription.Overflow, modelTestSubscription.Unsubscribe
			payload, err := json.Marshal(modelTestSubscription.Jobs)
			if err == nil && send("model_tests", "snapshot", payload) != nil {
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
