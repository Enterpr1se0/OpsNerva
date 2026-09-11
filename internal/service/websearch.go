package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/websearch"
)

var ErrWebSearchDisabled = errors.New("Tavily Web is disabled")

func (s *Service) WebSearchSettings(ctx context.Context) (domain.WebSearchSettings, error) {
	settings, err := s.store.GetWebSearchSettings(ctx)
	if err != nil {
		return domain.WebSearchSettings{}, err
	}
	return publicWebSearchSettings(settings), nil
}

func (s *Service) SaveWebSearchSettings(ctx context.Context, input domain.WebSearchSettingsInput, actor string) (domain.WebSearchSettings, error) {
	current, err := s.store.GetWebSearchSettings(ctx)
	if err != nil {
		return domain.WebSearchSettings{}, err
	}
	baseURL, err := websearch.NormalizeBaseURL(input.BaseURL)
	if err != nil {
		return domain.WebSearchSettings{}, err
	}
	input.ProxyID = strings.TrimSpace(input.ProxyID)
	if input.ProxyID != "" {
		if _, err := s.store.GetProxy(ctx, input.ProxyID); err != nil {
			return domain.WebSearchSettings{}, fmt.Errorf("load proxy %q: %w", input.ProxyID, err)
		}
	}
	if input.TimeoutSeconds < domain.MinWebSearchTimeoutSeconds || input.TimeoutSeconds > domain.MaxWebSearchTimeoutSeconds {
		return domain.WebSearchSettings{}, fmt.Errorf("timeout_seconds must be between %d and %d", domain.MinWebSearchTimeoutSeconds, domain.MaxWebSearchTimeoutSeconds)
	}
	if input.MaxResults < domain.MinWebSearchMaxResults || input.MaxResults > domain.MaxWebSearchMaxResults {
		return domain.WebSearchSettings{}, fmt.Errorf("max_results must be between %d and %d", domain.MinWebSearchMaxResults, domain.MaxWebSearchMaxResults)
	}
	apiKeyCipher := current.APIKeyCipher
	if input.ClearAPIKey {
		apiKeyCipher = ""
	}
	if apiKey := strings.TrimSpace(input.APIKey); apiKey != "" {
		apiKeyCipher, err = s.encryptor.Encrypt([]byte(apiKey))
		if err != nil {
			return domain.WebSearchSettings{}, err
		}
	}
	if input.Enabled && apiKeyCipher == "" {
		return domain.WebSearchSettings{}, fmt.Errorf("Tavily API key is required when Tavily Web is enabled")
	}

	saved, err := s.store.SaveWebSearchSettings(ctx, domain.WebSearchSettings{
		Enabled: input.Enabled, Provider: "tavily", BaseURL: baseURL, APIKeyCipher: apiKeyCipher,
		ProxyID:        input.ProxyID,
		TimeoutSeconds: input.TimeoutSeconds, MaxResults: input.MaxResults,
	})
	if err != nil {
		return domain.WebSearchSettings{}, err
	}
	s.audit(ctx, "", "web_search_settings_updated", actor, map[string]any{
		"enabled": saved.Enabled, "provider": saved.Provider, "base_url": saved.BaseURL,
		"proxy_id": saved.ProxyID, "timeout_seconds": saved.TimeoutSeconds, "max_results": saved.MaxResults,
	})
	return publicWebSearchSettings(saved), nil
}

func decorateWebSearchSettings(settings domain.WebSearchSettings) domain.WebSearchSettings {
	if settings.Provider == "" {
		settings.Provider = "tavily"
	}
	if settings.BaseURL == "" {
		settings.BaseURL = domain.DefaultWebSearchBaseURL
	}
	if settings.TimeoutSeconds == 0 {
		settings.TimeoutSeconds = domain.DefaultWebSearchTimeoutSeconds
	}
	if settings.MaxResults == 0 {
		settings.MaxResults = domain.DefaultWebSearchMaxResults
	}
	settings.HasAPIKey = settings.APIKeyCipher != ""
	return settings
}

func publicWebSearchSettings(settings domain.WebSearchSettings) domain.WebSearchSettings {
	settings = decorateWebSearchSettings(settings)
	settings.APIKeyCipher = ""
	return settings
}

