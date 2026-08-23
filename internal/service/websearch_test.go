package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestTavilyWebSearchUsesConfiguredProxyAndKeepsCredentialsEncrypted(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		http.Error(w, "request bypassed proxy", http.StatusBadGateway)
	}))
	defer target.Close()

	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/search" {
			t.Errorf("unexpected proxied request: %s %s", r.Method, r.URL.String())
		}
		if r.Header.Get("Authorization") != "Bearer tvly-test-secret" {
			t.Errorf("missing Tavily bearer token: %q", r.Header.Get("Authorization"))
		}
		wantProxyAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("proxy-user:proxy-secret"))
		if r.Header.Get("Proxy-Authorization") != wantProxyAuth {
			t.Errorf("unexpected proxy authorization: %q", r.Header.Get("Proxy-Authorization"))
		}
		var input tavilySearchRequest
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if input.Query != "current Go release" || input.MaxResults != 2 || input.TimeRange != "month" || len(input.IncludeDomains) != 1 || input.IncludeDomains[0] != "go.dev" || input.IncludeAnswer || input.IncludeRaw {
			t.Errorf("unexpected Tavily request: %#v", input)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"Go release","url":"https://go.dev/doc/devel/release","content":"reflected tvly-test-secret and proxy-secret","score":0.9,"published_date":"2026-07-01"}],"response_time":0.12}`))
	}))
	defer proxy.Close()

	sharedProxy, err := svc.SaveProxy(ctx, domain.ProxyInput{
		Name: "Tavily proxy", URL: proxy.URL, Username: "proxy-user", Password: "proxy-secret",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := svc.SaveWebSearchSettings(ctx, domain.WebSearchSettingsInput{
		Enabled: true, BaseURL: target.URL, APIKey: "tvly-test-secret", ProxyID: sharedProxy.ID,
		TimeoutSeconds: 10, MaxResults: 4,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !saved.HasAPIKey || saved.ProxyID != sharedProxy.ID || saved.APIKeyCipher != "" {
		t.Fatalf("public settings exposed or lost credential state: %#v", saved)
	}
	serialized, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(serialized), "tvly-test-secret") || strings.Contains(string(serialized), "proxy-secret") {
		t.Fatalf("settings JSON exposed credentials: %s", serialized)
	}
	stored, err := svc.store.GetWebSearchSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.APIKeyCipher == "" || stored.APIKeyCipher == "tvly-test-secret" {
		t.Fatalf("credentials were not encrypted at rest: %#v", stored)
	}
	storedProxy, err := svc.store.GetProxy(ctx, sharedProxy.ID)
	if err != nil || storedProxy.PasswordCipher == "" || storedProxy.PasswordCipher == "proxy-secret" {
		t.Fatalf("proxy credentials were not encrypted at rest: proxy=%#v err=%v", storedProxy, err)
	}

	result, err := svc.SearchWeb(ctx, domain.WebSearchRequest{
		Query: "current Go release", MaxResults: 2, TimeRange: "month", IncludeDomains: []string{"GO.DEV"},
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if proxyHits.Load() != 1 || targetHits.Load() != 0 {
		t.Fatalf("proxy routing failed: proxy=%d target=%d", proxyHits.Load(), targetHits.Load())
	}
	if result.Provider != "tavily" || !result.ContentIsUntrusted || len(result.Results) != 1 || result.Results[0].Title != "Go release" {
		t.Fatalf("unexpected normalized result: %#v", result)
	}
	if strings.Contains(result.Results[0].Content, "tvly-test-secret") || strings.Contains(result.Results[0].Content, "proxy-secret") {
		t.Fatalf("provider response exposed configured credentials: %#v", result.Results[0])
	}

	preserved, err := svc.SaveWebSearchSettings(ctx, domain.WebSearchSettingsInput{
		Enabled: false, BaseURL: target.URL, ProxyID: sharedProxy.ID,
		TimeoutSeconds: 10, MaxResults: 4,
	}, "test")
	if err != nil || !preserved.HasAPIKey || preserved.ProxyID != sharedProxy.ID {
		t.Fatalf("blank secret input did not preserve credentials: settings=%#v err=%v", preserved, err)
	}
	cleared, err := svc.SaveProxy(ctx, domain.ProxyInput{
		ID: sharedProxy.ID, Name: sharedProxy.Name, URL: sharedProxy.URL, Username: sharedProxy.Username, ClearPassword: true,
	}, "test")
	if err != nil || cleared.HasPassword {
		t.Fatalf("proxy password was not cleared independently: proxy=%#v err=%v", cleared, err)
	}
}

func TestWebSearchValidatesConfigurationAndInput(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.SearchWeb(ctx, domain.WebSearchRequest{Query: "test"}, "test"); !errors.Is(err, ErrWebSearchDisabled) {
		t.Fatalf("disabled search returned %v", err)
	}
	if _, err := svc.SaveWebSearchSettings(ctx, domain.WebSearchSettingsInput{
		Enabled: true, BaseURL: domain.DefaultWebSearchBaseURL, TimeoutSeconds: 20, MaxResults: 5,
	}, "test"); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("enabled search without key was accepted: %v", err)
	}
	if _, err := normalizeWebSearchRequest(domain.WebSearchRequest{Query: "test", IncludeDomains: []string{"https://example.com/path"}}, 5); err == nil {
		t.Fatal("domain with scheme and path was accepted")
	}
	defaulted, err := normalizeWebSearchRequest(domain.WebSearchRequest{Query: "test"}, 17)
	if err != nil || defaulted.MaxResults != defaultWebSearchRequestResults {
		t.Fatalf("omitted max_results did not use the bounded tool default: request=%#v err=%v", defaulted, err)
	}
	if _, err := normalizeWebSearchRequest(domain.WebSearchRequest{Query: "test", MaxResults: 18}, 17); err == nil {
		t.Fatal("max_results above the administrator limit was accepted")
	}
	for _, proxyURL := range []string{
		"http://127.0.0.1:7890", "https://proxy.example:8443", "socks5://127.0.0.1:1080", "socks5h://proxy.example:1080",
	} {
		if saved, err := svc.SaveProxy(ctx, domain.ProxyInput{Name: proxyURL, URL: proxyURL}, "test"); err != nil || saved.URL != proxyURL {
			t.Errorf("proxy URL %q normalized to %q with error %v", proxyURL, saved.URL, err)
		}
	}
	if _, err := svc.SaveProxy(ctx, domain.ProxyInput{Name: "invalid", URL: "ftp://proxy.example:21"}, "test"); err == nil {
		t.Fatal("unsupported proxy scheme was accepted")
	}
	if normalized, err := normalizeTavilyBaseURL("https://api.tavily.com/extract"); err != nil || normalized != "https://api.tavily.com" {
		t.Fatalf("extract endpoint was not normalized to its API base: url=%q err=%v", normalized, err)
	}
}

func TestTavilyWebExtractUsesConfiguredProxyAndReturnsPartialResults(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		http.Error(w, "request bypassed proxy", http.StatusBadGateway)
	}))
	defer target.Close()

	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/extract" {
			t.Errorf("unexpected proxied request: %s %s", r.Method, r.URL.String())
		}
		if r.Header.Get("Authorization") != "Bearer tvly-extract-secret" {
			t.Errorf("missing Tavily bearer token: %q", r.Header.Get("Authorization"))
		}
		var input tavilyExtractRequest
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if len(input.URLs) != 2 || input.URLs[0] != "https://example.com/guide" || input.ExtractDepth != "basic" || input.Format != "markdown" || input.IncludeImages {
			t.Errorf("unexpected Tavily extract request: %#v", input)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"url":"https://example.com/guide","raw_content":"guide containing tvly-extract-secret and proxy-extract-secret"}],"failed_results":[{"url":"https://example.org/missing","error":"fetch failed with proxy-extract-secret"}],"response_time":0.21}`))
	}))
	defer proxy.Close()

	sharedProxy, err := svc.SaveProxy(ctx, domain.ProxyInput{
		Name: "Tavily extract proxy", URL: proxy.URL, Username: "proxy-user", Password: "proxy-extract-secret",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.SaveWebSearchSettings(ctx, domain.WebSearchSettingsInput{
		Enabled: true, BaseURL: target.URL, APIKey: "tvly-extract-secret", ProxyID: sharedProxy.ID,
		TimeoutSeconds: 10, MaxResults: 4,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.ExtractWeb(ctx, domain.WebExtractRequest{URLs: []string{
		"https://example.com/guide#install", "https://example.org/missing",
	}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if proxyHits.Load() != 1 || targetHits.Load() != 0 {
		t.Fatalf("proxy routing failed: proxy=%d target=%d", proxyHits.Load(), targetHits.Load())
	}
	if result.Provider != "tavily" || !result.ContentIsUntrusted || len(result.Results) != 1 || len(result.FailedResults) != 1 {
		t.Fatalf("unexpected extract result: %#v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "tvly-extract-secret") || strings.Contains(string(encoded), "proxy-extract-secret") {
		t.Fatalf("extract result exposed configured credentials: %s", encoded)
	}
}

func TestWebExtractValidatesURLs(t *testing.T) {
	normalized, err := normalizeWebExtractRequest(domain.WebExtractRequest{URLs: []string{
		"https://example.com/docs#one", "https://example.com/docs#two", "HTTPS://EXAMPLE.COM:443/docs#three",
	}})
	if err != nil || len(normalized.URLs) != 1 || normalized.URLs[0] != "https://example.com/docs" {
		t.Fatalf("URLs were not normalized and deduplicated: request=%#v err=%v", normalized, err)
	}
	for _, value := range []string{
		"", "file:///etc/passwd", "https://user:secret@example.com/", "http://localhost/test",
		"http://127.0.0.1/test", "http://127.1/test", "http://10.0.0.1/test", "http://169.254.169.254/latest/meta-data", "https://host.internal/docs", "https://example.com:bad/docs",
	} {
		if _, err := normalizeWebExtractRequest(domain.WebExtractRequest{URLs: []string{value}}); err == nil {
			t.Errorf("unsafe extract URL %q was accepted", value)
		}
	}
	tooMany := make([]string, maxWebExtractURLs+1)
	for index := range tooMany {
		tooMany[index] = fmt.Sprintf("https://example.com/%d", index)
	}
	if _, err := normalizeWebExtractRequest(domain.WebExtractRequest{URLs: tooMany}); err == nil {
		t.Fatal("too many extract URLs were accepted")
	}
}

func TestWebExtractPreservesCompleteContent(t *testing.T) {
	svc, _, _ := newTestService(t)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/extract" {
			t.Errorf("unexpected Tavily path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tavilyExtractResponse{Results: []domain.WebExtractResult{{
			URL: "https://example.com/large", RawContent: strings.Repeat("x", 9<<10),
		}}})
	}))
	defer provider.Close()

	_, err := svc.SaveWebSearchSettings(context.Background(), domain.WebSearchSettingsInput{
		Enabled: true, BaseURL: provider.URL, APIKey: "test-key", TimeoutSeconds: 20, MaxResults: 5,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.ExtractWeb(context.Background(), domain.WebExtractRequest{URLs: []string{"https://example.com/large"}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || len(result.Results[0].RawContent) != 9<<10 {
		t.Fatalf("complete extracted content was not preserved: %#v", result)
	}
}

func TestTavilyRequestPreservesContextCancellation(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var response tavilyExtractResponse
	_, err := svc.requestTavily(ctx, resolvedWebSearchSettings{
		WebSearchSettings: domain.WebSearchSettings{BaseURL: "http://127.0.0.1:1", TimeoutSeconds: 5},
		APIKey:            "test",
	}, "/extract", tavilyExtractRequest{URLs: []string{"https://example.com"}}, &response)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Tavily request returned %v", err)
	}
}

func TestWebRequestsValidateAdvancedRetrievalParameters(t *testing.T) {
	search, err := normalizeWebSearchRequest(domain.WebSearchRequest{
		Query: " releases ", Topic: "NEWS", SearchDepth: "advanced", StartDate: "2026-07-01", EndDate: "2026-08-01",
		ChunksPerSource: 2, IncludeDomains: []string{"GO.DEV"}, ExcludeDomains: []string{"example.com"},
	}, 17)
	if err != nil {
		t.Fatal(err)
	}
	if search.Query != "releases" || search.MaxResults != defaultWebSearchRequestResults || search.Topic != "news" || search.SearchDepth != "advanced" || search.ChunksPerSource != 2 || search.IncludeDomains[0] != "go.dev" {
		t.Fatalf("advanced search normalization = %#v", search)
	}
	for _, input := range []domain.WebSearchRequest{
		{Query: "test", TimeRange: "week", StartDate: "2026-01-01"},
		{Query: "test", StartDate: "2026-02-01", EndDate: "2026-01-01"},
		{Query: "test", SearchDepth: "basic", ChunksPerSource: 1},
		{Query: "test", IncludeDomains: []string{"example.com"}, ExcludeDomains: []string{"example.com"}},
	} {
		if _, err := normalizeWebSearchRequest(input, 10); err == nil {
			t.Errorf("invalid search parameters were accepted: %#v", input)
		}
	}

	extract, err := normalizeWebExtractRequest(domain.WebExtractRequest{
		URLs: []string{"https://example.com/docs"}, Query: " installation ", ExtractDepth: "ADVANCED", ChunksPerSource: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if extract.Query != "installation" || extract.ExtractDepth != "advanced" || extract.ChunksPerSource != 4 {
		t.Fatalf("advanced extract normalization = %#v", extract)
	}
	if _, err := normalizeWebExtractRequest(domain.WebExtractRequest{URLs: []string{"https://example.com"}, ChunksPerSource: 1}); err == nil {
		t.Fatal("extract chunks_per_source without query was accepted")
	}
}

func TestTavilyAdvancedParametersAndUsageMetadata(t *testing.T) {
	svc, _, _ := newTestService(t)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/search":
			var input tavilySearchRequest
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			if input.Topic != "news" || input.SearchDepth != "advanced" || input.StartDate != "2026-07-01" || input.EndDate != "2026-08-01" || input.ChunksPerSource != 2 {
				t.Errorf("advanced search payload = %#v", input)
			}
			_, _ = w.Write([]byte(`{"results":[{"title":"Release","url":"https://go.dev/release","content":"details"}],"request_id":"req-search","usage":{"credits":2}}`))
		case "/extract":
			var input tavilyExtractRequest
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			if input.Query != "installation" || input.ExtractDepth != "advanced" || input.ChunksPerSource != 4 {
				t.Errorf("advanced extract payload = %#v", input)
			}
			_, _ = w.Write([]byte(`{"results":[{"url":"https://go.dev/release","raw_content":"details"}],"request_id":"req-extract","usage":{"credits":2}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	_, err := svc.SaveWebSearchSettings(context.Background(), domain.WebSearchSettingsInput{
		Enabled: true, BaseURL: provider.URL, APIKey: "test-key", TimeoutSeconds: 20, MaxResults: 10,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	search, err := svc.SearchWeb(context.Background(), domain.WebSearchRequest{
		Query: "releases", Topic: "news", SearchDepth: "advanced", StartDate: "2026-07-01", EndDate: "2026-08-01", ChunksPerSource: 2,
	}, "test")
	if err != nil || search.RequestID != "req-search" || search.Credits != 2 {
		t.Fatalf("search metadata = %#v, err=%v", search, err)
	}
	extract, err := svc.ExtractWeb(context.Background(), domain.WebExtractRequest{
		URLs: []string{"https://go.dev/release"}, Query: "installation", ExtractDepth: "advanced", ChunksPerSource: 4,
	}, "test")
	if err != nil || extract.RequestID != "req-extract" || extract.Credits != 2 {
		t.Fatalf("extract metadata = %#v, err=%v", extract, err)
	}
}

func TestWebSearchBoundsModelPayloadAndFiltersProviderURLs(t *testing.T) {
	svc, _, _ := newTestService(t)
	providerResults := []domain.WebSearchResult{
		{Title: "unsafe", URL: "http://127.0.0.1/private", Content: "private"},
		{Title: "duplicate", URL: "https://source-0.example.com/page#duplicate", Content: "duplicate"},
	}
	largeContent := strings.Repeat("界🙂", 1200)
	for index := 0; index < 20; index++ {
		providerResults = append(providerResults, domain.WebSearchResult{
			Title: fmt.Sprintf("Source %d", index), URL: fmt.Sprintf("https://source-%d.example.com/page#section", index),
			Content: largeContent, Score: 1 - float64(index)/100,
		})
	}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tavilySearchResponse{Results: providerResults, RequestID: "req-large", Usage: tavilyUsage{Credits: 2}})
	}))
	defer provider.Close()
	_, err := svc.SaveWebSearchSettings(context.Background(), domain.WebSearchSettingsInput{
		Enabled: true, BaseURL: provider.URL, APIKey: "test-key", TimeoutSeconds: 20, MaxResults: 20,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.SearchWeb(context.Background(), domain.WebSearchRequest{Query: "large", MaxResults: 20}, "test")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > maxWebSearchModelResponseBytes {
		t.Fatalf("search model payload = %d, want <= %d", len(encoded), maxWebSearchModelResponseBytes)
	}
	if len(result.Results) != 20 || !result.Truncated || result.OmittedResults != 2 || result.OriginalBytes <= result.ReturnedBytes {
		t.Fatalf("search budget metadata = %#v", result)
	}
	seen := make(map[string]bool, len(result.Results))
	for _, item := range result.Results {
		if !utf8.ValidString(item.Content) || len(item.Content) > maxWebSearchResultContentBytes || item.Truncated != (item.ReturnedBytes < item.OriginalBytes) || item.ReturnedBytes != len(item.Content) {
			t.Fatalf("invalid bounded search result: %#v", item)
		}
		if strings.Contains(item.URL, "127.0.0.1") || strings.Contains(item.URL, "#") || seen[item.URL] {
			t.Fatalf("unsafe or duplicate search URL survived: %q", item.URL)
		}
		seen[item.URL] = true
	}
}

func TestWebExtractBoundsAggregateModelPayload(t *testing.T) {
	svc, _, _ := newTestService(t)
	providerResults := make([]domain.WebExtractResult, 5)
	largeContent := strings.Repeat("文🙂", 8000)
	for index := range providerResults {
		providerResults[index] = domain.WebExtractResult{URL: fmt.Sprintf("https://source-%d.example.com/page", index), RawContent: largeContent}
	}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tavilyExtractResponse{Results: providerResults, RequestID: "req-large-extract"})
	}))
	defer provider.Close()
	_, err := svc.SaveWebSearchSettings(context.Background(), domain.WebSearchSettingsInput{
		Enabled: true, BaseURL: provider.URL, APIKey: "test-key", TimeoutSeconds: 20, MaxResults: 10,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	urls := make([]string, len(providerResults))
	for index := range providerResults {
		urls[index] = providerResults[index].URL
	}
	result, err := svc.ExtractWeb(context.Background(), domain.WebExtractRequest{URLs: urls}, "test")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > maxWebExtractModelResponseBytes {
		t.Fatalf("extract model payload = %d, want <= %d", len(encoded), maxWebExtractModelResponseBytes)
	}
	if len(result.Results) != 5 || !result.Truncated || result.OriginalBytes <= result.ReturnedBytes {
		t.Fatalf("extract budget metadata = %#v", result)
	}
	for _, item := range result.Results {
		if !utf8.ValidString(item.RawContent) || len(item.RawContent) > maxWebExtractResultContentBytes || !item.Truncated || item.ReturnedBytes != len(item.RawContent) {
			t.Fatalf("invalid bounded extract result: %#v", item)
		}
	}
}

func TestTavilyProviderErrorsAreClassifiedAndRetriedOnce(t *testing.T) {
	testCases := []struct {
		name       string
		status     int
		retryAfter string
		wantCode   string
		wantHits   int32
		wantOK     bool
		retryable  bool
	}{
		{name: "invalid request", status: http.StatusBadRequest, wantCode: WebSearchErrorInvalidRequest, wantHits: 1},
		{name: "authentication", status: http.StatusUnauthorized, wantCode: WebSearchErrorAuthenticationFailed, wantHits: 1},
		{name: "short rate limit", status: http.StatusTooManyRequests, wantHits: 2, wantOK: true},
		{name: "long rate limit", status: http.StatusTooManyRequests, retryAfter: "10", wantCode: WebSearchErrorRateLimited, wantHits: 1},
		{name: "provider unavailable", status: http.StatusServiceUnavailable, wantHits: 2, wantOK: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			svc, _, _ := newTestService(t)
			var hits atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				current := hits.Add(1)
				if current == 1 || testCase.wantHits == 1 {
					if testCase.retryAfter != "" {
						w.Header().Set("Retry-After", testCase.retryAfter)
					}
					w.WriteHeader(testCase.status)
					_, _ = w.Write([]byte(`{"error":"` + strings.Repeat("x", 8<<10) + `"}`))
					return
				}
				_, _ = w.Write([]byte(`{"results":[]}`))
			}))
			defer provider.Close()
			var output tavilySearchResponse
			meta, err := svc.requestTavily(context.Background(), resolvedWebSearchSettings{
				WebSearchSettings: domain.WebSearchSettings{BaseURL: provider.URL, TimeoutSeconds: 5}, APIKey: "test",
			}, "/search", tavilySearchRequest{Query: "test", SearchDepth: "basic", MaxResults: 1}, &output)
			if testCase.wantOK {
				if err != nil || meta.RetryCount != 1 {
					t.Fatalf("retry result meta=%#v err=%v", meta, err)
				}
			} else {
				var providerError *WebSearchProviderError
				if !errors.As(err, &providerError) || providerError.Code != testCase.wantCode || providerError.Retryable != testCase.retryable || len(err.Error()) > maxWebSearchErrorBytes+256 {
					t.Fatalf("provider error = %#v, err=%v", providerError, err)
				}
			}
			if hits.Load() != testCase.wantHits {
				t.Fatalf("provider hits = %d, want %d", hits.Load(), testCase.wantHits)
			}
		})
	}
}

func TestTavilyIdenticalInflightRequestsAreCoalesced(t *testing.T) {
	svc, _, _ := newTestService(t)
	var hits atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			close(started)
		}
		<-release
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer provider.Close()
	settings := resolvedWebSearchSettings{WebSearchSettings: domain.WebSearchSettings{BaseURL: provider.URL, TimeoutSeconds: 5}, APIKey: "test"}
	var group sync.WaitGroup
	errorsSeen := make(chan error, 2)
	for index := 0; index < 2; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			var output tavilySearchResponse
			_, err := svc.requestTavily(context.Background(), settings, "/search", tavilySearchRequest{Query: "same", SearchDepth: "basic", MaxResults: 1}, &output)
			errorsSeen <- err
		}()
	}
	<-started
	time.Sleep(20 * time.Millisecond)
	close(release)
	group.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("identical in-flight requests produced %d provider calls", hits.Load())
	}
}
