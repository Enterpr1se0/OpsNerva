package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/proxyx"
)

const (
	maxModelMetadataBytes   = 4 << 20
	modelMetadataCacheTTL   = 24 * time.Hour
	modelMetadataRetryDelay = time.Minute
	modelsDevMetadataURL    = "https://models.dev/models.json"
)

var errModelMetadataUnavailable = errors.New("model metadata unavailable")

type modelsDevModel struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Family           string `json:"family"`
	Attachment       bool   `json:"attachment"`
	Reasoning        bool   `json:"reasoning"`
	ToolCall         bool   `json:"tool_call"`
	StructuredOutput bool   `json:"structured_output"`
	Temperature      bool   `json:"temperature"`
	Knowledge        string `json:"knowledge"`
	ReleaseDate      string `json:"release_date"`
	LastUpdated      string `json:"last_updated"`
	Status           string `json:"status"`
	Limit            struct {
		Context int `json:"context"`
		Input   int `json:"input"`
		Output  int `json:"output"`
	} `json:"limit"`
	Modalities struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"modalities"`
}

type modelMetadataCache struct {
	mu        sync.Mutex
	url       string
	fetchedAt time.Time
	retryAt   time.Time
	etag      string
	entries   map[string]domain.ModelMetadata
	aliases   map[string]string
	lastErr   error
}

func newModelMetadataCache(endpoint string) *modelMetadataCache {
	return &modelMetadataCache{url: endpoint}
}

func (c *modelMetadataCache) failed(err error) (map[string]domain.ModelMetadata, map[string]string, error) {
	c.retryAt = time.Now().Add(modelMetadataRetryDelay)
	c.lastErr = err
	if len(c.entries) > 0 {
		return c.entries, c.aliases, nil
	}
	return nil, nil, err
}

func boundedMetadataText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) > limit {
		return ""
	}
	return value
}

func boundedMetadataTokens(value int) int {
	if value < 0 || value > domain.MaxModelContextWindow {
		return 0
	}
	return value
}

func boundedMetadataModalities(values []string) []string {
	if len(values) > 16 {
		return nil
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = boundedMetadataText(value, 32)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func normalizeModelsDevEntry(key string, raw modelsDevModel) (domain.ModelMetadata, bool) {
	key = boundedMetadataText(key, 256)
	id := boundedMetadataText(raw.ID, 256)
	if id == "" {
		id = key
	}
	if key == "" || id == "" {
		return domain.ModelMetadata{}, false
	}
	contextWindow := boundedMetadataTokens(raw.Limit.Context)
	if contextWindow > 0 && contextWindow < domain.MinModelContextWindow {
		contextWindow = 0
	}
	return domain.ModelMetadata{
		ID: id, Name: boundedMetadataText(raw.Name, 256), Family: boundedMetadataText(raw.Family, 128),
		ContextWindow: contextWindow, InputTokenLimit: boundedMetadataTokens(raw.Limit.Input), OutputTokenLimit: boundedMetadataTokens(raw.Limit.Output),
		Attachment: raw.Attachment, Reasoning: raw.Reasoning, ToolCall: raw.ToolCall,
		StructuredOutput: raw.StructuredOutput, Temperature: raw.Temperature,
		Knowledge: boundedMetadataText(raw.Knowledge, 32), ReleaseDate: boundedMetadataText(raw.ReleaseDate, 32),
		LastUpdated: boundedMetadataText(raw.LastUpdated, 32), Status: boundedMetadataText(raw.Status, 32),
		InputModalities: boundedMetadataModalities(raw.Modalities.Input), OutputModalities: boundedMetadataModalities(raw.Modalities.Output),
	}, true
}

func modelMetadataAlias(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	id = strings.TrimPrefix(id, "models/")
	return id
}

func buildModelMetadataIndex(raw map[string]modelsDevModel) (map[string]domain.ModelMetadata, map[string]string) {
	entries := make(map[string]domain.ModelMetadata, len(raw))
	aliases := make(map[string]string, len(raw)*2)
	ambiguous := make(map[string]struct{})
	addAlias := func(alias, key string) {
		alias = modelMetadataAlias(alias)
		if alias == "" {
			return
		}
		if existing, exists := aliases[alias]; exists && existing != key {
			delete(aliases, alias)
			ambiguous[alias] = struct{}{}
			return
		}
		if _, exists := ambiguous[alias]; !exists {
			aliases[alias] = key
		}
	}
	for rawKey, rawEntry := range raw {
		metadata, ok := normalizeModelsDevEntry(rawKey, rawEntry)
		if !ok {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(rawKey))
		entries[key] = metadata
		addAlias(rawKey, key)
		addAlias(metadata.ID, key)
		if slash := strings.IndexByte(rawKey, '/'); slash >= 0 && slash+1 < len(rawKey) {
			addAlias(rawKey[slash+1:], key)
		}
	}
	return entries, aliases
}

func metadataProviderID(kind string) string {
	switch kind {
	case "openai", "anthropic", "deepseek":
		return kind
	default:
		return ""
	}
}

func lookupModelMetadata(entries map[string]domain.ModelMetadata, aliases map[string]string, kind, model string) (domain.ModelMetadata, bool) {
	model = modelMetadataAlias(model)
	if model == "" {
		return domain.ModelMetadata{}, false
	}
	candidates := []string{model}
	if providerID := metadataProviderID(kind); providerID != "" && !strings.Contains(model, "/") {
		candidates = append([]string{providerID + "/" + model}, candidates...)
	}
	for _, candidate := range candidates {
		key := candidate
		if alias, exists := aliases[candidate]; exists {
			key = alias
		}
		if metadata, exists := entries[key]; exists {
			return metadata, true
		}
	}
	return domain.ModelMetadata{}, false
}

func (s *Service) cachedModelMetadata(kind, model string) (domain.ModelMetadata, bool) {
	cache := s.modelMetadata
	if cache == nil {
		return domain.ModelMetadata{}, false
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return lookupModelMetadata(cache.entries, cache.aliases, kind, model)
}

func modelMetadataHTTPClient(resolved resolvedModelProvider) (*http.Client, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	if resolved.ProxyURL != "" {
		var err error
		client, err = proxyx.NewHTTPClient(resolved.ProxyURL, resolved.ProxyUsername, resolved.ProxyPassword, 10*time.Second)
		if err != nil {
			return nil, err
		}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client, nil
}

func (s *Service) loadModelMetadata(ctx context.Context, resolved resolvedModelProvider) (map[string]domain.ModelMetadata, map[string]string, error) {
	cache := s.modelMetadata
	if cache == nil {
		return nil, nil, errModelMetadataUnavailable
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.url == "" {
		return nil, nil, errModelMetadataUnavailable
	}
	if time.Now().Before(cache.retryAt) {
		if len(cache.entries) > 0 {
			return cache.entries, cache.aliases, nil
		}
		return nil, nil, cache.lastErr
	}
	if len(cache.entries) > 0 && time.Since(cache.fetchedAt) < modelMetadataCacheTTL {
		return cache.entries, cache.aliases, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, cache.url, nil)
	if err != nil {
		return cache.failed(err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "OpsNerva/1")
	if cache.etag != "" {
		request.Header.Set("If-None-Match", cache.etag)
	}
	client, err := modelMetadataHTTPClient(resolved)
	if err != nil {
		return cache.failed(err)
	}
	response, err := client.Do(request)
	if err != nil {
		return cache.failed(fmt.Errorf("%w: %v", errModelMetadataUnavailable, err))
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified && len(cache.entries) > 0 {
		cache.fetchedAt = time.Now()
		cache.retryAt = time.Time{}
		cache.lastErr = nil
		return cache.entries, cache.aliases, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return cache.failed(fmt.Errorf("%w: HTTP %d", errModelMetadataUnavailable, response.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxModelMetadataBytes+1))
	if err != nil {
		return cache.failed(fmt.Errorf("%w: read response: %v", errModelMetadataUnavailable, err))
	}
	if len(body) > maxModelMetadataBytes {
		return cache.failed(fmt.Errorf("%w: response exceeds %d bytes", errModelMetadataUnavailable, maxModelMetadataBytes))
	}
	var raw map[string]modelsDevModel
	if err := json.Unmarshal(body, &raw); err != nil {
		return cache.failed(fmt.Errorf("%w: invalid JSON response", errModelMetadataUnavailable))
	}
	entries, aliases := buildModelMetadataIndex(raw)
	if len(entries) == 0 {
		return cache.failed(fmt.Errorf("%w: response contains no models", errModelMetadataUnavailable))
	}
	cache.entries = entries
	cache.aliases = aliases
	cache.fetchedAt = time.Now()
	cache.retryAt = time.Time{}
	cache.etag = response.Header.Get("ETag")
	cache.lastErr = nil
	return cache.entries, cache.aliases, nil
}
