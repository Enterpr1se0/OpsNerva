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
