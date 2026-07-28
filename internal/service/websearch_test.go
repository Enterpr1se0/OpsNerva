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
	"sync/atomic"
	"testing"

	"eino-ops-agent/internal/domain"
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
	if err != nil || defaulted.MaxResults != 17 {
		t.Fatalf("omitted max_results did not use the administrator default: request=%#v err=%v", defaulted, err)
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
		"https://example.com/docs#one", "https://example.com/docs#two",
	}})
	if err != nil || len(normalized.URLs) != 1 || normalized.URLs[0] != "https://example.com/docs" {
		t.Fatalf("URLs were not normalized and deduplicated: request=%#v err=%v", normalized, err)
	}
	for _, value := range []string{
		"", "file:///etc/passwd", "https://user:secret@example.com/", "http://localhost/test",
		"http://127.0.0.1/test", "http://127.1/test", "http://10.0.0.1/test", "http://169.254.169.254/latest/meta-data", "https://host.internal/docs",
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
	err := svc.requestTavily(ctx, resolvedWebSearchSettings{
		WebSearchSettings: domain.WebSearchSettings{BaseURL: "http://127.0.0.1:1", TimeoutSeconds: 5},
		APIKey:            "test",
	}, "/extract", tavilyExtractRequest{URLs: []string{"https://example.com"}}, &response)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Tavily request returned %v", err)
	}
}
