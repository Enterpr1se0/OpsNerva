package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"eino-ops-agent/internal/domain"
)

func TestConfigurationPackageMovesCredentialsAcrossMasterKeys(t *testing.T) {
	ctx := context.Background()
	source, _, _ := newTestService(t)
	proxy, err := source.SaveProxy(ctx, domain.ProxyInput{Name: "migration proxy", URL: "socks5://127.0.0.1:1080", Username: "proxy-user", Password: "proxy-secret"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	agentEnabled := true
	passwordHost, err := source.SaveHost(ctx, domain.HostInput{
		Name: "password migration", Address: "192.0.2.10", Port: 22, User: "ops", AgentEnabled: &agentEnabled,
		AuthType: "password", Password: "ssh-secret", ProxyID: proxy.ID, SudoMode: "password", SudoPassword: "sudo-secret",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	keyHost, err := source.SaveHost(ctx, domain.HostInput{
		Name: "key migration", Address: "192.0.2.11", Port: 22, User: "ops", AgentEnabled: &agentEnabled,
		AuthType: "key", PrivateKey: string(testSSHPrivateKey(t)), ProxyJumpHostID: passwordHost.ID, SudoMode: "none",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := source.SaveModelProvider(ctx, domain.ModelProviderInput{Name: "migration model", Kind: "openai", Model: "gpt-migrate", APIKey: "model-secret", ProxyID: proxy.ID}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.ActivateModelProvider(ctx, provider.ID, "test"); err != nil {
		t.Fatal(err)
	}
	pkg, err := source.ExportConfiguration(ctx, true, "test", "test")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(pkg)
	for _, secret := range []string{"proxy-secret", "ssh-secret", "sudo-secret", "model-secret", "PRIVATE KEY"} {
		if !strings.Contains(string(encoded), secret) {
			t.Fatalf("encrypted package plaintext fixture is missing %q before envelope encryption", secret)
		}
	}

	target, _, _ := newTestService(t)
	result, err := target.ImportConfiguration(ctx, pkg, true, "test")
	if err != nil {
		t.Fatal(err)
	}
	if result.Proxies != len(pkg.Proxies) || result.Hosts != len(pkg.Hosts) || result.ModelProviders != len(pkg.ModelProviders) || !result.SecretsImported {
		t.Fatalf("unexpected import result: %#v", result)
	}
	targetProxy, err := target.store.GetProxy(ctx, proxy.ID)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := target.encryptor.Decrypt(targetProxy.PasswordCipher)
	if err != nil || string(plain) != "proxy-secret" {
		t.Fatalf("proxy credential did not migrate: %q, %v", plain, err)
	}
	targetPasswordHost, err := target.store.GetHost(ctx, passwordHost.ID)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := target.encryptor.Decrypt(targetPasswordHost.PasswordCipher)
	sudoPassword, _ := target.encryptor.Decrypt(targetPasswordHost.SudoCipher)
	if string(password) != "ssh-secret" || string(sudoPassword) != "sudo-secret" || targetPasswordHost.ProxyID != proxy.ID {
		t.Fatalf("host credentials or proxy reference did not migrate: %#v", targetPasswordHost)
	}
	targetKeyHost, err := target.store.GetHost(ctx, keyHost.ID)
	if err != nil {
		t.Fatal(err)
	}
	privateKey, _ := target.encryptor.Decrypt(targetKeyHost.PrivateKeyCipher)
	if !strings.Contains(string(privateKey), "PRIVATE KEY") || targetKeyHost.ProxyJumpHostID != passwordHost.ID {
		t.Fatalf("private key or ProxyJump reference did not migrate: %#v", targetKeyHost)
	}
	targetProvider, err := target.store.GetModelProvider(ctx, provider.ID)
	if err != nil {
		t.Fatal(err)
	}
	apiKey, _ := target.encryptor.Decrypt(targetProvider.APIKeyCipher)
	if string(apiKey) != "model-secret" || !targetProvider.Active || targetProvider.ProxyID != proxy.ID {
		t.Fatalf("model credential or state did not migrate: %#v", targetProvider)
	}
}

func TestPlainConfigurationNeverExportsCredentials(t *testing.T) {
	ctx := context.Background()
	source, _, _ := newTestService(t)
	provider, err := source.SaveModelProvider(ctx, domain.ModelProviderInput{Name: "plain model", Kind: "openai", Model: "gpt-plain", APIKey: "never-export-this"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.ActivateModelProvider(ctx, provider.ID, "test"); err != nil {
		t.Fatal(err)
	}
	pkg, err := source.ExportConfiguration(ctx, false, "test", "test")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(pkg)
	if strings.Contains(string(encoded), "never-export-this") || strings.Contains(string(encoded), "cipher") || pkg.SecretsIncluded {
		t.Fatalf("plain package exposed credentials: %s", encoded)
	}
	target, _, _ := newTestService(t)
	if _, err := target.ImportConfiguration(ctx, pkg, false, "test"); err != nil {
		t.Fatal(err)
	}
	imported, err := target.store.GetModelProvider(ctx, provider.ID)
	if err != nil {
		t.Fatal(err)
	}
	if imported.APIKeyCipher != "" || imported.Active {
		t.Fatalf("plain import invented credentials or activated an unusable provider: %#v", imported)
	}
}

func TestConfigurationImportRejectsProxyJumpCycleBeforeWriting(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestService(t)
	pkg := domain.ConfigurationPackage{
		Schema: domain.ConfigurationSchema, SchemaVersion: domain.ConfigurationSchemaVersion,
		Proxies: []domain.ConfigurationProxy{}, ModelProviders: []domain.ConfigurationModelProvider{},
		Hosts: []domain.ConfigurationHost{
			{ID: "host-cycle-a", Name: "cycle a", Address: "192.0.2.21", Port: 22, User: "ops", AgentEnabled: true, AuthType: "agent", ProxyJumpHostID: "host-cycle-b", SudoMode: "none"},
			{ID: "host-cycle-b", Name: "cycle b", Address: "192.0.2.22", Port: 22, User: "ops", AgentEnabled: true, AuthType: "agent", ProxyJumpHostID: "host-cycle-a", SudoMode: "none"},
		},
	}
	if _, err := svc.ImportConfiguration(ctx, pkg, false, "test"); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("ProxyJump cycle was accepted: %v", err)
	}
	if _, err := svc.store.GetHost(ctx, "host-cycle-a"); err == nil {
		t.Fatal("invalid package was partially written")
	}
}
