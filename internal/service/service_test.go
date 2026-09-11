package service

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/security"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"
	"github.com/Enterpr1se0/opsnerva/internal/store"
	"github.com/Enterpr1se0/opsnerva/internal/transfer"
	"golang.org/x/crypto/ssh"
)

type fakeTransport struct {
	mu                    sync.Mutex
	calls                 []domain.ExecRequest
	hosts                 []domain.Host
	stdout                []byte
	stderr                []byte
	exitCode              int
	execErr               error
	execStarted           chan struct{}
	execRelease           <-chan struct{}
	execStartOnce         sync.Once
	tunnelOpenErrs        []error
	tunnelClients         []*fakeTunnelClient
	tunnelSpecs           []sshx.ConnectionSpec
	storedKeys            map[string]sshx.HostKey
	probeCalls            int
	probeErr              error
	connectionShells      []string
	shellConnectionShells []string
	shellSessions         []*fakeShellSession
	shellOpenErrs         []error
}

type fakeTunnelClient struct {
	closed chan struct{}
	once   sync.Once
}

func newFakeTunnelClient() *fakeTunnelClient {
	return &fakeTunnelClient{closed: make(chan struct{})}
}

func (client *fakeTunnelClient) Dial(_ string, address string) (net.Conn, error) {
	return net.DialTimeout("tcp", address, time.Second)
}

func (client *fakeTunnelClient) Listen(network, address string) (net.Listener, error) {
	return net.Listen(network, address)
}

func (client *fakeTunnelClient) Wait() error {
	<-client.closed
	return nil
}

func (client *fakeTunnelClient) Close() error {
	client.once.Do(func() { close(client.closed) })
	return nil
}

func (f *fakeTransport) OpenTunnel(_ context.Context, connection sshx.ConnectionSpec) (sshx.TunnelClient, error) {
	f.mu.Lock()
	if len(f.tunnelOpenErrs) > 0 {
		err := f.tunnelOpenErrs[0]
		f.tunnelOpenErrs = f.tunnelOpenErrs[1:]
		f.mu.Unlock()
		return nil, err
	}
	client := newFakeTunnelClient()
	f.tunnelClients = append(f.tunnelClients, client)
	f.tunnelSpecs = append(f.tunnelSpecs, connection)
	f.mu.Unlock()
	return client, nil
}

type fakeStreamChunk struct {
	stream string
	data   string
}

type streamingFakeTransport struct {
	*fakeTransport
	chunks []fakeStreamChunk
}

func (f *streamingFakeTransport) ExecStream(ctx context.Context, connection sshx.ConnectionSpec, req domain.ExecRequest, emit func(string, []byte)) (sshx.RawResult, error) {
	for _, chunk := range f.chunks {
		emit(chunk.stream, []byte(chunk.data))
	}
	return f.fakeTransport.Exec(ctx, connection, req)
}

func (f *fakeTransport) Exec(ctx context.Context, connection sshx.ConnectionSpec, req domain.ExecRequest) (sshx.RawResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.hosts = append(f.hosts, connection.Target)
	f.connectionShells = append(f.connectionShells, connection.ShellPath)
	stdout, stderr, exitCode, execErr := f.stdout, f.stderr, f.exitCode, f.execErr
	started, release := f.execStarted, f.execRelease
	f.mu.Unlock()
	if started != nil {
		f.execStartOnce.Do(func() { close(started) })
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return sshx.RawResult{Duration: time.Millisecond}, ctx.Err()
		}
	}
	if stdout == nil {
		stdout = []byte("password=secret-value\nok\n")
	}
	return sshx.RawResult{ExitCode: exitCode, Stdout: stdout, Stderr: stderr, Duration: time.Millisecond}, execErr
}

func (f *fakeTransport) TransferFile(_ context.Context, source, destination sshx.ConnectionSpec, req domain.ExecRequest, progress transfer.Reporter) (sshx.RawResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.hosts = append(f.hosts, source.Target, destination.Target)
	f.mu.Unlock()
	if progress != nil {
		progress(transfer.Progress{Transferred: 12, Total: 12})
	}
	return sshx.RawResult{ExitCode: 0, Stdout: []byte(`{"bytes":12,"sha256":"transfer-digest"}` + "\n"), Duration: time.Millisecond}, nil
}

func (f *fakeTransport) UploadWorkspaceFile(ctx context.Context, connection sshx.ConnectionSpec, req domain.ExecRequest, progress transfer.Reporter) (sshx.RawResult, error) {
	if progress != nil {
		progress(transfer.Progress{Transferred: 12, Total: 12})
	}
	return f.Exec(ctx, connection, req)
}

func testSSHPrivateKey(t *testing.T) []byte {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(privateKey, "service-test")
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(block)
}

func (f *fakeTransport) Probe(context.Context, sshx.ConnectionSpec) (sshx.HostInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probeCalls++
	return sshx.HostInfo{Hostname: "fixture", Shell: "bash", ShellPath: "/usr/bin/bash"}, f.probeErr
}

func (f *fakeTransport) ScanHostKey(context.Context, sshx.ConnectionSpec) (sshx.HostKey, error) {
	return sshx.HostKey{Fingerprint: "SHA256:test"}, nil
}

func (f *fakeTransport) TrustHostKey(context.Context, sshx.ConnectionSpec, string) (sshx.HostKey, error) {
	return sshx.HostKey{Fingerprint: "SHA256:test"}, nil
}

func (f *fakeTransport) StoredHostKey(host domain.Host) (sshx.HostKey, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key, ok := f.storedKeys[host.ID]
	return key, ok
}

func newTestService(t *testing.T) (*Service, *fakeTransport, domain.Host) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	encryptor, err := security.NewEncryptor("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	transport := &fakeTransport{}
	limits := config.Default().Limits
	svc := New(st, transport, encryptor, security.NewRedactor(), limits)
	svc.modelMetadata.url = ""
	t.Cleanup(func() { svc.explainWG.Wait() })
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := svc.Shutdown(shutdownCtx); err != nil {
			t.Errorf("shutdown service: %v", err)
		}
	})
	host, err := svc.AddHost(ctx, domain.Host{Name: "fixture", Address: "127.0.0.1", Port: 22, User: "test", AgentEnabled: true}, "test")
	if err != nil {
		t.Fatal(err)
	}
	return svc, transport, host
}
