package websearch

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

const (
	maxWebSearchResultContentBytes  = 2 << 10
	maxWebSearchModelResponseBytes  = 32 << 10
	maxWebExtractResultContentBytes = 16 << 10
	maxWebExtractModelResponseBytes = 48 << 10
	maxWebResultTitleBytes          = 512
	maxWebResultDateBytes           = 128
	maxWebFailedResultErrorBytes    = 512
	maxWebRequestIDBytes            = 256
)

func truncateUTF8Bytes(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	end := maxBytes
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end]
}

func fairTruncateWebContents(values []string, maxBytes int) []string {
	result := make([]string, len(values))
	if maxBytes <= 0 || len(values) == 0 {
		return result
	}
	pending := make([]int, len(values))
	for index := range values {
		pending[index] = index
	}
	remaining := maxBytes
	for len(pending) > 0 && remaining > 0 {
		share := remaining / len(pending)
		if share == 0 {
			break
		}
		next := pending[:0]
		completed := false
		for _, index := range pending {
			if len(values[index]) <= share {
				result[index] = values[index]
				remaining -= len(values[index])
				completed = true
				continue
			}
			next = append(next, index)
		}
		if completed {
			pending = next
			continue
		}
		for _, index := range pending {
			result[index] = truncateUTF8Bytes(values[index], share)
			remaining -= len(result[index])
		}
		break
	}
	return result
}

func fitWebSearchResponseBudget(response *domain.WebSearchResponse, maxBytes int) {
	if response == nil {
		return
	}
	originalTotal := 0
	desired := make([]string, len(response.Results))
	for index := range response.Results {
		originalBytes := response.Results[index].OriginalBytes
		if originalBytes == 0 && response.Results[index].Content != "" {
			originalBytes = len(response.Results[index].Content)
		}
		response.Results[index].OriginalBytes = originalBytes
		originalTotal += originalBytes
		desired[index] = truncateUTF8Bytes(response.Results[index].Content, maxWebSearchResultContentBytes)
		response.Results[index].Content = ""
		response.Results[index].ReturnedBytes = 0
		response.Results[index].Truncated = originalBytes > 0
	}
	response.OriginalBytes = originalTotal
	baseBytes := marshaledWebBytes(response)
	available := max(0, maxBytes-baseBytes-512)
	contents := fairTruncateWebContents(desired, available)
	for index := range response.Results {
		response.Results[index].Content = contents[index]
		response.Results[index].ReturnedBytes = len(contents[index])
		response.Results[index].Truncated = response.Results[index].ReturnedBytes < response.Results[index].OriginalBytes
	}
	for attempt := 0; attempt < 4; attempt++ {
		refreshWebSearchResponseStats(response)
		if marshaledWebBytes(response) <= maxBytes {
			return
		}
		shrinkWebSearchResponse(response, maxBytes)
	}
	refreshWebSearchResponseStats(response)
}

func refreshWebSearchResponseStats(response *domain.WebSearchResponse) {
	response.ReturnedBytes = 0
	response.Truncated = response.OmittedResults > 0
	for index := range response.Results {
		response.Results[index].ReturnedBytes = len(response.Results[index].Content)
		response.Results[index].Truncated = response.Results[index].ReturnedBytes < response.Results[index].OriginalBytes
		response.ReturnedBytes += response.Results[index].ReturnedBytes
		response.Truncated = response.Truncated || response.Results[index].Truncated
	}
}

func shrinkWebSearchResponse(response *domain.WebSearchResponse, maxBytes int) {
	for marshaledWebBytes(response) > maxBytes {
		largest := -1
		for index := range response.Results {
			if largest < 0 || len(response.Results[index].Content) > len(response.Results[largest].Content) {
				largest = index
			}
		}
		if largest >= 0 && response.Results[largest].Content != "" {
			response.Results[largest].Content = truncateUTF8Bytes(response.Results[largest].Content, len(response.Results[largest].Content)/2)
			response.Results[largest].ReturnedBytes = len(response.Results[largest].Content)
			response.Results[largest].Truncated = true
			response.Truncated = true
			continue
		}
		if len(response.Results) <= 1 {
			break
		}
		response.Results = response.Results[:len(response.Results)-1]
		response.OmittedResults++
		response.Truncated = true
	}
}

