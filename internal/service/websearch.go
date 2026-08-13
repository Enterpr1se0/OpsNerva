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
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/proxyx"
)

const (
	maxWebSearchResponseBytes       = 2 << 20
	maxWebSearchErrorBytes          = 4 << 10
	maxWebExtractURLs               = 5
	defaultWebSearchRequestResults  = 5
	maxWebSearchResultContentBytes  = 2 << 10
	maxWebSearchModelResponseBytes  = 32 << 10
	maxWebExtractResultContentBytes = 16 << 10
	maxWebExtractModelResponseBytes = 48 << 10
	maxWebResultTitleBytes          = 512
	maxWebResultDateBytes           = 128
	maxWebFailedResultErrorBytes    = 512
	maxWebRequestIDBytes            = 256
	maxWebRetryAfter                = 2 * time.Second
	webRetryDelay                   = 100 * time.Millisecond
)

var (
	ErrWebSearchDisabled = errors.New("Tavily Web is disabled")
	ErrWebSearchUpstream = errors.New("Tavily provider request failed")
	webSearchDomain      = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
)

const (
	WebSearchErrorInvalidRequest       = "invalid_request"
	WebSearchErrorAuthenticationFailed = "authentication_failed"
	WebSearchErrorRateLimited          = "rate_limited"
	WebSearchErrorQuotaExhausted       = "quota_exhausted"
	WebSearchErrorProviderUnavailable  = "provider_unavailable"
	WebSearchErrorTimeout              = "timeout"
)

// WebSearchProviderError keeps provider failures machine-readable without
// exposing large response bodies to the model or audit log.
type WebSearchProviderError struct {
	Code       string
	StatusCode int
	Retryable  bool
	RetryAfter time.Duration
	Message    string
}

func (e *WebSearchProviderError) Error() string {
	if e == nil {
		return ErrWebSearchUpstream.Error()
	}
	if e.Message == "" {
		return fmt.Sprintf("%s: %s", ErrWebSearchUpstream, e.Code)
	}
	return fmt.Sprintf("%s: %s: %s", ErrWebSearchUpstream, e.Code, e.Message)
}

func (e *WebSearchProviderError) Unwrap() error { return ErrWebSearchUpstream }

type resolvedWebSearchSettings struct {
	domain.WebSearchSettings
	ProxyURL      string
	ProxyUsername string
	APIKey        string
	ProxyPassword string
}

type tavilySearchRequest struct {
	Query           string   `json:"query"`
	Topic           string   `json:"topic,omitempty"`
	SearchDepth     string   `json:"search_depth"`
	MaxResults      int      `json:"max_results"`
	TimeRange       string   `json:"time_range,omitempty"`
	StartDate       string   `json:"start_date,omitempty"`
	EndDate         string   `json:"end_date,omitempty"`
	ChunksPerSource int      `json:"chunks_per_source,omitempty"`
	IncludeDomains  []string `json:"include_domains,omitempty"`
	ExcludeDomains  []string `json:"exclude_domains,omitempty"`
	IncludeAnswer   bool     `json:"include_answer"`
	IncludeRaw      bool     `json:"include_raw_content"`
}

type tavilySearchResponse struct {
	Results      []domain.WebSearchResult `json:"results"`
	ResponseTime float64                  `json:"response_time"`
	RequestID    string                   `json:"request_id"`
	Usage        tavilyUsage              `json:"usage"`
}

type tavilyExtractRequest struct {
	URLs            []string `json:"urls"`
	Query           string   `json:"query,omitempty"`
	ExtractDepth    string   `json:"extract_depth"`
	ChunksPerSource int      `json:"chunks_per_source,omitempty"`
	Format          string   `json:"format"`
	IncludeImages   bool     `json:"include_images"`
}

type tavilyExtractResponse struct {
	Results       []domain.WebExtractResult       `json:"results"`
	FailedResults []domain.WebExtractFailedResult `json:"failed_results"`
	ResponseTime  float64                         `json:"response_time"`
	RequestID     string                          `json:"request_id"`
	Usage         tavilyUsage                     `json:"usage"`
}

