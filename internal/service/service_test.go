package service

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
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

type fakeCommandExplainer struct {
	mu     sync.Mutex
	review domain.CommandReview
	err    error
	inputs []domain.CommandReviewInput
}

type fakeAutomaticApprovalReviewer struct {
	mu     sync.Mutex
	review domain.CommandReview
	err    error
	inputs []domain.AutomaticApprovalInput
}

func (f *fakeAutomaticApprovalReviewer) Review(_ context.Context, input domain.AutomaticApprovalInput) (domain.CommandReview, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inputs = append(f.inputs, input)
	return f.review, f.err
}

func (f *fakeAutomaticApprovalReviewer) Inputs() []domain.AutomaticApprovalInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.AutomaticApprovalInput(nil), f.inputs...)
}

func (f *fakeCommandExplainer) Review(_ context.Context, input domain.CommandReviewInput) (domain.CommandReview, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inputs = append(f.inputs, input)
	return f.review, f.err
}

func (f *fakeCommandExplainer) Inputs() []domain.CommandReviewInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.CommandReviewInput(nil), f.inputs...)
}

type blockingCommandExplainer struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	review  domain.CommandReview
}

func (r *blockingCommandExplainer) Review(ctx context.Context, _ domain.CommandReviewInput) (domain.CommandReview, error) {
	r.once.Do(func() { close(r.started) })
	select {
	case <-r.release:
		return r.review, nil
	case <-ctx.Done():
		return domain.CommandReview{}, ctx.Err()
	}
}

type trackingCommandExplainer struct {
	started chan struct{}
	mu      sync.Mutex
	active  int
	maximum int
}

func (r *trackingCommandExplainer) Review(ctx context.Context, _ domain.CommandReviewInput) (domain.CommandReview, error) {
	r.mu.Lock()
	r.active++
	if r.active > r.maximum {
		r.maximum = r.active
	}
	r.mu.Unlock()
	r.started <- struct{}{}
	defer func() {
		r.mu.Lock()
		r.active--
		r.mu.Unlock()
	}()
	<-ctx.Done()
	return domain.CommandReview{}, ctx.Err()
}

func (r *trackingCommandExplainer) maxActive() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.maximum
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

