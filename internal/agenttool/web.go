package agenttool

import (
	"context"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

type WebService interface {
	SearchWeb(context.Context, domain.WebSearchRequest, string) (domain.WebSearchResponse, error)
	ExtractWeb(context.Context, domain.WebExtractRequest, string) (domain.WebExtractResponse, error)
}

type WebResultPolicy interface {
	Search(domain.WebSearchResponse, error) (domain.WebSearchResponse, error)
	Extract(domain.WebExtractResponse, error) (domain.WebExtractResponse, error)
}

type WebDependencies struct {
	Service WebService
	Results WebResultPolicy
}

type Web struct {
	dependencies WebDependencies
}

func NewWeb(dependencies WebDependencies) *Web {
	return &Web{dependencies: dependencies}
}

func (web *Web) RunSearch(ctx context.Context, input WebSearchInput, actor string) (domain.WebSearchResponse, error) {
	result, err := web.dependencies.Service.SearchWeb(ctx, domain.WebSearchRequest{
		Query: input.Query, MaxResults: input.MaxResults, Topic: input.Topic, SearchDepth: input.SearchDepth,
		TimeRange: input.TimeRange, StartDate: input.StartDate, EndDate: input.EndDate,
		ChunksPerSource: input.ChunksPerSource, IncludeDomains: input.IncludeDomains,
		ExcludeDomains: input.ExcludeDomains,
	}, actor)
	if result.Query == "" {
		result.Query = input.Query
	}
	if result.Provider == "" {
		result.Provider = "tavily"
	}
	return web.dependencies.Results.Search(result, err)
}

func (web *Web) RunExtract(ctx context.Context, input WebExtractInput, actor string) (domain.WebExtractResponse, error) {
	result, err := web.dependencies.Service.ExtractWeb(ctx, domain.WebExtractRequest{
		URLs: input.URLs, Query: input.Query, ExtractDepth: input.ExtractDepth,
		ChunksPerSource: input.ChunksPerSource,
	}, actor)
	if result.Provider == "" {
		result.Provider = "tavily"
	}
	if result.Query == "" {
		result.Query = input.Query
	}
	return web.dependencies.Results.Extract(result, err)
}
