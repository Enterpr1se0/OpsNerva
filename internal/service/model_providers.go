package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func (s *Service) SaveModelProvider(ctx context.Context, input domain.ModelProviderInput, actor string) (domain.ModelProvider, error) {
	input.ID = strings.TrimSpace(input.ID)
	input.Name = strings.TrimSpace(input.Name)
	input.Kind = strings.TrimSpace(input.Kind)
	input.BaseURL = strings.TrimSpace(input.BaseURL)
	input.Model = strings.TrimSpace(input.Model)
	input.ProxyID = strings.TrimSpace(input.ProxyID)
	contextWindow := 0
	if input.ContextWindow != nil {
		contextWindow = *input.ContextWindow
		if contextWindow != 0 && (contextWindow < domain.MinModelContextWindow || contextWindow > domain.MaxModelContextWindow) {
			return domain.ModelProvider{}, fmt.Errorf("context_window must be between %d and %d", domain.MinModelContextWindow, domain.MaxModelContextWindow)
		}
	}
	reasoningEffort := ""
	if input.ReasoningEffort != nil {
		var err error
		reasoningEffort, err = normalizeReasoningEffort(*input.ReasoningEffort)
		if err != nil {
			return domain.ModelProvider{}, err
		}
	}
	userAgent := ""
	if input.UserAgent != nil {
		normalizedUserAgent, err := validateProviderUserAgent(*input.UserAgent)
		if err != nil {
			return domain.ModelProvider{}, err
		}
		userAgent = normalizedUserAgent
	}
	if input.Name == "" {
		return domain.ModelProvider{}, fmt.Errorf("provider name is required")
	}
	if input.Model == "" {
		return domain.ModelProvider{}, fmt.Errorf("model is required")
	}
	if input.Kind == "" {
		input.Kind = "openai_compatible"
	}
	switch input.Kind {
	case "openai", "deepseek", "anthropic", "openai_compatible", "ollama":
	default:
		return domain.ModelProvider{}, fmt.Errorf("invalid provider kind %q", input.Kind)
	}
	normalizedBaseURL, err := normalizeProviderBaseURL(input.BaseURL, input.Kind)
	if err != nil {
		return domain.ModelProvider{}, err
	}
	input.BaseURL = normalizedBaseURL
	if input.ProxyID != "" {
		if _, err := s.store.GetProxy(ctx, input.ProxyID); err != nil {
			return domain.ModelProvider{}, fmt.Errorf("load proxy %q: %w", input.ProxyID, err)
		}
	}

	var existing domain.ModelProvider
	if input.ID != "" {
		existing, err = s.store.GetModelProvider(ctx, input.ID)
		if err != nil {
			return domain.ModelProvider{}, err
		}
	}
	provider := domain.ModelProvider{
		ID: input.ID, Name: input.Name, Kind: input.Kind, BaseURL: input.BaseURL, Model: input.Model,
		ContextWindow: contextWindow,
		ProxyID:       input.ProxyID, UserAgent: userAgent, ReasoningEffort: reasoningEffort,
	}
	if existing.ID != "" {
		provider.CreatedAt = existing.CreatedAt
		provider.Active = existing.Active
		provider.APIKeyCipher = existing.APIKeyCipher
		if input.ContextWindow == nil {
			provider.ContextWindow = existing.ContextWindow
		}
		if input.UserAgent == nil {
			provider.UserAgent = existing.UserAgent
		}
		if input.ReasoningEffort == nil {
			provider.ReasoningEffort = existing.ReasoningEffort
		}
	}
	if key := strings.TrimSpace(input.APIKey); key != "" {
		cipher, err := s.encryptor.Encrypt([]byte(key))
		if err != nil {
			return domain.ModelProvider{}, err
		}
		provider.APIKeyCipher = cipher
	}
	if providerKindRequiresAPIKey(provider.Kind) && provider.APIKeyCipher == "" {
		return domain.ModelProvider{}, fmt.Errorf("api_key is required for %s", provider.Kind)
	}
	saved, err := s.store.UpsertModelProvider(ctx, provider)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique constraint") {
			return domain.ModelProvider{}, fmt.Errorf("provider name already exists")
		}
		return domain.ModelProvider{}, err
	}
	s.audit(ctx, "", "model_provider_saved", actor, map[string]any{
		"provider_id": saved.ID, "name": saved.Name, "kind": saved.Kind, "model": saved.Model,
		"proxy_id": saved.ProxyID, "reasoning_effort": saved.ReasoningEffort,
		"context_window": saved.ContextWindow,
	})
	return saved, nil
}

