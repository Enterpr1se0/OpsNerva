package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestWebSearchBoundsModelPayloadAndFiltersProviderURLs(t *testing.T) {
	client := New(nil)
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
	settings := Config{BaseURL: provider.URL, APIKey: "test-key", TimeoutSeconds: 20, MaxResults: 20}
	result, _, err := client.Search(context.Background(), settings, domain.WebSearchRequest{Query: "large", MaxResults: 20})
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
	client := New(nil)
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
	settings := Config{BaseURL: provider.URL, APIKey: "test-key", TimeoutSeconds: 20, MaxResults: 10}
	urls := make([]string, len(providerResults))
	for index := range providerResults {
		urls[index] = providerResults[index].URL
	}
	result, _, err := client.Extract(context.Background(), settings, domain.WebExtractRequest{URLs: urls})
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