type tavilyUsage struct {
	Credits float64 `json:"credits"`
}

type tavilyRequestMetadata struct {
	StatusCode    int
	RetryCount    int
	ResponseBytes int
}

type tavilyRawResponse struct {
	Body []byte
	Meta tavilyRequestMetadata
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
		Query: request.Query, Topic: request.Topic, SearchDepth: request.SearchDepth, MaxResults: request.MaxResults, TimeRange: request.TimeRange,
		StartDate: request.StartDate, EndDate: request.EndDate, ChunksPerSource: request.ChunksPerSource,
		IncludeDomains: request.IncludeDomains, ExcludeDomains: request.ExcludeDomains, IncludeAnswer: false, IncludeRaw: false,
	}
	queryDigest := sha256.Sum256([]byte(request.Query))
	started := time.Now()
	var decoded tavilySearchResponse
	requestMeta, err := s.requestTavily(ctx, settings, "/search", payload, &decoded)
	if err != nil {
		auditData := map[string]any{
			"provider": "tavily", "query_sha256": hex.EncodeToString(queryDigest[:]), "duration_ms": time.Since(started).Milliseconds(),
		}
		addTavilyAuditMetadata(auditData, requestMeta, err)
		s.audit(ctx, "", "web_search_failed", actor, auditData)
		return domain.WebSearchResponse{}, err
	}
	results := make([]domain.WebSearchResult, 0, min(len(decoded.Results), request.MaxResults))
	seen := make(map[string]struct{}, request.MaxResults)
	for _, result := range decoded.Results {
		if len(results) == request.MaxResults {
			break
		}
		normalizedURL, err := normalizePublicWebURL(result.URL)
		if err != nil || containsWebSearchSecret(normalizedURL, settings) {
			continue
		}
		if _, duplicate := seen[normalizedURL]; duplicate {
			continue
		}
		seen[normalizedURL] = struct{}{}
		result.Title = truncateUTF8Bytes(s.scrubWebSearchText(result.Title, settings), maxWebResultTitleBytes)
		result.URL = normalizedURL
		result.Content = s.scrubWebSearchText(result.Content, settings)
		result.PublishedDate = truncateUTF8Bytes(s.scrubWebSearchText(result.PublishedDate, settings), maxWebResultDateBytes)
		result.OriginalBytes = len(result.Content)
		result.ReturnedBytes = result.OriginalBytes
		results = append(results, result)
	}
	result := domain.WebSearchResponse{
		Query: request.Query, Provider: "tavily", Results: results, ResponseTime: decoded.ResponseTime,
		RequestID: truncateUTF8Bytes(s.scrubWebSearchText(decoded.RequestID, settings), maxWebRequestIDBytes), Credits: decoded.Usage.Credits,
		OmittedResults: max(0, len(decoded.Results)-len(results)), ContentIsUntrusted: true,
	}
	fitWebSearchResponseBudget(&result, maxWebSearchModelResponseBytes)
	auditData := map[string]any{
		"provider": "tavily", "query_sha256": hex.EncodeToString(queryDigest[:]), "result_count": len(results),
		"duration_ms": time.Since(started).Milliseconds(), "proxy_used": settings.ProxyURL != "", "request_id": result.RequestID,
		"credits": result.Credits, "original_bytes": result.OriginalBytes, "returned_bytes": result.ReturnedBytes,
		"truncated": result.Truncated, "omitted_results": result.OmittedResults,
	}
	addTavilyAuditMetadata(auditData, requestMeta, nil)
	s.audit(ctx, "", "web_search_completed", actor, auditData)
	return result, nil
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
	requestMeta, err := s.requestTavily(ctx, settings, "/extract", tavilyExtractRequest{
		URLs: request.URLs, Query: request.Query, ExtractDepth: request.ExtractDepth, ChunksPerSource: request.ChunksPerSource,
		Format: "markdown", IncludeImages: false,
	}, &decoded)
	if err != nil {
		auditData := map[string]any{
			"provider": "tavily", "urls_sha256": hex.EncodeToString(urlsDigest[:]), "url_count": len(request.URLs),
			"duration_ms": time.Since(started).Milliseconds(), "proxy_used": settings.ProxyURL != "",
		}
		addTavilyAuditMetadata(auditData, requestMeta, err)
		s.audit(ctx, "", "web_extract_failed", actor, auditData)
		return domain.WebExtractResponse{Provider: "tavily", ContentIsUntrusted: true}, err
	}

	result := domain.WebExtractResponse{
		Provider: "tavily", Query: request.Query, Results: make([]domain.WebExtractResult, 0, len(decoded.Results)),
		FailedResults: make([]domain.WebExtractFailedResult, 0, len(decoded.FailedResults)),
		ResponseTime:  decoded.ResponseTime,
		RequestID:     truncateUTF8Bytes(s.scrubWebSearchText(decoded.RequestID, settings), maxWebRequestIDBytes),
		Credits:       decoded.Usage.Credits, ContentIsUntrusted: true,
	}
	seen := make(map[string]struct{}, maxWebExtractURLs)
	for _, extracted := range decoded.Results {
		if len(result.Results)+len(result.FailedResults) == maxWebExtractURLs {
			break
		}
		normalizedURL, err := normalizePublicWebURL(extracted.URL)
		if err != nil || containsWebSearchSecret(normalizedURL, settings) {
			continue
		}
		if _, duplicate := seen[normalizedURL]; duplicate {
			continue
		}
		seen[normalizedURL] = struct{}{}
		content := s.scrubWebSearchText(extracted.RawContent, settings)
		if content == "" {
			result.FailedResults = append(result.FailedResults, domain.WebExtractFailedResult{URL: normalizedURL, Error: "Tavily returned empty content"})
			continue
		}
		result.Results = append(result.Results, domain.WebExtractResult{
			URL: normalizedURL, RawContent: content, OriginalBytes: len(content), ReturnedBytes: len(content),
		})
	}
	for _, failed := range decoded.FailedResults {
		if len(result.FailedResults) == maxWebExtractURLs || len(result.Results)+len(result.FailedResults) == maxWebExtractURLs {
			break
		}
		normalizedURL, err := normalizePublicWebURL(failed.URL)
		if err != nil || containsWebSearchSecret(normalizedURL, settings) {
			continue
		}
		if _, duplicate := seen[normalizedURL]; duplicate {
			continue
		}
		seen[normalizedURL] = struct{}{}
		result.FailedResults = append(result.FailedResults, domain.WebExtractFailedResult{
			URL: normalizedURL, Error: truncateUTF8Bytes(s.scrubWebSearchText(failed.Error, settings), maxWebFailedResultErrorBytes),
		})
	}
	result.OmittedResults = max(0, len(decoded.Results)+len(decoded.FailedResults)-len(result.Results)-len(result.FailedResults))
	fitWebExtractResponseBudget(&result, maxWebExtractModelResponseBytes)
	eventType := "web_extract_completed"
	if len(result.Results) == 0 {
		eventType = "web_extract_failed"
	}
	auditData := map[string]any{
		"provider": "tavily", "urls_sha256": hex.EncodeToString(urlsDigest[:]), "url_count": len(request.URLs),
		"result_count": len(result.Results), "failed_count": len(result.FailedResults),
		"duration_ms": time.Since(started).Milliseconds(), "proxy_used": settings.ProxyURL != "", "request_id": result.RequestID,
		"credits": result.Credits, "original_bytes": result.OriginalBytes, "returned_bytes": result.ReturnedBytes,
		"truncated": result.Truncated, "omitted_results": result.OmittedResults,
	}
	addTavilyAuditMetadata(auditData, requestMeta, nil)
	s.audit(ctx, "", eventType, actor, auditData)
	if len(result.Results) == 0 {
		return result, &WebSearchProviderError{
			Code: WebSearchErrorProviderUnavailable, Retryable: true, Message: "Tavily did not extract any requested URL",
		}
	}
	return result, nil
}

