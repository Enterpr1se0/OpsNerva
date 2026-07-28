package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/proxyx"
)

const (
	maxWebSearchResponseBytes = 2 << 20
	maxWebExtractURLs         = 5
)

var (
	ErrWebSearchDisabled = errors.New("Tavily Web is disabled")
	ErrWebSearchUpstream = errors.New("Tavily provider request failed")
	webSearchDomain      = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
)

type resolvedWebSearchSettings struct {
	domain.WebSearchSettings
	ProxyURL      string
	ProxyUsername string
	APIKey        string
	ProxyPassword string
}

type tavilySearchRequest struct {
	Query          string   `json:"query"`
	SearchDepth    string   `json:"search_depth"`
	MaxResults     int      `json:"max_results"`
	TimeRange      string   `json:"time_range,omitempty"`
	IncludeDomains []string `json:"include_domains,omitempty"`
	ExcludeDomains []string `json:"exclude_domains,omitempty"`
	IncludeAnswer  bool     `json:"include_answer"`
	IncludeRaw     bool     `json:"include_raw_content"`
}

type tavilySearchResponse struct {
	Results      []domain.WebSearchResult `json:"results"`
	ResponseTime float64                  `json:"response_time"`
}

type tavilyExtractRequest struct {
	URLs          []string `json:"urls"`
	ExtractDepth  string   `json:"extract_depth"`
	Format        string   `json:"format"`
	IncludeImages bool     `json:"include_images"`
}

type tavilyExtractResponse struct {
	Results       []domain.WebExtractResult       `json:"results"`
	FailedResults []domain.WebExtractFailedResult `json:"failed_results"`
	ResponseTime  float64                         `json:"response_time"`
}

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
	baseURL, err := normalizeTavilyBaseURL(input.BaseURL)
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

func (s *Service) resolveWebSearchSettings(ctx context.Context) (resolvedWebSearchSettings, error) {
	settings, err := s.store.GetWebSearchSettings(ctx)
	if err != nil {
		return resolvedWebSearchSettings{}, err
	}
	settings = decorateWebSearchSettings(settings)
	if !settings.Enabled {
		return resolvedWebSearchSettings{}, ErrWebSearchDisabled
	}
	if settings.APIKeyCipher == "" {
		return resolvedWebSearchSettings{}, fmt.Errorf("%w: Tavily API key is not configured", ErrWebSearchDisabled)
	}
	apiKey, err := s.encryptor.Decrypt(settings.APIKeyCipher)
	if err != nil {
		return resolvedWebSearchSettings{}, fmt.Errorf("decrypt Tavily API key: %w", err)
	}
	proxy, err := s.resolveProxy(ctx, settings.ProxyID)
	if err != nil {
		return resolvedWebSearchSettings{}, err
	}
	return resolvedWebSearchSettings{
		WebSearchSettings: settings, APIKey: string(apiKey),
		ProxyURL: proxy.URL, ProxyUsername: proxy.Username, ProxyPassword: proxy.Password,
	}, nil
}

func (s *Service) SearchWeb(ctx context.Context, input domain.WebSearchRequest, actor string) (domain.WebSearchResponse, error) {
	settings, err := s.resolveWebSearchSettings(ctx)
	if err != nil {
		return domain.WebSearchResponse{}, err
	}
	request, err := normalizeWebSearchRequest(input, settings.MaxResults)
	if err != nil {
		return domain.WebSearchResponse{}, err
	}
	payload := tavilySearchRequest{
		Query: request.Query, SearchDepth: "basic", MaxResults: request.MaxResults, TimeRange: request.TimeRange,
		IncludeDomains: request.IncludeDomains, ExcludeDomains: request.ExcludeDomains, IncludeAnswer: false, IncludeRaw: false,
	}
	queryDigest := sha256.Sum256([]byte(request.Query))
	started := time.Now()
	var decoded tavilySearchResponse
	err = s.requestTavily(ctx, settings, "/search", payload, &decoded)
	if err != nil {
		s.audit(ctx, "", "web_search_failed", actor, map[string]any{
			"provider": "tavily", "query_sha256": hex.EncodeToString(queryDigest[:]), "duration_ms": time.Since(started).Milliseconds(),
		})
		return domain.WebSearchResponse{}, err
	}
	results := make([]domain.WebSearchResult, 0, min(len(decoded.Results), request.MaxResults))
	for _, result := range decoded.Results {
		if len(results) == request.MaxResults {
			break
		}
		parsed, err := url.Parse(strings.TrimSpace(result.URL))
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			continue
		}
		if containsWebSearchSecret(parsed.String(), settings) {
			continue
		}
		result.Title = s.scrubWebSearchText(result.Title, settings)
		result.URL = parsed.String()
		result.Content = s.scrubWebSearchText(result.Content, settings)
		result.PublishedDate = s.scrubWebSearchText(result.PublishedDate, settings)
		results = append(results, result)
	}
	s.audit(ctx, "", "web_search_completed", actor, map[string]any{
		"provider": "tavily", "query_sha256": hex.EncodeToString(queryDigest[:]), "result_count": len(results),
		"duration_ms": time.Since(started).Milliseconds(), "proxy_used": settings.ProxyURL != "",
	})
	return domain.WebSearchResponse{
		Query: request.Query, Provider: "tavily", Results: results, ResponseTime: decoded.ResponseTime, ContentIsUntrusted: true,
	}, nil
}

