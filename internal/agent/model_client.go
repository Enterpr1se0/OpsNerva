package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"eino-ops-agent/internal/config"
	"eino-ops-agent/internal/observability"
	"eino-ops-agent/internal/proxyx"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/cloudwego/eino-ext/components/model/claude"
	modelopenai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// The Anthropic API rejects requests without max_tokens, so the value cannot
// be left to the upstream. fallbackAnthropicMaxTokens applies when the
// provider's model catalog does not report the model's real output limit.
const fallbackAnthropicMaxTokens = 32000

// anthropicMaxTokensCeiling bounds catalog-reported limits so a single
// response cannot request an excessive output budget.
const anthropicMaxTokensCeiling = 64000

func newChatModel(ctx context.Context, cfg config.Model, maxTokens int) (model.ToolCallingChatModel, error) {
	httpClient, err := modelHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	if cfg.UserAgent != "" {
		httpClient = proxyx.WrapHeaders(httpClient, map[string]string{"User-Agent": cfg.UserAgent})
	}
	if cfg.Kind == "anthropic" {
		if maxTokens <= 0 {
			maxTokens = resolveAnthropicMaxTokens(ctx, cfg, httpClient)
		}
		claudeCfg := &claude.Config{
			APIKey: cfg.APIKey, Model: cfg.Name, MaxTokens: maxTokens,
			HTTPClient: httpClient,
		}
		if cfg.ReasoningEffort != "" {
			claudeCfg.ThinkingConfig = &anthropic.ThinkingConfigParamUnion{
				OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{},
			}
			claudeCfg.AdditionalRequestFields = map[string]any{
				"output_config.effort": cfg.ReasoningEffort,
			}
		}
		if cfg.BaseURL != "" {
			claudeCfg.BaseURL = &cfg.BaseURL
		}
		return claude.NewChatModel(ctx, claudeCfg)
	}
	openaiCfg := &modelopenai.ChatModelConfig{
		APIKey: cfg.APIKey, BaseURL: cfg.BaseURL, Model: cfg.Name, ReasoningEffort: modelopenai.ReasoningEffortLevel(cfg.ReasoningEffort), HTTPClient: httpClient,
	}
	if maxTokens > 0 {
		name := strings.ToLower(strings.TrimSpace(cfg.Name))
		if strings.HasPrefix(name, "o1") || strings.HasPrefix(name, "o3") || strings.HasPrefix(name, "o4") || strings.HasPrefix(name, "gpt-5") {
			openaiCfg.MaxCompletionTokens = &maxTokens
		} else {
			openaiCfg.MaxTokens = &maxTokens
		}
	}
	chatModel, err := modelopenai.NewChatModel(ctx, openaiCfg)
	if err != nil {
		return nil, err
	}
	return &reasoningContentCompatModel{inner: chatModel, force: cfg.ReasoningEffort != ""}, nil
}

// reasoningContentCompatModel keeps the multi-step thinking tool protocol
// intact for OpenAI-compatible providers. Some of them require an explicit
// reasoning_content field even when the model returned an empty value.
type reasoningContentCompatModel struct {
	inner model.ToolCallingChatModel
	force bool
}

func (*reasoningContentCompatModel) GetType() string {
	return "OpenAI"
}

func (*reasoningContentCompatModel) IsCallbacksEnabled() bool {
	return true
}

func (m *reasoningContentCompatModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	return m.inner.Generate(ctx, input, m.options(input, opts)...)
}

func (m *reasoningContentCompatModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return m.inner.Stream(ctx, input, m.options(input, opts)...)
}

func (m *reasoningContentCompatModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	inner, err := m.inner.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return &reasoningContentCompatModel{inner: inner, force: m.force}, nil
}

func (m *reasoningContentCompatModel) options(input []*schema.Message, opts []model.Option) []model.Option {
	if !shouldPreserveReasoningContent(input, m.force) {
		return opts
	}
	result := make([]model.Option, 0, len(opts)+1)
	result = append(result, opts...)
	result = append(result, modelopenai.WithRequestPayloadModifier(preserveReasoningContentPayload))
	return result
}

func shouldPreserveReasoningContent(messages []*schema.Message, force bool) bool {
	thinking := force
	hasToolCalls := false
	for _, message := range messages {
		if message == nil {
			continue
		}
		if message.ReasoningContent != "" {
			thinking = true
		}
		if message.Role == schema.Assistant && len(message.ToolCalls) > 0 {
			hasToolCalls = true
		}
	}
	return thinking && hasToolCalls
}

