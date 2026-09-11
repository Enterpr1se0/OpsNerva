package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/websearch"
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
		var input struct {
			Query          string
			MaxResults     int      `json:"max_results"`
			TimeRange      string   `json:"time_range"`
			IncludeDomains []string `json:"include_domains"`
			IncludeAnswer  bool     `json:"include_answer"`
			IncludeRaw     bool     `json:"include_raw_content"`
		}
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

func TestWebSearchUsesLiveConfigurationWithoutApproval(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	var hits atomic.Int32
	var wantAuthorization atomic.Value
	wantAuthorization.Store("Bearer first-key")
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != wantAuthorization.Load().(string) {
			t.Errorf("request used stale credentials: %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"results":[{"url":"https://example.com/docs","content":"search","raw_content":"extract"}]}`))
	}))
	defer provider.Close()
	input := domain.WebSearchSettingsInput{Enabled: true, BaseURL: provider.URL, APIKey: "first-key", TimeoutSeconds: 5, MaxResults: 2}
	if _, err := svc.SaveWebSearchSettings(ctx, input, "test"); err != nil {
		t.Fatal(err)
	}
	client := svc.webSearch
	reviewer := &fakeAutomaticApprovalReviewer{}
	explainer := &fakeCommandExplainer{}
	svc.SetAutomaticApprovalReviewer(reviewer)
	svc.SetApprovalReviewer(explainer)
	for _, mode := range []string{domain.ApprovalModeManual, domain.ApprovalModeAuto} {
		saveApprovalMode(t, svc, mode)
		if _, err := svc.SearchWeb(ctx, domain.WebSearchRequest{Query: "docs"}, "eino-agent"); err != nil {
			t.Fatalf("search in %s mode: %v", mode, err)
		}
		if _, err := svc.ExtractWeb(ctx, domain.WebExtractRequest{URLs: []string{"https://example.com/docs"}}, "mcp-client"); err != nil {
			t.Fatalf("extract in %s mode: %v", mode, err)
		}
	}
	approvals, err := svc.ListApprovals(ctx, "", 100)
	if err != nil || len(approvals) != 0 || len(reviewer.Inputs()) != 0 || len(explainer.Inputs()) != 0 {
		t.Fatalf("web request entered approval: approvals=%v err=%v", approvals, err)
	}
	input.APIKey = "second-key"
	input.MaxResults = 1
	wantAuthorization.Store("Bearer second-key")
	if _, err := svc.SaveWebSearchSettings(ctx, input, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SearchWeb(ctx, domain.WebSearchRequest{Query: "docs"}, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SearchWeb(ctx, domain.WebSearchRequest{Query: "docs", MaxResults: 2}, "test"); err == nil {
		t.Fatal("new result limit was not applied")
	}
	input.Enabled = false
	input.APIKey = ""
	if _, err := svc.SaveWebSearchSettings(ctx, input, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SearchWeb(ctx, domain.WebSearchRequest{Query: "docs"}, "test"); !errors.Is(err, ErrWebSearchDisabled) {
		t.Fatalf("disabled search: %v", err)
	}
	if _, err := svc.ExtractWeb(ctx, domain.WebExtractRequest{URLs: []string{"https://example.com/docs"}}, "test"); !errors.Is(err, ErrWebSearchDisabled) {
		t.Fatalf("disabled extract: %v", err)
	}
	if svc.webSearch != client || hits.Load() != 5 {
		t.Fatalf("client recreated or rejected request reached provider: hits=%d", hits.Load())
	}
}

func TestWebSearchAuditsProviderOutcomesWithoutRawInput(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		extract   bool
		invalid   bool
		status    int
		body      string
		wantEvent string
		wantError string
	}{
		{name: "search", status: 200, body: `{"results":[],"request_id":"request-search","usage":{"credits":2}}`, wantEvent: "web_search_completed"},
		{name: "search authentication", status: 401, body: `{"error":"private-api-key"}`, wantEvent: "web_search_failed", wantError: websearch.ErrorAuthenticationFailed},
		{name: "invalid search", invalid: true, status: 200},
		{name: "extract partial", extract: true, status: 200, body: `{"results":[{"url":"https://example.com/docs","raw_content":"page"}],"failed_results":[{"url":"https://example.org/docs","error":"unavailable"}]}`, wantEvent: "web_extract_completed"},
		{name: "extract empty", extract: true, status: 200, body: `{"results":[]}`, wantEvent: "web_extract_failed", wantError: websearch.ErrorProviderUnavailable},
		{name: "extract authentication", extract: true, status: 401, body: `{}`, wantEvent: "web_extract_failed", wantError: websearch.ErrorAuthenticationFailed},
		{name: "invalid extract", extract: true, invalid: true, status: 200},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			svc, _, _ := newTestService(t)
			ctx := context.Background()
			var hits atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				w.WriteHeader(testCase.status)
				_, _ = w.Write([]byte(testCase.body))
			}))
			defer provider.Close()
			if _, err := svc.SaveWebSearchSettings(ctx, domain.WebSearchSettingsInput{
				Enabled: true, BaseURL: provider.URL, APIKey: "private-api-key", TimeoutSeconds: 5, MaxResults: 2,
			}, "test"); err != nil {
				t.Fatal(err)
			}
			var callErr error
			digestInput, digestField := "private query", "query_sha256"
			if testCase.extract {
				digestInput, digestField = "https://example.com/docs\nhttps://example.org/docs", "urls_sha256"
				urls := []string{"https://example.com/docs#one", "https://example.com/docs#two", "https://example.org/docs"}
				if testCase.invalid {
					urls = nil
				}
				_, callErr = svc.ExtractWeb(ctx, domain.WebExtractRequest{URLs: urls}, "eino-agent")
			} else {
				query := " private query "
				if testCase.invalid {
					query = ""
				}
				_, callErr = svc.SearchWeb(ctx, domain.WebSearchRequest{Query: query}, "eino-agent")
			}
			if (callErr != nil) != (testCase.invalid || testCase.wantError != "") {
				t.Fatalf("call error: %v", callErr)
			}
			events, err := svc.ListAudit(ctx, "", 100)
			if err != nil {
				t.Fatal(err)
			}
			calls := make([]domain.AuditEvent, 0, 1)
			for _, event := range events {
				if event.Actor == "eino-agent" {
					calls = append(calls, event)
				}
			}
			if testCase.invalid {
				if len(calls) != 0 || hits.Load() != 0 {
					t.Fatalf("invalid input produced calls: events=%v hits=%d", calls, hits.Load())
				}
				return
			}
			if len(calls) != 1 || calls[0].Type != testCase.wantEvent || calls[0].RunID != "" || hits.Load() != 1 {
				t.Fatalf("unexpected audit: events=%v hits=%d", calls, hits.Load())
			}
			data := calls[0].Data
			digest := sha256.Sum256([]byte(digestInput))
			if data[digestField] != hex.EncodeToString(digest[:]) || data["http_status"] != float64(testCase.status) || data["duration_ms"] == nil {
				t.Fatalf("lost call metadata: %v", data)
			}
			if testCase.extract && data["url_count"] != float64(2) {
				t.Fatalf("URL count was not normalized: %v", data)
			}
			if testCase.wantError != "" && data["error_code"] != testCase.wantError {
				t.Fatalf("lost provider error: %v", data)
			}
			encoded, err := json.Marshal(calls)
			if err != nil {
				t.Fatal(err)
			}
			for _, raw := range []string{"private query", "private-api-key", "https://example.com/docs", "https://example.org/docs"} {
				if strings.Contains(string(encoded), raw) {
					t.Fatalf("audit exposed raw input or credentials: %s", encoded)
				}
			}
		})
	}
}

func TestWebSearchValidatesConfiguration(t *testing.T) {
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
		var input struct {
			URLs          []string
			ExtractDepth  string `json:"extract_depth"`
			Format        string
			IncludeImages bool `json:"include_images"`
		}
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