func (s *Service) ExtractWeb(ctx context.Context, input domain.WebExtractRequest, actor string) (domain.WebExtractResponse, error) {
	settings, err := s.resolveWebSearchSettings(ctx)
	if err != nil {
		return domain.WebExtractResponse{}, err
	}
	request, err := normalizeWebExtractRequest(input)
	if err != nil {
		return domain.WebExtractResponse{}, err
	}
	urlsDigest := sha256.Sum256([]byte(strings.Join(request.URLs, "\n")))
	started := time.Now()
	var decoded tavilyExtractResponse
	err = s.requestTavily(ctx, settings, "/extract", tavilyExtractRequest{
		URLs: request.URLs, ExtractDepth: "basic", Format: "markdown", IncludeImages: false,
	}, &decoded)
	if err != nil {
		s.audit(ctx, "", "web_extract_failed", actor, map[string]any{
			"provider": "tavily", "urls_sha256": hex.EncodeToString(urlsDigest[:]), "url_count": len(request.URLs),
			"duration_ms": time.Since(started).Milliseconds(), "proxy_used": settings.ProxyURL != "",
		})
		return domain.WebExtractResponse{Provider: "tavily", ContentIsUntrusted: true}, err
	}

	result := domain.WebExtractResponse{
		Provider: "tavily", Results: make([]domain.WebExtractResult, 0, len(decoded.Results)),
		FailedResults: make([]domain.WebExtractFailedResult, 0, len(decoded.FailedResults)),
		ResponseTime:  decoded.ResponseTime, ContentIsUntrusted: true,
	}
	resultLimit := min(len(decoded.Results), maxWebExtractURLs)
	for _, extracted := range decoded.Results[:resultLimit] {
		normalizedURL, err := normalizePublicWebURL(extracted.URL)
		if err != nil || containsWebSearchSecret(normalizedURL, settings) {
			continue
		}
		content := s.scrubWebSearchText(extracted.RawContent, settings)
		if content == "" {
			result.FailedResults = append(result.FailedResults, domain.WebExtractFailedResult{URL: normalizedURL, Error: "Tavily returned empty content"})
			continue
		}
		result.Results = append(result.Results, domain.WebExtractResult{URL: normalizedURL, RawContent: content})
	}
	for _, failed := range decoded.FailedResults {
		if len(result.FailedResults) == maxWebExtractURLs {
			break
		}
		normalizedURL, err := normalizePublicWebURL(failed.URL)
		if err != nil || containsWebSearchSecret(normalizedURL, settings) {
			continue
		}
		result.FailedResults = append(result.FailedResults, domain.WebExtractFailedResult{
			URL: normalizedURL, Error: s.scrubWebSearchText(failed.Error, settings),
		})
	}
	eventType := "web_extract_completed"
	if len(result.Results) == 0 {
		eventType = "web_extract_failed"
	}
	s.audit(ctx, "", eventType, actor, map[string]any{
		"provider": "tavily", "urls_sha256": hex.EncodeToString(urlsDigest[:]), "url_count": len(request.URLs),
		"result_count": len(result.Results), "failed_count": len(result.FailedResults),
		"duration_ms": time.Since(started).Milliseconds(), "proxy_used": settings.ProxyURL != "",
	})
	if len(result.Results) == 0 {
		return result, fmt.Errorf("%w: Tavily did not extract any requested URL", ErrWebSearchUpstream)
	}
	return result, nil
}

func (s *Service) requestTavily(ctx context.Context, settings resolvedWebSearchSettings, path string, payload, output any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	endpoint := strings.TrimRight(settings.BaseURL, "/") + path
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	httpRequest.Header.Set("Authorization", "Bearer "+settings.APIKey)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("User-Agent", "OpsPilot-Tavily/1.0")
	client, err := webSearchHTTPClient(settings)
	if err != nil {
		return err
	}
	response, err := client.Do(httpRequest)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return fmt.Errorf("%w: %s", ErrWebSearchUpstream, s.scrubWebSearchText(err.Error(), settings))
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxWebSearchResponseBytes+1))
	if err != nil {
		return fmt.Errorf("%w: read response: %v", ErrWebSearchUpstream, err)
	}
	if len(body) > maxWebSearchResponseBytes {
		return fmt.Errorf("%w: response exceeded 2 MiB", ErrWebSearchUpstream)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := s.scrubWebSearchText(string(body), settings)
		return fmt.Errorf("%w: Tavily returned %s: %s", ErrWebSearchUpstream, response.Status, message)
	}
	if err := json.Unmarshal(body, output); err != nil {
		return fmt.Errorf("%w: decode response: %v", ErrWebSearchUpstream, err)
	}
	return nil
}

