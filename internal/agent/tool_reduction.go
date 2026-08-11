package agent

import (
	"context"
	"encoding/json"

	"eino-ops-agent/internal/observability"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/middlewares/reduction"
	"github.com/cloudwego/eino/schema"
)

const (
	modelReducedToolArgumentBytes = 2 << 10
	modelReducedToolResultBytes   = 8 << 10
	modelToolPayloadMaxBytes      = 96 << 10
	modelStoredToolResultMaxBytes = 48 << 10
	modelHistoryMaxBytes          = 512 << 10
)

// newToolReductionMiddleware uses Eino's reduction lifecycle so the original
// tool events can still be persisted for audit and UI use while only the state
// sent to the next ChatModel call is compacted. ClearAtLeastTokens makes Eino
// edit a copy of the messages instead of mutating already-published events.
func newToolReductionMiddleware(ctx context.Context, descriptors []ToolDescriptor) (adk.ChatModelAgentMiddleware, error) {
	toolConfig := make(map[string]*reduction.ToolReductionConfig, len(descriptors))
	for _, descriptor := range descriptors {
		toolConfig[descriptor.Name] = &reduction.ToolReductionConfig{
			SkipTruncation: true,
			ClearHandler:   reduceToolPayload,
		}
	}
	return reduction.New(ctx, &reduction.Config{
		SkipTruncation:            true,
		MaxTokensForClear:         modelToolPayloadMaxBytes / 4,
		ClearRetentionSuffixLimit: -1,
		ClearAtLeastTokens:        1,
		TokenCounter:              toolPayloadTokenCounter,
		ToolConfig:                toolConfig,
		ClearPostProcess: func(clearCtx context.Context, state *adk.ChatModelAgentState) context.Context {
			observability.FromContext(clearCtx).InfoContext(clearCtx, "reduced oversized tool payload before model request",
				"tool_payload_bytes", toolPayloadBytes(state.Messages))
			return clearCtx
		},
	})
}

func toolPayloadTokenCounter(_ context.Context, messages []*schema.Message, _ []*schema.ToolInfo) (int64, error) {
	return int64((toolPayloadBytes(messages) + 3) / 4), nil
}

func toolPayloadBytes(messages []*schema.Message) int {
	total := 0
	for _, message := range messages {
		if message == nil {
			continue
		}
		for _, call := range message.ToolCalls {
			total += len(call.Function.Arguments)
		}
		if message.Role == schema.Tool {
			total += len(message.Content)
		}
	}
	return total
}

func reduceToolPayload(_ context.Context, detail *reduction.ToolDetail) (*reduction.ClearResult, error) {
	if detail == nil || detail.ToolArgument == nil || detail.ToolResult == nil {
		return &reduction.ClearResult{}, nil
	}
	changed := false
	argument := detail.ToolArgument.Text
	if len(argument) > 256 {
		argument = reducedModelPayload(argument, modelReducedToolArgumentBytes, true)
		changed = true
	}
	parts := append([]schema.ToolOutputPart(nil), detail.ToolResult.Parts...)
	for index := range parts {
		if parts[index].Type != schema.ToolPartTypeText || len(parts[index].Text) <= 512 {
			continue
		}
		parts[index].Text = reducedModelPayload(parts[index].Text, modelReducedToolResultBytes, true)
		changed = true
	}
	if !changed {
		return &reduction.ClearResult{}, nil
	}
	return &reduction.ClearResult{
		NeedClear:    true,
		ToolArgument: &schema.ToolArgument{Text: argument},
		ToolResult:   &schema.ToolResult{Parts: parts},
	}, nil
}

type compactedModelPayload struct {
	Reduced       bool   `json:"_context_reduced"`
	OriginalBytes int    `json:"original_bytes"`
	Preview       string `json:"preview,omitempty"`
}

func compactModelPayload(content string, maxBytes int, includePreview bool) string {
	if maxBytes > 0 && len(content) <= maxBytes {
		return content
	}
	return reducedModelPayload(content, maxBytes, includePreview)
}

func reducedModelPayload(content string, maxBytes int, includePreview bool) string {
	payload := compactedModelPayload{Reduced: true, OriginalBytes: len(content)}
	if !includePreview || maxBytes <= 0 {
		encoded, _ := json.Marshal(payload)
		return string(encoded)
	}
	previewBytes := maxBytes / 3
	for previewBytes > 0 {
		prefixBytes := previewBytes / 2
		suffixBytes := previewBytes - prefixBytes
		payload.Preview = content[:min(prefixBytes, len(content))]
		if suffixBytes > 0 && len(content) > prefixBytes {
			start := max(prefixBytes, len(content)-suffixBytes)
			payload.Preview += "\n...[content reduced]...\n" + content[start:]
		}
		encoded, _ := json.Marshal(payload)
		if len(encoded) <= maxBytes {
			return string(encoded)
		}
		previewBytes /= 2
	}
	payload.Preview = ""
	encoded, _ := json.Marshal(payload)
	return string(encoded)
}
