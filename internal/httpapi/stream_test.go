package httpapi

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/agent"
)

type flushRecorder struct {
	*httptest.ResponseRecorder
	flushes int
}

func TestStreamAgentEventsLeavesAgentRunningAfterClientDisconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest("POST", "/api/v1/chat", nil).WithContext(ctx)
	response := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	runStarted := make(chan struct{})
	releaseRun := make(chan struct{})
	runExited := make(chan struct{})
	handlerExited := make(chan struct{})

	go func() {
		streamAgentEvents(response, request, time.Hour, func(emit func(agent.Event)) {
			close(runStarted)
			<-releaseRun
			emit(agent.Event{Type: "done", SessionID: "session_background"})
			close(runExited)
		})
		close(handlerExited)
	}()

	<-runStarted
	cancel()
	select {
	case <-handlerExited:
	case <-time.After(time.Second):
		t.Fatal("SSE handler did not release the disconnected client")
	}
	close(releaseRun)
	select {
	case <-runExited:
	case <-time.After(time.Second):
		t.Fatal("Agent run stopped with the browser connection")
	}
}

func (r *flushRecorder) Flush() {
	r.flushes++
	r.ResponseRecorder.Flush()
}

func TestStreamAgentEventsFlushesHeartbeatsAndTerminalEvent(t *testing.T) {
	request := httptest.NewRequest("POST", "/api/v1/chat", nil)
	response := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}

	streamAgentEvents(response, request, 5*time.Millisecond, func(emit func(agent.Event)) {
		emit(agent.Event{
			Type: "tool", ToolName: "ssh_exec", ToolCallID: "call-live",
			Status: "in_progress", Content: `{"status":"in_progress"}`, SessionID: "session_test",
		})
		time.Sleep(12 * time.Millisecond)
		emit(agent.Event{Type: "done", SessionID: "session_test", Content: "complete"})
	})

	if got := response.Header().Get("Content-Type"); got != "text/event-stream; charset=utf-8" {
		t.Fatalf("content type = %q", got)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-cache, no-transform" {
		t.Fatalf("cache control = %q", got)
	}
	body := response.Body.String()
	if !strings.HasPrefix(body, ": connected\n\n") {
		t.Fatalf("stream did not start immediately: %q", body)
	}
	if strings.Count(body, ": heartbeat\n\n") < 2 {
		t.Fatalf("heartbeats were not emitted while the Agent was quiet: %q", body)
	}
	if !strings.Contains(body, "event: tool\ndata: ") || !strings.Contains(body, `"tool_call_id":"call-live"`) || !strings.Contains(body, `"status":"in_progress"`) {
		t.Fatalf("tool lifecycle event missing: %q", body)
	}
	if !strings.Contains(body, "event: done\ndata: ") || !strings.Contains(body, `"session_id":"session_test"`) {
		t.Fatalf("terminal event missing: %q", body)
	}
	if response.flushes < 5 {
		t.Fatalf("flush count = %d, want connected + tool + heartbeats + terminal event", response.flushes)
	}
}
