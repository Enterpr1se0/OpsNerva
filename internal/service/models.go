package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"eino-ops-agent/internal/config"
	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/proxyx"
)

var (
	ErrModelProviderUpstream = errors.New("model provider request failed")
	ErrModelProviderInUse    = errors.New("model provider is in use")
)

const maxModelCatalogBytes = 2 << 20

type modelCatalogEntry struct {
	ID               string          `json:"id"`
	Name             string          `json:"name"`
	Model            string          `json:"model"`
	ContextLength    json.RawMessage `json:"context_length"`
	ContextWindow    json.RawMessage `json:"context_window"`
	MaxModelLen      json.RawMessage `json:"max_model_len"`
	MaxContextLength json.RawMessage `json:"max_context_length"`
	MaxInputTokens   json.RawMessage `json:"max_input_tokens"`
	InputTokenLimit  json.RawMessage `json:"input_token_limit"`
}

type resolvedModelProvider struct {
	ID              string
	Kind            string
	BaseURL         string
	APIKey          string
	ProxyID         string
	ProxyURL        string
	ProxyUsername   string
	ProxyPassword   string
	UserAgent       string
	ReasoningEffort string
}

func parseContextWindow(values ...json.RawMessage) int {
	for _, raw := range values {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
			continue
		}
		var number float64
		if raw[0] == '"' {
			var text string
			if json.Unmarshal(raw, &text) != nil {
				continue
			}
			parsed, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
			if err != nil {
				continue
			}
			number = parsed
		} else if json.Unmarshal(raw, &number) != nil {
			continue
		}
		if number < domain.MinModelContextWindow || number > domain.MaxModelContextWindow {
			continue
		}
		window := int(number)
		if number == float64(window) {
			return window
		}
	}
	return 0
}

func modelCatalogContextWindow(entry modelCatalogEntry) int {
	return parseContextWindow(
		entry.ContextLength,
		entry.ContextWindow,
		entry.MaxModelLen,
		entry.MaxContextLength,
		entry.MaxInputTokens,
		entry.InputTokenLimit,
	)
}

func modelProviderHTTPClient(resolved resolvedModelProvider) (*http.Client, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	var err error
	if resolved.ProxyURL != "" {
		client, err = proxyx.NewHTTPClient(resolved.ProxyURL, resolved.ProxyUsername, resolved.ProxyPassword, 15*time.Second)
		if err != nil {
			return nil, err
		}
	}
	if resolved.UserAgent != "" {
		client = proxyx.WrapHeaders(client, map[string]string{"User-Agent": resolved.UserAgent})
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client, nil
}

func setModelProviderRequestHeaders(request *http.Request, resolved resolvedModelProvider) {
	request.Header.Set("Accept", "application/json")
	if resolved.Kind == "anthropic" {
		request.Header.Set("x-api-key", resolved.APIKey)
		request.Header.Set("anthropic-version", "2023-06-01")
	} else if resolved.APIKey != "" {
		request.Header.Set("Authorization", "Bearer "+resolved.APIKey)
	}
}

func normalizeReasoningEffort(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "", "low", "medium", "high", "xhigh":
		return value, nil
	default:
		return "", fmt.Errorf("reasoning_effort must be low, medium, high, xhigh, or empty")
	}
}

func validateProviderUserAgent(value string) (string, error) {
	value = strings.TrimSpace(value)
	// net/http rejects header values containing any control byte, so a stored
	// value with one would fail every subsequent request to the provider.
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("user agent cannot contain control characters")
		}
	}
	if len(value) > 256 {
		return "", fmt.Errorf("user agent is too long")
	}
	return value, nil
}

func providerKindRequiresAPIKey(kind string) bool {
	return kind == "openai" || kind == "deepseek" || kind == "anthropic"
}