func (s *Service) requestTavily(ctx context.Context, settings resolvedWebSearchSettings, path string, payload, output any) (tavilyRequestMetadata, error) {
	if err := ctx.Err(); err != nil {
		return tavilyRequestMetadata{}, err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return tavilyRequestMetadata{}, err
	}
	keyDigest := sha256.Sum256(bytes.Join([][]byte{
		[]byte(settings.BaseURL), []byte(settings.ProxyURL), []byte(settings.ProxyUsername), []byte(settings.ProxyPassword),
		[]byte(settings.APIKey), []byte(path), encoded,
	}, []byte{'\n'}))
	requestKey := hex.EncodeToString(keyDigest[:])
	resultChannel := s.webRequests.DoChan(requestKey, func() (any, error) {
		workerCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Duration(settings.TimeoutSeconds)*time.Second)
		defer cancel()
		return s.requestTavilyRaw(workerCtx, settings, path, encoded)
	})
	select {
	case <-ctx.Done():
		return tavilyRequestMetadata{}, ctx.Err()
	case shared := <-resultChannel:
		if shared.Err != nil {
			var requestError *tavilyRequestFailure
			if errors.As(shared.Err, &requestError) {
				return requestError.Meta, requestError.Err
			}
			return tavilyRequestMetadata{}, shared.Err
		}
		raw, ok := shared.Val.(tavilyRawResponse)
		if !ok {
			return tavilyRequestMetadata{}, fmt.Errorf("%w: invalid shared response", ErrWebSearchUpstream)
		}
		if err := json.Unmarshal(raw.Body, output); err != nil {
			return raw.Meta, &WebSearchProviderError{Code: WebSearchErrorProviderUnavailable, StatusCode: raw.Meta.StatusCode, Retryable: true, Message: "decode response: " + err.Error()}
		}
		return raw.Meta, nil
	}
}

