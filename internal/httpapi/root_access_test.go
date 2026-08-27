package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/security"
	"github.com/Enterpr1se0/opsnerva/internal/service"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"
	"github.com/Enterpr1se0/opsnerva/internal/store"
)

type rootAccessTransport struct{}

func (rootAccessTransport) Exec(context.Context, sshx.ConnectionSpec, domain.ExecRequest) (sshx.RawResult, error) {
	return sshx.RawResult{}, nil
}
func (rootAccessTransport) Probe(context.Context, sshx.ConnectionSpec) (sshx.HostInfo, error) {
	return sshx.HostInfo{Shell: "sh", ShellPath: "/bin/sh"}, nil
}
func (rootAccessTransport) ScanHostKey(context.Context, sshx.ConnectionSpec) (sshx.HostKey, error) {
	return sshx.HostKey{}, nil
}
func (rootAccessTransport) TrustHostKey(context.Context, sshx.ConnectionSpec, string) (sshx.HostKey, error) {
	return sshx.HostKey{}, nil
}
func (rootAccessTransport) StoredHostKey(domain.Host) (sshx.HostKey, bool) {
	return sshx.HostKey{}, false
}

func TestHostAgentRootEndpointValidatesAndPersistsSetting(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dataDir, "root-access.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	encryptor, err := security.NewEncryptor("", dataDir)
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(st, rootAccessTransport{}, encryptor, security.NewRedactor(), config.Default().Limits)
	agentEnabled := true
	rootHost, err := svc.SaveHost(ctx, domain.HostInput{
		Name: "root fixture", Address: "127.0.0.2", Port: 22, User: "root", AgentEnabled: &agentEnabled,
		AuthType: "password", Password: "fixture-password", SudoMode: "none",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	regularHost, err := svc.SaveHost(ctx, domain.HostInput{
		Name: "regular fixture", Address: "127.0.0.3", Port: 22, User: "ops", AgentEnabled: &agentEnabled,
		AuthType: "password", Password: "fixture-password", SudoMode: "none",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New(svc, nil, Options{}).Handler())
	defer server.Close()

	if response := putHostRootAccess(t, server, regularHost.ID, true); response.StatusCode != http.StatusBadRequest {
		response.Body.Close()
		t.Fatalf("regular host root grant status = %d", response.StatusCode)
	} else {
		response.Body.Close()
	}
	response := putHostRootAccess(t, server, rootHost.ID, true)
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		t.Fatalf("root host grant status = %d", response.StatusCode)
	}
	var saved domain.Host
	if err := json.NewDecoder(response.Body).Decode(&saved); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	stored, err := svc.GetHost(ctx, rootHost.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.AgentRootEnabled || !stored.AgentRootEnabled {
		t.Fatalf("root setting was not persisted: response=%v stored=%v", saved.AgentRootEnabled, stored.AgentRootEnabled)
	}
	response = putHostRootAccess(t, server, rootHost.ID, false)
	response.Body.Close()
	stored, err = svc.GetHost(ctx, rootHost.ID)
	if err != nil || stored.AgentRootEnabled {
		t.Fatalf("root setting was not disabled: host=%#v err=%v", stored, err)
	}
}

func putHostRootAccess(t *testing.T, server *httptest.Server, hostID string, enabled bool) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]any{"enabled": enabled})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPut, server.URL+"/api/v1/hosts/"+hostID+"/agent-root", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