func containsWebSearchSecret(value string, settings resolvedWebSearchSettings) bool {
	return settings.APIKey != "" && strings.Contains(value, settings.APIKey) ||
		settings.ProxyPassword != "" && strings.Contains(value, settings.ProxyPassword)
}

func (s *Service) scrubWebSearchText(value string, settings resolvedWebSearchSettings) string {
	for _, secret := range []string{settings.APIKey, settings.ProxyPassword} {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	if s.redactor != nil {
		value = s.redactor.Redact(value)
	}
	return value
}

func normalizeTavilyBaseURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = domain.DefaultWebSearchBaseURL
	}
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("invalid Tavily base_url")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.Path = strings.TrimSuffix(strings.TrimSuffix(parsed.Path, "/search"), "/extract")
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func webSearchHTTPClient(settings resolvedWebSearchSettings) (*http.Client, error) {
	timeout := time.Duration(settings.TimeoutSeconds) * time.Second
	if settings.ProxyURL != "" {
		return proxyx.NewHTTPClient(settings.ProxyURL, settings.ProxyUsername, settings.ProxyPassword, timeout)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.ResponseHeaderTimeout = timeout
	return &http.Client{Transport: transport, Timeout: timeout}, nil
}

func normalizeWebSearchRequest(input domain.WebSearchRequest, configuredMax int) (domain.WebSearchRequest, error) {
	input.Query = strings.TrimSpace(input.Query)
	if input.Query == "" || len(input.Query) > 2000 {
		return domain.WebSearchRequest{}, fmt.Errorf("query is required and must not exceed 2000 bytes")
	}
	if input.MaxResults == 0 {
		input.MaxResults = configuredMax
	}
	if input.MaxResults < domain.MinWebSearchMaxResults || input.MaxResults > configuredMax {
		return domain.WebSearchRequest{}, fmt.Errorf("max_results must be between %d and %d", domain.MinWebSearchMaxResults, configuredMax)
	}
	input.TimeRange = strings.ToLower(strings.TrimSpace(input.TimeRange))
	if input.TimeRange != "" && input.TimeRange != "day" && input.TimeRange != "week" && input.TimeRange != "month" && input.TimeRange != "year" {
		return domain.WebSearchRequest{}, fmt.Errorf("time_range must be day, week, month, or year")
	}
	var err error
	if input.IncludeDomains, err = normalizeWebSearchDomains(input.IncludeDomains); err != nil {
		return domain.WebSearchRequest{}, fmt.Errorf("include_domains: %w", err)
	}
	if input.ExcludeDomains, err = normalizeWebSearchDomains(input.ExcludeDomains); err != nil {
		return domain.WebSearchRequest{}, fmt.Errorf("exclude_domains: %w", err)
	}
	return input, nil
}

func normalizeWebExtractRequest(input domain.WebExtractRequest) (domain.WebExtractRequest, error) {
	if len(input.URLs) == 0 || len(input.URLs) > maxWebExtractURLs {
		return domain.WebExtractRequest{}, fmt.Errorf("urls must contain between 1 and %d public URLs", maxWebExtractURLs)
	}
	result := domain.WebExtractRequest{URLs: make([]string, 0, len(input.URLs))}
	seen := make(map[string]struct{}, len(input.URLs))
	for _, value := range input.URLs {
		normalized, err := normalizePublicWebURL(value)
		if err != nil {
			return domain.WebExtractRequest{}, err
		}
		if _, exists := seen[normalized]; exists {
			continue
		}
		seen[normalized] = struct{}{}
		result.URLs = append(result.URLs, normalized)
	}
	return result, nil
}

func normalizePublicWebURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 2048 {
		return "", fmt.Errorf("URL is required and must not exceed 2048 bytes")
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return "", fmt.Errorf("invalid public HTTP/HTTPS URL %q", value)
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return "", fmt.Errorf("URL host %q is not public", parsed.Hostname())
	}
	if address := net.ParseIP(host); address != nil {
		if !address.IsGlobalUnicast() || address.IsPrivate() {
			return "", fmt.Errorf("URL host %q is not public", parsed.Hostname())
		}
	} else {
		if !strings.Contains(host, ".") || isNumericWebHost(host) {
			return "", fmt.Errorf("URL host %q is not a public domain", parsed.Hostname())
		}
	}
	parsed.Fragment = ""
	return parsed.String(), nil
}

func isNumericWebHost(host string) bool {
	for _, character := range host {
		if character != '.' && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func normalizeWebSearchDomains(values []string) ([]string, error) {
	if len(values) > 10 {
		return nil, fmt.Errorf("at most 10 domains are allowed")
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if !webSearchDomain.MatchString(value) || strings.Contains(value, "..") {
			return nil, fmt.Errorf("invalid domain %q", value)
		}
		if _, exists := seen[value]; !exists {
			seen[value] = struct{}{}
			result = append(result, value)
		}
	}
	return result, nil
}