type tavilyRequestFailure struct {
	Meta tavilyRequestMetadata
	Err  error
}

func (e *tavilyRequestFailure) Error() string { return e.Err.Error() }
func (e *tavilyRequestFailure) Unwrap() error { return e.Err }

func (s *Service) requestTavilyRaw(ctx context.Context, settings resolvedWebSearchSettings, path string, encoded []byte) (tavilyRawResponse, error) {
	endpoint := strings.TrimRight(settings.BaseURL, "/") + path
	client, err := webSearchHTTPClient(settings)
	if err != nil {
		return tavilyRawResponse{}, err
	}
	select {
	case s.webSem <- struct{}{}:
		defer func() { <-s.webSem }()
	case <-ctx.Done():
		return tavilyRawResponse{}, ctx.Err()
	}
	meta := tavilyRequestMetadata{}
	for attempt := 0; attempt < 2; attempt++ {
		meta.RetryCount = attempt
		httpRequest, requestErr := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
		if requestErr != nil {
			return tavilyRawResponse{}, requestErr
		}
		httpRequest.Header.Set("Authorization", "Bearer "+settings.APIKey)
		httpRequest.Header.Set("Content-Type", "application/json")
		httpRequest.Header.Set("Accept", "application/json")
		httpRequest.Header.Set("User-Agent", "OpsNerva-Tavily/1.1")
		response, requestErr := client.Do(httpRequest)
		if requestErr != nil {
			if errors.Is(requestErr, context.Canceled) {
				return tavilyRawResponse{}, requestErr
			}
			providerError := &WebSearchProviderError{
				Code: WebSearchErrorProviderUnavailable, Retryable: true,
				Message: truncateUTF8Bytes(s.scrubWebSearchText(requestErr.Error(), settings), maxWebSearchErrorBytes),
			}
			if errors.Is(requestErr, context.DeadlineExceeded) {
				providerError.Code = WebSearchErrorTimeout
				providerError.Retryable = false
			}
			if attempt == 0 && providerError.Retryable {
				if err := waitWebRetry(ctx, webRetryDelay); err != nil {
					return tavilyRawResponse{}, err
				}
				continue
			}
			return tavilyRawResponse{}, &tavilyRequestFailure{Meta: meta, Err: providerError}
		}
		meta.StatusCode = response.StatusCode
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, maxWebSearchErrorBytes+1))
			_ = response.Body.Close()
			if readErr != nil {
				body = []byte("unable to read provider error")
			}
			message := truncateUTF8Bytes(s.scrubWebSearchText(string(body), settings), maxWebSearchErrorBytes)
			providerError := classifyTavilyStatus(response.StatusCode, response.Header.Get("Retry-After"), message)
			if attempt == 0 && providerError.Retryable {
				delay := providerError.RetryAfter
				if delay == 0 {
					delay = webRetryDelay
				}
				if delay <= maxWebRetryAfter {
					if err := waitWebRetry(ctx, delay); err != nil {
						return tavilyRawResponse{}, err
					}
					continue
				}
			}
			return tavilyRawResponse{}, &tavilyRequestFailure{Meta: meta, Err: providerError}
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, maxWebSearchResponseBytes+1))
		_ = response.Body.Close()
		if readErr != nil {
			return tavilyRawResponse{}, &tavilyRequestFailure{Meta: meta, Err: &WebSearchProviderError{
				Code: WebSearchErrorProviderUnavailable, StatusCode: response.StatusCode, Retryable: true, Message: "read response: " + readErr.Error(),
			}}
		}
		meta.ResponseBytes = len(body)
		if len(body) > maxWebSearchResponseBytes {
			return tavilyRawResponse{}, &tavilyRequestFailure{Meta: meta, Err: &WebSearchProviderError{
				Code: WebSearchErrorProviderUnavailable, StatusCode: response.StatusCode, Retryable: false, Message: "response exceeded 2 MiB",
			}}
		}
		return tavilyRawResponse{Body: body, Meta: meta}, nil
	}
	return tavilyRawResponse{}, &tavilyRequestFailure{Meta: meta, Err: &WebSearchProviderError{Code: WebSearchErrorProviderUnavailable, Retryable: true}}
}

