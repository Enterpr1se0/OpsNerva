package agent

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/Enterpr1se0/opsnerva/internal/observability"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/middlewares/reduction"
	"github.com/cloudwego/eino/schema"
)

const (
	modelReducedToolArgumentBytes = 2 << 10
	modelReducedWebArgumentBytes  = 4 << 10
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
		clearHandler := reduceToolPayload
		if descriptor.Name == "web_search" || descriptor.Name == "web_extract" {
			clearHandler = reduceWebToolPayload
		}
		toolConfig[descriptor.Name] = &reduction.ToolReductionConfig{
			SkipTruncation: true,
			ClearHandler:   clearHandler,
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

func reduceWebToolPayload(ctx context.Context, detail *reduction.ToolDetail) (*reduction.ClearResult, error) {
	if detail == nil || detail.ToolArgument == nil || detail.ToolResult == nil {
		return &reduction.ClearResult{}, nil
	}
	argument := detail.ToolArgument.Text
	changed := false
	if len(argument) > 256 {
		if reduced, ok := reduceWebToolArguments(argument, modelReducedWebArgumentBytes); ok {
			argument = reduced
		} else {
			argument = reducedModelPayload(argument, modelReducedToolArgumentBytes, true)
		}
		changed = true
	}
	parts := append([]schema.ToolOutputPart(nil), detail.ToolResult.Parts...)
	for index := range parts {
		if parts[index].Type != schema.ToolPartTypeText || len(parts[index].Text) <= 512 {
			continue
		}
		reduced, ok := reduceWebToolJSON(parts[index].Text, modelReducedToolResultBytes)
		if !ok {
			fallback, err := reduceToolPayload(ctx, detail)
			return fallback, err
		}
		parts[index].Text = reduced
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

type webReducedArguments struct {
	Query              string   `json:"query,omitempty"`
	URLs               []string `json:"urls,omitempty"`
	MaxResults         int      `json:"max_results,omitempty"`
	Topic              string   `json:"topic,omitempty"`
	SearchDepth        string   `json:"search_depth,omitempty"`
	ExtractDepth       string   `json:"extract_depth,omitempty"`
	TimeRange          string   `json:"time_range,omitempty"`
	StartDate          string   `json:"start_date,omitempty"`
	EndDate            string   `json:"end_date,omitempty"`
	ChunksPerSource    int      `json:"chunks_per_source,omitempty"`
	IncludeDomains     []string `json:"include_domains,omitempty"`
	ExcludeDomains     []string `json:"exclude_domains,omitempty"`
	ContextReduced     bool     `json:"_context_reduced"`
	ContextSourceBytes int      `json:"_context_source_bytes"`
	OmittedURLs        int      `json:"omitted_urls,omitempty"`
}

func reduceWebToolArguments(content string, maxBytes int) (string, bool) {
	var payload webReducedArguments
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return "", false
	}
	payload.ContextReduced = true
	payload.ContextSourceBytes = len(content)
	payload.Query = compactWebHistoryText(payload.Query, 512)
	for {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return "", false
		}
		if maxBytes <= 0 || len(encoded) <= maxBytes {
			return string(encoded), true
		}
		switch {
		case len(payload.Query) > 128:
			payload.Query = compactWebHistoryText(payload.Query, len(payload.Query)/2)
		case len(payload.URLs) > 0:
			payload.URLs = payload.URLs[:len(payload.URLs)-1]
			payload.OmittedURLs++
		case len(payload.IncludeDomains) > 0:
			payload.IncludeDomains = payload.IncludeDomains[:len(payload.IncludeDomains)-1]
		case len(payload.ExcludeDomains) > 0:
			payload.ExcludeDomains = payload.ExcludeDomains[:len(payload.ExcludeDomains)-1]
		default:
			return reducedModelPayload(content, maxBytes, false), true
		}
	}
}

type webReducedResult struct {
	Title         string  `json:"title,omitempty"`
	URL           string  `json:"url"`
	Content       string  `json:"content,omitempty"`
	RawContent    string  `json:"raw_content,omitempty"`
	Score         float64 `json:"score,omitempty"`
	PublishedDate string  `json:"published_date,omitempty"`
	Truncated     bool    `json:"truncated,omitempty"`
	OriginalBytes int     `json:"original_bytes,omitempty"`
	ReturnedBytes int     `json:"returned_bytes,omitempty"`
}

type webReducedFailedResult struct {
	URL   string `json:"url"`
	Error string `json:"error"`
}

type webReducedPayload struct {
	ToolVersion        string                   `json:"tool_version,omitempty"`
	OK                 bool                     `json:"ok"`
	Code               string                   `json:"code,omitempty"`
	Message            string                   `json:"message,omitempty"`
	Retryable          bool                     `json:"retryable,omitempty"`
	NextAction         string                   `json:"next_action,omitempty"`
	Query              string                   `json:"query,omitempty"`
	Provider           string                   `json:"provider,omitempty"`
	Results            []webReducedResult       `json:"results,omitempty"`
	FailedResults      []webReducedFailedResult `json:"failed_results,omitempty"`
	ResponseTime       float64                  `json:"response_time,omitempty"`
	RequestID          string                   `json:"request_id,omitempty"`
	Credits            float64                  `json:"credits,omitempty"`
	Truncated          bool                     `json:"truncated,omitempty"`
	OriginalBytes      int                      `json:"original_bytes,omitempty"`
	ReturnedBytes      int                      `json:"returned_bytes,omitempty"`
	OmittedResults     int                      `json:"omitted_results,omitempty"`
	ContentIsUntrusted bool                     `json:"content_is_untrusted"`
	ContextReduced     bool                     `json:"_context_reduced"`
	ContextSourceBytes int                      `json:"_context_source_bytes"`
}

func reduceWebToolJSON(content string, maxBytes int) (string, bool) {
	var payload webReducedPayload
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return "", false
	}
	payload.ContextReduced = true
	payload.ContextSourceBytes = len(content)
	payload.Truncated = true
	for index := range payload.Results {
		payload.Results[index].Content = compactWebHistoryText(payload.Results[index].Content, 256)
		payload.Results[index].RawContent = compactWebHistoryText(payload.Results[index].RawContent, 512)
		if payload.Results[index].Content != "" {
			payload.Results[index].ReturnedBytes = len(payload.Results[index].Content)
		}
		if payload.Results[index].RawContent != "" {
			payload.Results[index].ReturnedBytes = len(payload.Results[index].RawContent)
		}
		payload.Results[index].Truncated = true
	}
	payload.Message = compactWebHistoryText(payload.Message, 256)
	payload.NextAction = compactWebHistoryText(payload.NextAction, 256)
	for index := range payload.FailedResults {
		payload.FailedResults[index].Error = compactWebHistoryText(payload.FailedResults[index].Error, 256)
	}
	for {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return "", false
		}
		if maxBytes <= 0 || len(encoded) <= maxBytes {
			return string(encoded), true
		}
		largest := -1
		for index := range payload.Results {
			textBytes := len(payload.Results[index].Content) + len(payload.Results[index].RawContent)
			if largest < 0 || textBytes > len(payload.Results[largest].Content)+len(payload.Results[largest].RawContent) {
				largest = index
			}
		}
		if largest >= 0 && (payload.Results[largest].Content != "" || payload.Results[largest].RawContent != "") {
			payload.Results[largest].Content = ""
			payload.Results[largest].RawContent = ""
			payload.Results[largest].ReturnedBytes = 0
			continue
		}
		if len(payload.Results) > 1 {
			payload.Results = payload.Results[:len(payload.Results)-1]
			payload.OmittedResults++
			continue
		}
		return reducedModelPayload(content, maxBytes, false), true
	}
}

func compactWebHistoryText(value string, maxBytes int) string {
	value = strings.TrimSpace(value)
	if len(value) <= maxBytes {
		return value
	}
	end := maxBytes
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end]
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