func TestExecutionPreservesCompleteOutput(t *testing.T) {
	svc, transport, host := newTestService(t)
	saveApprovalMode(t, svc, domain.ApprovalModeFullAccess)
	wantStdout := strings.Repeat("stdout-data-", 30_000) + "stdout-end"
	wantStderr := strings.Repeat("stderr-data-", 12_000) + "stderr-end"
	transport.mu.Lock()
	transport.stdout = []byte(wantStdout)
	transport.stderr = []byte(wantStderr)
	transport.mu.Unlock()

	result, err := svc.Submit(context.Background(), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "uname", Args: []string{"-a"}, Reason: "verify complete output capture",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != wantStdout || result.Stderr != wantStderr {
		t.Fatalf("tool output was not preserved: stdout=%d/%d stderr=%d/%d", len(result.Stdout), len(wantStdout), len(result.Stderr), len(wantStderr))
	}
	stored, err := svc.store.GetRun(context.Background(), result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.StdoutRedacted != wantStdout || stored.StderrRedacted != wantStderr {
		t.Fatalf("persisted output was not preserved: stdout=%d/%d stderr=%d/%d", len(stored.StdoutRedacted), len(wantStdout), len(stored.StderrRedacted), len(wantStderr))
	}
}

func TestExecutionWithUsableOutputAndNonzeroExitIsPartial(t *testing.T) {
	svc, transport, host := newTestService(t)
	transport.mu.Lock()
	transport.stdout = []byte("matched configuration\n")
	transport.stderr = nil
	transport.exitCode = 2
	transport.mu.Unlock()

	result, err := svc.Submit(context.Background(), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "uname", Args: []string{"-a"}, Reason: "inspect system identity",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "partial" || result.ExitCode != 2 || result.Stdout != "matched configuration\n" {
		t.Fatalf("nonzero result with usable output was not classified as partial: %#v", result)
	}
	stored, err := svc.store.GetRun(context.Background(), result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "partial" || stored.Error != "remote command exited with code 2" {
		t.Fatalf("partial run was not persisted accurately: %#v", stored)
	}
}

func TestExecutionWithNonzeroExitAndNoOutputRemainsFailed(t *testing.T) {
	svc, transport, host := newTestService(t)
	transport.mu.Lock()
	transport.stdout = []byte{}
	transport.stderr = []byte("not found\n")
	transport.exitCode = 1
	transport.mu.Unlock()

	result, err := svc.Submit(context.Background(), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "uname", Args: []string{"-a"}, Reason: "inspect system identity",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "failed" || result.ExitCode != 1 {
		t.Fatalf("nonzero result without usable output did not remain failed: %#v", result)
	}
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

func TestElevatedExecutionUsesManagedSecretAfterApproval(t *testing.T) {
	svc, transport, _ := newTestService(t)
	agentRootEnabled := true
	host, err := svc.SaveHost(context.Background(), domain.HostInput{
		Name: "sudo-host", Address: "192.0.2.11", Port: 22, User: "ops", AuthType: "password",
		Password: "ssh-secret", SudoMode: "password", SudoPassword: "sudo-secret",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	host, err = svc.SetHostAgentRootEnabled(context.Background(), host.ID, agentRootEnabled, "test")
	if err != nil {
		t.Fatal(err)
	}
	callArguments := `{"host_id":"sudo-host","program":"id","elevated":true,"reason":"verify managed root access"}`
	ctx := WithExecutionOwner(context.Background(), "call-elevated", "ssh_exec", callArguments)
	result, err := svc.Submit(ctx, domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "id", Elevated: true, Reason: "verify managed root access",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "approval_required" {
		t.Fatalf("elevated request bypassed approval: %#v", result)
	}
	stored, err := svc.store.GetRun(context.Background(), result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ToolName != "ssh_exec" || stored.ToolArgumentsJSON != callArguments || !strings.Contains(stored.RequestJSON, `"elevated":true`) {
		t.Fatalf("elevated Tool Call was not preserved in history: %#v", stored)
	}
	if _, err := svc.Approve(context.Background(), result.ApprovalID, "root access reviewed", "operator"); err != nil {
		t.Fatal(err)
	}
	if len(transport.hosts) != 1 || transport.hosts[0].Password != "ssh-secret" || transport.hosts[0].SudoPassword != "sudo-secret" {
		t.Fatalf("transport did not receive transient managed credentials: %#v", transport.hosts)
	}
}

func TestDirectSudoIsRejectedInProgramAndScriptModes(t *testing.T) {
	svc, _, host := newTestService(t)
	requests := []domain.ExecRequest{
		{HostID: host.ID, Mode: domain.ExecProgram, Program: "sudo", Args: []string{"id"}, Reason: "bad direct sudo"},
		{HostID: host.ID, Mode: domain.ExecScript, Script: "echo preparing\nsudo systemctl restart api", Reason: "bad script sudo"},
	}
	for _, req := range requests {
		if _, err := svc.Submit(context.Background(), req, "test"); err == nil || !strings.Contains(err.Error(), "elevated=true") {
			t.Fatalf("direct sudo was not rejected: %v", err)
		}
	}
}

func TestInteractiveCommandsAndPackagePromptsAreRejected(t *testing.T) {
	svc, transport, host := newTestService(t)
	routingRequests := []struct {
		request       domain.ExecRequest
		suggestedTool string
	}{
		{domain.ExecRequest{HostID: host.ID, Mode: domain.ExecProgram, Program: "bash", Reason: "open shell"}, "ssh_shell"},
		{domain.ExecRequest{HostID: host.ID, Mode: domain.ExecProgram, Program: "bash", Args: []string{"-lc", "uname -a | head -1"}, Reason: "inspect kernel"}, "ssh_run_script"},
		{domain.ExecRequest{HostID: host.ID, Mode: domain.ExecProgram, Program: "top", Reason: "inspect processes"}, "ssh_exec"},
	}
	for _, testCase := range routingRequests {
		_, err := svc.Submit(context.Background(), testCase.request, "test")
		var selectionErr *ExecutionToolSelectionError
		if !errors.As(err, &selectionErr) || selectionErr.SuggestedTool != testCase.suggestedTool || selectionErr.NextAction == "" || len(selectionErr.Example) == 0 {
			t.Fatalf("interactive request did not return actionable routing details: request=%#v err=%#v", testCase.request, err)
		}
	}
	requests := []domain.ExecRequest{
		{HostID: host.ID, Mode: domain.ExecProgram, Program: "pacman", Args: []string{"-S", "nginx"}, Reason: "install nginx"},
		{HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"edit", "nginx"}, Reason: "edit unit"},
	}
	for _, request := range requests {
		if _, err := svc.Submit(context.Background(), request, "test"); err == nil {
			t.Fatalf("interactive request was accepted: %#v", request)
		}
	}
	if len(transport.calls) != 0 {
		t.Fatal("rejected interactive commands reached transport")
	}
}

func TestApprovedRequestRejectsChangedSSHConnection(t *testing.T) {
	svc, transport, host := newTestService(t)
	result, err := svc.Submit(context.Background(), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "example.service"},
		Reason: "restart the example service",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "approval_required" {
		t.Fatalf("expected an approval, got %#v", result)
	}
	_, err = svc.SaveHost(context.Background(), domain.HostInput{
		ID: host.ID, Name: host.Name, Address: "127.0.0.2", Port: host.Port, User: host.User,
		AuthType: host.AuthType, SudoMode: host.SudoMode,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	approved, err := svc.Approve(context.Background(), result.ApprovalID, "connection reviewed", "operator")
	if err == nil || !strings.Contains(err.Error(), "changed after submission") {
		t.Fatalf("changed SSH connection was executed: result=%#v error=%v", approved, err)
	}
	if len(transport.calls) != 0 {
		t.Fatal("changed SSH connection reached the transport")
	}
}

func TestProxyJumpChainResolutionAndCycleDetection(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	outer, err := svc.SaveHost(ctx, domain.HostInput{
		Name: "outer-jump", Address: "192.0.2.20", Port: 22, User: "ops",
		AuthType: "agent", SudoMode: "none",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	inner, err := svc.SaveHost(ctx, domain.HostInput{
		Name: "inner-jump", Address: "192.0.2.21", Port: 22, User: "ops",
		AuthType: "agent", ProxyJumpHostID: outer.ID, SudoMode: "none",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	target, err := svc.SaveHost(ctx, domain.HostInput{
		Name: "jump-target", Address: "192.0.2.22", Port: 22, User: "ops",
		AuthType: "agent", ProxyJumpHostID: inner.ID, SudoMode: "none",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	connection, digest, err := svc.resolveSSHConnection(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if digest == "" || len(connection.Jumps) != 2 || connection.Jumps[0].ID != outer.ID || connection.Jumps[1].ID != inner.ID {
		t.Fatalf("unexpected resolved jump chain: %#v digest=%q", connection, digest)
	}
	outer, err = svc.SaveHost(ctx, domain.HostInput{
		ID: outer.ID, Name: outer.Name, Address: outer.Address, Port: outer.Port, User: outer.User,
		AuthType: "agent", ProxyJumpHostID: target.ID, SudoMode: "none",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.resolveSSHConnection(ctx, target); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("ProxyJump cycle was not rejected: %v", err)
	}
}

func TestWaitTaskBlocksUntilNewOutput(t *testing.T) {
	svc, _, host := newTestService(t)
	state := &taskState{
		task:   domain.Task{ID: "task-wait", HostID: host.ID, Status: "running", StartedAt: time.Now().UTC()},
		result: domain.ExecResult{Status: "running", Stdout: "old"},
	}
	svc.taskMu.Lock()
	svc.tasks[state.task.ID] = state
	svc.taskMu.Unlock()
	t.Cleanup(func() {
		svc.taskMu.Lock()
		delete(svc.tasks, state.task.ID)
		svc.taskMu.Unlock()
	})
	go func() {
		time.Sleep(120 * time.Millisecond)
		svc.taskMu.Lock()
		state.result.Stdout = "old-new"
		notifyTaskWaitersLocked(state)
		svc.taskMu.Unlock()
	}()
	started := time.Now()
	task, result, _, waitDeadlineReached, err := svc.WaitTask(context.Background(), state.task.ID, len("old"), 0, time.Second, "output")
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != state.task.ID || result.Stdout != "old-new" || waitDeadlineReached || time.Since(started) < 100*time.Millisecond {
		t.Fatalf("task wait returned early or lost output: task=%#v result=%#v elapsed=%s", task, result, time.Since(started))
	}
}

func TestWaitTaskDeadlineDoesNotChangeRunningTask(t *testing.T) {
	svc, _, host := newTestService(t)
	state := &taskState{
		task:   domain.Task{ID: "task-wait-deadline", HostID: host.ID, Status: "running", StartedAt: time.Now().UTC()},
		result: domain.ExecResult{Status: "running", Stdout: "unchanged"},
	}
	svc.taskMu.Lock()
	svc.tasks[state.task.ID] = state
	svc.taskMu.Unlock()
	t.Cleanup(func() {
		svc.taskMu.Lock()
		delete(svc.tasks, state.task.ID)
		svc.taskMu.Unlock()
	})
	task, result, _, waitDeadlineReached, err := svc.WaitTask(context.Background(), state.task.ID, len("unchanged"), 0, 30*time.Millisecond, "terminal")
	if err != nil {
		t.Fatal(err)
	}
	if !waitDeadlineReached || task.Status != "running" || result.Status != "running" || result.Stdout != "unchanged" {
		t.Fatalf("task wait deadline mutated the task: task=%#v result=%#v deadline=%t", task, result, waitDeadlineReached)
	}
}

func TestNormalizeRequestUsesSeparateSyncAndBackgroundTimeoutDefaults(t *testing.T) {
	limits := config.Limits{SyncTimeoutSeconds: 60, MaxTimeoutSeconds: 600}
	synchronous := domain.ExecRequest{Mode: domain.ExecProgram}
	normalizeRequest(&synchronous, limits)
	if synchronous.TimeoutSeconds != 60 {
		t.Fatalf("synchronous default timeout = %d, want 60", synchronous.TimeoutSeconds)
	}
	background := domain.ExecRequest{Mode: domain.ExecProgram, Background: true}
	normalizeRequest(&background, limits)
	if background.TimeoutSeconds != 600 {
		t.Fatalf("background default timeout = %d, want 600", background.TimeoutSeconds)
	}
	explicit := domain.ExecRequest{Mode: domain.ExecProgram, Background: true, TimeoutSeconds: 90}
	normalizeRequest(&explicit, limits)
	if explicit.TimeoutSeconds != 90 {
		t.Fatalf("explicit background timeout = %d, want 90", explicit.TimeoutSeconds)
	}
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

func assertNoPendingApprovals(t *testing.T, svc *Service) {
	t.Helper()
	approvals, err := svc.ListApprovals(context.Background(), "pending", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(approvals) != 0 {
		t.Fatalf("operator connection unexpectedly created approvals: %#v", approvals)
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

func TestAgentHostAvailabilityFiltersAndEnforcesAccess(t *testing.T) {
	svc, transport, enabled := newTestService(t)
	ctx := context.Background()
	disabled := false
	hidden, err := svc.SaveHost(ctx, domain.HostInput{
		Name: "manual-only", Address: "192.0.2.90", Port: 22, User: "ops",
		AgentEnabled: &disabled, AuthType: "agent", SudoMode: "none",
	}, "operator")
	if err != nil {
		t.Fatal(err)
	}
	hidden, err = svc.SaveHost(ctx, domain.HostInput{
		ID: hidden.ID, Name: hidden.Name, Address: hidden.Address, Port: hidden.Port, User: hidden.User,
		AuthType: hidden.AuthType, SudoMode: hidden.SudoMode,
	}, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if hidden.AgentEnabled {
		t.Fatal("editing an Agent-disabled host without changing the switch re-enabled it")
	}
	target, err := svc.SaveHost(ctx, domain.HostInput{
		Name: "hidden-jump-target", Address: "192.0.2.91", Port: 22, User: "ops",
		ProxyJumpHostID: hidden.ID, AuthType: "agent", SudoMode: "none",
	}, "operator")
	if err != nil {
		t.Fatal(err)
	}

	capabilities, err := svc.ListHostCapabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(capabilities) != 1 || capabilities[0].ID != enabled.ID {
		t.Fatalf("Agent host catalog included unavailable hosts: %#v", capabilities)
	}
	if capabilities[0].Root || capabilities[0].Shell != "bash" {
		t.Fatalf("Agent host catalog returned incorrect effective capabilities: %#v", capabilities[0])
	}
	for _, hostID := range []string{hidden.ID, target.ID} {
		_, err := svc.Submit(ctx, domain.ExecRequest{
			HostID: hostID, Mode: domain.ExecProgram, Program: "uname", Reason: "verify Agent host access",
		}, "eino-agent")
		if !errors.Is(err, ErrAgentHostAccessDenied) {
			t.Fatalf("Agent request for unavailable host %q was not rejected: %v", hostID, err)
		}
	}
	if _, err := svc.ProbeHost(ctx, hidden.ID, "eino-agent"); !errors.Is(err, ErrAgentHostAccessDenied) {
		t.Fatalf("Agent probe for unavailable host was not rejected: %v", err)
	}

	result, err := svc.Submit(ctx, domain.ExecRequest{
		HostID: hidden.ID, Mode: domain.ExecProgram, Program: "uname", Reason: "verify manual host access",
	}, "operator")
	if err != nil || result.Status != "completed" {
		t.Fatalf("manual operation on Agent-disabled host failed: result=%#v err=%v", result, err)
	}
	if len(transport.calls) != 1 {
		t.Fatalf("rejected Agent requests reached SSH transport: %d calls", len(transport.calls))
	}
}

func TestAgentHostCatalogKeepsProbeFailureExplicit(t *testing.T) {
	svc, transport, enabled := newTestService(t)
	transport.probeErr = errors.New("host unreachable")

	capabilities, err := svc.ListHostCapabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(capabilities) != 1 || capabilities[0].ID != enabled.ID || capabilities[0].Shell != "unknown" {
		t.Fatalf("Agent host catalog did not preserve a probe failure explicitly: %#v", capabilities)
	}
	transport.probeErr = nil
	capabilities, err = svc.ListHostCapabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if capabilities[0].Shell != "bash" || transport.probeCalls != 2 {
		t.Fatalf("successful retry did not refresh shell capability: capability=%#v probes=%d", capabilities[0], transport.probeCalls)
	}
	if _, err := svc.ListHostCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	if transport.probeCalls != 2 {
		t.Fatalf("successful shell detection was not reused: probes=%d", transport.probeCalls)
	}
}

func TestToolInputValidationIsTyped(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "execution contract",
			err: validateExecutionRequest(domain.Host{}, domain.ExecRequest{
				Mode: domain.ExecScript, Script: "sudo id", Reason: "test direct sudo rejection",
			}),
		},
		{name: "remote path", err: validateRemoteFilePath("relative.txt")},
		{name: "file search", err: validateFileSearchInput("needle", "contains", 0)},
		{
			name: "host transfer",
			err: validateSSHFileTransferRequest(domain.ExecRequest{
				HostID: "destination", SourceHostID: "source", SourcePath: "relative.txt",
			}),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var validation *InputValidationError
			if !errors.As(test.err, &validation) {
				t.Fatalf("input error is not typed validation: %T %v", test.err, test.err)
			}
		})
	}
}

func waitForApproval(t *testing.T, svc *Service, approvalID string, ready func(domain.Approval) bool) domain.Approval {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		approvals, err := svc.ListApprovals(context.Background(), "", 200)
		if err != nil {
			t.Fatal(err)
		}
		for _, approval := range approvals {
			if approval.ID == approvalID && ready(approval) {
				return approval
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for approval %s", approvalID)
	return domain.Approval{}
}

func saveApprovalMode(t *testing.T, svc *Service, mode string) {
	t.Helper()
	explanationsEnabled := false
	if _, err := svc.SaveSystemSettings(context.Background(), domain.SystemSettingsInput{
		AgentMaxIterations:          domain.DefaultAgentMaxIterations,
		ApprovalMode:                &mode,
		ApprovalExplanationsEnabled: &explanationsEnabled,
	}, "test"); err != nil {
		t.Fatal(err)
	}
}

func TestSystemSettingsValidatePersistAndReturnDefault(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	settings, err := svc.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settings.AgentMaxIterations != domain.DefaultAgentMaxIterations || settings.ApprovalMode != domain.ApprovalModeManual || !settings.ApprovalExplanationsEnabled || settings.SubagentModelProviderID != "" || settings.AutomaticApprovalModelProviderID != "" || settings.SubagentTimeoutSeconds != domain.DefaultSubagentTimeoutSeconds || !settings.ContextCompressionEnabled || settings.ContextCompressionPercent != domain.DefaultContextCompressionPercent || settings.WorkspaceShellMode != domain.DefaultWorkspaceShellMode(runtime.GOOS) {
		t.Fatalf("unexpected default max iterations: %#v", settings)
	}
	if strings.Join(settings.ChatImageAllowedTypes, ",") != strings.Join(domain.DefaultChatImageAllowedTypes, ",") {
		t.Fatalf("unexpected default chat image formats: %#v", settings.ChatImageAllowedTypes)
	}
	if settings.SystemPrompt != domain.DefaultSystemPrompt || settings.DefaultSystemPrompt != domain.DefaultSystemPrompt {
		t.Fatalf("unexpected default system prompt: %#v", settings)
	}
	if _, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{AgentMaxIterations: 4}, "test"); err == nil {
		t.Fatal("expected lower-bound validation error")
	}
	if _, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{AgentMaxIterations: domain.MaxAgentMaxIterations + 1}, "test"); err == nil {
		t.Fatal("expected upper-bound validation error")
	}
	tooShort := domain.MinSubagentTimeoutSeconds - 1
	if _, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{AgentMaxIterations: 20, SubagentTimeoutSeconds: &tooShort}, "test"); err == nil {
		t.Fatal("expected subagent timeout validation error")
	}
	missingProvider := "model_missing"
	if _, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{AgentMaxIterations: 20, SubagentModelProviderID: &missingProvider}, "test"); err == nil {
		t.Fatal("expected missing subagent provider validation error")
	}
	if _, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{AgentMaxIterations: 20, AutomaticApprovalModelProviderID: &missingProvider}, "test"); err == nil {
		t.Fatal("expected missing Auto approval provider validation error")
	}
	invalidCompressionPercent := domain.MinContextCompressionPercent - 1
	if _, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{AgentMaxIterations: 20, ContextCompressionPercent: &invalidCompressionPercent}, "test"); err == nil {
		t.Fatal("expected context compression threshold validation error")
	}
	provider, err := svc.SaveModelProvider(ctx, domain.ModelProviderInput{
		Name: "subagent", Kind: "ollama", BaseURL: "http://127.0.0.1:11434/v1", Model: "small-model",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	automaticProvider, err := svc.SaveModelProvider(ctx, domain.ModelProviderInput{
		Name: "auto-approval", Kind: "ollama", BaseURL: "http://127.0.0.1:11434/v1", Model: "approval-model",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	explanationsEnabled := false
	timeoutSeconds := 45
	hostShell := domain.WorkspaceShellModeHost
	imageTypes := []string{"image/png", "image/webp"}
	systemPrompt := "You are my personal operations agent."
	approvalMode := domain.ApprovalModeAuto
	compressionEnabled := false
	compressionPercent := 80
	saved, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{
		AgentMaxIterations: 30, ApprovalExplanationsEnabled: &explanationsEnabled,
		ApprovalMode: &approvalMode,
		SystemPrompt: &systemPrompt, SubagentModelProviderID: &provider.ID, AutomaticApprovalModelProviderID: &automaticProvider.ID, SubagentTimeoutSeconds: &timeoutSeconds,
		ChatImageAllowedTypes: imageTypes, WorkspaceShellMode: &hostShell,
		ContextCompressionEnabled: &compressionEnabled, ContextCompressionPercent: &compressionPercent,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if saved.AgentMaxIterations != 30 || saved.SystemPrompt != systemPrompt || saved.ApprovalMode != domain.ApprovalModeAuto || saved.ApprovalExplanationsEnabled || saved.SubagentModelProviderID != provider.ID || saved.AutomaticApprovalModelProviderID != automaticProvider.ID || saved.SubagentTimeoutSeconds != timeoutSeconds || saved.ContextCompressionEnabled || saved.ContextCompressionPercent != compressionPercent || strings.Join(saved.ChatImageAllowedTypes, ",") != strings.Join(imageTypes, ",") || saved.WorkspaceShellMode != domain.WorkspaceShellModeHost || saved.UpdatedAt.IsZero() {
		t.Fatalf("unexpected saved settings: %#v", saved)
	}
	reloaded, err := svc.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.AgentMaxIterations != 30 || reloaded.SystemPrompt != systemPrompt || reloaded.ApprovalMode != domain.ApprovalModeAuto || reloaded.ApprovalExplanationsEnabled || reloaded.SubagentModelProviderID != provider.ID || reloaded.AutomaticApprovalModelProviderID != automaticProvider.ID || reloaded.SubagentTimeoutSeconds != timeoutSeconds || reloaded.ContextCompressionEnabled || reloaded.ContextCompressionPercent != compressionPercent || strings.Join(reloaded.ChatImageAllowedTypes, ",") != strings.Join(imageTypes, ",") || reloaded.WorkspaceShellMode != domain.WorkspaceShellModeHost {
		t.Fatalf("system settings were not persisted: %#v", reloaded)
	}
	if _, err := svc.DeleteModelProvider(ctx, provider.ID, "test"); !errors.Is(err, ErrModelProviderInUse) || !strings.Contains(err.Error(), "selected for the approval Agent") {
		t.Fatalf("selected subagent provider deletion was not blocked: %v", err)
	}
	if _, err := svc.DeleteModelProvider(ctx, automaticProvider.ID, "test"); !errors.Is(err, ErrModelProviderInUse) || !strings.Contains(err.Error(), "selected for the Auto approval Agent") {
		t.Fatalf("selected Auto approval provider deletion was not blocked: %v", err)
	}
	invalidMode := "automatic"
	if _, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{AgentMaxIterations: 30, ApprovalMode: &invalidMode}, "test"); err == nil {
		t.Fatal("invalid approval mode was accepted")
	}
	if _, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{AgentMaxIterations: 30, WorkspaceShellMode: &invalidMode}, "test"); err == nil {
		t.Fatal("invalid workspace shell mode was accepted")
	}
	if _, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{AgentMaxIterations: 30, ChatImageAllowedTypes: []string{}}, "test"); err == nil {
		t.Fatal("empty chat image format selection was accepted")
	}
	if _, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{AgentMaxIterations: 30, ChatImageAllowedTypes: []string{"image/svg+xml"}}, "test"); err == nil {
		t.Fatal("unsupported chat image format was accepted")
	}
}

func TestMCPHTTPSettingsGenerateRotateAndAuthorizeToken(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	enabled := true
	started, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{
		AgentMaxIterations: domain.DefaultAgentMaxIterations,
		MCPHTTPEnabled:     &enabled,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !started.MCPHTTPEnabled || started.MCPHTTPToken == "" || !started.MCPHTTPTokenConfigured || started.MCPHTTPTokenHash == "" {
		t.Fatalf("MCP HTTP start did not generate a token: %#v", started)
	}
	firstToken := started.MCPHTTPToken
	reloaded, err := svc.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.MCPHTTPToken != "" || !reloaded.MCPHTTPTokenConfigured {
		t.Fatalf("stored MCP HTTP token was exposed or lost: %#v", reloaded)
	}
	accessEnabled, authorized, err := svc.MCPHTTPAccess(ctx, firstToken)
	if err != nil || !accessEnabled || !authorized {
		t.Fatalf("generated MCP HTTP token was not authorized: enabled=%v authorized=%v err=%v", accessEnabled, authorized, err)
	}
	if _, authorized, err := svc.MCPHTTPAccess(ctx, "wrong-token"); err != nil || authorized {
		t.Fatalf("invalid MCP HTTP token was accepted: authorized=%v err=%v", authorized, err)
	}
	rotated, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{
		AgentMaxIterations: domain.DefaultAgentMaxIterations,
		MCPHTTPEnabled:     &enabled,
		RotateMCPHTTPToken: true,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.MCPHTTPToken == "" || rotated.MCPHTTPToken == firstToken {
		t.Fatal("MCP HTTP token was not rotated")
	}
	if _, authorized, err := svc.MCPHTTPAccess(ctx, firstToken); err != nil || authorized {
		t.Fatalf("rotated MCP HTTP token remained valid: authorized=%v err=%v", authorized, err)
	}
	disabled := false
	if _, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{
		AgentMaxIterations: domain.DefaultAgentMaxIterations,
		MCPHTTPEnabled:     &disabled,
	}, "test"); err != nil {
		t.Fatal(err)
	}
	accessEnabled, authorized, err = svc.MCPHTTPAccess(ctx, rotated.MCPHTTPToken)
	if err != nil || accessEnabled || authorized {
		t.Fatalf("disabled MCP HTTP endpoint remained accessible: enabled=%v authorized=%v err=%v", accessEnabled, authorized, err)
	}
}

func TestManualApprovalModeKeepsHumanApproval(t *testing.T) {
	svc, transport, host := newTestService(t)
	automatic := &fakeAutomaticApprovalReviewer{review: domain.CommandReview{
		Status: "completed", Decision: domain.ApprovalAgentAllow, Reason: "范围明确",
		Explanation: &domain.CommandExplanation{Summary: "重启 demo", Mechanism: "systemd 重启单元"}, ReviewedAt: time.Now().UTC(),
	}}
	svc.SetAutomaticApprovalReviewer(automatic)
	svc.SetApprovalReviewer(&fakeCommandExplainer{review: domain.CommandReview{
		Status: "completed", Decision: domain.ApprovalAgentAllow, Reason: "范围明确", ReviewedAt: time.Now().UTC(),
	}})
	result, err := svc.Submit(context.Background(), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"}, Reason: "recover demo",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "approval_required" || result.AutoApproved || result.ApprovalID == "" || len(transport.calls) != 0 || len(automatic.Inputs()) != 0 {
		t.Fatalf("manual mode bypassed human approval: result=%#v calls=%d", result, len(transport.calls))
	}
}

func TestAutoApprovalModeExecutesAllowedOperation(t *testing.T) {
	svc, transport, host := newTestService(t)
	saveApprovalMode(t, svc, domain.ApprovalModeAuto)
	reviewer := &fakeAutomaticApprovalReviewer{review: domain.CommandReview{
		Status: "completed", Decision: domain.ApprovalAgentAllow, Reason: "目标与范围明确",
		Explanation: &domain.CommandExplanation{Summary: "重启 demo", Mechanism: "systemd 重启单元"}, ReviewedAt: time.Now().UTC(),
	}}
	svc.SetAutomaticApprovalReviewer(reviewer)
	ctx := WithApprovalUserRequest(context.Background(), "重启 demo 服务以恢复运行")
	result, err := svc.Submit(ctx, domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"}, Reason: "recover demo",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" || !result.AutoApproved || len(transport.calls) != 1 {
		t.Fatalf("approval Agent did not allow execution: result=%#v calls=%d", result, len(transport.calls))
	}
	run, err := svc.store.GetRun(context.Background(), result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.AIReview == nil || run.AIReview.Kind != domain.CommandReviewKindAutomaticApproval || run.AIReview.Decision != domain.ApprovalAgentAllow {
		t.Fatalf("approval Agent decision was not persisted: %#v", run.AIReview)
	}
	inputs := reviewer.Inputs()
	if len(inputs) != 1 || inputs[0].UserRequest != "重启 demo 服务以恢复运行" {
		t.Fatalf("approval Agent did not receive the current user request: %#v", inputs)
	}
}

func TestAutoApprovalModeRejectsOperation(t *testing.T) {
	svc, transport, host := newTestService(t)
	saveApprovalMode(t, svc, domain.ApprovalModeAuto)
	svc.SetAutomaticApprovalReviewer(&fakeAutomaticApprovalReviewer{review: domain.CommandReview{
		Status: "completed", Decision: domain.ApprovalAgentReject, Reason: "请求范围过大",
		Explanation: &domain.CommandExplanation{Summary: "重启 demo", Mechanism: "systemd 重启单元"}, ReviewedAt: time.Now().UTC(),
	}})
	result, err := svc.Submit(WithApprovalUserRequest(context.Background(), "只查看 demo 状态"), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"}, Reason: "recover demo",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "rejected" || !strings.Contains(result.Stderr, "请求范围过大") || len(transport.calls) != 0 {
		t.Fatalf("approval Agent rejection was not enforced: result=%#v calls=%d", result, len(transport.calls))
	}
	approvals, err := svc.ListApprovals(context.Background(), "pending", 10)
	if err != nil || len(approvals) != 0 {
		t.Fatalf("automatic rejection created a human approval: %#v err=%v", approvals, err)
	}
}

func TestAutoApprovalModeFallsBackToHuman(t *testing.T) {
	svc, transport, host := newTestService(t)
	saveApprovalMode(t, svc, domain.ApprovalModeAuto)
	result, err := svc.Submit(WithApprovalUserRequest(context.Background(), "重启 demo 服务"), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"}, Reason: "recover demo",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "approval_required" || result.ApprovalID == "" || len(transport.calls) != 0 {
		t.Fatalf("unavailable approval Agent did not fall back to a human: result=%#v calls=%d", result, len(transport.calls))
	}
	approval := waitForApproval(t, svc, result.ApprovalID, func(value domain.Approval) bool {
		return value.AIReview != nil
	})
	if approval.AIReview.Status != "unavailable" {
		t.Fatalf("fallback reason was not persisted: %#v", approval.AIReview)
	}
}

func TestAutoApprovalModeDoesNotReuseExplanationAgent(t *testing.T) {
	svc, transport, host := newTestService(t)
	saveApprovalMode(t, svc, domain.ApprovalModeAuto)
	explainer := &fakeCommandExplainer{review: domain.CommandReview{
		Status: "completed", Decision: domain.ApprovalAgentAllow, Reason: "范围明确", ReviewedAt: time.Now().UTC(),
	}}
	svc.SetApprovalReviewer(explainer)
	result, err := svc.Submit(WithApprovalUserRequest(context.Background(), "重启 demo 服务"), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"}, Reason: "recover demo",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "approval_required" || result.ApprovalID == "" || len(transport.calls) != 0 || len(explainer.Inputs()) != 0 {
		t.Fatalf("Auto mode reused the explanation Agent: result=%#v calls=%d explanation_reviews=%d", result, len(transport.calls), len(explainer.Inputs()))
	}
}

func TestAutoApprovalModeFallsBackOnInvalidReview(t *testing.T) {
	svc, transport, host := newTestService(t)
	saveApprovalMode(t, svc, domain.ApprovalModeAuto)
	svc.SetAutomaticApprovalReviewer(&fakeAutomaticApprovalReviewer{review: domain.CommandReview{
		Status: "completed", Decision: domain.ApprovalAgentAllow, ReviewedAt: time.Now().UTC(),
	}})
	result, err := svc.Submit(WithApprovalUserRequest(context.Background(), "重启 demo 服务"), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"}, Reason: "recover demo",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "approval_required" || len(transport.calls) != 0 {
		t.Fatalf("invalid approval Agent response bypassed the human fallback: result=%#v calls=%d", result, len(transport.calls))
	}
}

func TestAutoApprovalModeUsesManualDecisionWithoutExecuting(t *testing.T) {
	svc, transport, host := newTestService(t)
	saveApprovalMode(t, svc, domain.ApprovalModeAuto)
	svc.SetAutomaticApprovalReviewer(&fakeAutomaticApprovalReviewer{review: domain.CommandReview{
		Status: "completed", Decision: domain.ApprovalAgentManual, Reason: "目标范围需要用户确认",
		Explanation: &domain.CommandExplanation{Summary: "重启 demo", Mechanism: "systemd 重启单元"}, ReviewedAt: time.Now().UTC(),
	}})
	result, err := svc.Submit(WithApprovalUserRequest(context.Background(), "检查并修复 demo"), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"}, Reason: "recover demo",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "approval_required" || result.ApprovalID == "" || len(transport.calls) != 0 {
		t.Fatalf("manual approval Agent decision did not fall back to the operator: result=%#v calls=%d", result, len(transport.calls))
	}
	run, err := svc.store.GetRun(context.Background(), result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.AIReview == nil || run.AIReview.Decision != domain.ApprovalAgentManual {
		t.Fatalf("manual approval Agent decision was not persisted: %#v", run.AIReview)
	}
}

func TestAutoApprovalModeRequiresCurrentUserRequest(t *testing.T) {
	svc, transport, host := newTestService(t)
	saveApprovalMode(t, svc, domain.ApprovalModeAuto)
	reviewer := &fakeAutomaticApprovalReviewer{review: domain.CommandReview{
		Status: "completed", Decision: domain.ApprovalAgentAllow, Reason: "范围明确",
		Explanation: &domain.CommandExplanation{Summary: "重启 demo", Mechanism: "systemd 重启单元"}, ReviewedAt: time.Now().UTC(),
	}}
	svc.SetAutomaticApprovalReviewer(reviewer)
	result, err := svc.Submit(context.Background(), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"}, Reason: "recover demo",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "approval_required" || result.ApprovalID == "" || len(transport.calls) != 0 || len(reviewer.Inputs()) != 0 {
		t.Fatalf("missing user request did not fail closed to manual approval: result=%#v calls=%d reviews=%d", result, len(transport.calls), len(reviewer.Inputs()))
	}
}

func TestFullAccessModeBypassesPolicyOnlyForAgent(t *testing.T) {
	svc, transport, host := newTestService(t)
	saveApprovalMode(t, svc, domain.ApprovalModeFullAccess)
	request := domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecScript, Script: "cat ~/.ssh/id_ed25519", Reason: "inspect configured credential",
	}
	result, err := svc.Submit(context.Background(), request, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" || result.AutoApproved || len(transport.calls) != 1 {
		t.Fatalf("full access did not execute the Agent request directly: result=%#v calls=%d", result, len(transport.calls))
	}
	mcpResult, err := svc.Submit(context.Background(), request, "mcp-client")
	if err != nil {
		t.Fatal(err)
	}
	if mcpResult.Status != "completed" || mcpResult.AutoApproved || len(transport.calls) != 2 {
		t.Fatalf("full access did not apply to the LLM-facing MCP server: result=%#v calls=%d", mcpResult, len(transport.calls))
	}
	operatorResult, err := svc.Submit(context.Background(), request, "admin-web")
	if err != nil {
		t.Fatal(err)
	}
	if operatorResult.Status != "completed" || operatorResult.AutoApproved || len(transport.calls) != 3 {
		t.Fatalf("authenticated operator request did not execute directly: result=%#v calls=%d", operatorResult, len(transport.calls))
	}
}

func TestCommandReviewDeadlineUsesReadableConfiguredTimeout(t *testing.T) {
	svc, _, _ := newTestService(t)
	review := svc.normalizeCommandReview(
		domain.CommandReview{Model: "small-model"},
		fmt.Errorf("[NodeRunError] failed to create chat completion: %w", context.DeadlineExceeded),
		45,
	)
	if review.Status != "unavailable" || review.Model != "small-model" || len(review.Errors) != 1 || review.Errors[0] != "approval Agent did not respond within 45 seconds" {
		t.Fatalf("unexpected normalized deadline review: %#v", review)
	}
}

func TestCommandReviewRedactsModelOutput(t *testing.T) {
	svc, _, _ := newTestService(t)
	review := svc.normalizeCommandReview(domain.CommandReview{
		Status: "completed", Decision: domain.ApprovalAgentReject, Reason: "password=review-secret",
		Explanation: &domain.CommandExplanation{
			Summary: "password=summary-secret", Mechanism: "api_key=mechanism-secret", Risks: []string{"password=risk-secret"},
		},
	}, nil, 30)
	encoded, err := json.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"review-secret", "summary-secret", "mechanism-secret", "risk-secret"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("approval Agent output retained secret %q: %s", secret, encoded)
		}
	}
}

func TestCommandExplainerPersistsAdviceForManualApproval(t *testing.T) {
	svc, _, host := newTestService(t)
	explainer := &fakeCommandExplainer{review: domain.CommandReview{
		Status:      "completed",
		Explanation: &domain.CommandExplanation{Summary: "重启服务", Mechanism: "由 systemd 停止并重新启动单元"},
		ReviewedAt:  time.Now().UTC(),
	}}
	svc.SetApprovalReviewer(explainer)
	result, err := svc.Submit(context.Background(), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"},
		Reason: "recover demo",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "approval_required" {
		t.Fatalf("manual approval was bypassed: %#v", result)
	}
	waitForApproval(t, svc, result.ApprovalID, func(approval domain.Approval) bool {
		return approval.AIReview != nil && approval.AIReview.Status != "pending"
	})
	approvals, err := svc.ListApprovals(context.Background(), "pending", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(approvals) != 1 || approvals[0].AIReview == nil || approvals[0].AIReview.Explanation == nil {
		t.Fatalf("structured explanation was not normalized and persisted: %#v", approvals)
	}
	inputs := explainer.Inputs()
	if len(inputs) != 1 || inputs[0].RequestDigest == "" {
		t.Fatalf("explanation Agent did not receive bounded context: %#v", inputs)
	}
}

func TestApprovalIsCreatedWithoutWaitingForCommandExplanation(t *testing.T) {
	svc, _, host := newTestService(t)
	explainer := &blockingCommandExplainer{
		started: make(chan struct{}), release: make(chan struct{}),
		review: domain.CommandReview{
			Status: "completed", Explanation: &domain.CommandExplanation{Summary: "重启服务", Mechanism: "systemd 重启单元"}, ReviewedAt: time.Now().UTC(),
		},
	}
	svc.SetApprovalReviewer(explainer)
	released := false
	defer func() {
		if !released {
			close(explainer.release)
		}
	}()

	type outcome struct {
		result domain.ExecResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := svc.Submit(context.Background(), domain.ExecRequest{
			HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"},
			Reason: "recover demo",
		}, "eino-agent")
		done <- outcome{result: result, err: err}
	}()

	var submitted outcome
	select {
	case submitted = <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("approval creation blocked on command explanation")
	}
	if submitted.err != nil || submitted.result.Status != "approval_required" {
		t.Fatalf("unexpected immediate approval result: %#v err=%v", submitted.result, submitted.err)
	}
	select {
	case <-explainer.started:
	case <-time.After(time.Second):
		t.Fatal("background explanation did not start")
	}
	waitForApproval(t, svc, submitted.result.ApprovalID, func(approval domain.Approval) bool {
		return approval.AIReview != nil && approval.AIReview.Status == "pending"
	})
	close(explainer.release)
	released = true
	waitForApproval(t, svc, submitted.result.ApprovalID, func(approval domain.Approval) bool {
		return approval.AIReview != nil && approval.AIReview.Status == "completed"
	})
}

func TestApprovalDecisionCancelsCommandExplanation(t *testing.T) {
	svc, transport, host := newTestService(t)
	explainer := &blockingCommandExplainer{
		started: make(chan struct{}), release: make(chan struct{}),
		review: domain.CommandReview{
			Status: "completed", Explanation: &domain.CommandExplanation{Summary: "重启服务", Mechanism: "systemd 重启单元"}, ReviewedAt: time.Now().UTC(),
		},
	}
	svc.SetApprovalReviewer(explainer)
	pending, err := svc.Submit(context.Background(), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"},
		Reason: "recover demo",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-explainer.started:
	case <-time.After(time.Second):
		t.Fatal("background explanation did not start")
	}
	if _, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed operation", "operator"); err != nil {
		t.Fatal(err)
	}
	svc.explainWG.Wait()

	approval, err := svc.store.GetApproval(context.Background(), pending.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	if approval.Status != "approved" {
		t.Fatalf("late explanation overwrote the approval decision: %#v", approval)
	}
	run, err := svc.store.GetRun(context.Background(), pending.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.AIReview != nil || run.AIReviewJSON != "" {
		t.Fatalf("canceled explanation remained attached to the decided run: %#v", run.AIReview)
	}
	if len(transport.calls) != 1 {
		t.Fatalf("operation was not executed exactly once: %#v", transport.calls)
	}
}

func TestCommandExplanationConcurrencyIsBounded(t *testing.T) {
	svc, _, host := newTestService(t)
	explainer := &trackingCommandExplainer{started: make(chan struct{}, 4)}
	svc.SetApprovalReviewer(explainer)
	results := make([]domain.ExecResult, 0, 3)
	for index := 0; index < 3; index++ {
		result, err := svc.Submit(context.Background(), domain.ExecRequest{
			HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", fmt.Sprintf("demo-%d", index)},
			Reason: "recover demo",
		}, "eino-agent")
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, result)
	}
	for index := 0; index < maxConcurrentApprovalExplanations; index++ {
		select {
		case <-explainer.started:
		case <-time.After(time.Second):
			t.Fatal("expected explanation did not start")
		}
	}
	select {
	case <-explainer.started:
		t.Fatal("explanation concurrency limit was exceeded")
	case <-time.After(100 * time.Millisecond):
	}
	if err := svc.Reject(context.Background(), results[0].ApprovalID, "not approved", "operator"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-explainer.started:
	case <-time.After(time.Second):
		t.Fatal("queued explanation did not start after a slot was released")
	}
	for _, result := range results[1:] {
		if err := svc.Reject(context.Background(), result.ApprovalID, "not approved", "operator"); err != nil {
			t.Fatal(err)
		}
	}
	svc.explainWG.Wait()
	if maximum := explainer.maxActive(); maximum != maxConcurrentApprovalExplanations {
		t.Fatalf("maximum concurrent explanations = %d", maximum)
	}
}

func TestCommandExplanationQueueIsBounded(t *testing.T) {
	svc, _, host := newTestService(t)
	svc.explanationSem = make(chan struct{}, 1)
	svc.explanationSlots = make(chan struct{}, 1)
	explainer := &trackingCommandExplainer{started: make(chan struct{}, 2)}
	svc.SetApprovalReviewer(explainer)
	first, err := svc.Submit(context.Background(), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo-one"},
		Reason: "recover demo",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-explainer.started:
	case <-time.After(time.Second):
		t.Fatal("first explanation did not start")
	}
	second, err := svc.Submit(context.Background(), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo-two"},
		Reason: "recover demo",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	skipped := waitForApproval(t, svc, second.ApprovalID, func(approval domain.Approval) bool {
		return approval.AIReview != nil && approval.AIReview.Status == "unavailable"
	})
	if len(skipped.AIReview.Errors) != 1 || !strings.Contains(skipped.AIReview.Errors[0], "queue is full") {
		t.Fatalf("queue overflow was not reported clearly: %#v", skipped.AIReview)
	}
	if err := svc.Reject(context.Background(), first.ApprovalID, "not approved", "operator"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Reject(context.Background(), second.ApprovalID, "not approved", "operator"); err != nil {
		t.Fatal(err)
	}
	svc.explainWG.Wait()
}

func TestRetryApprovalExplanationDoesNotExecute(t *testing.T) {
	svc, transport, host := newTestService(t)
	ctx := context.Background()
	pending, err := svc.Submit(ctx, domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"},
		Reason: "recover demo",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	explainer := &fakeCommandExplainer{review: domain.CommandReview{
		Status: "completed", Explanation: &domain.CommandExplanation{
			Summary: "重启服务", Mechanism: "systemd 会停止并重新启动服务",
			Risks: []string{"可能短暂中断请求"},
		}, ReviewedAt: time.Now().UTC(),
	}}
	svc.SetApprovalReviewer(explainer)

	updated, err := svc.RetryApprovalExplanation(ctx, pending.ApprovalID, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "pending" || updated.AIReview == nil || updated.AIReview.Explanation == nil {
		t.Fatalf("explanation retry changed the pending approval: %#v", updated)
	}
	if len(transport.calls) != 0 {
		t.Fatalf("explanation retry executed the operation: %#v", transport.calls)
	}
	inputs := explainer.Inputs()
	if len(inputs) != 1 || inputs[0].RequestDigest != updated.RequestDigest {
		t.Fatalf("explanation retry did not receive the exact pending request: %#v", inputs)
	}
	if _, err := svc.Approve(ctx, updated.ID, "reviewed operation", "operator"); err != nil {
		t.Fatal(err)
	}
	if len(transport.calls) != 1 {
		t.Fatalf("approved operation was not executed exactly once: %#v", transport.calls)
	}
}

func TestRetryApprovalExplanationPersistsDegradedResultAndKeepsPending(t *testing.T) {
	svc, transport, host := newTestService(t)
	ctx := context.Background()
	pending, err := svc.Submit(ctx, domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"},
		Reason: "recover demo",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	svc.SetApprovalReviewer(&fakeCommandExplainer{err: errors.New("model timed out")})
	updated, err := svc.RetryApprovalExplanation(ctx, pending.ApprovalID, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "pending" || updated.AIReview == nil || updated.AIReview.Status != "unavailable" {
		t.Fatalf("degraded retry changed the approval boundary: %#v", updated)
	}
	if len(updated.AIReview.Errors) != 1 || !strings.Contains(updated.AIReview.Errors[0], "model timed out") {
		t.Fatalf("degraded retry error was not preserved: %#v", updated.AIReview)
	}
	if len(transport.calls) != 0 {
		t.Fatalf("degraded explanation retry executed the operation: %#v", transport.calls)
	}
	listed, err := svc.ListApprovals(ctx, "pending", 10)
	if err != nil || len(listed) != 1 || listed[0].AIReview == nil || listed[0].AIReview.Status != "unavailable" {
		t.Fatalf("degraded retry was not persisted: approvals=%#v err=%v", listed, err)
	}
}

func TestApprovalDecisionCancelsRetriedCommandExplanation(t *testing.T) {
	svc, _, host := newTestService(t)
	explainer := &trackingCommandExplainer{started: make(chan struct{}, 2)}
	svc.SetApprovalReviewer(explainer)
	pending, err := svc.Submit(context.Background(), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"},
		Reason: "recover demo",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-explainer.started:
	case <-time.After(time.Second):
		t.Fatal("automatic explanation did not start")
	}

	retryDone := make(chan error, 1)
	go func() {
		_, retryErr := svc.RetryApprovalExplanation(context.Background(), pending.ApprovalID, "operator")
		retryDone <- retryErr
	}()
	select {
	case <-explainer.started:
	case <-time.After(time.Second):
		t.Fatal("retried explanation did not start")
	}
	if err := svc.Reject(context.Background(), pending.ApprovalID, "not approved", "operator"); err != nil {
		t.Fatal(err)
	}
	select {
	case retryErr := <-retryDone:
		if !errors.Is(retryErr, context.Canceled) {
			t.Fatalf("retry error = %v", retryErr)
		}
	case <-time.After(time.Second):
		t.Fatal("retried explanation continued after approval rejection")
	}
}

func TestCurrentAgentTaskPrefersInProgressThenUnblockedPending(t *testing.T) {
	tasks := domain.AgentTaskList{Items: []domain.AgentTask{
		{ID: "1", Subject: "Blocked", Status: "pending", BlockedBy: []string{"2"}},
		{ID: "2", Subject: "Ready", Status: "pending"},
		{ID: "3", Subject: "Running", Status: "in_progress"},
	}}
	if got := currentAgentTask(tasks); got != "#3 Running" {
		t.Fatalf("current task = %q", got)
	}
	tasks.Items[2].Status = "completed"
	if got := currentAgentTask(tasks); got != "#2 Ready" {
		t.Fatalf("ready task = %q", got)
	}
	tasks.Items[1].Status = "completed"
	if got := currentAgentTask(tasks); got != "#1 Blocked" {
		t.Fatalf("resolved dependency task = %q", got)
	}
}

func TestReadOnlyExecutesAndAuditIsRedacted(t *testing.T) {
	svc, transport, host := newTestService(t)
	result, err := svc.Submit(context.Background(), domain.ExecRequest{HostID: host.ID, Mode: domain.ExecProgram, Program: "uname", Args: []string{"-a"}, Reason: "test read"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" || len(transport.calls) != 1 {
		t.Fatalf("unexpected result %#v calls=%d", result, len(transport.calls))
	}
	if strings.Contains(result.Stdout, "secret-value") {
		t.Fatalf("model output was not redacted: %q", result.Stdout)
	}
	history, err := svc.GetRun(context.Background(), result.RunID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(history.StdoutRaw, "secret-value") {
		t.Fatal("encrypted raw output did not round-trip")
	}
}

func TestRunCapturesAgentSessionFromContext(t *testing.T) {
	svc, _, host := newTestService(t)
	ctx := WithSessionID(context.Background(), "session_audit_group")
	result, err := svc.Submit(ctx, domain.ExecRequest{HostID: host.ID, Mode: domain.ExecProgram, Program: "uname", Reason: "verify session audit binding"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	run, err := svc.store.GetRun(context.Background(), result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.SessionID != "session_audit_group" {
		t.Fatalf("run session ID = %q", run.SessionID)
	}
}

func TestChangeRequiresApprovalThenExecutes(t *testing.T) {
	svc, transport, host := newTestService(t)
	result, err := svc.Submit(context.Background(), domain.ExecRequest{HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"}, Reason: "recover service"}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "approval_required" || result.ApprovalID == "" || len(transport.calls) != 0 {
		t.Fatalf("unexpected pending result %#v calls=%d", result, len(transport.calls))
	}
	approved, err := svc.Approve(context.Background(), result.ApprovalID, "reviewed", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != "completed" || len(transport.calls) != 1 {
		t.Fatalf("unexpected approved result %#v calls=%d", approved, len(transport.calls))
	}
}

func TestAgentApprovalDecisionDoesNotExecuteBeforeCheckpointResume(t *testing.T) {
	svc, transport, host := newTestService(t)
	ctx := WithAgentApprovalContinuation(WithSessionID(context.Background(), "session_agent_resume"), "checkpoint_agent_resume")
	pending, err := svc.Submit(ctx, domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"},
		Reason: "restart demo after review",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != "approval_required" || pending.ApprovalID == "" || len(transport.calls) != 0 {
		t.Fatalf("unexpected pending result %#v calls=%d", pending, len(transport.calls))
	}
	preparing, err := svc.GetApproval(context.Background(), pending.ApprovalID)
	if err != nil || preparing.Status != domain.ApprovalStatusPreparing || preparing.CheckpointID != "checkpoint_agent_resume" {
		t.Fatalf("Agent continuation was not prepared: approval=%#v err=%v", preparing, err)
	}
	if listed, err := svc.ListApprovals(context.Background(), domain.ApprovalStatusPending, 10); err != nil || len(listed) != 0 {
		t.Fatalf("approval was visible before its checkpoint: approvals=%#v err=%v", listed, err)
	}
	if err := svc.store.Set(context.Background(), preparing.CheckpointID, []byte("checkpoint")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ActivateAgentApprovals(context.Background(), preparing.CheckpointID, map[string]string{preparing.ID: "interrupt_agent_resume"}); err != nil {
		t.Fatal(err)
	}
	decision, err := svc.DecideAgentApproval(context.Background(), preparing.ID, domain.ApprovalStatusApproved, "reviewed", "operator")
	if err != nil || decision.Status != domain.ApprovalStatusApproved || len(transport.calls) != 0 {
		t.Fatalf("approval decision executed the operation: result=%#v calls=%d err=%v", decision, len(transport.calls), err)
	}
	run, err := svc.store.GetRun(context.Background(), pending.RunID)
	if err != nil || run.Status != "approval_required" {
		t.Fatalf("approved run was claimed before resume: run=%#v err=%v", run, err)
	}
	completed, err := svc.ResumeAgentApproval(context.Background(), preparing.ID)
	if err != nil || completed.Status != "completed" || len(transport.calls) != 1 {
		t.Fatalf("resumed operation = %#v calls=%d err=%v", completed, len(transport.calls), err)
	}
	if replayed, err := svc.ResumeAgentApproval(context.Background(), preparing.ID); err != nil || replayed.Status != "completed" || len(transport.calls) != 1 {
		t.Fatalf("completed approval was not replay-safe: result=%#v calls=%d err=%v", replayed, len(transport.calls), err)
	}
}

func TestRejectedAgentApprovalResumesAsToolRejectionWithoutExecution(t *testing.T) {
	svc, transport, host := newTestService(t)
	ctx := WithAgentApprovalContinuation(WithSessionID(context.Background(), "session_agent_reject"), "checkpoint_agent_reject")
	pending, err := svc.Submit(ctx, domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"},
		Reason: "restart demo after review",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.store.Set(context.Background(), "checkpoint_agent_reject", []byte("checkpoint")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ActivateAgentApprovals(context.Background(), "checkpoint_agent_reject", map[string]string{pending.ApprovalID: "interrupt_agent_reject"}); err != nil {
		t.Fatal(err)
	}
	const instruction = "inspect logs first"
	if _, err := svc.DecideAgentApproval(context.Background(), pending.ApprovalID, domain.ApprovalStatusRejected, instruction, "operator"); err != nil {
		t.Fatal(err)
	}
	result, err := svc.ResumeAgentApproval(context.Background(), pending.ApprovalID)
	if err != nil || result.Status != domain.ApprovalStatusRejected || result.OperatorInstruction != instruction {
		t.Fatalf("rejected resume = %#v err=%v", result, err)
	}
	if len(transport.calls) != 0 {
		t.Fatalf("rejected Agent approval executed %d operations", len(transport.calls))
	}
}

func waitForBackgroundTaskApproval(t *testing.T, svc *Service, taskID string) (domain.Task, domain.ExecResult) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		task, result, _, err := svc.GetTask(taskID)
		if err == nil && task.Status == "approval_required" && result.RunID != "" && result.ApprovalID != "" {
			return task, result
		}
		if time.Now().After(deadline) {
			t.Fatalf("task did not enter approval_required: task=%#v result=%#v err=%v", task, result, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestBackgroundApprovalReturnsImmediatelyAndTracksExecution(t *testing.T) {
	svc, transport, host := newTestService(t)
	base, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ctx := WithSessionID(base, "session_blocking_task")
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	transport.mu.Lock()
	transport.execStarted = started
	transport.execRelease = release
	transport.mu.Unlock()

	startedAt := time.Now()
	task, err := svc.StartTask(ctx, domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"},
		Reason: "restart demo as a managed task",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(startedAt) > 500*time.Millisecond || task.ID == "" || task.Status != "running" {
		t.Fatalf("background task did not return immediately: %#v elapsed=%s", task, time.Since(startedAt))
	}

	_, pending := waitForBackgroundTaskApproval(t, svc, task.ID)
	if pending.Status != "approval_required" || pending.RunID == "" || pending.ApprovalID == "" {
		t.Fatalf("invalid background approval state: %#v", pending)
	}
	if _, err := svc.ApproveAsync(context.Background(), pending.ApprovalID, "reviewed", "operator"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-base.Done():
		t.Fatal("approved background task did not start")
	}
	deadline := time.Now().Add(time.Second)
	for {
		task, _, _, err := svc.GetTask(task.ID)
		if err == nil && task.Status == "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("approved task did not enter running: task=%#v err=%v", task, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	releaseOnce.Do(func() { close(release) })
	deadline = time.Now().Add(time.Second)
	for {
		completed, result, taskErr, err := svc.GetTask(task.ID)
		if err == nil && completed.Status == "completed" {
			if result.Status != "completed" || taskErr != "" {
				t.Fatalf("completed task result = %#v error=%q", result, taskErr)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("background task did not complete: task=%#v result=%#v error=%q err=%v", completed, result, taskErr, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestBackgroundApprovalRejectionUpdatesTask(t *testing.T) {
	svc, transport, host := newTestService(t)
	ctx := WithSessionID(context.Background(), "session_rejected_task")
	task, err := svc.StartTask(ctx, domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"},
		Reason: "restart demo as a managed task",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	_, pending := waitForBackgroundTaskApproval(t, svc, task.ID)
	const instruction = "inspect logs instead"
	if err := svc.Reject(context.Background(), pending.ApprovalID, instruction, "operator"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		rejected, result, _, err := svc.GetTask(task.ID)
		if err == nil && rejected.Status == "rejected" {
			if result.Status != "rejected" || result.OperatorInstruction != instruction {
				t.Fatalf("rejected task lost operator instruction: task=%#v result=%#v", rejected, result)
			}
			if len(transport.calls) != 0 {
				t.Fatalf("rejected background task executed %d times", len(transport.calls))
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("background task did not become rejected: task=%#v result=%#v err=%v", rejected, result, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestApprovedBackgroundTaskCanBeCancelledWhileRunning(t *testing.T) {
	svc, transport, host := newTestService(t)
	started := make(chan struct{})
	release := make(chan struct{})
	transport.mu.Lock()
	transport.execStarted = started
	transport.execRelease = release
	transport.mu.Unlock()
	ctx := WithSessionID(context.Background(), "session_cancel_task")
	task, err := svc.StartTask(ctx, domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"},
		Reason: "restart demo as a managed task",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	_, pending := waitForBackgroundTaskApproval(t, svc, task.ID)
	if _, err := svc.ApproveAsync(context.Background(), pending.ApprovalID, "reviewed", "operator"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("approved background task did not start")
	}
	deadline := time.Now().Add(time.Second)
	for {
		running, _, _, err := svc.GetTask(task.ID)
		if err == nil && running.Status == "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task did not enter running before cancellation: %#v err=%v", running, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := svc.CancelTask(task.ID, "eino-agent"); err != nil {
		t.Fatal(err)
	}
	cancelled, result, _, err := svc.GetTask(task.ID)
	if err != nil || cancelled.Status != "cancelled" || result.Status != "cancelled" {
		t.Fatalf("cancelled background task = %#v result=%#v err=%v", cancelled, result, err)
	}
	deadline = time.Now().Add(time.Second)
	for {
		run, err := svc.store.GetRun(context.Background(), pending.RunID)
		if err == nil && terminalExecutionStatus(run.Status) {
			if run.Status != "interrupted" {
				t.Fatalf("cancelled execution run status = %s", run.Status)
			}
			cancelled, result, _, taskErr := svc.GetTask(task.ID)
			if taskErr != nil || cancelled.Status != "cancelled" || result.Status != "cancelled" {
				t.Fatalf("worker completion overwrote cancellation: task=%#v result=%#v err=%v", cancelled, result, taskErr)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancelled remote execution did not stop: run=%#v err=%v", run, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestApproveAsyncReturnsBeforeExecutionAndSurvivesRequestCancellation(t *testing.T) {
	svc, transport, host := newTestService(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	transport.mu.Lock()
	transport.execStarted = started
	transport.execRelease = release
	transport.mu.Unlock()

	pending, err := svc.Submit(WithSessionID(context.Background(), "async_approval"), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"}, Reason: "restart demo service",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	approveCtx, cancelApprove := context.WithCancel(context.Background())
	type approvalOutcome struct {
		result domain.ExecResult
		err    error
	}
	decision := make(chan approvalOutcome, 1)
	go func() {
		result, approveErr := svc.ApproveAsync(approveCtx, pending.ApprovalID, "reviewed", "operator")
		decision <- approvalOutcome{result: result, err: approveErr}
	}()

	var approved approvalOutcome
	select {
	case approved = <-decision:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("async approval waited for command execution")
	}
	if approved.err != nil || approved.result.Status != "running" || approved.result.RunID != pending.RunID {
		t.Fatalf("unexpected async approval result: %#v err=%v", approved.result, approved.err)
	}
	cancelApprove()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("approved execution did not start")
	}
	time.Sleep(25 * time.Millisecond)
	run, err := svc.store.GetRun(context.Background(), pending.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "running" {
		t.Fatalf("request cancellation stopped approved execution: status=%s error=%s", run.Status, run.Error)
	}

	releaseOnce.Do(func() { close(release) })
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		run, err = svc.store.GetRun(context.Background(), pending.RunID)
		if err == nil && run.Status == "completed" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("approved execution did not complete after release: status=%s error=%s", run.Status, run.Error)
}

func TestApprovedExecutionStreamsRedactedOutputToAgentSession(t *testing.T) {
	svc, transport, host := newTestService(t)
	transport.mu.Lock()
	transport.stdout = []byte("password=split-secret\nready\n")
	transport.stderr = []byte("warning\n")
	transport.mu.Unlock()
	svc.transport = &streamingFakeTransport{
		fakeTransport: transport,
		chunks: []fakeStreamChunk{
			{stream: "stdout", data: "password=split-"},
			{stream: "stderr", data: "warning\n"},
			{stream: "stdout", data: "secret\nready\n"},
		},
	}

	const sessionID = "streaming_approval"
	const toolCallID = "call_streaming_approval"
	events, unsubscribe := svc.SubscribeExecutionEvents(sessionID)
	defer unsubscribe()
	runCtx := WithExecutionOwner(WithSessionID(context.Background(), sessionID), toolCallID, "ssh_exec", `{"host_id":"test","program":"systemctl","args":["restart","demo"]}`)
	pending, err := svc.Submit(runCtx, domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"}, Reason: "restart demo service",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != "approval_required" {
		t.Fatalf("expected approval, got %#v", pending)
	}
	if _, err := svc.ApproveAsync(context.Background(), pending.ApprovalID, "reviewed", "operator"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr string
	started := false
	deadline := time.After(2 * time.Second)
	for {
		select {
		case event := <-events:
			if event.RunID != pending.RunID || event.SessionID != sessionID || event.ToolCallID != toolCallID || event.ToolName != "ssh_exec" || event.Sequence == 0 {
				t.Fatalf("stream event lost its run identity: %#v", event)
			}
			switch event.Status {
			case "running":
				started = true
			case "completed":
				if !started {
					t.Fatal("completion arrived without a running event")
				}
				if stdout != "password=[REDACTED]\nready\n" || stderr != "warning\n" {
					t.Fatalf("unexpected streamed output: stdout=%q stderr=%q", stdout, stderr)
				}
				if strings.Contains(stdout, "split-secret") {
					t.Fatalf("stream exposed a split secret: %q", stdout)
				}
				return
			}
			if event.Stream == "stdout" {
				stdout += event.Content
			}
			if event.Stream == "stderr" {
				stderr += event.Content
			}
		case <-deadline:
			t.Fatal("timed out waiting for approved execution stream")
		}
	}
}

func TestBackgroundTaskKeepsItsToolCallForStreamEvents(t *testing.T) {
	svc, transport, host := newTestService(t)
	transport.mu.Lock()
	transport.stdout = []byte("first\nsecond\n")
	transport.mu.Unlock()
	svc.transport = &streamingFakeTransport{
		fakeTransport: transport,
		chunks: []fakeStreamChunk{
			{stream: "stdout", data: "first\n"},
			{stream: "stdout", data: "second\n"},
		},
	}

	const sessionID = "streaming_background"
	const toolCallID = "call_streaming_background"
	events, unsubscribe := svc.SubscribeExecutionEvents(sessionID)
	defer unsubscribe()
	taskCtx := WithExecutionOwner(WithSessionID(context.Background(), sessionID), toolCallID, "ssh_exec", `{"host_id":"test","program":"uname","background":true}`)
	task, err := svc.StartTask(taskCtx, domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "uname", Args: []string{"-a"}, Reason: "inspect the host kernel",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "running" {
		t.Fatalf("background task did not start: %#v", task)
	}

	var output string
	deadline := time.After(2 * time.Second)
	for {
		select {
		case event := <-events:
			if event.ToolCallID != toolCallID || event.ToolName != "ssh_exec" {
				t.Fatalf("background stream was attached to the wrong tool: %#v", event)
			}
			if event.Stream == "stdout" {
				output += event.Content
			}
			if event.Status == "completed" {
				if output != "first\nsecond\n" {
					t.Fatalf("unexpected background output: %q", output)
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for background stream")
		}
	}
}

func TestRecoveryMarksUnconfirmedToolResultUnknown(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.store.CreateChatSession(ctx, "session-recovery", ""); err != nil {
		t.Fatal(err)
	}
	userMessageID, err := svc.store.AppendPendingChatMessage(ctx, "session-recovery", "user", "continue")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.StartChatToolCall(ctx, domain.ChatToolCall{
		SessionID: "session-recovery", UserMessageID: userMessageID,
		ToolCallID: "call-unknown", ToolName: "mcp__external__mutate", ArgumentsJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecoverInterruptedTasks(ctx); err != nil {
		t.Fatal(err)
	}
	call, err := svc.store.GetChatToolCall(ctx, "session-recovery", "call-unknown")
	if err != nil {
		t.Fatal(err)
	}
	if call.Status != domain.ChatToolCallUnknown || !strings.Contains(call.ResultJSON, `"status":"unknown"`) {
		t.Fatalf("recovered tool call = %#v", call)
	}
	messages, err := svc.store.ListChatContextMessages(ctx, "session-recovery")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Status != "failed" || messages[1].ToolStatus != domain.ChatToolCallUnknown {
		t.Fatalf("recovered context = %#v", messages)
	}
}

func TestRecoveryInterruptsRunsWithoutAnExecutionOwner(t *testing.T) {
	svc, _, host := newTestService(t)
	ctx := context.Background()
	run := domain.Run{
		ID: "run-recovery", HostID: host.ID, RequestJSON: `{}`, RequestDigest: "digest",
		Status: "running", StartedAt: time.Now().UTC(),
	}
	if err := svc.store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecoverInterruptedTasks(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, err := svc.store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != "interrupted" || recovered.CompletedAt.IsZero() || !strings.Contains(recovered.Error, "control plane restarted") {
		t.Fatalf("orphaned run was not interrupted: %#v", recovered)
	}
}

func TestApproveAsyncExecutesConcurrentDecisionOnlyOnce(t *testing.T) {
	svc, transport, host := newTestService(t)
	pending, err := svc.Submit(WithSessionID(context.Background(), "concurrent_approval"), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"}, Reason: "restart demo service",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, approveErr := svc.ApproveAsync(context.Background(), pending.ApprovalID, "reviewed", "operator")
			results <- approveErr
		}()
	}
	close(start)
	successes := 0
	for range 2 {
		if err := <-results; err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent approval decisions succeeded %d times, want exactly once", successes)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		transport.mu.Lock()
		callCount := len(transport.calls)
		transport.mu.Unlock()
		if callCount == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	transport.mu.Lock()
	callCount := len(transport.calls)
	transport.mu.Unlock()
	t.Fatalf("approved operation executed %d times, want once", callCount)
}

func TestManualApprovalReasonIsOptional(t *testing.T) {
	svc, transport, host := newTestService(t)
	result, err := svc.Submit(context.Background(), domain.ExecRequest{HostID: host.ID, Mode: domain.ExecProgram, Program: "rm", Args: []string{"-rf", "/tmp/demo"}, Reason: "clean fixture"}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "approval_required" {
		t.Fatalf("unexpected manual approval result %#v", result)
	}
	approved, err := svc.Approve(context.Background(), result.ApprovalID, "", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != "completed" || len(transport.calls) != 1 {
		t.Fatalf("unexpected approved result %#v", approved)
	}
}

func TestCredentialReadUsesManualApproval(t *testing.T) {
	svc, transport, host := newTestService(t)
	result, err := svc.Submit(context.Background(), domain.ExecRequest{HostID: host.ID, Mode: domain.ExecScript, Script: "cat ~/.ssh/id_ed25519", Reason: "inspect configured credential"}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "approval_required" || result.ApprovalID == "" || len(transport.calls) != 0 {
		t.Fatalf("credential read bypassed manual approval: %#v", result)
	}
	if err := svc.Reject(context.Background(), result.ApprovalID, "not approved", "operator"); err != nil {
		t.Fatal(err)
	}
}

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

func TestChatSessionsCanBeListedLoadedAndDeleted(t *testing.T) {
	svc, _, host := newTestService(t)
	ctx := context.Background()
	if err := svc.store.AppendChatMessage(ctx, "session-one", "user", "Investigate disk usage"); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.AppendChatMessage(ctx, "session-one", "assistant", "Disk usage is healthy"); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.AppendChatMessage(ctx, "session-one", "reasoning", "I should inspect the filesystem first"); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.AppendChatMessage(ctx, "session-one", "tool", `{"status":"completed","run_id":"run_test"}`, "ssh_exec"); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.AppendChatMessage(ctx, "session-two", "user", "Deploy the API"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	run := domain.Run{
		ID: "run-session-one", SessionID: "session-one", HostID: host.ID, ToolName: "ssh_exec",
		RequestJSON: `{}`, RequestDigest: "digest-session-one", Status: "completed", StartedAt: now, CompletedAt: now,
	}
	if err := svc.store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	approval := domain.Approval{
		ID: "approval-session-one", RunID: run.ID, HostID: host.ID, RequestJSON: `{}`, RequestDigest: run.RequestDigest,
		Status: "rejected", CreatedAt: now,
	}
	if err := svc.store.CreateApproval(ctx, approval); err != nil {
		t.Fatal(err)
	}
	task := domain.Task{ID: "task-session-one", RunID: run.ID, HostID: host.ID, Status: "completed", StartedAt: now, EndedAt: now}
	if err := svc.store.UpsertTask(ctx, task, domain.ExecResult{RunID: run.ID, Status: "completed"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.WriteAgentTaskFile(ctx, "session-one", "agent-tasks/1.json", `{"id":"1","subject":"Inspect","description":"Inspect","status":"in_progress","blocks":[],"blockedBy":[]}`); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.AppendAudit(ctx, domain.AuditEvent{RunID: run.ID, Type: "command_completed", Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.AppendAudit(ctx, domain.AuditEvent{Type: "agent_task_created", Actor: "test", Data: map[string]any{"session_id": "session-one"}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.AppendAudit(ctx, domain.AuditEvent{Type: "agent_task_created", Actor: "test", Data: map[string]any{"session_id": "session-two"}}); err != nil {
		t.Fatal(err)
	}
	sessions, err := svc.ListChatSessions(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 || sessions[0].ID != "session-two" || sessions[1].Title != "Investigate disk usage" || sessions[1].MessageCount != 4 {
		t.Fatalf("unexpected sessions %#v", sessions)
	}
	renamed, err := svc.RenameChatSession(ctx, "session-one", "Disk health", "test")
	if err != nil || renamed.Title != "Disk health" {
		t.Fatalf("renamed session = %#v, err=%v", renamed, err)
	}
	if _, err := svc.RenameChatSession(ctx, "session-one", "", "test"); err == nil {
		t.Fatal("empty conversation title was accepted")
	}
	messages, err := svc.ListChatMessages(ctx, "session-one", 10)
	if err != nil || len(messages) != 4 || messages[1].Role != "assistant" || messages[2].Role != "reasoning" || messages[3].Role != "tool" || messages[3].ToolName != "ssh_exec" {
		t.Fatalf("unexpected messages %#v err=%v", messages, err)
	}
	modelMessages, err := svc.store.ListChatModelMessages(ctx, "session-one", 10)
	if err != nil || len(modelMessages) != 2 || modelMessages[0].Role != "user" || modelMessages[1].Role != "assistant" {
		t.Fatalf("reasoning and tool history leaked into model messages: %#v err=%v", modelMessages, err)
	}
	if err := svc.DeleteChatSession(ctx, "session-one"); err != nil {
		t.Fatal(err)
	}
	messages, err = svc.ListChatMessages(ctx, "session-one", 10)
	if err != nil || len(messages) != 0 {
		t.Fatalf("deleted messages still exist: %#v err=%v", messages, err)
	}
	if retained, err := svc.store.GetRun(ctx, run.ID); err != nil || retained.SessionID != "session-one" {
		t.Fatalf("conversation audit run was not retained: run=%#v err=%v", retained, err)
	}
	if _, err := svc.store.GetApproval(ctx, approval.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("conversation approval survived deletion: %v", err)
	}
	if _, _, _, err := svc.store.GetTask(ctx, task.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("conversation task survived deletion: %v", err)
	}
	if agentTasks, err := svc.store.ListAgentTasks(ctx, "session-one"); err != nil || len(agentTasks.Items) != 0 {
		t.Fatalf("conversation Agent tasks survived deletion: tasks=%#v err=%v", agentTasks, err)
	}
	audit, err := svc.store.ListAudit(ctx, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	keptDeletedSession := false
	keptOtherSession := false
	for _, event := range audit {
		if event.RunID == run.ID || event.Data["session_id"] == "session-one" {
			keptDeletedSession = true
		}
		if event.Data["session_id"] == "session-two" {
			keptOtherSession = true
		}
	}
	if !keptOtherSession {
		t.Fatalf("another conversation's audit was deleted: %#v", audit)
	}
	if !keptDeletedSession {
		t.Fatalf("deleted conversation's audit was not retained: %#v", audit)
	}
	if err := svc.DeleteChatSession(ctx, "session-one"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected not found on second delete, got %v", err)
	}
}

func TestAuditPersistsAfterRequestCancellation(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc.audit(ctx, "run-cancelled", "request_finished", "test", map[string]any{"status": "completed"})
	events, err := svc.ListAudit(context.Background(), "run-cancelled", 10)
	if err != nil || len(events) != 1 || events[0].Type != "request_finished" {
		t.Fatalf("audit after request cancellation = %#v, err=%v", events, err)
	}
}

func TestAbortApprovalsForSession(t *testing.T) {
	svc, transport, host := newTestService(t)
	request := domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"},
		Reason: "recover demo service",
	}
	target, err := svc.Submit(WithSessionID(context.Background(), "session_stop"), request, "eino-agent")
	if err != nil || target.ApprovalID == "" {
		t.Fatalf("target approval = %#v err=%v", target, err)
	}
	request.Args = []string{"restart", "other"}
	other, err := svc.Submit(WithSessionID(context.Background(), "session_other"), request, "eino-agent")
	if err != nil || other.ApprovalID == "" {
		t.Fatalf("other approval = %#v err=%v", other, err)
	}

	rejected, err := svc.AbortApprovalsForSession(context.Background(), "session_stop", "Agent run stopped by the operator", "operator")
	if err != nil || rejected != 1 {
		t.Fatalf("rejected approvals = %d err=%v", rejected, err)
	}
	targetApproval, err := svc.store.GetApproval(context.Background(), target.ApprovalID)
	if err != nil || targetApproval.Status != "rejected" {
		t.Fatalf("target approval = %#v err=%v", targetApproval, err)
	}
	otherApproval, err := svc.store.GetApproval(context.Background(), other.ApprovalID)
	if err != nil || otherApproval.Status != "pending" {
		t.Fatalf("unrelated approval changed = %#v err=%v", otherApproval, err)
	}
	if len(transport.calls) != 0 {
		t.Fatalf("rejected approval executed %d commands", len(transport.calls))
	}
}

func TestAbortApprovalsForSessionRejectsPartiallyDecidedAgentGroup(t *testing.T) {
	svc, transport, host := newTestService(t)
	const (
		sessionID    = "session_stop_agent_group"
		checkpointID = "checkpoint_stop_agent_group"
	)
	ctx := WithAgentApprovalContinuation(WithSessionID(context.Background(), sessionID), checkpointID)
	approvals := make([]domain.ExecResult, 0, 2)
	for _, unit := range []string{"first", "second"} {
		result, err := svc.Submit(ctx, domain.ExecRequest{
			HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", unit},
			Reason: "test stopping a partially decided approval group",
		}, "eino-agent")
		if err != nil || result.ApprovalID == "" {
			t.Fatalf("Agent approval = %#v err=%v", result, err)
		}
		approvals = append(approvals, result)
	}
	if err := svc.store.Set(ctx, checkpointID, []byte("checkpoint")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ActivateAgentApprovals(ctx, checkpointID, map[string]string{
		approvals[0].ApprovalID: "interrupt-first", approvals[1].ApprovalID: "interrupt-second",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DecideAgentApproval(ctx, approvals[0].ApprovalID, domain.ApprovalStatusApproved, "approved first", "operator"); err != nil {
		t.Fatal(err)
	}
	aborted, err := svc.AbortApprovalsForSession(ctx, sessionID, "Agent run stopped by the operator", "operator")
	if err != nil || aborted != 2 {
		t.Fatalf("aborted approvals = %d err=%v", aborted, err)
	}
	for _, result := range approvals {
		approval, approvalErr := svc.store.GetApproval(ctx, result.ApprovalID)
		run, runErr := svc.store.GetRun(ctx, result.RunID)
		if approvalErr != nil || runErr != nil || approval.Status != domain.ApprovalStatusRejected || run.Status != domain.ApprovalStatusRejected {
			t.Fatalf("aborted Agent approval=%#v run=%#v errors=%v/%v", approval, run, approvalErr, runErr)
		}
	}
	if len(transport.calls) != 0 {
		t.Fatalf("aborted Agent group executed %d operations", len(transport.calls))
	}
}