func addTavilyAuditMetadata(data map[string]any, meta tavilyRequestMetadata, err error) {
	if meta.StatusCode != 0 {
		data["http_status"] = meta.StatusCode
	}
	if meta.RetryCount != 0 {
		data["retry_count"] = meta.RetryCount
	}
	if meta.ResponseBytes != 0 {
		data["provider_response_bytes"] = meta.ResponseBytes
	}
	var providerError *WebSearchProviderError
	if errors.As(err, &providerError) {
		data["error_code"] = providerError.Code
		data["retryable"] = providerError.Retryable
	}
}

func classifyTavilyStatus(statusCode int, retryAfterValue, message string) *WebSearchProviderError {
	retryAfter := parseWebRetryAfter(retryAfterValue, time.Now())
	result := &WebSearchProviderError{StatusCode: statusCode, Message: message, RetryAfter: retryAfter}
	switch {
	case statusCode == http.StatusBadRequest || statusCode == http.StatusUnprocessableEntity:
		result.Code = WebSearchErrorInvalidRequest
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		result.Code = WebSearchErrorAuthenticationFailed
	case statusCode == 432 || statusCode == 433:
		result.Code = WebSearchErrorQuotaExhausted
	case statusCode == http.StatusTooManyRequests:
		result.Code = WebSearchErrorRateLimited
		result.Retryable = retryAfter == 0 || retryAfter <= maxWebRetryAfter
	case statusCode == http.StatusRequestTimeout:
		result.Code = WebSearchErrorTimeout
		result.Retryable = true
	case statusCode >= 500:
		result.Code = WebSearchErrorProviderUnavailable
		result.Retryable = true
	default:
		result.Code = WebSearchErrorInvalidRequest
	}
	return result
}

func parseWebRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0
	}
	return when.Sub(now)
}

func waitWebRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func truncateUTF8Bytes(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	end := maxBytes
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end]
}

func fairTruncateWebContents(values []string, maxBytes int) []string {
	result := make([]string, len(values))
	if maxBytes <= 0 || len(values) == 0 {
		return result
	}
	pending := make([]int, len(values))
	for index := range values {
		pending[index] = index
	}
	remaining := maxBytes
	for len(pending) > 0 && remaining > 0 {
		share := remaining / len(pending)
		if share == 0 {
			break
		}
		next := pending[:0]
		completed := false
		for _, index := range pending {
			if len(values[index]) <= share {
				result[index] = values[index]
				remaining -= len(values[index])
				completed = true
				continue
			}
			next = append(next, index)
		}
		if completed {
			pending = next
			continue
		}
		for _, index := range pending {
			result[index] = truncateUTF8Bytes(values[index], share)
			remaining -= len(result[index])
		}
		break
	}
	return result
}

