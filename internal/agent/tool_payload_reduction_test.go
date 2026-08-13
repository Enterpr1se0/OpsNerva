package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

func TestToolPayloadReductionBoundsCurrentModelRequestAndPreservesOriginalEvents(t *testing.T) {
	arguments := `{"path":"/tmp/large","content":"` + strings.Repeat("a", modelToolPayloadMaxBytes) + `"}`
	result := `{"status":"completed","stdout":"` + strings.Repeat("b", modelToolPayloadMaxBytes) + `"}`
	assistant := schema.AssistantMessage("", []schema.ToolCall{{
		ID: "call-large", Type: "function", Function: schema.FunctionCall{Name: "workspace_file_edit", Arguments: arguments},
	}})
	toolResult := schema.ToolMessage(result, "call-large", schema.WithToolName("workspace_file_edit"))
	state := &adk.ChatModelAgentState{Messages: []*schema.Message{assistant, toolResult}}

	middleware, err := newToolReductionMiddleware(context.Background(), []ToolDescriptor{{Name: "workspace_file_edit"}})
	if err != nil {
		t.Fatal(err)
	}
	_, reduced, err := middleware.BeforeModelRewriteState(context.Background(), state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reduced.Messages[0] == assistant || reduced.Messages[1] == toolResult {
		t.Fatal("tool reduction mutated the event messages instead of returning a model-only copy")
	}
	if assistant.ToolCalls[0].Function.Arguments != arguments || toolResult.Content != result {
		t.Fatal("original tool event content was changed")
	}
	reducedArguments := reduced.Messages[0].ToolCalls[0].Function.Arguments
	reducedResult := reduced.Messages[1].Content
	if len(reducedArguments) > modelReducedToolArgumentBytes || len(reducedResult) > modelReducedToolResultBytes {
		t.Fatalf("reduced payload sizes = arguments %d, result %d", len(reducedArguments), len(reducedResult))
	}
	for _, value := range []string{reducedArguments, reducedResult} {
		var payload compactedModelPayload
		if err := json.Unmarshal([]byte(value), &payload); err != nil || !payload.Reduced || payload.OriginalBytes == 0 {
			t.Fatalf("reduced payload = %q, %v", value, err)
		}
	}
}

func TestToolPayloadReductionBoundsAggregateToolContext(t *testing.T) {
	messages := make([]*schema.Message, 0, 12)
	for index := 0; index < 6; index++ {
		callID := string(rune('a' + index))
		messages = append(messages,
			schema.AssistantMessage("", []schema.ToolCall{{
				ID: callID, Function: schema.FunctionCall{Name: "tool", Arguments: strings.Repeat("a", 16<<10)},
			}}),
			schema.ToolMessage(strings.Repeat("b", 48<<10), callID, schema.WithToolName("tool")),
		)
	}
	state := &adk.ChatModelAgentState{Messages: messages}
	middleware, err := newToolReductionMiddleware(context.Background(), []ToolDescriptor{{Name: "tool"}})
	if err != nil {
		t.Fatal(err)
	}
	_, reduced, err := middleware.BeforeModelRewriteState(context.Background(), state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if size := toolPayloadBytes(reduced.Messages); size > modelToolPayloadMaxBytes {
		t.Fatalf("aggregate tool payload = %d, want <= %d", size, modelToolPayloadMaxBytes)
	}
}

func TestToolPayloadReductionLeavesSmallContextUntouched(t *testing.T) {
	state := &adk.ChatModelAgentState{Messages: []*schema.Message{
		schema.AssistantMessage("", []schema.ToolCall{{ID: "small", Function: schema.FunctionCall{Name: "tool", Arguments: `{}`}}}),
		schema.ToolMessage(`{"ok":true}`, "small", schema.WithToolName("tool")),
	}}
	middleware, err := newToolReductionMiddleware(context.Background(), []ToolDescriptor{{Name: "tool"}})
	if err != nil {
		t.Fatal(err)
	}
	_, reduced, err := middleware.BeforeModelRewriteState(context.Background(), state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reduced != state {
		t.Fatal("small tool context was copied or reduced")
	}
}

func TestWebToolPayloadReductionPreservesStructuredSources(t *testing.T) {
	searchResult := `{"tool_version":"1.1","ok":true,"code":"completed","query":"current release","provider":"tavily","request_id":"req-search","results":[` +
		`{"title":"Official release","url":"https://go.dev/release","content":"` + strings.Repeat("界🙂", 15000) + `","score":0.99,"published_date":"2026-08-01","original_bytes":105000,"returned_bytes":105000},` +
		`{"title":"Release notes","url":"https://go.dev/doc/devel/release","content":"` + strings.Repeat("notes ", 15000) + `","score":0.95}],"content_is_untrusted":true}`
	assistant := schema.AssistantMessage("", []schema.ToolCall{{
		ID: "web-search", Type: "function", Function: schema.FunctionCall{Name: "web_search", Arguments: `{"query":"current release"}`},
	}})
	toolResult := schema.ToolMessage(searchResult, "web-search", schema.WithToolName("web_search"))
	state := &adk.ChatModelAgentState{Messages: []*schema.Message{assistant, toolResult}}
	middleware, err := newToolReductionMiddleware(context.Background(), []ToolDescriptor{{Name: "web_search"}})
	if err != nil {
		t.Fatal(err)
	}
	_, reduced, err := middleware.BeforeModelRewriteState(context.Background(), state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reduced.Messages[1] == toolResult || toolResult.Content != searchResult {
		t.Fatal("web reduction did not preserve the original event")
	}
	if len(reduced.Messages[1].Content) > modelReducedToolResultBytes {
		t.Fatalf("reduced Web result = %d bytes", len(reduced.Messages[1].Content))
	}
	var payload webReducedPayload
	if err := json.Unmarshal([]byte(reduced.Messages[1].Content), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.ContextReduced || payload.ContextSourceBytes != len(searchResult) || payload.Query != "current release" || payload.RequestID != "req-search" || len(payload.Results) != 2 {
		t.Fatalf("reduced Web envelope = %#v", payload)
	}
	if payload.Results[0].URL != "https://go.dev/release" || payload.Results[0].Title != "Official release" || payload.Results[1].URL != "https://go.dev/doc/devel/release" {
		t.Fatalf("reduced Web sources = %#v", payload.Results)
	}
}

func TestWebExtractPayloadReductionPreservesFailuresAndUTF8(t *testing.T) {
	extractResult := `{"tool_version":"1.1","ok":true,"code":"partial","provider":"tavily","results":[{"url":"https://example.com/docs","raw_content":"` + strings.Repeat("文🙂", 5000) + `"}],"failed_results":[{"url":"https://example.org/missing","error":"blocked"}],"content_is_untrusted":true}`
	reduced, ok := reduceWebToolJSON(extractResult, modelReducedToolResultBytes)
	if !ok || len(reduced) > modelReducedToolResultBytes {
		t.Fatalf("Web extract reduction failed: ok=%v bytes=%d", ok, len(reduced))
	}
	var payload webReducedPayload
	if err := json.Unmarshal([]byte(reduced), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Results) != 1 || payload.Results[0].URL != "https://example.com/docs" || !strings.HasPrefix(payload.Results[0].RawContent, "文🙂") || len(payload.FailedResults) != 1 || payload.FailedResults[0].URL != "https://example.org/missing" {
		t.Fatalf("reduced extract structure = %#v", payload)
	}
}

func TestWebToolArgumentReductionPreservesUsableURLsAndQuery(t *testing.T) {
	arguments, err := json.Marshal(map[string]any{
		"query":         strings.Repeat("focused query ", 120),
		"extract_depth": "advanced",
		"urls": []string{
			"https://example.com/" + strings.Repeat("a", 900),
			"https://example.org/" + strings.Repeat("b", 900),
			"https://example.net/" + strings.Repeat("c", 900),
			"https://example.edu/" + strings.Repeat("d", 900),
			"https://example.dev/" + strings.Repeat("e", 900),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	reduced, ok := reduceWebToolArguments(string(arguments), modelReducedWebArgumentBytes)
	if !ok || len(reduced) > modelReducedWebArgumentBytes {
		t.Fatalf("Web arguments reduction failed: ok=%v bytes=%d", ok, len(reduced))
	}
	var payload webReducedArguments
	if err := json.Unmarshal([]byte(reduced), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.ContextReduced || payload.ContextSourceBytes != len(arguments) || payload.Query == "" || len(payload.Query) > 512 || payload.ExtractDepth != "advanced" || len(payload.URLs) == 0 || payload.OmittedURLs == 0 {
		t.Fatalf("reduced Web arguments = %#v", payload)
	}
	for _, sourceURL := range payload.URLs {
		if !strings.HasPrefix(sourceURL, "https://example.") {
			t.Fatalf("reduced URL is not usable: %q", sourceURL)
		}
	}
}
