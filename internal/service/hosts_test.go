package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"
	"golang.org/x/crypto/ssh"
)

func TestHostCredentialsAreEncryptedPreservedAndNeverSerialized(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	proxy, err := svc.SaveProxy(ctx, domain.ProxyInput{
		Name: "host-proxy", URL: "SOCKS5://127.0.0.1:1080/", Username: "proxy-user", Password: "proxy-super-secret",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	host, err := svc.SaveHost(ctx, domain.HostInput{
		Name: "password-host", Address: "192.0.2.10", Port: 22, User: "ops", AuthType: "password",
		Password: "ssh-super-secret", SudoMode: "password", SudoPassword: "sudo-super-secret",
		ProxyID: proxy.ID,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !host.HasPassword || !host.HasSudoPassword || host.ProxyID != proxy.ID {
		t.Fatalf("credential capability flags missing: %#v", host)
	}
	stored, err := svc.store.GetHost(ctx, host.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PasswordCipher == "" || stored.SudoCipher == "" || strings.Contains(stored.PasswordCipher, "super-secret") || strings.Contains(stored.SudoCipher, "super-secret") {
		t.Fatalf("host credentials were not encrypted: %#v", stored)
	}
	storedProxy, err := svc.store.GetProxy(ctx, proxy.ID)
	if err != nil || storedProxy.PasswordCipher == "" || strings.Contains(storedProxy.PasswordCipher, "super-secret") {
		t.Fatalf("proxy credentials were not encrypted separately: proxy=%#v err=%v", storedProxy, err)
	}
	publicJSON, _ := json.Marshal(host)
	if strings.Contains(string(publicJSON), "super-secret") || strings.Contains(string(publicJSON), "cipher") {
		t.Fatalf("host JSON exposed secret material: %s", publicJSON)
	}

	updated, err := svc.SaveHost(ctx, domain.HostInput{
		ID: host.ID, Name: "password-host-renamed", Address: host.Address, Port: host.Port, User: host.User,
		AuthType: "password", SudoMode: "password", ProxyID: host.ProxyID,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !updated.HasPassword || !updated.HasSudoPassword || updated.ProxyID != proxy.ID {
		t.Fatalf("blank edit erased stored credentials: %#v", updated)
	}
	connection, _, err := svc.resolveSSHConnection(ctx, updated)
	if err != nil {
		t.Fatal(err)
	}
	hydrated, err := svc.hydrateHostSecrets(connection.Target, true)
	if err != nil {
		t.Fatal(err)
	}
	if hydrated.Password != "ssh-super-secret" || hydrated.SudoPassword != "sudo-super-secret" || hydrated.ProxyPassword != "proxy-super-secret" {
		t.Fatal("encrypted host credentials did not round-trip")
	}
}

func TestUploadedPrivateKeyIsEncryptedPreservedAndNeverSerialized(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	privateKey := testSSHPrivateKey(t)
	host, err := svc.SaveHost(ctx, domain.HostInput{
		Name: "key-host", Address: "192.0.2.20", Port: 22, User: "ops", AuthType: "key",
		PrivateKey: string(privateKey), SudoMode: "none",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !host.HasPrivateKey {
		t.Fatalf("private key capability flag missing: %#v", host)
	}
	stored, err := svc.store.GetHost(ctx, host.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PrivateKeyCipher == "" || strings.Contains(stored.PrivateKeyCipher, "PRIVATE KEY") {
		t.Fatalf("private key was not encrypted: %#v", stored)
	}
	publicJSON, _ := json.Marshal(host)
	if strings.Contains(string(publicJSON), "PRIVATE KEY") || strings.Contains(string(publicJSON), "private_key_cipher") || strings.Contains(string(publicJSON), "private_key_path") {
		t.Fatalf("host JSON exposed private key material: %s", publicJSON)
	}

	updated, err := svc.SaveHost(ctx, domain.HostInput{
		ID: host.ID, Name: host.Name, Address: host.Address, Port: host.Port, User: host.User,
		AuthType: "key", SudoMode: "none",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !updated.HasPrivateKey {
		t.Fatal("blank edit erased the uploaded private key")
	}
	hydrated, err := svc.hydrateHostSecrets(updated, false)
	if err != nil {
		t.Fatal(err)
	}
	if string(hydrated.PrivateKey) != string(privateKey) {
		t.Fatal("encrypted private key did not round-trip")
	}

	withoutKey, err := svc.SaveHost(ctx, domain.HostInput{
		ID: host.ID, Name: host.Name, Address: host.Address, Port: host.Port, User: host.User,
		AuthType: "agent", SudoMode: "none",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if withoutKey.HasPrivateKey || withoutKey.PrivateKeyCipher != "" {
		t.Fatal("switching authentication away from key retained the private key")
	}
	if _, err := svc.SaveHost(ctx, domain.HostInput{
		Name: "invalid-key", Address: "192.0.2.21", Port: 22, User: "ops", AuthType: "key",
		PrivateKey: "not a private key", SudoMode: "none",
	}, "test"); err == nil || !strings.Contains(err.Error(), "invalid SSH private key upload") {
		t.Fatalf("invalid private key upload was accepted: %v", err)
	}
}

func TestHostsIncludeStoredHostKeyState(t *testing.T) {
	svc, transport, host := newTestService(t)
	transport.storedKeys = map[string]sshx.HostKey{
		host.ID: {Fingerprint: "SHA256:trusted", Algorithm: ssh.KeyAlgoED25519, Trusted: true},
	}

	listed, err := svc.ListHosts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].HostKey == nil || !listed[0].HostKey.Trusted || listed[0].HostKey.Fingerprint != "SHA256:trusted" {
		t.Fatalf("list did not include stored host key state: %#v", listed)
	}
	got, err := svc.GetHost(context.Background(), host.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HostKey == nil || got.HostKey.Algorithm != ssh.KeyAlgoED25519 {
		t.Fatalf("get did not include stored host key state: %#v", got)
	}
}
