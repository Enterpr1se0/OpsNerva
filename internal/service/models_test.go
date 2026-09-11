package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestModelProviderProxyIsEncryptedPreservedAndUsedForDiscovery(t *testing.T) {
	const proxyPassword = "model-proxy-secret"
	wantProxyAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("proxy-user:"+proxyPassword))
	proxyHits := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits++
		if r.Method != http.MethodGet || r.URL.Host != "model.invalid" || r.URL.Path != "/v1/models" {
			t.Errorf("unexpected proxied model request: %s %s", r.Method, r.URL.String())
		}
		if got := r.Header.Get("Proxy-Authorization"); got != wantProxyAuth {
			t.Errorf("unexpected proxy authorization %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"proxied-model"}]}`))
	}))
	defer proxy.Close()

	svc, _, _ := newTestService(t)
	ctx := context.Background()
	savedProxy, err := svc.SaveProxy(ctx, domain.ProxyInput{
		Name: "model proxy", URL: proxy.URL + "/", Username: "proxy-user", Password: proxyPassword,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := svc.SaveModelProvider(ctx, domain.ModelProviderInput{
		Name: "proxied", Kind: "openai_compatible", BaseURL: "http://model.invalid/v1", Model: "proxied-model",
		ProxyID: savedProxy.ID,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if provider.ProxyID != savedProxy.ID {
		t.Fatalf("unexpected public proxy configuration: %#v", provider)
	}
	serialized, err := json.Marshal(provider)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(serialized), proxyPassword) || strings.Contains(string(serialized), "cipher") {
		t.Fatalf("provider JSON exposed proxy credentials: %s", serialized)
	}
	stored, err := svc.store.GetProxy(ctx, savedProxy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PasswordCipher == "" || strings.Contains(stored.PasswordCipher, proxyPassword) {
		t.Fatalf("proxy password was not encrypted: %#v", stored)
	}
	cfg, _, err := svc.ModelProviderConfig(ctx, provider.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProxyURL != proxy.URL || cfg.ProxyUsername != "proxy-user" || cfg.ProxyPassword != proxyPassword {
		t.Fatalf("proxy credentials did not round-trip: %#v", cfg)
	}

	catalog, err := svc.DiscoverModels(ctx, domain.ModelDiscoveryInput{ID: provider.ID}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if proxyHits != 1 || catalog.Count != 1 || catalog.Models[0] != "proxied-model" {
		t.Fatalf("model discovery did not use the configured proxy: hits=%d catalog=%#v", proxyHits, catalog)
	}

	preserved, err := svc.SaveModelProvider(ctx, domain.ModelProviderInput{
		ID: provider.ID, Name: provider.Name, Kind: provider.Kind, BaseURL: provider.BaseURL, Model: provider.Model,
		ProxyID: provider.ProxyID,
	}, "test")
	if err != nil || preserved.ProxyID != savedProxy.ID {
		t.Fatalf("proxy reference was not preserved: provider=%#v err=%v", preserved, err)
	}
	changed, err := svc.SaveProxy(ctx, domain.ProxyInput{
		ID: savedProxy.ID, Name: savedProxy.Name, URL: savedProxy.URL, Username: "different-user",
	}, "test")
	if err != nil || changed.HasPassword {
		t.Fatalf("changed proxy identity reused the stored password: proxy=%#v err=%v", changed, err)
	}
}

func TestDiscoverModelsUsesStoredKeyAndRedactsUpstreamErrors(t *testing.T) {
	const secret = "fixture-secret-value"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad/models" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`api_key=` + secret))
			return
		}
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+secret {
			http.Error(w, "missing authorization", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"z-model"},{"id":"a-model"},{"id":"a-model"}]}`))
	}))
	defer server.Close()

	svc, _, _ := newTestService(t)
	ctx := context.Background()
	provider, err := svc.SaveModelProvider(ctx, domain.ModelProviderInput{
		Name: "catalog", Kind: "openai_compatible", BaseURL: server.URL + "/v1", Model: "a-model", APIKey: secret,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := svc.DiscoverModels(ctx, domain.ModelDiscoveryInput{ID: provider.ID}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Count != 2 || strings.Join(catalog.Models, ",") != "a-model,z-model" || len(catalog.ContextWindows) != 0 {
		t.Fatalf("unexpected catalog %#v", catalog)
	}

	badURL := server.URL + "/bad"
	_, err = svc.DiscoverModels(ctx, domain.ModelDiscoveryInput{
		Kind: "openai_compatible", BaseURL: &badURL, APIKey: secret,
	}, "test")
	if !errors.Is(err, ErrModelProviderUpstream) {
		t.Fatalf("expected upstream error, got %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("upstream error exposed API key: %v", err)
	}
}

func TestDiscoverModelsEnrichesAndCachesModelsDevMetadata(t *testing.T) {
	const secret = "catalog-secret"
	var catalogRequests, metadataRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			catalogRequests++
			if r.Header.Get("Authorization") != "Bearer "+secret {
				http.Error(w, "missing provider authorization", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-test"}]}`))
		case "/models.json":
			metadataRequests++
			if r.Header.Get("Authorization") != "" || r.Header.Get("x-api-key") != "" {
				http.Error(w, "provider credential leaked", http.StatusBadRequest)
				return
			}
			if r.Header.Get("User-Agent") != "OpsNerva/1" {
				http.Error(w, "missing metadata user agent", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", `"fixture"`)
			_, _ = w.Write([]byte(`{
				"openai/gpt-test": {
					"id":"openai/gpt-test","name":"GPT Test","family":"gpt","attachment":true,
					"reasoning":true,"tool_call":true,"structured_output":true,"temperature":false,
					"knowledge":"2026-01","release_date":"2026-02-01","last_updated":"2026-03-01",
					"limit":{"context":200000,"input":180000,"output":20000},
					"modalities":{"input":["text","image"],"output":["text"]}
				}
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	svc, _, _ := newTestService(t)
	svc.modelMetadata.url = server.URL + "/models.json"
	provider, err := svc.SaveModelProvider(context.Background(), domain.ModelProviderInput{
		Name: "metadata", Kind: "openai_compatible", BaseURL: server.URL + "/v1", Model: "gpt-test", APIKey: secret,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := svc.DiscoverModels(context.Background(), domain.ModelDiscoveryInput{ID: provider.ID}, "test")
	if err != nil {
		t.Fatal(err)
	}
	metadata, exists := catalog.Metadata["gpt-test"]
	if !exists || metadata.ID != "openai/gpt-test" || metadata.Name != "GPT Test" || metadata.ContextWindow != 200000 || metadata.InputTokenLimit != 180000 || metadata.OutputTokenLimit != 20000 || !metadata.Attachment || !metadata.Reasoning || !metadata.ToolCall || !metadata.StructuredOutput || metadata.Temperature {
		t.Fatalf("unexpected models.dev metadata: %#v", metadata)
	}
	if catalog.ContextWindows["gpt-test"] != 200000 {
		t.Fatalf("context window was not enriched: %#v", catalog)
	}
	providers, err := svc.ListModelProviders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var listed domain.ModelProvider
	for _, candidate := range providers {
		if candidate.ID == provider.ID {
			listed = candidate
			break
		}
	}
	if listed.ContextWindow != 0 || listed.ResolvedContextWindow != 200000 {
		t.Fatalf("automatic context window was not exposed: %#v", listed)
	}
	cfg, _, err := svc.ModelProviderConfig(context.Background(), provider.ID)
	if err != nil {
		t.Fatal(err)
	}
	window, err := svc.DetectModelContextWindow(context.Background(), cfg)
	if err != nil || window != 200000 {
		t.Fatalf("detected context window = %d, %v", window, err)
	}
	if catalogRequests != 1 || metadataRequests != 1 {
		t.Fatalf("request counts: catalog=%d metadata=%d", catalogRequests, metadataRequests)
	}
	gateway, err := svc.SaveModelProvider(context.Background(), domain.ModelProviderInput{
		Name: "OpenAI dialect gateway", Kind: "openai", BaseURL: server.URL + "/v1", Model: "gpt-test", APIKey: secret,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	gatewayCatalog, err := svc.DiscoverModels(context.Background(), domain.ModelDiscoveryInput{ID: gateway.ID}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(gatewayCatalog.Models, ",") != "gpt-test" || gatewayCatalog.Metadata["gpt-test"].ContextWindow != 200000 {
		t.Fatalf("unexpected gateway catalog: %#v", gatewayCatalog)
	}
	gatewayConfig, _, err := svc.ModelProviderConfig(context.Background(), gateway.ID)
	if err != nil {
		t.Fatal(err)
	}
	window, err = svc.DetectModelContextWindow(context.Background(), gatewayConfig)
	if err != nil || window != 200000 {
		t.Fatalf("gateway context window = %d, %v", window, err)
	}
	if catalogRequests != 2 || metadataRequests != 1 {
		t.Fatalf("gateway did not preserve provider discovery and models.dev cache: catalog=%d metadata=%d", catalogRequests, metadataRequests)
	}
}

func TestDiscoverModelsAnthropic(t *testing.T) {
	const secret = "sk-ant-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("limit") != "1000" {
			http.Error(w, "missing limit", http.StatusBadRequest)
			return
		}
		if r.Header.Get("x-api-key") != secret || r.Header.Get("anthropic-version") == "" {
			http.Error(w, "missing anthropic auth headers", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("User-Agent") != "OpsNerva-Test/1.0" {
			http.Error(w, "user agent was not rewritten", http.StatusForbidden)
			return
		}
		if r.Header.Get("Authorization") != "" {
			http.Error(w, "unexpected bearer authorization", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-opus-4-8"},{"id":"claude-haiku-4-5"}]}`))
	}))
	defer server.Close()

	svc, _, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.SaveModelProvider(ctx, domain.ModelProviderInput{
		Name: "claude", Kind: "anthropic", BaseURL: server.URL, Model: "claude-opus-4-8",
	}, "test"); err == nil {
		t.Fatal("anthropic provider without an API key was accepted")
	}
	ptr := func(value string) *string { return &value }
	for _, invalid := range []string{"broken\nagent", "escape\x1bagent"} {
		if _, err := svc.SaveModelProvider(ctx, domain.ModelProviderInput{
			Name: "claude", Kind: "anthropic", BaseURL: server.URL, Model: "claude-opus-4-8", APIKey: secret,
			UserAgent: ptr(invalid),
		}, "test"); err == nil {
			t.Fatalf("user agent %q with control characters was accepted", invalid)
		}
	}
	provider, err := svc.SaveModelProvider(ctx, domain.ModelProviderInput{
		Name: "claude", Kind: "anthropic", BaseURL: server.URL + "/v1", Model: "claude-opus-4-8", APIKey: secret,
		UserAgent: ptr("OpsNerva-Test/1.0"),
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if provider.BaseURL != server.URL {
		t.Fatalf("expected version segment stripped from base URL, got %q", provider.BaseURL)
	}
	if provider.UserAgent != "OpsNerva-Test/1.0" {
		t.Fatalf("user agent was not persisted, got %q", provider.UserAgent)
	}
	renamed, err := svc.SaveModelProvider(ctx, domain.ModelProviderInput{
		ID: provider.ID, Name: "claude renamed", Kind: "anthropic", BaseURL: server.URL, Model: "claude-opus-4-8",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if renamed.UserAgent != "OpsNerva-Test/1.0" {
		t.Fatalf("omitting user_agent on edit should keep the stored value, got %q", renamed.UserAgent)
	}
	cleared, err := svc.SaveModelProvider(ctx, domain.ModelProviderInput{
		ID: provider.ID, Name: "claude renamed", Kind: "anthropic", BaseURL: server.URL, Model: "claude-opus-4-8",
		UserAgent: ptr(""),
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if cleared.UserAgent != "" {
		t.Fatalf("explicit empty user_agent should clear the stored value, got %q", cleared.UserAgent)
	}
	if _, err := svc.SaveModelProvider(ctx, domain.ModelProviderInput{
		ID: provider.ID, Name: "claude renamed", Kind: "anthropic", BaseURL: server.URL, Model: "claude-opus-4-8",
		UserAgent: ptr("OpsNerva-Test/1.0"),
	}, "test"); err != nil {
		t.Fatal(err)
	}
	catalog, err := svc.DiscoverModels(ctx, domain.ModelDiscoveryInput{ID: provider.ID}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Count != 2 || strings.Join(catalog.Models, ",") != "claude-haiku-4-5,claude-opus-4-8" {
		t.Fatalf("unexpected catalog %#v", catalog)
	}
}

func TestNormalizeProviderBaseURL(t *testing.T) {
	tests := []struct {
		name  string
		value string
		kind  string
		want  string
	}{
		{name: "local IP", value: "127.0.0.1:11434/v1", kind: "ollama", want: "http://127.0.0.1:11434/v1"},
		{name: "localhost", value: "localhost:11434/v1/models", kind: "ollama", want: "http://localhost:11434/v1"},
		{name: "private IP", value: "192.168.1.8:8080/v1/chat/completions", kind: "openai_compatible", want: "http://192.168.1.8:8080/v1"},
		{name: "public domain", value: "api.example.com/v1", kind: "openai_compatible", want: "https://api.example.com/v1"},
		{name: "OpenAI default", value: "", kind: "openai", want: "https://api.openai.com/v1"},
		{name: "DeepSeek default", value: "", kind: "deepseek", want: "https://api.deepseek.com"},
		{name: "Anthropic default", value: "", kind: "anthropic", want: "https://api.anthropic.com"},
		{name: "Anthropic strips version segment", value: "https://api.anthropic.com/v1", kind: "anthropic", want: "https://api.anthropic.com"},
		{name: "Anthropic strips messages endpoint", value: "https://gateway.example.com/v1/messages", kind: "anthropic", want: "https://gateway.example.com"},
		{name: "Anthropic strips models endpoint", value: "https://api.anthropic.com/v1/models", kind: "anthropic", want: "https://api.anthropic.com"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := normalizeProviderBaseURL(test.value, test.kind)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("normalizeProviderBaseURL(%q) = %q, want %q", test.value, got, test.want)
			}
		})
	}
	if _, err := normalizeProviderBaseURL("", "openai_compatible"); err == nil {
		t.Fatal("empty custom provider URL was accepted")
	}
}
