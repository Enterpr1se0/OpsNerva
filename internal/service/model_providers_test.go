package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestModelProvidersEncryptKeysAndSwitchActiveProvider(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	reasoningEffort := "max"
	first, err := svc.SaveModelProvider(ctx, domain.ModelProviderInput{
		Name: "primary", Kind: "openai", Model: "gpt-test", ReasoningEffort: &reasoningEffort, APIKey: "sk-super-secret",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !first.HasAPIKey || first.Active || first.ReasoningEffort != "max" || first.ContextWindow != 0 {
		t.Fatalf("unexpected saved provider %#v", first)
	}
	stored, err := svc.store.GetModelProvider(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.APIKeyCipher == "" || strings.Contains(stored.APIKeyCipher, "sk-super-secret") {
		t.Fatalf("API key was not encrypted: %q", stored.APIKeyCipher)
	}
	publicJSON, _ := json.Marshal(first)
	if strings.Contains(string(publicJSON), "secret") || strings.Contains(string(publicJSON), "cipher") {
		t.Fatalf("provider JSON exposed secret material: %s", publicJSON)
	}

	second, err := svc.SaveModelProvider(ctx, domain.ModelProviderInput{
		Name: "local", Kind: "ollama", BaseURL: "http://127.0.0.1:11434/v1/", Model: "local-test",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if second.BaseURL != "http://127.0.0.1:11434/v1" {
		t.Fatalf("base URL was not normalized: %q", second.BaseURL)
	}
	active, err := svc.ActivateModelProvider(ctx, second.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !active.Active {
		t.Fatal("provider was not activated")
	}
	providers, err := svc.ListModelProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 2 || providers[0].ID != first.ID || providers[1].ID != second.ID {
		t.Fatalf("activating a provider changed list order: %#v", providers)
	}
	cfg, selected, err := svc.ActiveModelConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if selected.ID != second.ID || cfg.Name != "local-test" || cfg.BaseURL != second.BaseURL {
		t.Fatalf("unexpected active model config %#v provider=%#v", cfg, selected)
	}

	updated, err := svc.SaveModelProvider(ctx, domain.ModelProviderInput{
		ID: first.ID, Name: first.Name, Kind: first.Kind, Model: "gpt-updated",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	updatedCfg, _, err := svc.ModelProviderConfig(ctx, updated.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updatedCfg.APIKey != "sk-super-secret" || updatedCfg.ReasoningEffort != "max" {
		t.Fatalf("blank update did not preserve provider settings: %#v", updatedCfg)
	}
	contextWindow := 200000
	updated, err = svc.SaveModelProvider(ctx, domain.ModelProviderInput{
		ID: first.ID, Name: first.Name, Kind: first.Kind, Model: first.Model, ContextWindow: &contextWindow,
	}, "test")
	if err != nil || updated.ContextWindow != contextWindow {
		t.Fatalf("context window update = %#v, %v", updated, err)
	}
	updated, err = svc.SaveModelProvider(ctx, domain.ModelProviderInput{
		ID: first.ID, Name: first.Name, Kind: first.Kind, Model: first.Model,
	}, "test")
	if err != nil || updated.ContextWindow != contextWindow {
		t.Fatalf("nil update did not preserve context window = %#v, %v", updated, err)
	}
	zeroContextWindow := 0
	updated, err = svc.SaveModelProvider(ctx, domain.ModelProviderInput{
		ID: first.ID, Name: first.Name, Kind: first.Kind, Model: first.Model, ContextWindow: &zeroContextWindow,
	}, "test")
	if err != nil || updated.ContextWindow != 0 {
		t.Fatalf("explicit zero cleared context window = %#v, %v", updated, err)
	}
	invalidContextWindow := 100
	if _, err := svc.SaveModelProvider(ctx, domain.ModelProviderInput{
		Name: "invalid context", Kind: "openai", Model: "gpt-test", ContextWindow: &invalidContextWindow, APIKey: "test",
	}, "test"); err == nil {
		t.Fatal("invalid context window was accepted")
	}
	invalidReasoningEffort := "maximum"
	if _, err := svc.SaveModelProvider(ctx, domain.ModelProviderInput{
		ID: first.ID, Name: first.Name, Kind: first.Kind, Model: first.Model, ReasoningEffort: &invalidReasoningEffort,
	}, "test"); err == nil {
		t.Fatal("invalid reasoning effort was accepted")
	}
}
