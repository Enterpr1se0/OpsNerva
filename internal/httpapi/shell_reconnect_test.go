package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/security"
	"github.com/Enterpr1se0/opsnerva/internal/service"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"
	"github.com/Enterpr1se0/opsnerva/internal/store"
	"golang.org/x/net/websocket"
)

type httpShellTransport struct {
	mu       sync.Mutex
	sessions []*httpShellSession
}

func (*httpShellTransport) Exec(context.Context, sshx.ConnectionSpec, domain.ExecRequest) (sshx.RawResult, error) {
	return sshx.RawResult{}, nil
}
func (*httpShellTransport) Probe(context.Context, sshx.ConnectionSpec) (sshx.HostInfo, error) {
	return sshx.HostInfo{Shell: "sh", ShellPath: "/bin/sh"}, nil
}
func (*httpShellTransport) ScanHostKey(context.Context, sshx.ConnectionSpec) (sshx.HostKey, error) {
	return sshx.HostKey{}, nil
}
func (*httpShellTransport) TrustHostKey(context.Context, sshx.ConnectionSpec, string) (sshx.HostKey, error) {
	return sshx.HostKey{}, nil
}
func (*httpShellTransport) StoredHostKey(domain.Host) (sshx.HostKey, bool) {
	return sshx.HostKey{}, false
}
func (transport *httpShellTransport) OpenShell(_ context.Context, _ sshx.ConnectionSpec, _ domain.ExecRequest, _, _ int, output func(string, []byte)) (sshx.ShellSession, error) {
	session := &httpShellSession{done: make(chan struct{})}
	transport.mu.Lock()
	transport.sessions = append(transport.sessions, session)
	transport.mu.Unlock()
	output("stdout", []byte("fixture@test:$ "))
	return session, nil
}

type httpShellSession struct {
	mu   sync.Mutex
	once sync.Once
	done chan struct{}
	exit sshx.ShellExit
}

func (*httpShellSession) Write(data []byte) (int, error) { return len(data), nil }
func (*httpShellSession) Resize(int, int) error          { return nil }
func (*httpShellSession) Interrupt() error               { return nil }
func (session *httpShellSession) Wait() sshx.ShellExit {
	<-session.done
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.exit
}
func (session *httpShellSession) Close() error {
	session.once.Do(func() { close(session.done) })
	return nil
}
func (session *httpShellSession) disconnect() {
	session.mu.Lock()
	session.exit = sshx.ShellExit{Err: context.DeadlineExceeded}
	session.mu.Unlock()
	session.once.Do(func() { close(session.done) })
}

func TestShellReconnectReceivesAuthoritativeState(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "shell-reconnect.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.Default()
	svc := service.New(st, nil, nil, nil, cfg.Limits, cfg)
	t.Cleanup(func() { _ = svc.Shutdown(context.Background()) })
	for _, status := range []string{"running", "completed"} {
		if err := st.CreateSSHShell(ctx, domain.SSHShell{ID: status, RunID: "run-" + status, Kind: "ssh", Surface: "agent", Status: status, StartedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(New(svc, nil, Options{}).Handler())
	defer server.Close()
	for _, test := range []struct{ id, eventType string }{{"running", "ready"}, {"completed", "ended"}, {"missing", "unavailable"}} {
		t.Run(test.id, func(t *testing.T) {
			connection, err := websocket.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/v1/ssh-shells/"+test.id+"/ws", "", server.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
			var event sshShellWebSocketEvent
			if err := websocket.JSON.Receive(connection, &event); err != nil {
				t.Fatal(err)
			}
			if event.Type != test.eventType {
				t.Fatalf("reconnect event = %#v, want %s", event, test.eventType)
			}
			if test.id != "missing" && (event.Shell == nil || event.Shell.Status != test.id) {
				t.Fatalf("reconnect state = %#v", event.Shell)
			}
		})
	}
}

func TestOperatorShellReconnectEndpointKeepsShellID(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dataDir, "operator-shell-reconnect.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	encryptor, err := security.NewEncryptor("", dataDir)
	if err != nil {
		t.Fatal(err)
	}
	transport := &httpShellTransport{}
	cfg := config.Default()
	svc := service.New(st, transport, encryptor, security.NewRedactor(), cfg.Limits, cfg)
	t.Cleanup(func() { _ = svc.Shutdown(context.Background()) })
	agentEnabled := true
	host, err := svc.SaveHost(ctx, domain.HostInput{
		Name: "shell fixture", Address: "127.0.0.2", Port: 22, User: "ops", AgentEnabled: &agentEnabled,
		AuthType: "password", Password: "fixture-password", SudoMode: "none",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New(svc, nil, Options{}).Handler())
	defer server.Close()

	body, _ := json.Marshal(map[string]string{"host_id": host.ID, "surface": domain.SSHShellSurfaceQuick})
	response, err := server.Client().Post(server.URL+"/api/v1/ssh-shells", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusCreated {
		response.Body.Close()
		t.Fatalf("start status = %d", response.StatusCode)
	}
	var started domain.SSHShell
	if err := json.NewDecoder(response.Body).Decode(&started); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	transport.mu.Lock()
	first := transport.sessions[0]
	transport.mu.Unlock()
	first.disconnect()

	deadline := time.Now().Add(time.Second)
	for {
		list, listErr := svc.ListSSHShells(ctx, "", true, "", "")
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(list.Shells) == 1 && list.Shells[0].Status == "failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("disconnected shell was not retained: %#v", list)
		}
		time.Sleep(10 * time.Millisecond)
	}

	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/ssh-shells/"+started.ID+"/reconnect", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err = server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		t.Fatalf("reconnect status = %d", response.StatusCode)
	}
	var reconnected domain.SSHShell
	if err := json.NewDecoder(response.Body).Decode(&reconnected); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if reconnected.ID != started.ID || reconnected.Status != "running" {
		t.Fatalf("endpoint replaced shell: before=%#v after=%#v", started, reconnected)
	}
	transport.mu.Lock()
	opened := len(transport.sessions)
	transport.mu.Unlock()
	if opened != 2 {
		t.Fatalf("transport generations = %d, want 2", opened)
	}

	request, _ = http.NewRequest(http.MethodPost, server.URL+"/api/v1/ssh-shells/"+started.ID+"/reconnect", nil)
	response, err = server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("running reconnect status = %d, want %d", response.StatusCode, http.StatusConflict)
	}
}
