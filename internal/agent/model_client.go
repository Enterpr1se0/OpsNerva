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
	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
)

// The Anthropic API rejects requests without max_tokens, so the value cannot
// be left to the upstream. fallbackAnthropicMaxTokens applies when the
// provider's model catalog does not report the model's real output limit.
const fallbackAnthropicMaxTokens = 32000

// anthropicMaxTokensCeiling bounds catalog-reported limits so a single
// response cannot run unbounded against request timeouts.
const anthropicMaxTokensCeiling = 64000

func newChatModel(ctx context.Context, cfg config.Model, timeout time.Duration, maxTokens int) (model.ToolCallingChatModel, error) {
	httpClient, err := modelHTTPClient(cfg, timeout)
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
			HTTPClient: httpClient, RequestTimeout: timeout,
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
	openaiCfg := &openai.ChatModelConfig{
		APIKey: cfg.APIKey, BaseURL: cfg.BaseURL, Model: cfg.Name, ReasoningEffort: openai.ReasoningEffortLevel(cfg.ReasoningEffort), Timeout: timeout, HTTPClient: httpClient,
	}
	if maxTokens > 0 {
		name := strings.ToLower(strings.TrimSpace(cfg.Name))
		if strings.HasPrefix(name, "o1") || strings.HasPrefix(name, "o3") || strings.HasPrefix(name, "o4") || strings.HasPrefix(name, "gpt-5") {
			openaiCfg.MaxCompletionTokens = &maxTokens
		} else {
			openaiCfg.MaxTokens = &maxTokens
		}
	}
	return openai.NewChatModel(ctx, openaiCfg)
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

func modelHTTPClient(cfg config.Model, timeout time.Duration) (*http.Client, error) {
	if cfg.Kind != "anthropic" && cfg.ProxyURL == "" && cfg.UserAgent == "" {
		return nil, nil
	}
	client := &http.Client{Timeout: timeout}
	if cfg.ProxyURL != "" {
		proxyClient, err := proxyx.NewHTTPClient(cfg.ProxyURL, cfg.ProxyUsername, cfg.ProxyPassword, timeout)
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
