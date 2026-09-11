package websearch

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

const (
	maxWebExtractURLs              = 5
	defaultWebSearchRequestResults = 5
)

var webSearchDomain = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)

// NormalizeBaseURL validates a configured Tavily API endpoint.
func NormalizeBaseURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = domain.DefaultWebSearchBaseURL
	}
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("invalid Tavily base_url")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.Path = strings.TrimSuffix(strings.TrimSuffix(parsed.Path, "/search"), "/extract")
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func normalizeWebSearchRequest(input domain.WebSearchRequest, configuredMax int) (domain.WebSearchRequest, error) {
	input.Query = strings.TrimSpace(input.Query)
	if input.Query == "" || len(input.Query) > 2000 {
		return domain.WebSearchRequest{}, fmt.Errorf("query is required and must not exceed 2000 bytes")
	}
	if input.MaxResults == 0 {
		input.MaxResults = min(defaultWebSearchRequestResults, configuredMax)
	}
	if input.MaxResults < domain.MinWebSearchMaxResults || input.MaxResults > configuredMax {
		return domain.WebSearchRequest{}, fmt.Errorf("max_results must be between %d and %d", domain.MinWebSearchMaxResults, configuredMax)
	}
	input.Topic = strings.ToLower(strings.TrimSpace(input.Topic))
	if input.Topic == "" {
		input.Topic = "general"
	}
	if input.Topic != "general" && input.Topic != "news" && input.Topic != "finance" {
		return domain.WebSearchRequest{}, fmt.Errorf("topic must be general, news, or finance")
	}
	input.SearchDepth = strings.ToLower(strings.TrimSpace(input.SearchDepth))
	if input.SearchDepth == "" {
		input.SearchDepth = "basic"
	}
	if input.SearchDepth != "basic" && input.SearchDepth != "advanced" && input.SearchDepth != "fast" && input.SearchDepth != "ultra-fast" {
		return domain.WebSearchRequest{}, fmt.Errorf("search_depth must be basic, advanced, fast, or ultra-fast")
	}
	if input.ChunksPerSource < 0 || input.ChunksPerSource > 3 {
		return domain.WebSearchRequest{}, fmt.Errorf("chunks_per_source must be between 1 and 3 when set")
	}
	if input.ChunksPerSource > 0 && input.SearchDepth != "advanced" {
		return domain.WebSearchRequest{}, fmt.Errorf("chunks_per_source requires search_depth=advanced")
	}
	input.TimeRange = strings.ToLower(strings.TrimSpace(input.TimeRange))
	if input.TimeRange != "" && input.TimeRange != "day" && input.TimeRange != "week" && input.TimeRange != "month" && input.TimeRange != "year" {
		return domain.WebSearchRequest{}, fmt.Errorf("time_range must be day, week, month, or year")
	}
	input.StartDate = strings.TrimSpace(input.StartDate)
	input.EndDate = strings.TrimSpace(input.EndDate)
	if input.TimeRange != "" && (input.StartDate != "" || input.EndDate != "") {
		return domain.WebSearchRequest{}, fmt.Errorf("time_range cannot be combined with start_date or end_date")
	}
	var startDate, endDate time.Time
	var err error
	if input.StartDate != "" {
		startDate, err = time.Parse(time.DateOnly, input.StartDate)
		if err != nil {
			return domain.WebSearchRequest{}, fmt.Errorf("start_date must use YYYY-MM-DD")
		}
	}
	if input.EndDate != "" {
		endDate, err = time.Parse(time.DateOnly, input.EndDate)
		if err != nil {
			return domain.WebSearchRequest{}, fmt.Errorf("end_date must use YYYY-MM-DD")
		}
	}
	if !startDate.IsZero() && !endDate.IsZero() && startDate.After(endDate) {
		return domain.WebSearchRequest{}, fmt.Errorf("start_date must not be after end_date")
	}
	if input.IncludeDomains, err = normalizeWebSearchDomains(input.IncludeDomains); err != nil {
		return domain.WebSearchRequest{}, fmt.Errorf("include_domains: %w", err)
	}
	if input.ExcludeDomains, err = normalizeWebSearchDomains(input.ExcludeDomains); err != nil {
		return domain.WebSearchRequest{}, fmt.Errorf("exclude_domains: %w", err)
	}
	excluded := make(map[string]struct{}, len(input.ExcludeDomains))
	for _, value := range input.ExcludeDomains {
		excluded[value] = struct{}{}
	}
	for _, value := range input.IncludeDomains {
		if _, conflict := excluded[value]; conflict {
			return domain.WebSearchRequest{}, fmt.Errorf("domain %q cannot be both included and excluded", value)
		}
	}
	return input, nil
}