func fitWebSearchResponseBudget(response *domain.WebSearchResponse, maxBytes int) {
	if response == nil {
		return
	}
	originalTotal := 0
	desired := make([]string, len(response.Results))
	for index := range response.Results {
		originalBytes := response.Results[index].OriginalBytes
		if originalBytes == 0 && response.Results[index].Content != "" {
			originalBytes = len(response.Results[index].Content)
		}
		response.Results[index].OriginalBytes = originalBytes
		originalTotal += originalBytes
		desired[index] = truncateUTF8Bytes(response.Results[index].Content, maxWebSearchResultContentBytes)
		response.Results[index].Content = ""
		response.Results[index].ReturnedBytes = 0
		response.Results[index].Truncated = originalBytes > 0
	}
	response.OriginalBytes = originalTotal
	baseBytes := marshaledWebBytes(response)
	available := max(0, maxBytes-baseBytes-512)
	contents := fairTruncateWebContents(desired, available)
	for index := range response.Results {
		response.Results[index].Content = contents[index]
		response.Results[index].ReturnedBytes = len(contents[index])
		response.Results[index].Truncated = response.Results[index].ReturnedBytes < response.Results[index].OriginalBytes
	}
	for attempt := 0; attempt < 4; attempt++ {
		refreshWebSearchResponseStats(response)
		if marshaledWebBytes(response) <= maxBytes {
			return
		}
		shrinkWebSearchResponse(response, maxBytes)
	}
	refreshWebSearchResponseStats(response)
}

func refreshWebSearchResponseStats(response *domain.WebSearchResponse) {
	response.ReturnedBytes = 0
	response.Truncated = response.OmittedResults > 0
	for index := range response.Results {
		response.Results[index].ReturnedBytes = len(response.Results[index].Content)
		response.Results[index].Truncated = response.Results[index].ReturnedBytes < response.Results[index].OriginalBytes
		response.ReturnedBytes += response.Results[index].ReturnedBytes
		response.Truncated = response.Truncated || response.Results[index].Truncated
	}
}

func shrinkWebSearchResponse(response *domain.WebSearchResponse, maxBytes int) {
	for marshaledWebBytes(response) > maxBytes {
		largest := -1
		for index := range response.Results {
			if largest < 0 || len(response.Results[index].Content) > len(response.Results[largest].Content) {
				largest = index
			}
		}
		if largest >= 0 && response.Results[largest].Content != "" {
			response.Results[largest].Content = truncateUTF8Bytes(response.Results[largest].Content, len(response.Results[largest].Content)/2)
			response.Results[largest].ReturnedBytes = len(response.Results[largest].Content)
			response.Results[largest].Truncated = true
			response.Truncated = true
			continue
		}
		if len(response.Results) <= 1 {
			break
		}
		response.Results = response.Results[:len(response.Results)-1]
		response.OmittedResults++
		response.Truncated = true
	}
}

func fitWebExtractResponseBudget(response *domain.WebExtractResponse, maxBytes int) {
	if response == nil {
		return
	}
	originalTotal := 0
	desired := make([]string, len(response.Results))
	for index := range response.Results {
		originalBytes := response.Results[index].OriginalBytes
		if originalBytes == 0 && response.Results[index].RawContent != "" {
			originalBytes = len(response.Results[index].RawContent)
		}
		response.Results[index].OriginalBytes = originalBytes
		originalTotal += originalBytes
		desired[index] = truncateUTF8Bytes(response.Results[index].RawContent, maxWebExtractResultContentBytes)
		response.Results[index].RawContent = ""
		response.Results[index].ReturnedBytes = 0
		response.Results[index].Truncated = originalBytes > 0
	}
	response.OriginalBytes = originalTotal
	baseBytes := marshaledWebBytes(response)
	available := max(0, maxBytes-baseBytes-512)
	contents := fairTruncateWebContents(desired, available)
	for index := range response.Results {
		response.Results[index].RawContent = contents[index]
		response.Results[index].ReturnedBytes = len(contents[index])
		response.Results[index].Truncated = response.Results[index].ReturnedBytes < response.Results[index].OriginalBytes
	}
	for attempt := 0; attempt < 4; attempt++ {
		refreshWebExtractResponseStats(response)
		if marshaledWebBytes(response) <= maxBytes {
			return
		}
		shrinkWebExtractResponse(response, maxBytes)
	}
	refreshWebExtractResponseStats(response)
}

