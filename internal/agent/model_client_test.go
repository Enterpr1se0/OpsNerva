package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"eino-ops-agent/internal/config"

	"github.com/cloudwego/eino/schema"
)

func TestOpenAIThinkingToolHistoryPreservesEmptyReasoningContent(t *testing.T) {
	requests := make(chan []map[string]json.RawMessage, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var payload struct {
			Messages []map[string]json.RawMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		requests <- payload.Messages
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-final\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"fixture-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"done\"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-final\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"fixture-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	chatModel, err := newChatModel(context.Background(), config.Model{
		APIKey: "fixture-key", BaseURL: server.URL + "/v1", Name: "fixture-model", ReasoningEffort: "medium",
	}, 5*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	toolCall := schema.ToolCall{
		ID: "call-1", Type: "function",
		Function: schema.FunctionCall{Name: "inspect", Arguments: `{}`},
	}
	stream, err := chatModel.Stream(context.Background(), []*schema.Message{
		schema.UserMessage("inspect the target"),
		schema.AssistantMessage("", []schema.ToolCall{toolCall}),
		schema.ToolMessage(`{"status":"completed"}`, toolCall.ID, schema.WithToolName(toolCall.Function.Name)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := schema.ConcatMessageStream(stream); err != nil {
		t.Fatal(err)
	}

	messages := <-requests
	if len(messages) != 3 {
		t.Fatalf("request messages = %#v", messages)
	}
	reasoningContent, ok := messages[1]["reasoning_content"]
	if !ok {
		t.Fatal("assistant tool-call message omitted reasoning_content")
	}
	if string(reasoningContent) != `""` {
		t.Fatalf("reasoning_content = %s, want empty string", reasoningContent)
	}
}

func TestOpenAIStandardToolHistoryDoesNotAddEmptyReasoningContent(t *testing.T) {
	toolCall := schema.ToolCall{
		ID: "call-1", Type: "function",
		Function: schema.FunctionCall{Name: "inspect", Arguments: `{}`},
	}
	messages := []*schema.Message{
		schema.UserMessage("inspect the target"),
		schema.AssistantMessage("", []schema.ToolCall{toolCall}),
	}
	if shouldPreserveReasoningContent(messages, false) {
		t.Fatal("standard tool-call history was treated as thinking mode")
	}
	messages[1].ReasoningContent = "checking the target"
	if !shouldPreserveReasoningContent(messages, false) {
		t.Fatal("existing reasoning content did not activate protocol preservation")
	}
}

func TestPreserveReasoningContentPayloadRestoresEveryToolCallMessage(t *testing.T) {
	first := schema.AssistantMessage("", []schema.ToolCall{{
		ID: "call-1", Type: "function", Function: schema.FunctionCall{Name: "first", Arguments: `{}`},
	}})
	first.ReasoningContent = "first thought"
	second := schema.AssistantMessage("", []schema.ToolCall{{
		ID: "call-2", Type: "function", Function: schema.FunctionCall{Name: "second", Arguments: `{}`},
	}})
	raw := []byte(`{"model":"fixture","messages":[{"role":"assistant","tool_calls":[{}]},{"role":"tool","content":"ok"},{"role":"assistant","tool_calls":[{}]}]}`)
	encoded, err := preserveReasoningContentPayload(context.Background(), []*schema.Message{
		first,
		schema.ToolMessage("ok", "call-1"),
		second,
	}, raw)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	if string(payload.Messages[0]["reasoning_content"]) != `"first thought"` {
		t.Fatalf("first reasoning_content = %s", payload.Messages[0]["reasoning_content"])
	}
	if string(payload.Messages[2]["reasoning_content"]) != `""` {
		t.Fatalf("second reasoning_content = %s", payload.Messages[2]["reasoning_content"])
	}
}