func normalizeWebExtractRequest(input domain.WebExtractRequest) (domain.WebExtractRequest, error) {
	if len(input.URLs) == 0 || len(input.URLs) > maxWebExtractURLs {
		return domain.WebExtractRequest{}, fmt.Errorf("urls must contain between 1 and %d public URLs", maxWebExtractURLs)
	}
	result := domain.WebExtractRequest{
		URLs: make([]string, 0, len(input.URLs)), Query: strings.TrimSpace(input.Query),
		ExtractDepth: strings.ToLower(strings.TrimSpace(input.ExtractDepth)), ChunksPerSource: input.ChunksPerSource,
	}
	if len(result.Query) > 2000 {
		return domain.WebExtractRequest{}, fmt.Errorf("query must not exceed 2000 bytes")
	}
	if result.ExtractDepth == "" {
		result.ExtractDepth = "basic"
	}
	if result.ExtractDepth != "basic" && result.ExtractDepth != "advanced" {
		return domain.WebExtractRequest{}, fmt.Errorf("extract_depth must be basic or advanced")
	}
	if result.ChunksPerSource < 0 || result.ChunksPerSource > 5 {
		return domain.WebExtractRequest{}, fmt.Errorf("chunks_per_source must be between 1 and 5 when set")
	}
	if result.ChunksPerSource > 0 && result.Query == "" {
		return domain.WebExtractRequest{}, fmt.Errorf("chunks_per_source requires query")
	}
	seen := make(map[string]struct{}, len(input.URLs))
	for _, value := range input.URLs {
		normalized, err := normalizePublicWebURL(value)
		if err != nil {
			return domain.WebExtractRequest{}, err
		}
		if _, exists := seen[normalized]; exists {
			continue
		}
		seen[normalized] = struct{}{}
		result.URLs = append(result.URLs, normalized)
	}
	return result, nil
}

func normalizePublicWebURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 2048 {
		return "", fmt.Errorf("URL is required and must not exceed 2048 bytes")
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return "", fmt.Errorf("invalid public HTTP/HTTPS URL %q", value)
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return "", fmt.Errorf("URL host %q is not public", parsed.Hostname())
	}
	if address := net.ParseIP(host); address != nil {
		if !address.IsGlobalUnicast() || address.IsPrivate() {
			return "", fmt.Errorf("URL host %q is not public", parsed.Hostname())
		}
	} else {
		if !strings.Contains(host, ".") || isNumericWebHost(host) {
			return "", fmt.Errorf("URL host %q is not a public domain", parsed.Hostname())
		}
	}
	port := parsed.Port()
	if strings.Contains(parsed.Host, ":") && port == "" && net.ParseIP(host) == nil {
		return "", fmt.Errorf("invalid URL port in %q", value)
	}
	if port != "" {
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return "", fmt.Errorf("invalid URL port in %q", value)
		}
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if port == "" || parsed.Scheme == "http" && port == "80" || parsed.Scheme == "https" && port == "443" {
		if strings.Contains(host, ":") {
			parsed.Host = "[" + host + "]"
		} else {
			parsed.Host = host
		}
	} else {
		parsed.Host = net.JoinHostPort(host, port)
	}
	parsed.Fragment = ""
	return parsed.String(), nil
}

func isNumericWebHost(host string) bool {
	for _, character := range host {
		if character != '.' && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func normalizeWebSearchDomains(values []string) ([]string, error) {
	if len(values) > 10 {
		return nil, fmt.Errorf("at most 10 domains are allowed")
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if !webSearchDomain.MatchString(value) || strings.Contains(value, "..") {
			return nil, fmt.Errorf("invalid domain %q", value)
		}
		if _, exists := seen[value]; !exists {
			seen[value] = struct{}{}
			result = append(result, value)
		}
	}
	return result, nil
}
