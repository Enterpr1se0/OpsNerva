package websearch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestWebExtractPreservesCompleteContent(t *testing.T) {
	client := New(nil)
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

	settings := Config{BaseURL: provider.URL, APIKey: "test-key", TimeoutSeconds: 20, MaxResults: 5}
	result, _, err := client.Extract(context.Background(), settings, domain.WebExtractRequest{URLs: []string{"https://example.com/large"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || len(result.Results[0].RawContent) != 9<<10 {
		t.Fatalf("complete extracted content was not preserved: %#v", result)
	}
}

func TestTavilyAdvancedParametersAndUsageMetadata(t *testing.T) {
	client := New(nil)
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
	settings := Config{BaseURL: provider.URL, APIKey: "test-key", TimeoutSeconds: 20, MaxResults: 10}
	search, _, err := client.Search(context.Background(), settings, domain.WebSearchRequest{
		Query: "releases", Topic: "news", SearchDepth: "advanced", StartDate: "2026-07-01", EndDate: "2026-08-01", ChunksPerSource: 2,
	})
	if err != nil || search.RequestID != "req-search" || search.Credits != 2 {
		t.Fatalf("search metadata = %#v, err=%v", search, err)
	}
	extract, _, err := client.Extract(context.Background(), settings, domain.WebExtractRequest{
		URLs: []string{"https://go.dev/release"}, Query: "installation", ExtractDepth: "advanced", ChunksPerSource: 4,
	})
	if err != nil || extract.RequestID != "req-extract" || extract.Credits != 2 {
		t.Fatalf("extract metadata = %#v, err=%v", extract, err)
	}
}
