package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/config"

	"github.com/cloudwego/eino/schema"
)

func TestModelHTTPClientDoesNotAddRequestTimeout(t *testing.T) {
	client, err := modelHTTPClient(config.Model{Kind: "openai_compatible"})
	if err != nil || client != nil {
		t.Fatalf("plain OpenAI-compatible client = %#v, %v; want SDK client with no configured timeout", client, err)
	}

	client, err = modelHTTPClient(config.Model{Kind: "openai_compatible", ProxyURL: "http://127.0.0.1:7890"})
	if err != nil {
		t.Fatal(err)
	}
	if client.Timeout != 0 {
		t.Fatalf("proxy model client timeout = %v, want 0", client.Timeout)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("proxy model transport = %T, want *http.Transport", client.Transport)
	}
	if transport.ResponseHeaderTimeout != 0 {
		t.Fatalf("proxy model response-header timeout = %v, want 0", transport.ResponseHeaderTimeout)
	}

	client, err = modelHTTPClient(config.Model{Kind: "anthropic"})
	if err != nil || client == nil || client.Timeout != 0 || client.CheckRedirect == nil {
		t.Fatalf("Anthropic model client = %#v, %v", client, err)
	}
}

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
	}, 0)
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

func TestAnthropicHistorySendsPersistedThinkingSignature(t *testing.T) {
	requests := make(chan []struct {
		Role    string `json:"role"`
		Content []struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
		} `json:"content"`
	}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var payload struct {
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Type      string `json:"type"`
					Thinking  string `json:"thinking"`
					Signature string `json:"signature"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		requests <- payload.Messages
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_fixture","type":"message","role":"assistant","model":"claude-fixture","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer server.Close()

	chatModel, err := newChatModel(context.Background(), config.Model{
		APIKey: "fixture-key", Kind: "anthropic", BaseURL: server.URL, Name: "claude-fixture",
	}, 256)
	if err != nil {
		t.Fatal(err)
	}
	thinking := schema.AssistantMessage("", nil)
	thinking.ReasoningContent = "I checked the prior result."
	thinking.Extra = map[string]any{
		claudeThinkingExtraKey:  thinking.ReasoningContent,
		claudeSignatureExtraKey: "signed-thinking",
	}
	if _, err := chatModel.Generate(context.Background(), []*schema.Message{
		schema.UserMessage("check"), thinking, schema.AssistantMessage("healthy", nil), schema.UserMessage("continue"),
	}); err != nil {
		t.Fatal(err)
	}

	messages := <-requests
	if len(messages) != 4 || messages[1].Role != "assistant" || len(messages[1].Content) != 1 {
		t.Fatalf("Anthropic messages = %#v", messages)
	}
	block := messages[1].Content[0]
	if block.Type != "thinking" || block.Thinking != thinking.ReasoningContent || block.Signature != "signed-thinking" {
		t.Fatalf("Anthropic thinking block = %#v", block)
	}
}

func TestStreamedAnthropicThinkingMetadataCanBePersisted(t *testing.T) {
	chunks := []*schema.Message{
		{Role: schema.Assistant, ReasoningContent: "check ", Extra: map[string]any{claudeThinkingExtraKey: "check "}},
		{Role: schema.Assistant, ReasoningContent: "state", Extra: map[string]any{claudeThinkingExtraKey: "state"}},
		{Role: schema.Assistant, Extra: map[string]any{claudeSignatureExtraKey: "signed-thinking"}},
	}
	merged, err := schema.ConcatMessages(chunks)
	if err != nil {
		t.Fatal(err)
	}
	extra := persistedReasoningModelExtra(merged)
	if extra[claudeThinkingExtraKey] != "check state" || extra[claudeSignatureExtraKey] != "signed-thinking" {
		t.Fatalf("persisted streamed thinking metadata = %#v", extra)
	}
}
