package websearch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/security"
	"golang.org/x/sync/singleflight"
)

// Config is a resolved configuration snapshot for one call. It contains no
// persistence identifiers or ciphertext; changes apply to the next call.
type Config struct {
	BaseURL        string
	APIKey         string
	ProxyURL       string
	ProxyUsername  string
	ProxyPassword  string
	TimeoutSeconds int
	MaxResults     int
}

// Client owns request concurrency and in-flight deduplication. It has no
// dependency on Service, Store, approval, or audit persistence.
type Client struct {
	redactor *security.Redactor
	sem      chan struct{}
	requests singleflight.Group
}

func New(redactor *security.Redactor) *Client {
	return &Client{redactor: redactor, sem: make(chan struct{}, 4)}
}

// CallInfo describes a validated call without query text, URLs, or credentials.
type CallInfo struct {
	InputDigest   string
	InputCount    int
	Duration      time.Duration
	StatusCode    int
	RetryCount    int
	ResponseBytes int
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

// Search returns nil call info when input validation fails before a provider request.
func (c *Client) Search(ctx context.Context, settings Config, input domain.WebSearchRequest) (domain.WebSearchResponse, *CallInfo, error) {
	request, err := normalizeWebSearchRequest(input, settings.MaxResults)
	if err != nil {
		return domain.WebSearchResponse{}, nil, err
	}
	payload := tavilySearchRequest{
		Query: request.Query, Topic: request.Topic, SearchDepth: request.SearchDepth, MaxResults: request.MaxResults, TimeRange: request.TimeRange,
		StartDate: request.StartDate, EndDate: request.EndDate, ChunksPerSource: request.ChunksPerSource,
		IncludeDomains: request.IncludeDomains, ExcludeDomains: request.ExcludeDomains, IncludeAnswer: false, IncludeRaw: false,
	}
	queryDigest := sha256.Sum256([]byte(request.Query))
	started := time.Now()
	var decoded tavilySearchResponse
	requestMeta, err := c.requestTavily(ctx, settings, "/search", payload, &decoded)
	requestMeta.InputDigest = hex.EncodeToString(queryDigest[:])
	requestMeta.InputCount = 1
	defer func() { requestMeta.Duration = time.Since(started) }()
	if err != nil {
		return domain.WebSearchResponse{}, &requestMeta, err
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
		result.Title = truncateUTF8Bytes(c.scrubWebSearchText(result.Title, settings), maxWebResultTitleBytes)
		result.URL = normalizedURL
		result.Content = c.scrubWebSearchText(result.Content, settings)
		result.PublishedDate = truncateUTF8Bytes(c.scrubWebSearchText(result.PublishedDate, settings), maxWebResultDateBytes)
		result.OriginalBytes = len(result.Content)
		result.ReturnedBytes = result.OriginalBytes
		results = append(results, result)
	}
	result := domain.WebSearchResponse{
		Query: request.Query, Provider: "tavily", Results: results, ResponseTime: decoded.ResponseTime,
		RequestID: truncateUTF8Bytes(c.scrubWebSearchText(decoded.RequestID, settings), maxWebRequestIDBytes), Credits: decoded.Usage.Credits,
		OmittedResults: max(0, len(decoded.Results)-len(results)), ContentIsUntrusted: true,
	}
	fitWebSearchResponseBudget(&result, maxWebSearchModelResponseBytes)
	return result, &requestMeta, nil
}

// Extract returns nil call info when input validation fails before a provider request.
func (c *Client) Extract(ctx context.Context, settings Config, input domain.WebExtractRequest) (domain.WebExtractResponse, *CallInfo, error) {
	request, err := normalizeWebExtractRequest(input)
	if err != nil {
		return domain.WebExtractResponse{}, nil, err
	}
	urlsDigest := sha256.Sum256([]byte(strings.Join(request.URLs, "\n")))
	started := time.Now()
	var decoded tavilyExtractResponse
	requestMeta, err := c.requestTavily(ctx, settings, "/extract", tavilyExtractRequest{
		URLs: request.URLs, Query: request.Query, ExtractDepth: request.ExtractDepth, ChunksPerSource: request.ChunksPerSource,
		Format: "markdown", IncludeImages: false,
	}, &decoded)
	requestMeta.InputDigest = hex.EncodeToString(urlsDigest[:])
	requestMeta.InputCount = len(request.URLs)
	defer func() { requestMeta.Duration = time.Since(started) }()
	if err != nil {
		return domain.WebExtractResponse{Provider: "tavily", ContentIsUntrusted: true}, &requestMeta, err
	}

	result := domain.WebExtractResponse{
		Provider: "tavily", Query: request.Query, Results: make([]domain.WebExtractResult, 0, len(decoded.Results)),
		FailedResults: make([]domain.WebExtractFailedResult, 0, len(decoded.FailedResults)),
		ResponseTime:  decoded.ResponseTime,
		RequestID:     truncateUTF8Bytes(c.scrubWebSearchText(decoded.RequestID, settings), maxWebRequestIDBytes),
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
		content := c.scrubWebSearchText(extracted.RawContent, settings)
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
			URL: normalizedURL, Error: truncateUTF8Bytes(c.scrubWebSearchText(failed.Error, settings), maxWebFailedResultErrorBytes),
		})
	}
	result.OmittedResults = max(0, len(decoded.Results)+len(decoded.FailedResults)-len(result.Results)-len(result.FailedResults))
	fitWebExtractResponseBudget(&result, maxWebExtractModelResponseBytes)
	if len(result.Results) == 0 {
		return result, &requestMeta, &ProviderError{
			Code: ErrorProviderUnavailable, Retryable: true, Message: "Tavily did not extract any requested URL",
		}
	}
	return result, &requestMeta, nil
}