func refreshWebExtractResponseStats(response *domain.WebExtractResponse) {
	response.ReturnedBytes = 0
	response.Truncated = response.OmittedResults > 0
	for index := range response.Results {
		response.Results[index].ReturnedBytes = len(response.Results[index].RawContent)
		response.Results[index].Truncated = response.Results[index].ReturnedBytes < response.Results[index].OriginalBytes
		response.ReturnedBytes += response.Results[index].ReturnedBytes
		response.Truncated = response.Truncated || response.Results[index].Truncated
	}
}

func shrinkWebExtractResponse(response *domain.WebExtractResponse, maxBytes int) {
	for marshaledWebBytes(response) > maxBytes {
		largest := -1
		for index := range response.Results {
			if largest < 0 || len(response.Results[index].RawContent) > len(response.Results[largest].RawContent) {
				largest = index
			}
		}
		if largest >= 0 && response.Results[largest].RawContent != "" {
			response.Results[largest].RawContent = truncateUTF8Bytes(response.Results[largest].RawContent, len(response.Results[largest].RawContent)/2)
			response.Results[largest].ReturnedBytes = len(response.Results[largest].RawContent)
			response.Results[largest].Truncated = true
			response.Truncated = true
			continue
		}
		if len(response.Results) <= 1 {
			break
		}
		response.Results = response.Results[:len(response.Results)-1]
		response.OmittedResults++
		response.Truncated = true
	}
}

