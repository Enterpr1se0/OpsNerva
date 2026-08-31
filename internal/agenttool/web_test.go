package agenttool

import (
	"context"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

type webServiceRecorder struct {
	searchRequest  domain.WebSearchRequest
	extractRequest domain.WebExtractRequest
}

func (service *webServiceRecorder) SearchWeb(_ context.Context, request domain.WebSearchRequest, _ string) (domain.WebSearchResponse, error) {
	service.searchRequest = request
	return domain.WebSearchResponse{}, nil
}

func (service *webServiceRecorder) ExtractWeb(_ context.Context, request domain.WebExtractRequest, _ string) (domain.WebExtractResponse, error) {
	service.extractRequest = request
	return domain.WebExtractResponse{}, nil
}

type webResultRecorder struct {
	search  domain.WebSearchResponse
	extract domain.WebExtractResponse
}

func (policy *webResultRecorder) Search(result domain.WebSearchResponse, err error) (domain.WebSearchResponse, error) {
	policy.search = result
	return result, err
}

func (policy *webResultRecorder) Extract(result domain.WebExtractResponse, err error) (domain.WebExtractResponse, error) {
	policy.extract = result
	return result, err
}

func TestWebToolsMapInputsAndCompleteIdentity(t *testing.T) {
	service := &webServiceRecorder{}
	results := &webResultRecorder{}
	web := NewWeb(WebDependencies{Service: service, Results: results})

	search, err := web.RunSearch(context.Background(), WebSearchInput{
		Query: "current release", MaxResults: 3, SearchDepth: "advanced",
		IncludeDomains: []string{"example.com"},
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if service.searchRequest.Query != "current release" || service.searchRequest.MaxResults != 3 ||
		service.searchRequest.SearchDepth != "advanced" || len(service.searchRequest.IncludeDomains) != 1 {
		t.Fatalf("Web search request = %#v", service.searchRequest)
	}
	if search.Query != "current release" || search.Provider != "tavily" ||
		results.search.Query != search.Query || results.search.Provider != search.Provider {
		t.Fatalf("Web search identity = %#v", search)
	}

	extract, err := web.RunExtract(context.Background(), WebExtractInput{
		URLs: []string{"https://example.com"}, Query: "release notes", ExtractDepth: "advanced", ChunksPerSource: 2,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(service.extractRequest.URLs) != 1 || service.extractRequest.Query != "release notes" ||
		service.extractRequest.ExtractDepth != "advanced" || service.extractRequest.ChunksPerSource != 2 {
		t.Fatalf("Web extract request = %#v", service.extractRequest)
	}
	if extract.Query != "release notes" || extract.Provider != "tavily" ||
		results.extract.Query != extract.Query || results.extract.Provider != extract.Provider {
		t.Fatalf("Web extract identity = %#v", extract)
	}
}
