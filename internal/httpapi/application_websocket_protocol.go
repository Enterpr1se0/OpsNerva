package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/service"

	"golang.org/x/net/websocket"
)

const applicationWebSocketMaxMessage = 16 << 10
const applicationTaskSubscriptionLimit = 32
const applicationModelTestSubscriptionLimit = 32

var applicationSampleIntervals = map[string]time.Duration{
	"connections": 5 * time.Second,
	"health":      30 * time.Second,
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
	"logs":         {},
	"model_tests":  {},
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
	ModelTestIDs []string                      `json:"model_test_ids,omitempty"`
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
	modelTestIDs []string
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
	case "model_tests":
		return !slices.Equal(previous.modelTestIDs, next.modelTestIDs)
	default:
		return false
	}
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
		taskIDs, err := applicationSubscriptionIDs(command.TaskIDs, "task_id", applicationTaskSubscriptionLimit)
		if err != nil {
			return err
		}
		modelTestIDs, err := applicationSubscriptionIDs(command.ModelTestIDs, "model_test_id", applicationModelTestSubscriptionLimit)
		if err != nil {
			return err
		}
		if _, subscribed := topics["chat_state"]; subscribed && command.SessionID == "" {
			return fmt.Errorf("session_id is required for application event topic chat_state")
		}
		if _, subscribed := topics["tasks"]; subscribed && len(taskIDs) == 0 {
			return fmt.Errorf("task_ids is required for application event topic tasks")
		}
		if _, subscribed := topics["tasks"]; subscribed && command.SessionID == "" {
			return fmt.Errorf("session_id is required for application event topic tasks")
		}
		if _, subscribed := topics["model_tests"]; subscribed && len(modelTestIDs) == 0 {
			return fmt.Errorf("model_test_ids is required for application event topic model_tests")
		}
		if len(command.SessionID) > 256 {
			return fmt.Errorf("session_id must not exceed 256 bytes")
		}
		if len(command.MCPSessionID) > 256 {
			return fmt.Errorf("mcp_session_id must not exceed 256 bytes")
		}
		next := applicationWebSocketSubscription{topics: topics, logs: command.Logs, sessionID: command.SessionID, mcpSessionID: command.MCPSessionID, taskIDs: taskIDs, modelTestIDs: modelTestIDs}
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

func applicationSubscriptionIDs(values []string, field string, limit int) ([]string, error) {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		id := strings.TrimSpace(value)
		if id == "" {
			continue
		}
		if len(id) > 256 {
			return nil, fmt.Errorf("%s must not exceed 256 bytes", field)
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	if len(result) > limit {
		return nil, fmt.Errorf("%ss must not contain more than %d entries", field, limit)
	}
	sort.Strings(result)
	return result, nil
}
