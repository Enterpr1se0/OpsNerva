package agent

import (
	"eino-ops-agent/internal/domain"

	"github.com/cloudwego/eino/schema"
)

func normalizedTokenUsage(usage *schema.TokenUsage) domain.ChatTokenUsage {
	if usage == nil {
		return domain.ChatTokenUsage{}
	}
	result := domain.ChatTokenUsage{
		InputTokens:     max(0, usage.PromptTokens),
		OutputTokens:    max(0, usage.CompletionTokens),
		CachedTokens:    max(0, usage.PromptTokenDetails.CachedTokens),
		ReasoningTokens: max(0, usage.CompletionTokensDetails.ReasoningTokens),
	}
	result.TotalTokens = max(0, usage.TotalTokens)
	if result.TotalTokens == 0 {
		result.TotalTokens = result.InputTokens + result.OutputTokens
	}
	return result
}

func mergeTokenUsageSnapshot(current, next domain.ChatTokenUsage) domain.ChatTokenUsage {
	current.InputTokens = max(current.InputTokens, next.InputTokens)
	current.OutputTokens = max(current.OutputTokens, next.OutputTokens)
	current.TotalTokens = max(current.TotalTokens, next.TotalTokens)
	current.CachedTokens = max(current.CachedTokens, next.CachedTokens)
	current.ReasoningTokens = max(current.ReasoningTokens, next.ReasoningTokens)
	return current
}