func normalizeProviderBaseURL(value, kind string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		switch kind {
		case "openai":
			value = "https://api.openai.com/v1"
		case "deepseek":
			value = "https://api.deepseek.com"
		case "anthropic":
			value = "https://api.anthropic.com"
		case "ollama":
			value = "http://127.0.0.1:11434/v1"
		case "openai_compatible":
			return "", fmt.Errorf("base_url is required for an OpenAI-compatible provider")
		default:
			return "", fmt.Errorf("invalid provider kind %q", kind)
		}
	}
	if !strings.Contains(value, "://") {
		scheme := "https://"
		hostPort := value
		if index := strings.IndexByte(hostPort, '/'); index >= 0 {
			hostPort = hostPort[:index]
		}
		hostname := hostPort
		if host, _, err := net.SplitHostPort(hostPort); err == nil {
			hostname = host
		}
		hostname = strings.Trim(hostname, "[]")
		ip := net.ParseIP(hostname)
		if strings.EqualFold(hostname, "localhost") || strings.HasSuffix(strings.ToLower(hostname), ".localhost") ||
			strings.HasSuffix(strings.ToLower(hostname), ".local") || !strings.Contains(hostname, ".") ||
			(ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())) {
			scheme = "http://"
		}
		value = scheme + value
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("invalid base_url: enter a host or an absolute http/https URL, for example 127.0.0.1:11434/v1")
	}
	path := strings.TrimRight(parsed.Path, "/")
	for _, suffix := range []string{"/chat/completions", "/models"} {
		if strings.HasSuffix(path, suffix) {
			path = strings.TrimSuffix(path, suffix)
			break
		}
	}
	if kind == "anthropic" {
		// The Anthropic SDK appends /v1/... itself, so the stored base URL
		// must not end with the version segment. DiscoverModels and the
		// agent's max_tokens lookup rely on this by appending /v1/... too.
		path = strings.TrimSuffix(path, "/messages")
		path = strings.TrimSuffix(path, "/v1")
	}
	parsed.Path = strings.TrimRight(path, "/")
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func (s *Service) resolveModelProvider(
	ctx context.Context,
	providerID, kind string,
	inputBaseURL *string,
	inputAPIKey string,
	inputProxyID *string,
	inputUserAgent *string,
	inputReasoningEffort *string,
) (resolvedModelProvider, error) {
	result := resolvedModelProvider{
		ID: strings.TrimSpace(providerID), Kind: strings.TrimSpace(kind), APIKey: strings.TrimSpace(inputAPIKey),
	}
	providerID = result.ID
	if providerID != "" {
		cfg, provider, err := s.ModelProviderConfig(ctx, providerID)
		if err != nil {
			return resolvedModelProvider{}, err
		}
		if result.Kind == "" {
			result.Kind = provider.Kind
		}
		result.BaseURL = cfg.BaseURL
		result.ProxyID = provider.ProxyID
		result.UserAgent = cfg.UserAgent
		result.ReasoningEffort = cfg.ReasoningEffort
		if result.APIKey == "" {
			result.APIKey = cfg.APIKey
		}
	}
	if inputBaseURL != nil {
		result.BaseURL = strings.TrimSpace(*inputBaseURL)
	}
	if inputProxyID != nil {
		result.ProxyID = strings.TrimSpace(*inputProxyID)
	}
	if inputUserAgent != nil {
		result.UserAgent = *inputUserAgent
	}
	if inputReasoningEffort != nil {
		result.ReasoningEffort = *inputReasoningEffort
	}
	normalizedUserAgent, err := validateProviderUserAgent(result.UserAgent)
	if err != nil {
		return resolvedModelProvider{}, err
	}
	result.UserAgent = normalizedUserAgent
	result.ReasoningEffort, err = normalizeReasoningEffort(result.ReasoningEffort)
	if err != nil {
		return resolvedModelProvider{}, err
	}
	if result.Kind == "" {
		result.Kind = "openai_compatible"
	}
	if providerKindRequiresAPIKey(result.Kind) && result.APIKey == "" {
		return resolvedModelProvider{}, fmt.Errorf("api_key is required for %s", result.Kind)
	}
	normalizedBaseURL, err := normalizeProviderBaseURL(result.BaseURL, result.Kind)
	if err != nil {
		return resolvedModelProvider{}, err
	}
	result.BaseURL = normalizedBaseURL
	proxy, err := s.resolveProxy(ctx, result.ProxyID)
	if err != nil {
		return resolvedModelProvider{}, err
	}
	result.ProxyURL = proxy.URL
	result.ProxyUsername = proxy.Username
	result.ProxyPassword = proxy.Password
	return result, nil
}

