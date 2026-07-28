package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"eino-ops-agent/internal/domain"
)

func TestProxyIsEncryptedReferencedAndProtectedFromDeletion(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	proxy, err := svc.SaveProxy(ctx, domain.ProxyInput{
		Name: "shared", URL: "SOCKS5H://127.0.0.1:1080/", Username: "proxy-user", Password: "proxy-secret",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if proxy.URL != "socks5h://127.0.0.1:1080" || !proxy.HasPassword || !proxy.SSHCompatible {
		t.Fatalf("unexpected public proxy: %#v", proxy)
	}
	serialized, err := json.Marshal(proxy)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(serialized), "proxy-secret") || strings.Contains(string(serialized), "cipher") {
		t.Fatalf("proxy JSON exposed encrypted credentials: %s", serialized)
	}
	stored, err := svc.store.GetProxy(ctx, proxy.ID)
	if err != nil || stored.PasswordCipher == "" || strings.Contains(stored.PasswordCipher, "proxy-secret") {
		t.Fatalf("proxy password was not encrypted: proxy=%#v err=%v", stored, err)
	}

	provider, err := svc.SaveModelProvider(ctx, domain.ModelProviderInput{
		Name: "shared proxy model", Kind: "openai_compatible", BaseURL: "http://model.invalid/v1",
		Model: "fixture", ProxyID: proxy.ID,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteProxy(ctx, proxy.ID, "test"); !errors.Is(err, ErrProxyInUse) || !strings.Contains(err.Error(), provider.Name) {
		t.Fatalf("referenced proxy deletion returned %v", err)
	}
	if _, err := svc.SaveModelProvider(ctx, domain.ModelProviderInput{
		ID: provider.ID, Name: provider.Name, Kind: provider.Kind, BaseURL: provider.BaseURL, Model: provider.Model,
		ProxyID: "",
	}, "test"); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteProxy(ctx, proxy.ID, "test"); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPSProxyCannotBeAssignedToSSHHost(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	proxy, err := svc.SaveProxy(ctx, domain.ProxyInput{Name: "HTTPS", URL: "https://proxy.example:8443"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if proxy.SSHCompatible {
		t.Fatalf("HTTPS proxy was marked SSH compatible: %#v", proxy)
	}
	_, err = svc.SaveHost(ctx, domain.HostInput{
		Name: "invalid proxy host", Address: "192.0.2.50", Port: 22, User: "ops",
		AuthType: "agent", SudoMode: "none", ProxyID: proxy.ID,
	}, "test")
	if err == nil || !strings.Contains(err.Error(), "not compatible with SSH") {
		t.Fatalf("HTTPS proxy was accepted for SSH: %v", err)
	}

	noPort, err := svc.SaveProxy(ctx, domain.ProxyInput{Name: "HTTP default port", URL: "http://proxy.example"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if noPort.SSHCompatible {
		t.Fatalf("proxy without an explicit port was marked SSH compatible: %#v", noPort)
	}
}
