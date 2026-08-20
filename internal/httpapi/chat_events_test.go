package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"eino-ops-agent/internal/agent"
)

func TestChatEventHubReplaysMissedEventsAndContinuesLive(t *testing.T) {
	hub := newChatEventHub()
	first := hub.publish("session_test", agent.Event{Type: "session"})
	second := hub.publish("session_test", agent.Event{Type: "reasoning", Content: "inspect"})
	if first.EventID != 1 || second.EventID != 2 {
		t.Fatalf("event ids = %d, %d", first.EventID, second.EventID)
	}
	replay, events, done, unsubscribe := hub.subscribe("session_test", 1)
	defer unsubscribe()
	if done || len(replay) != 1 || replay[0].EventID != 2 || replay[0].Content != "inspect" {
		t.Fatalf("replay = %#v, done = %t", replay, done)
	}
	third := hub.publish("session_test", agent.Event{Type: "message", Content: "ready"})
	select {
	case event := <-events:
		if event.EventID != third.EventID || event.Content != "ready" {
			t.Fatalf("live event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("live event was not delivered")
	}
	doneEvent := hub.publish("session_test", agent.Event{Type: "done"})
	select {
	case event := <-events:
		if event.EventID != doneEvent.EventID || event.Type != "done" {
			t.Fatalf("terminal event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal event was not delivered")
	}
	if _, open := <-events; open {
		t.Fatal("terminal event did not close the subscription")
	}
}

func TestChatEventHubStartsFreshSequenceForNextTurn(t *testing.T) {
	hub := newChatEventHub()
	hub.publish("session_test", agent.Event{Type: "session"})
	hub.publish("session_test", agent.Event{Type: "done"})
	next := hub.publish("session_test", agent.Event{Type: "session"})
	if next.EventID != 1 {
		t.Fatalf("next turn event id = %d", next.EventID)
	}
	replay, _, done, unsubscribe := hub.subscribe("session_test", 0)
	defer unsubscribe()
	if done || len(replay) != 1 || replay[0].Type != "session" {
		t.Fatalf("next turn replay = %#v, done = %t", replay, done)
	}
}

func TestModelErrorTerminatesTheModelEventStream(t *testing.T) {
	hub := newChatEventHub()
	hub.publish("session_test", agent.Event{Type: "session"})
	hub.publish("session_test", agent.Event{Type: "tool", ToolCallID: "call-live", Status: "in_progress"})
	hub.publish("session_test", agent.Event{Type: "model_error", Error: "provider unavailable"})
	replay, _, done, unsubscribe := hub.subscribe("session_test", 0)
	defer unsubscribe()
	if !done || len(replay) != 3 || replay[2].Type != "model_error" {
		t.Fatalf("model error stream = replay %#v, done %t", replay, done)
	}
}

func TestChatEventHubReplaysMessageLifecycleWithStableID(t *testing.T) {
	hub := newChatEventHub()
	messageID := "msg_lifecycle"
	hub.publish("session_test", agent.Event{Type: "message_start", MessageID: messageID, Role: "assistant"})
	hub.publish("session_test", agent.Event{Type: "message", MessageID: messageID, Role: "assistant", Content: "ready"})
	hub.publish("session_test", agent.Event{Type: "message_commit", MessageID: messageID, Role: "assistant"})

	replay, _, done, unsubscribe := hub.subscribe("session_test", 0)
	defer unsubscribe()
	if done || len(replay) != 3 {
		t.Fatalf("replay = %#v, done = %t", replay, done)
	}
	for index, event := range replay {
		if event.EventID != uint64(index+1) || event.MessageID != messageID {
			t.Fatalf("lifecycle replay[%d] = %#v", index, event)
		}
	}
}

func TestQueuedTurnCompletionDoesNotCloseChatEventStream(t *testing.T) {
	hub := newChatEventHub()
	hub.publish("session_test", agent.Event{Type: "turn_done"})
	hub.publish("session_test", agent.Event{Type: "queue_started", MessageID: "queue_1"})
	replay, _, done, unsubscribe := hub.subscribe("session_test", 0)
	defer unsubscribe()
	if done || len(replay) != 2 {
		t.Fatalf("queued turn replay = %#v, done = %t", replay, done)
	}
}

func TestSteeredTurnDoesNotCloseChatEventStream(t *testing.T) {
	hub := newChatEventHub()
	hub.publish("session_test", agent.Event{Type: "turn_steered", UserMessageID: "msg_current"})
	hub.publish("session_test", agent.Event{Type: "queue_started", MessageID: "queue_steer", QueueMode: chatQueueModeSteering})
	replay, events, done, unsubscribe := hub.subscribe("session_test", 0)
	defer unsubscribe()
	if done || events == nil || len(replay) != 2 || replay[0].Type != "turn_steered" || replay[1].QueueMode != chatQueueModeSteering {
		t.Fatalf("steering replay = %#v, done = %t", replay, done)
	}
}

func TestChatEventsStreamReturnsReplayWithSSEEventIDs(t *testing.T) {
	hub := newChatEventHub()
	const userMessageID = "msg_user"
	hub.publish("session_test", agent.Event{Type: "session"})
	hub.publish("session_test", agent.Event{Type: "message", UserMessageID: userMessageID, Content: "complete"})
	hub.publish("session_test", agent.Event{Type: "done", UserMessageID: userMessageID})
	server := &Server{agent: &agent.Runtime{}, chatEvents: hub}
	request := httptest.NewRequest("GET", "/api/v1/chat/session_test/events?after=1", nil)
	request.SetPathValue("id", "session_test")
	response := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}

	server.chatEventsStream(response, request)

	if response.Code != 200 {
		t.Fatalf("status = %d", response.Code)
	}
	body := response.Body.String()
	if strings.Contains(body, "id: 1\n") || !strings.Contains(body, "id: 2\nevent: message\n") || !strings.Contains(body, "id: 3\nevent: done\n") ||
		strings.Count(body, `"user_message_id":"`+userMessageID+`"`) != 2 {
		t.Fatalf("replay body = %q", body)
	}
	if response.flushes < 3 {
		t.Fatalf("flushes = %d", response.flushes)
	}
}
