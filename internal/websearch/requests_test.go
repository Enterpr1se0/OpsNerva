package websearch

import (
	"fmt"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestWebSearchValidatesInput(t *testing.T) {
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
	if normalized, err := NormalizeBaseURL("https://api.tavily.com/extract"); err != nil || normalized != "https://api.tavily.com" {
		t.Fatalf("extract endpoint was not normalized to its API base: url=%q err=%v", normalized, err)
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