func (s *Service) ModelTestConfig(ctx context.Context, input domain.ModelTestInput) (config.Model, error) {
	resolved, err := s.resolveModelProvider(
		ctx, input.ID, input.Kind, input.BaseURL, input.APIKey,
		input.ProxyID,
		input.UserAgent,
		input.ReasoningEffort,
	)
	if err != nil {
		return config.Model{}, err
	}
	model := strings.TrimSpace(input.Model)
	if model == "" {
		return config.Model{}, fmt.Errorf("model is required")
	}
	return config.Model{
		APIKey: resolved.APIKey, Kind: resolved.Kind, BaseURL: resolved.BaseURL, Name: model, ReasoningEffort: resolved.ReasoningEffort, UserAgent: resolved.UserAgent,
		ProxyURL: resolved.ProxyURL, ProxyUsername: resolved.ProxyUsername, ProxyPassword: resolved.ProxyPassword,
	}, nil
}

func (s *Service) DiscoverModels(ctx context.Context, input domain.ModelDiscoveryInput, actor string) (domain.ModelCatalog, error) {
	resolved, err := s.resolveModelProvider(
		ctx, input.ID, input.Kind, input.BaseURL, input.APIKey,
		input.ProxyID,
		input.UserAgent,
		nil,
	)
	if err != nil {
		return domain.ModelCatalog{}, err
	}
	catalog, err := s.discoverModels(ctx, resolved)
	if err != nil {
		return domain.ModelCatalog{}, err
	}
	s.audit(ctx, "", "model_catalog_discovered", actor, map[string]any{
		"provider_id": resolved.ID, "kind": resolved.Kind, "model_count": catalog.Count,
	})
	return catalog, nil
}

