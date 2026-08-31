package toolresult

import (
	"context"
	"errors"
	"strings"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/service"
)

type WebPolicy struct{}

func (WebPolicy) Search(result domain.WebSearchResponse, err error) (domain.WebSearchResponse, error) {
	return NormalizeWebSearch(result, err)
}

func (WebPolicy) Extract(result domain.WebExtractResponse, err error) (domain.WebExtractResponse, error) {
	return NormalizeWebExtract(result, err)
}

func NormalizeWebSearch(result domain.WebSearchResponse, err error) (domain.WebSearchResponse, error) {
	result.ToolVersion = "1.1"
	result.ContentIsUntrusted = true
	if err == nil {
		result.OK = true
		result.Code = "completed"
		return result, nil
	}
	if errors.Is(err, context.Canceled) {
		return result, err
	}
	result.OK = false
	result.Message = err.Error()
	switch {
	case errors.Is(err, service.ErrWebSearchDisabled):
		result.Code = "configuration_required"
		result.NextAction = "tell the operator that Tavily Web must be enabled and configured in Settings; do not retry"
	case errors.Is(err, context.DeadlineExceeded):
		result.Code = "timeout"
		result.Retryable = true
		result.NextAction = "retry once with a narrower query or fewer results"
	case errors.Is(err, service.ErrWebSearchUpstream):
		result.Code, result.Retryable, result.NextAction = classifyWebProviderError(err)
	case strings.Contains(strings.ToLower(err.Error()), "timeout"):
		result.Code = "timeout"
		result.Retryable = true
		result.NextAction = "retry once with a narrower query or fewer results"
	default:
		result.Code, result.Retryable, result.NextAction = ClassifyExecError(err)
	}
	return result, nil
}

func NormalizeWebExtract(result domain.WebExtractResponse, err error) (domain.WebExtractResponse, error) {
	result.ToolVersion = "1.1"
	result.ContentIsUntrusted = true
	if err == nil {
		result.OK = true
		result.Code = "completed"
		if len(result.FailedResults) > 0 {
			result.Code = "partial"
			result.Message = "some URLs could not be extracted"
			result.NextAction = "use the successful pages and retry only failed URLs when they are still necessary"
		}
		return result, nil
	}
	if errors.Is(err, context.Canceled) {
		return result, err
	}
	result.OK = false
	result.Message = err.Error()
	switch {
	case errors.Is(err, service.ErrWebSearchDisabled):
		result.Code = "configuration_required"
		result.NextAction = "tell the operator that Tavily Web must be enabled and configured in Settings; do not retry"
	case errors.Is(err, context.DeadlineExceeded):
		result.Code = "timeout"
		result.Retryable = true
		result.NextAction = "retry once with fewer URLs"
	case errors.Is(err, service.ErrWebSearchUpstream):
		result.Code, result.Retryable, result.NextAction = classifyWebProviderError(err)
	case strings.Contains(strings.ToLower(err.Error()), "timeout"):
		result.Code = "timeout"
		result.Retryable = true
		result.NextAction = "retry once with fewer URLs"
	default:
		result.Code, result.Retryable, result.NextAction = ClassifyExecError(err)
	}
	return result, nil
}

func classifyWebProviderError(err error) (string, bool, string) {
	var providerError *service.WebSearchProviderError
	if !errors.As(err, &providerError) {
		return "provider_failed", true, "retry once only when the provider failure appears transient"
	}
	switch providerError.Code {
	case service.WebSearchErrorInvalidRequest:
		return providerError.Code, false, "correct the search or extraction parameters; do not repeat unchanged input"
	case service.WebSearchErrorAuthenticationFailed:
		return providerError.Code, false, "tell the operator to verify the Tavily API key in Settings; do not retry"
	case service.WebSearchErrorQuotaExhausted:
		return providerError.Code, false, "tell the operator that Tavily quota is exhausted; do not retry"
	case service.WebSearchErrorRateLimited:
		if providerError.Retryable {
			return providerError.Code, true, "retry once after a short delay with fewer results or URLs"
		}
		return providerError.Code, false, "do not retry in this turn; continue with sources already available"
	case service.WebSearchErrorTimeout:
		return providerError.Code, providerError.Retryable, "retry once with fewer results or URLs only when the operation is still necessary"
	case service.WebSearchErrorProviderUnavailable:
		return providerError.Code, providerError.Retryable, "retry once only when the operation is still necessary; otherwise report the provider outage"
	default:
		return "provider_failed", providerError.Retryable, "retry once only when the provider failure appears transient"
	}
}