func preserveReasoningContentPayload(ctx context.Context, messages []*schema.Message, rawBody []byte) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		return nil, fmt.Errorf("decode chat completion payload: %w", err)
	}
	var wireMessages []map[string]json.RawMessage
	if err := json.Unmarshal(payload["messages"], &wireMessages); err != nil {
		return nil, fmt.Errorf("decode chat completion messages: %w", err)
	}
	if len(wireMessages) != len(messages) {
		return nil, fmt.Errorf("chat completion message count changed during serialization: got %d, want %d", len(wireMessages), len(messages))
	}

	patched := 0
	for index, message := range messages {
		if message == nil || message.Role != schema.Assistant || len(message.ToolCalls) == 0 {
			continue
		}
		reasoningContent, err := json.Marshal(message.ReasoningContent)
		if err != nil {
			return nil, fmt.Errorf("encode assistant reasoning content: %w", err)
		}
		current, exists := wireMessages[index]["reasoning_content"]
		if exists && string(current) == string(reasoningContent) {
			continue
		}
		wireMessages[index]["reasoning_content"] = reasoningContent
		patched++
	}
	if patched == 0 {
		return rawBody, nil
	}

	encodedMessages, err := json.Marshal(wireMessages)
	if err != nil {
		return nil, fmt.Errorf("encode chat completion messages: %w", err)
	}
	payload["messages"] = encodedMessages
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode chat completion payload: %w", err)
	}
	observability.FromContext(ctx).DebugContext(ctx, "preserved reasoning content in tool-call history", "messages", patched)
	return encoded, nil
}

// resolveAnthropicMaxTokens lets the upstream decide the output budget: the
// Anthropic model catalog reports each model's output limit as max_tokens.
// Gateways that do not expose the catalog fall back to a per-family estimate.
func resolveAnthropicMaxTokens(ctx context.Context, cfg config.Model, httpClient *http.Client) int {
	logger := observability.FromContext(ctx).With("component", "agent", "model", cfg.Name)
	if limit, err := lookupAnthropicOutputLimit(ctx, cfg, httpClient); err == nil {
		resolved := min(limit, anthropicMaxTokensCeiling)
		logger.DebugContext(ctx, "anthropic max_tokens resolved from model catalog", "catalog_limit", limit, "max_tokens", resolved)
		return resolved
	} else {
		logger.DebugContext(ctx, "anthropic model catalog lookup failed; using max_tokens fallback", "reason", redactModelError(cfg, err))
	}
	name := strings.ToLower(strings.TrimSpace(cfg.Name))
	resolved := fallbackAnthropicMaxTokens
	switch {
	case strings.HasPrefix(name, "claude-3-opus"), strings.HasPrefix(name, "claude-3-haiku"), strings.HasPrefix(name, "claude-3-sonnet"):
		resolved = 4096
	case strings.HasPrefix(name, "claude-3-5"), strings.HasPrefix(name, "claude-3-7"):
		resolved = 8192
	}
	logger.DebugContext(ctx, "anthropic max_tokens resolved from fallback", "max_tokens", resolved)
	return resolved
}

func lookupAnthropicOutputLimit(ctx context.Context, cfg config.Model, httpClient *http.Client) (int, error) {
	if cfg.BaseURL == "" {
		return 0, fmt.Errorf("base URL is empty")
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	endpoint := cfg.BaseURL + "/v1/models/" + url.PathEscape(cfg.Name)
	request, err := http.NewRequestWithContext(lookupCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, fmt.Errorf("build model catalog request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("x-api-key", cfg.APIKey)
	request.Header.Set("anthropic-version", "2023-06-01")
	client := httpClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second, CheckRedirect: rejectModelProviderRedirect}
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, fmt.Errorf("request model catalog: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return 0, fmt.Errorf("model catalog returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return 0, fmt.Errorf("read model catalog response: %w", err)
	}
	var payload struct {
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, fmt.Errorf("decode model catalog response: %w", err)
	}
	if payload.MaxTokens < 1024 || payload.MaxTokens > 200000 {
		return 0, fmt.Errorf("model catalog returned invalid max_tokens")
	}
	return payload.MaxTokens, nil
}

func modelHTTPClient(cfg config.Model) (*http.Client, error) {
	if cfg.Kind != "anthropic" && cfg.ProxyURL == "" && cfg.UserAgent == "" {
		return nil, nil
	}
	// Model requests are bounded by their caller's context. Do not add a client
	// or response-header timeout: providers may legitimately spend a long time
	// reasoning before the first stream chunk.
	client := &http.Client{}
	if cfg.ProxyURL != "" {
		proxyClient, err := proxyx.NewHTTPClient(cfg.ProxyURL, cfg.ProxyUsername, cfg.ProxyPassword, 0)
		if err != nil {
			return nil, err
		}
		client = proxyClient
	}
	if cfg.Kind == "anthropic" {
		client.CheckRedirect = rejectModelProviderRedirect
	}
	return client, nil
}

func rejectModelProviderRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

func redactModelError(cfg config.Model, err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	for _, secret := range []string{cfg.APIKey, cfg.ProxyPassword} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	return errors.New(message)
}