func (s *Service) resolveWebSearchSettings(ctx context.Context) (websearch.Config, error) {
	settings, err := s.store.GetWebSearchSettings(ctx)
	if err != nil {
		return websearch.Config{}, err
	}
	settings = decorateWebSearchSettings(settings)
	if !settings.Enabled {
		return websearch.Config{}, ErrWebSearchDisabled
	}
	if settings.APIKeyCipher == "" {
		return websearch.Config{}, fmt.Errorf("%w: Tavily API key is not configured", ErrWebSearchDisabled)
	}
	apiKey, err := s.encryptor.Decrypt(settings.APIKeyCipher)
	if err != nil {
		return websearch.Config{}, fmt.Errorf("decrypt Tavily API key: %w", err)
	}
	proxy, err := s.resolveProxy(ctx, settings.ProxyID)
	if err != nil {
		return websearch.Config{}, err
	}
	return websearch.Config{
		BaseURL: settings.BaseURL, TimeoutSeconds: settings.TimeoutSeconds, MaxResults: settings.MaxResults,
		APIKey:   string(apiKey),
		ProxyURL: proxy.URL, ProxyUsername: proxy.Username, ProxyPassword: proxy.Password,
	}, nil
}

func (s *Service) SearchWeb(ctx context.Context, input domain.WebSearchRequest, actor string) (domain.WebSearchResponse, error) {
	settings, err := s.resolveWebSearchSettings(ctx)
	if err != nil {
		return domain.WebSearchResponse{}, err
	}
	result, info, err := s.webSearch.Search(ctx, settings, input)
	if info == nil {
		return result, err
	}
	auditData := map[string]any{
		"provider": "tavily", "query_sha256": info.InputDigest, "duration_ms": info.Duration.Milliseconds(),
	}
	eventType := "web_search_failed"
	if err == nil {
		eventType = "web_search_completed"
		auditData["result_count"] = len(result.Results)
		auditData["proxy_used"] = settings.ProxyURL != ""
		auditData["request_id"] = result.RequestID
		auditData["credits"] = result.Credits
		auditData["original_bytes"] = result.OriginalBytes
		auditData["returned_bytes"] = result.ReturnedBytes
		auditData["truncated"] = result.Truncated
		auditData["omitted_results"] = result.OmittedResults
	}
	addWebAuditMetadata(auditData, info, err)
	s.audit(ctx, "", eventType, actor, auditData)
	return result, err
}

func (s *Service) ExtractWeb(ctx context.Context, input domain.WebExtractRequest, actor string) (domain.WebExtractResponse, error) {
	settings, err := s.resolveWebSearchSettings(ctx)
	if err != nil {
		return domain.WebExtractResponse{}, err
	}
	result, info, err := s.webSearch.Extract(ctx, settings, input)
	if info == nil {
		return result, err
	}
	auditData := map[string]any{
		"provider": "tavily", "urls_sha256": info.InputDigest, "url_count": info.InputCount,
		"duration_ms": info.Duration.Milliseconds(), "proxy_used": settings.ProxyURL != "",
	}
	eventType := "web_extract_failed"
	if result.Results != nil {
		if len(result.Results) > 0 {
			eventType = "web_extract_completed"
		}
		auditData["result_count"] = len(result.Results)
		auditData["failed_count"] = len(result.FailedResults)
		auditData["request_id"] = result.RequestID
		auditData["credits"] = result.Credits
		auditData["original_bytes"] = result.OriginalBytes
		auditData["returned_bytes"] = result.ReturnedBytes
		auditData["truncated"] = result.Truncated
		auditData["omitted_results"] = result.OmittedResults
	}
	addWebAuditMetadata(auditData, info, err)
	s.audit(ctx, "", eventType, actor, auditData)
	return result, err
}

func addWebAuditMetadata(data map[string]any, meta *websearch.CallInfo, err error) {
	if meta.StatusCode != 0 {
		data["http_status"] = meta.StatusCode
	}
	if meta.RetryCount != 0 {
		data["retry_count"] = meta.RetryCount
	}
	if meta.ResponseBytes != 0 {
		data["provider_response_bytes"] = meta.ResponseBytes
	}
	var providerError *websearch.ProviderError
	if errors.As(err, &providerError) {
		data["error_code"] = providerError.Code
		data["retryable"] = providerError.Retryable
	}
}
