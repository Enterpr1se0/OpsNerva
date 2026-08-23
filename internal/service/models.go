package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/proxyx"
)

var (
	ErrModelProviderUpstream = errors.New("model provider request failed")
	ErrModelProviderInUse    = errors.New("model provider is in use")
)

const maxModelCatalogBytes = 2 << 20

type modelCatalogEntry struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Model string `json:"model"`
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
		if _, exists := unique[name]; exists {
			continue
		}
		unique[name] = struct{}{}
		models = append(models, name)
	}
	if len(models) == 0 {
		return domain.ModelCatalog{}, fmt.Errorf("%w: response contains no model IDs", ErrModelProviderUpstream)
	}
	sort.Strings(models)
	contextWindows := make(map[string]int)
	metadata := make(map[string]domain.ModelMetadata)
	if entries, aliases, metadataErr := s.loadModelMetadata(ctx, resolved); metadataErr == nil {
		for _, model := range models {
			entry, exists := lookupModelMetadata(entries, aliases, resolved.Kind, model)
			if !exists {
				continue
			}
			metadata[model] = entry
			if entry.ContextWindow > 0 {
				contextWindows[model] = entry.ContextWindow
			}
		}
	}
	if len(metadata) == 0 {
		metadata = nil
	}
	return domain.ModelCatalog{Models: models, ContextWindows: contextWindows, Metadata: metadata, Count: len(models)}, nil
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
	resolved := resolvedModelProvider{
		Kind:     kind,
		ProxyURL: cfg.ProxyURL, ProxyUsername: cfg.ProxyUsername, ProxyPassword: cfg.ProxyPassword,
	}
	entries, aliases, err := s.loadModelMetadata(ctx, resolved)
	if err != nil {
		return 0, err
	}
	metadata, exists := lookupModelMetadata(entries, aliases, kind, model)
	if !exists {
		return 0, nil
	}
	return metadata.ContextWindow, nil
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