func fitWebExtractResponseBudget(response *domain.WebExtractResponse, maxBytes int) {
	if response == nil {
		return
	}
	originalTotal := 0
	desired := make([]string, len(response.Results))
	for index := range response.Results {
		originalBytes := response.Results[index].OriginalBytes
		if originalBytes == 0 && response.Results[index].RawContent != "" {
			originalBytes = len(response.Results[index].RawContent)
		}
		response.Results[index].OriginalBytes = originalBytes
		originalTotal += originalBytes
		desired[index] = truncateUTF8Bytes(response.Results[index].RawContent, maxWebExtractResultContentBytes)
		response.Results[index].RawContent = ""
		response.Results[index].ReturnedBytes = 0
		response.Results[index].Truncated = originalBytes > 0
	}
	response.OriginalBytes = originalTotal
	baseBytes := marshaledWebBytes(response)
	available := max(0, maxBytes-baseBytes-512)
	contents := fairTruncateWebContents(desired, available)
	for index := range response.Results {
		response.Results[index].RawContent = contents[index]
		response.Results[index].ReturnedBytes = len(contents[index])
		response.Results[index].Truncated = response.Results[index].ReturnedBytes < response.Results[index].OriginalBytes
	}
	for attempt := 0; attempt < 4; attempt++ {
		refreshWebExtractResponseStats(response)
		if marshaledWebBytes(response) <= maxBytes {
			return
		}
		shrinkWebExtractResponse(response, maxBytes)
	}
	refreshWebExtractResponseStats(response)
}

func refreshWebExtractResponseStats(response *domain.WebExtractResponse) {
	response.ReturnedBytes = 0
	response.Truncated = response.OmittedResults > 0
	for index := range response.Results {
		response.Results[index].ReturnedBytes = len(response.Results[index].RawContent)
		response.Results[index].Truncated = response.Results[index].ReturnedBytes < response.Results[index].OriginalBytes
		response.ReturnedBytes += response.Results[index].ReturnedBytes
		response.Truncated = response.Truncated || response.Results[index].Truncated
	}
}

func shrinkWebExtractResponse(response *domain.WebExtractResponse, maxBytes int) {
	for marshaledWebBytes(response) > maxBytes {
		largest := -1
		for index := range response.Results {
			if largest < 0 || len(response.Results[index].RawContent) > len(response.Results[largest].RawContent) {
				largest = index
			}
		}
		if largest >= 0 && response.Results[largest].RawContent != "" {
			response.Results[largest].RawContent = truncateUTF8Bytes(response.Results[largest].RawContent, len(response.Results[largest].RawContent)/2)
			response.Results[largest].ReturnedBytes = len(response.Results[largest].RawContent)
			response.Results[largest].Truncated = true
			response.Truncated = true
			continue
		}
		if len(response.Results) <= 1 {
			break
		}
		response.Results = response.Results[:len(response.Results)-1]
		response.OmittedResults++
		response.Truncated = true
	}
}

func marshaledWebBytes(value any) int {
	encoded, err := json.Marshal(value)
	if err != nil {
		return 0
	}
	return len(encoded)
}

func containsWebSearchSecret(value string, settings Config) bool {
	return settings.APIKey != "" && strings.Contains(value, settings.APIKey) ||
		settings.ProxyPassword != "" && strings.Contains(value, settings.ProxyPassword)
}

func (c *Client) scrubWebSearchText(value string, settings Config) string {
	for _, secret := range []string{settings.APIKey, settings.ProxyPassword} {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	if c.redactor != nil {
		value = c.redactor.Redact(value)
	}
	return value
}