func marshaledWebBytes(value any) int {
	encoded, err := json.Marshal(value)
	if err != nil {
		return 0
	}
	return len(encoded)
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
		input.MaxResults = min(defaultWebSearchRequestResults, configuredMax)
	}
	if input.MaxResults < domain.MinWebSearchMaxResults || input.MaxResults > configuredMax {
		return domain.WebSearchRequest{}, fmt.Errorf("max_results must be between %d and %d", domain.MinWebSearchMaxResults, configuredMax)
	}
	input.Topic = strings.ToLower(strings.TrimSpace(input.Topic))
	if input.Topic == "" {
		input.Topic = "general"
	}
	if input.Topic != "general" && input.Topic != "news" && input.Topic != "finance" {
		return domain.WebSearchRequest{}, fmt.Errorf("topic must be general, news, or finance")
	}
	input.SearchDepth = strings.ToLower(strings.TrimSpace(input.SearchDepth))
	if input.SearchDepth == "" {
		input.SearchDepth = "basic"
	}
	if input.SearchDepth != "basic" && input.SearchDepth != "advanced" && input.SearchDepth != "fast" && input.SearchDepth != "ultra-fast" {
		return domain.WebSearchRequest{}, fmt.Errorf("search_depth must be basic, advanced, fast, or ultra-fast")
	}
	if input.ChunksPerSource < 0 || input.ChunksPerSource > 3 {
		return domain.WebSearchRequest{}, fmt.Errorf("chunks_per_source must be between 1 and 3 when set")
	}
	if input.ChunksPerSource > 0 && input.SearchDepth != "advanced" {
		return domain.WebSearchRequest{}, fmt.Errorf("chunks_per_source requires search_depth=advanced")
	}
	input.TimeRange = strings.ToLower(strings.TrimSpace(input.TimeRange))
	if input.TimeRange != "" && input.TimeRange != "day" && input.TimeRange != "week" && input.TimeRange != "month" && input.TimeRange != "year" {
		return domain.WebSearchRequest{}, fmt.Errorf("time_range must be day, week, month, or year")
	}
	input.StartDate = strings.TrimSpace(input.StartDate)
	input.EndDate = strings.TrimSpace(input.EndDate)
	if input.TimeRange != "" && (input.StartDate != "" || input.EndDate != "") {
		return domain.WebSearchRequest{}, fmt.Errorf("time_range cannot be combined with start_date or end_date")
	}
	var startDate, endDate time.Time
	var err error
	if input.StartDate != "" {
		startDate, err = time.Parse(time.DateOnly, input.StartDate)
		if err != nil {
			return domain.WebSearchRequest{}, fmt.Errorf("start_date must use YYYY-MM-DD")
		}
	}
	if input.EndDate != "" {
		endDate, err = time.Parse(time.DateOnly, input.EndDate)
		if err != nil {
			return domain.WebSearchRequest{}, fmt.Errorf("end_date must use YYYY-MM-DD")
		}
	}
	if !startDate.IsZero() && !endDate.IsZero() && startDate.After(endDate) {
		return domain.WebSearchRequest{}, fmt.Errorf("start_date must not be after end_date")
	}
	if input.IncludeDomains, err = normalizeWebSearchDomains(input.IncludeDomains); err != nil {
		return domain.WebSearchRequest{}, fmt.Errorf("include_domains: %w", err)
	}
	if input.ExcludeDomains, err = normalizeWebSearchDomains(input.ExcludeDomains); err != nil {
		return domain.WebSearchRequest{}, fmt.Errorf("exclude_domains: %w", err)
	}
	excluded := make(map[string]struct{}, len(input.ExcludeDomains))
	for _, value := range input.ExcludeDomains {
		excluded[value] = struct{}{}
	}
	for _, value := range input.IncludeDomains {
		if _, conflict := excluded[value]; conflict {
			return domain.WebSearchRequest{}, fmt.Errorf("domain %q cannot be both included and excluded", value)
		}
	}
	return input, nil
}

func normalizeWebExtractRequest(input domain.WebExtractRequest) (domain.WebExtractRequest, error) {
	if len(input.URLs) == 0 || len(input.URLs) > maxWebExtractURLs {
		return domain.WebExtractRequest{}, fmt.Errorf("urls must contain between 1 and %d public URLs", maxWebExtractURLs)
	}
	result := domain.WebExtractRequest{
		URLs: make([]string, 0, len(input.URLs)), Query: strings.TrimSpace(input.Query),
		ExtractDepth: strings.ToLower(strings.TrimSpace(input.ExtractDepth)), ChunksPerSource: input.ChunksPerSource,
	}
	if len(result.Query) > 2000 {
		return domain.WebExtractRequest{}, fmt.Errorf("query must not exceed 2000 bytes")
	}
	if result.ExtractDepth == "" {
		result.ExtractDepth = "basic"
	}
	if result.ExtractDepth != "basic" && result.ExtractDepth != "advanced" {
		return domain.WebExtractRequest{}, fmt.Errorf("extract_depth must be basic or advanced")
	}
	if result.ChunksPerSource < 0 || result.ChunksPerSource > 5 {
		return domain.WebExtractRequest{}, fmt.Errorf("chunks_per_source must be between 1 and 5 when set")
	}
	if result.ChunksPerSource > 0 && result.Query == "" {
		return domain.WebExtractRequest{}, fmt.Errorf("chunks_per_source requires query")
	}
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
	port := parsed.Port()
	if strings.Contains(parsed.Host, ":") && port == "" && net.ParseIP(host) == nil {
		return "", fmt.Errorf("invalid URL port in %q", value)
	}
	if port != "" {
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return "", fmt.Errorf("invalid URL port in %q", value)
		}
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if port == "" || parsed.Scheme == "http" && port == "80" || parsed.Scheme == "https" && port == "443" {
		if strings.Contains(host, ":") {
			parsed.Host = "[" + host + "]"
		} else {
			parsed.Host = host
		}
	} else {
		parsed.Host = net.JoinHostPort(host, port)
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