func (s *Service) ListModelProviders(ctx context.Context) ([]domain.ModelProvider, error) {
	providers, err := s.store.ListModelProviders(ctx)
	if err != nil {
		return nil, err
	}
	for index := range providers {
		if providers[index].ContextWindow != 0 {
			continue
		}
		metadata, exists := s.cachedModelMetadata(providers[index].Kind, providers[index].Model)
		if exists {
			providers[index].ResolvedContextWindow = metadata.ContextWindow
		}
	}
	return providers, nil
}

func (s *Service) ModelProviderConfig(ctx context.Context, id string) (config.Model, domain.ModelProvider, error) {
	provider, err := s.store.GetModelProvider(ctx, id)
	if err != nil {
		return config.Model{}, domain.ModelProvider{}, err
	}
	key, err := s.encryptor.Decrypt(provider.APIKeyCipher)
	if err != nil {
		return config.Model{}, domain.ModelProvider{}, fmt.Errorf("decrypt model provider API key: %w", err)
	}
	proxy, err := s.resolveProxy(ctx, provider.ProxyID)
	if err != nil {
		return config.Model{}, domain.ModelProvider{}, err
	}
	return config.Model{
		APIKey: string(key), Kind: provider.Kind, BaseURL: provider.BaseURL, Name: provider.Model, ContextWindow: provider.ContextWindow, ReasoningEffort: provider.ReasoningEffort, UserAgent: provider.UserAgent,
		ProxyURL: proxy.URL, ProxyUsername: proxy.Username, ProxyPassword: proxy.Password,
	}, provider, nil
}

func (s *Service) ActiveModelConfig(ctx context.Context) (config.Model, domain.ModelProvider, error) {
	provider, err := s.store.ActiveModelProvider(ctx)
	if err != nil {
		return config.Model{}, domain.ModelProvider{}, err
	}
	return s.ModelProviderConfig(ctx, provider.ID)
}

func (s *Service) ActivateModelProvider(ctx context.Context, id, actor string) (domain.ModelProvider, error) {
	provider, err := s.store.GetModelProvider(ctx, id)
	if err != nil {
		return domain.ModelProvider{}, err
	}
	if err := s.store.ActivateModelProvider(ctx, id); err != nil {
		return domain.ModelProvider{}, err
	}
	provider.Active = true
	s.audit(ctx, "", "model_provider_activated", actor, map[string]any{
		"provider_id": provider.ID, "name": provider.Name, "model": provider.Model,
	})
	return provider, nil
}

func (s *Service) DeleteModelProvider(ctx context.Context, id, actor string) (bool, error) {
	provider, err := s.store.GetModelProvider(ctx, id)
	if err != nil {
		return false, err
	}
	settings, err := s.store.GetSystemSettings(ctx)
	if err != nil {
		return false, err
	}
	if settings.SubagentModelProviderID == provider.ID {
		return false, fmt.Errorf("%w: %q is selected for the approval Agent; choose another provider in system settings before deleting it", ErrModelProviderInUse, provider.Name)
	}
	if settings.AutomaticApprovalModelProviderID == provider.ID {
		return false, fmt.Errorf("%w: %q is selected for the Auto approval Agent; choose another provider in system settings before deleting it", ErrModelProviderInUse, provider.Name)
	}
	if err := s.store.DeleteModelProvider(ctx, id); err != nil {
		return false, err
	}
	s.audit(ctx, "", "model_provider_deleted", actor, map[string]any{
		"provider_id": provider.ID, "name": provider.Name, "was_active": provider.Active,
	})
	return provider.Active, nil
}
