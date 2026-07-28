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
	ID    string `json:"id"`
	Name  string `json:"name"`
	Model string `json:"model"`
}

type resolvedModelProvider struct {
	ID            string
	Kind          string
	BaseURL       string
	APIKey        string
	ProxyURL      string
	ProxyUsername string
	ProxyPassword string
	UserAgent     string
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
	inputProxyURL, inputProxyUsername *string,
	inputProxyPassword string,
	clearProxyPassword bool,
	inputUserAgent *string,
) (resolvedModelProvider, error) {
	result := resolvedModelProvider{
		ID: strings.TrimSpace(providerID), Kind: strings.TrimSpace(kind), APIKey: strings.TrimSpace(inputAPIKey),
	}
	storedProxyURL := ""
	storedProxyUsername := ""
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
		result.ProxyURL = cfg.ProxyURL
		result.ProxyUsername = cfg.ProxyUsername
		result.ProxyPassword = cfg.ProxyPassword
		result.UserAgent = cfg.UserAgent
		storedProxyURL = cfg.ProxyURL
		storedProxyUsername = cfg.ProxyUsername
		if result.APIKey == "" {
			result.APIKey = cfg.APIKey
		}
	}
	if inputBaseURL != nil {
		result.BaseURL = strings.TrimSpace(*inputBaseURL)
	}
	if inputProxyURL != nil {
		result.ProxyURL = strings.TrimSpace(*inputProxyURL)
	}
	if inputProxyUsername != nil {
		result.ProxyUsername = strings.TrimSpace(*inputProxyUsername)
	}
	if inputUserAgent != nil {
		result.UserAgent = *inputUserAgent
	}
	normalizedUserAgent, err := validateProviderUserAgent(result.UserAgent)
	if err != nil {
		return resolvedModelProvider{}, err
	}
	result.UserAgent = normalizedUserAgent
	normalizedProxyURL, err := proxyx.NormalizeURL(result.ProxyURL)
	if err != nil {
		return resolvedModelProvider{}, err
	}
	result.ProxyURL = normalizedProxyURL
	if len(result.ProxyURL) > 2048 {
		return resolvedModelProvider{}, fmt.Errorf("proxy URL is too long")
	}
	if clearProxyPassword {
		result.ProxyPassword = ""
	} else if inputProxyPassword != "" {
		result.ProxyPassword = inputProxyPassword
	} else if result.ProxyURL != storedProxyURL || result.ProxyUsername != storedProxyUsername {
		result.ProxyPassword = ""
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
	if containsCredentialControl(result.ProxyUsername) || containsCredentialControl(result.ProxyPassword) {
		return resolvedModelProvider{}, fmt.Errorf("proxy credentials cannot contain NUL, carriage return, or newline characters")
	}
	if len(result.ProxyUsername) > 255 || len(result.ProxyPassword) > 255 {
		return resolvedModelProvider{}, fmt.Errorf("proxy credentials are too long")
	}
	if result.ProxyURL == "" {
		result.ProxyUsername = ""
		result.ProxyPassword = ""
	} else if result.ProxyUsername == "" {
		result.ProxyPassword = ""
	}
	return result, nil
}

func (s *Service) ModelTestConfig(ctx context.Context, input domain.ModelTestInput) (config.Model, error) {
	resolved, err := s.resolveModelProvider(
		ctx, input.ID, input.Kind, input.BaseURL, input.APIKey,
		input.ProxyURL, input.ProxyUsername, input.ProxyPassword, input.ClearProxyPassword,
		input.UserAgent,
	)
	if err != nil {
		return config.Model{}, err
	}
	model := strings.TrimSpace(input.Model)
	if model == "" {
		return config.Model{}, fmt.Errorf("model is required")
	}
	return config.Model{
		APIKey: resolved.APIKey, Kind: resolved.Kind, BaseURL: resolved.BaseURL, Name: model, UserAgent: resolved.UserAgent,
		ProxyURL: resolved.ProxyURL, ProxyUsername: resolved.ProxyUsername, ProxyPassword: resolved.ProxyPassword,
	}, nil
}

func (s *Service) DiscoverModels(ctx context.Context, input domain.ModelDiscoveryInput, actor string) (domain.ModelCatalog, error) {
	resolved, err := s.resolveModelProvider(
		ctx, input.ID, input.Kind, input.BaseURL, input.APIKey,
		input.ProxyURL, input.ProxyUsername, input.ProxyPassword, input.ClearProxyPassword,
		input.UserAgent,
	)
	if err != nil {
		return domain.ModelCatalog{}, err
	}
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
	request.Header.Set("Accept", "application/json")
	if resolved.Kind == "anthropic" {
		request.Header.Set("x-api-key", resolved.APIKey)
		request.Header.Set("anthropic-version", "2023-06-01")
	} else if resolved.APIKey != "" {
		request.Header.Set("Authorization", "Bearer "+resolved.APIKey)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	if resolved.ProxyURL != "" {
		client, err = proxyx.NewHTTPClient(resolved.ProxyURL, resolved.ProxyUsername, resolved.ProxyPassword, 15*time.Second)
		if err != nil {
			return domain.ModelCatalog{}, err
		}
	}
	if resolved.UserAgent != "" {
		client = proxyx.WrapHeaders(client, map[string]string{"User-Agent": resolved.UserAgent})
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
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
	s.audit(ctx, "", "model_catalog_discovered", actor, map[string]any{
		"provider_id": resolved.ID, "kind": resolved.Kind, "model_count": len(models),
	})
	return domain.ModelCatalog{Models: models, Count: len(models)}, nil
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