func (s *Service) discoverModels(ctx context.Context, resolved resolvedModelProvider) (domain.ModelCatalog, error) {
	// Anthropic's wire dialect: the catalog lives under /v1 (pages cap at 1000
	// entries, far above the current model count, so one request covers the
	// whole list) and authenticates via x-api-key instead of a Bearer token.
	endpoint := resolved.BaseURL + "/models"
	if resolved.Kind == "anthropic" {
		endpoint = resolved.BaseURL + "/v1/models?limit=1000"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return domain.ModelCatalog{}, fmt.Errorf("invalid model catalog endpoint: %w", err)
	}
	setModelProviderRequestHeaders(request, resolved)
	client, err := modelProviderHTTPClient(resolved)
	if err != nil {
		return domain.ModelCatalog{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return domain.ModelCatalog{}, fmt.Errorf("%w: %s", ErrModelProviderUpstream, s.scrubModelProviderText(err.Error(), resolved))
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxModelCatalogBytes+1))
	if err != nil {
		return domain.ModelCatalog{}, fmt.Errorf("%w: read response: %v", ErrModelProviderUpstream, err)
	}
	if len(body) > maxModelCatalogBytes {
		return domain.ModelCatalog{}, fmt.Errorf("%w: response exceeds %d bytes", ErrModelProviderUpstream, maxModelCatalogBytes)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		detail := strings.TrimSpace(string(body))
		detail = s.scrubModelProviderText(detail, resolved)
		if len(detail) > 500 {
			detail = detail[:500]
		}
		return domain.ModelCatalog{}, fmt.Errorf("%w: HTTP %d: %s", ErrModelProviderUpstream, response.StatusCode, detail)
	}
	var payload struct {
		Data   []modelCatalogEntry `json:"data"`
		Models []modelCatalogEntry `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return domain.ModelCatalog{}, fmt.Errorf("%w: invalid JSON response", ErrModelProviderUpstream)
	}
	entries := append(payload.Data, payload.Models...)
	unique := make(map[string]struct{}, len(entries))
	models := make([]string, 0, len(entries))
	contextWindows := make(map[string]int)
	for _, entry := range entries {
		name := strings.TrimSpace(entry.ID)
		if name == "" {
			name = strings.TrimSpace(entry.Name)
		}
		if name == "" {
			name = strings.TrimSpace(entry.Model)
		}
		if name == "" || len(name) > 256 {
			continue
		}
		window := modelCatalogContextWindow(entry)
		if _, exists := unique[name]; exists {
			if contextWindows[name] == 0 && window > 0 {
				contextWindows[name] = window
			}
			continue
		}
		unique[name] = struct{}{}
		models = append(models, name)
		if window > 0 {
			contextWindows[name] = window
		}
	}
	if len(models) == 0 {
		return domain.ModelCatalog{}, fmt.Errorf("%w: response contains no model IDs", ErrModelProviderUpstream)
	}
	sort.Strings(models)
	return domain.ModelCatalog{Models: models, ContextWindows: contextWindows, Count: len(models)}, nil
}

func (s *Service) DetectModelContextWindow(ctx context.Context, cfg config.Model) (int, error) {
	model := strings.TrimSpace(cfg.Name)
	if model == "" {
		return 0, fmt.Errorf("model is required")
	}
	kind := strings.TrimSpace(cfg.Kind)
	if kind == "" {
		if strings.TrimSpace(cfg.BaseURL) == "" {
			kind = "openai"
		} else {
			kind = "openai_compatible"
		}
	}
	baseURL, err := normalizeProviderBaseURL(cfg.BaseURL, kind)
	if err != nil {
		return 0, err
	}
	userAgent, err := validateProviderUserAgent(cfg.UserAgent)
	if err != nil {
		return 0, err
	}
	resolved := resolvedModelProvider{
		Kind: kind, BaseURL: baseURL, APIKey: cfg.APIKey,
		ProxyURL: cfg.ProxyURL, ProxyUsername: cfg.ProxyUsername, ProxyPassword: cfg.ProxyPassword,
		UserAgent: userAgent,
	}
	if resolved.Kind == "ollama" {
		return s.detectOllamaContextWindow(ctx, resolved, model)
	}
	catalog, err := s.discoverModels(ctx, resolved)
	if err != nil {
		return 0, err
	}
	return catalog.ContextWindows[model], nil
}

func (s *Service) detectOllamaContextWindow(ctx context.Context, resolved resolvedModelProvider, model string) (int, error) {
	parsed, err := url.Parse(resolved.BaseURL)
	if err != nil {
		return 0, err
	}
	parsed.Path = strings.TrimSuffix(strings.TrimRight(parsed.Path, "/"), "/v1") + "/api/show"
	parsed.RawPath = ""
	payload, err := json.Marshal(map[string]string{"model": model})
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, parsed.String(), bytes.NewReader(payload))
	if err != nil {
		return 0, fmt.Errorf("invalid Ollama model endpoint: %w", err)
	}
	setModelProviderRequestHeaders(request, resolved)
	request.Header.Set("Content-Type", "application/json")
	client, err := modelProviderHTTPClient(resolved)
	if err != nil {
		return 0, err
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, fmt.Errorf("%w: %s", ErrModelProviderUpstream, s.scrubModelProviderText(err.Error(), resolved))
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxModelCatalogBytes+1))
	if err != nil {
		return 0, fmt.Errorf("%w: read Ollama response: %v", ErrModelProviderUpstream, err)
	}
	if len(body) > maxModelCatalogBytes {
		return 0, fmt.Errorf("%w: Ollama response exceeds %d bytes", ErrModelProviderUpstream, maxModelCatalogBytes)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		detail := s.scrubModelProviderText(strings.TrimSpace(string(body)), resolved)
		if len(detail) > 500 {
			detail = detail[:500]
		}
		return 0, fmt.Errorf("%w: Ollama HTTP %d: %s", ErrModelProviderUpstream, response.StatusCode, detail)
	}
	var detail struct {
		Parameters string                     `json:"parameters"`
		ModelInfo  map[string]json.RawMessage `json:"model_info"`
	}
	if err := json.Unmarshal(body, &detail); err != nil {
		return 0, fmt.Errorf("%w: invalid Ollama JSON response", ErrModelProviderUpstream)
	}
	for _, line := range strings.Split(detail.Parameters, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "num_ctx" {
			continue
		}
		if window := parseContextWindow(json.RawMessage(fields[1])); window > 0 {
			return window, nil
		}
	}
	window := 0
	for key, value := range detail.ModelInfo {
		if !strings.HasSuffix(strings.ToLower(key), ".context_length") && !strings.HasSuffix(strings.ToLower(key), ".context_window") {
			continue
		}
		if candidate := parseContextWindow(value); candidate > window {
			window = candidate
		}
	}
	if window > 0 {
		return window, nil
	}
	return 0, nil
}

func (s *Service) scrubModelProviderText(value string, provider resolvedModelProvider) string {
	for _, secret := range []string{provider.APIKey, provider.ProxyPassword} {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	if s.redactor != nil {
		value = s.redactor.Redact(value)
	}
	return value
}
