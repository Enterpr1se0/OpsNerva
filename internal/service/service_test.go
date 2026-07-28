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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"eino-ops-agent/internal/config"
	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/policy"
	"eino-ops-agent/internal/security"
	"eino-ops-agent/internal/sshx"
	"eino-ops-agent/internal/store"

	"golang.org/x/crypto/ssh"
)

type fakeTransport struct {
	mu       sync.Mutex
	calls    []domain.ExecRequest
	hosts    []domain.Host
	stdout   []byte
	stderr   []byte
	exitCode int
	execErr  error
}

type fakeCommandExplainer struct {
	mu     sync.Mutex
	review domain.CommandReview
	err    error
	inputs []domain.CommandReviewInput
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

func (f *fakeTransport) Exec(_ context.Context, connection sshx.ConnectionSpec, req domain.ExecRequest) (sshx.RawResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.hosts = append(f.hosts, connection.Target)
	stdout, stderr, exitCode, execErr := f.stdout, f.stderr, f.exitCode, f.execErr
	f.mu.Unlock()
	if stdout == nil {
		stdout = []byte("password=secret-value\nok\n")
	}
	return sshx.RawResult{ExitCode: exitCode, Stdout: stdout, Stderr: stderr, Duration: time.Millisecond}, execErr
}

func TestExecutionPreservesCompleteOutput(t *testing.T) {
	svc, transport, host := newTestService(t)
	wantStdout := strings.Repeat("stdout-data-", 30_000) + "stdout-end"
	wantStderr := strings.Repeat("stderr-data-", 12_000) + "stderr-end"
	transport.mu.Lock()
	transport.stdout = []byte(wantStdout)
	transport.stderr = []byte(wantStderr)
	transport.mu.Unlock()

	result, err := svc.Submit(context.Background(), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "uname", Args: []string{"-a"}, Reason: "verify complete output capture",
	}, "test")
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

func (f *fakeTransport) TransferFile(_ context.Context, source, destination sshx.ConnectionSpec, req domain.ExecRequest) (sshx.RawResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.hosts = append(f.hosts, source.Target, destination.Target)
	f.mu.Unlock()
	return sshx.RawResult{ExitCode: 0, Stdout: []byte(`{"bytes":12,"sha256":"transfer-digest"}` + "\n"), Duration: time.Millisecond}, nil
}

func TestHostCredentialsAreEncryptedPreservedAndNeverSerialized(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	host, err := svc.SaveHost(ctx, domain.HostInput{
		Name: "password-host", Address: "192.0.2.10", Port: 22, User: "ops", AuthType: "password",
		Password: "ssh-super-secret", SudoMode: "password", SudoPassword: "sudo-super-secret",
		ProxyURL: "SOCKS5://127.0.0.1:1080/", ProxyUsername: "proxy-user", ProxyPassword: "proxy-super-secret",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !host.HasPassword || !host.HasSudoPassword || !host.HasProxyPassword || host.ProxyURL != "socks5://127.0.0.1:1080" {
		t.Fatalf("credential capability flags missing: %#v", host)
	}
	stored, err := svc.store.GetHost(ctx, host.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PasswordCipher == "" || stored.SudoCipher == "" || stored.ProxyPasswordCipher == "" || strings.Contains(stored.PasswordCipher, "super-secret") || strings.Contains(stored.SudoCipher, "super-secret") || strings.Contains(stored.ProxyPasswordCipher, "super-secret") {
		t.Fatalf("host credentials were not encrypted: %#v", stored)
	}
	publicJSON, _ := json.Marshal(host)
	if strings.Contains(string(publicJSON), "super-secret") || strings.Contains(string(publicJSON), "cipher") {
		t.Fatalf("host JSON exposed secret material: %s", publicJSON)
	}

	updated, err := svc.SaveHost(ctx, domain.HostInput{
		ID: host.ID, Name: "password-host-renamed", Address: host.Address, Port: host.Port, User: host.User,
		AuthType: "password", SudoMode: "password", ProxyURL: host.ProxyURL, ProxyUsername: host.ProxyUsername,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !updated.HasPassword || !updated.HasSudoPassword || !updated.HasProxyPassword {
		t.Fatalf("blank edit erased stored credentials: %#v", updated)
	}
	hydrated, err := svc.hydrateHostSecrets(updated, true)
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
	host, err := svc.SaveHost(context.Background(), domain.HostInput{
		Name: "sudo-host", Address: "192.0.2.11", Port: 22, User: "ops", AuthType: "password",
		Password: "ssh-secret", SudoMode: "password", SudoPassword: "sudo-secret",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.Submit(context.Background(), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "id", Elevated: true, Reason: "verify managed root access",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if result.Risk != domain.RiskCritical || result.Status != "approval_required" {
		t.Fatalf("elevated request bypassed approval: %#v", result)
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
	requests := []domain.ExecRequest{
		{HostID: host.ID, Mode: domain.ExecProgram, Program: "bash", Reason: "open shell"},
		{HostID: host.ID, Mode: domain.ExecProgram, Program: "pacman", Args: []string{"-S", "nginx"}, Reason: "install nginx", Rollback: "remove nginx"},
		{HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"edit", "nginx"}, Reason: "edit unit", Rollback: "remove override"},
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
		Reason: "restart the example service", ExpectedChanges: "service restarts", Rollback: "restart the previous service version",
	}, "test")
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

func (f *fakeTransport) Probe(context.Context, sshx.ConnectionSpec) (sshx.HostInfo, error) {
	return sshx.HostInfo{Hostname: "fixture"}, nil
}
func (f *fakeTransport) ScanHostKey(context.Context, sshx.ConnectionSpec) (sshx.HostKey, error) {
	return sshx.HostKey{Fingerprint: "SHA256:test"}, nil
}
func (f *fakeTransport) TrustHostKey(context.Context, sshx.ConnectionSpec, string) (sshx.HostKey, error) {
	return sshx.HostKey{Fingerprint: "SHA256:test"}, nil
}

func newTestService(t *testing.T) (*Service, *fakeTransport, domain.Host) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	engine, _ := policy.Load("")
	encryptor, err := security.NewEncryptor("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	transport := &fakeTransport{}
	limits := config.Default().Limits
	svc := New(st, engine, transport, encryptor, security.NewRedactor(), limits)
	t.Cleanup(func() { svc.explainWG.Wait() })
	host, err := svc.AddHost(ctx, domain.Host{Name: "fixture", Address: "127.0.0.1", Port: 22, User: "test"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	return svc, transport, host
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

func TestSystemSettingsValidatePersistAndReturnDefault(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	settings, err := svc.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settings.AgentMaxIterations != domain.DefaultAgentMaxIterations || !settings.ApprovalExplanationsEnabled || settings.SubagentModelProviderID != "" || settings.SubagentTimeoutSeconds != domain.DefaultSubagentTimeoutSeconds || settings.WorkspaceShellMode != domain.WorkspaceShellModeSandbox {
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
	provider, err := svc.SaveModelProvider(ctx, domain.ModelProviderInput{
		Name: "subagent", Kind: "ollama", BaseURL: "http://127.0.0.1:11434/v1", Model: "small-model",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	explanationsEnabled := false
	timeoutSeconds := 45
	hostShell := domain.WorkspaceShellModeHost
	imageTypes := []string{"image/png", "image/webp"}
	systemPrompt := "You are my personal operations agent."
	saved, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{
		AgentMaxIterations: 30, ApprovalExplanationsEnabled: &explanationsEnabled,
		SystemPrompt: &systemPrompt, SubagentModelProviderID: &provider.ID, SubagentTimeoutSeconds: &timeoutSeconds,
		ChatImageAllowedTypes: imageTypes, WorkspaceShellMode: &hostShell,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if saved.AgentMaxIterations != 30 || saved.SystemPrompt != systemPrompt || saved.ApprovalExplanationsEnabled || saved.SubagentModelProviderID != provider.ID || saved.SubagentTimeoutSeconds != timeoutSeconds || strings.Join(saved.ChatImageAllowedTypes, ",") != strings.Join(imageTypes, ",") || saved.WorkspaceShellMode != domain.WorkspaceShellModeHost || saved.UpdatedAt.IsZero() {
		t.Fatalf("unexpected saved settings: %#v", saved)
	}
	reloaded, err := svc.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.AgentMaxIterations != 30 || reloaded.SystemPrompt != systemPrompt || reloaded.ApprovalExplanationsEnabled || reloaded.SubagentModelProviderID != provider.ID || reloaded.SubagentTimeoutSeconds != timeoutSeconds || strings.Join(reloaded.ChatImageAllowedTypes, ",") != strings.Join(imageTypes, ",") || reloaded.WorkspaceShellMode != domain.WorkspaceShellModeHost {
		t.Fatalf("system settings were not persisted: %#v", reloaded)
	}
	if _, err := svc.DeleteModelProvider(ctx, provider.ID, "test"); !errors.Is(err, ErrModelProviderInUse) || !strings.Contains(err.Error(), "selected for the subagent") {
		t.Fatalf("selected subagent provider deletion was not blocked: %v", err)
	}
	invalidMode := "automatic"
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

func TestCommandReviewDeadlineUsesReadableConfiguredTimeout(t *testing.T) {
	svc, _, _ := newTestService(t)
	review := svc.normalizeCommandReview(
		domain.CommandReview{Model: "small-model"},
		fmt.Errorf("[NodeRunError] failed to create chat completion: %w", context.DeadlineExceeded),
		domain.RiskChange,
		45,
	)
	if review.Status != "unavailable" || review.Model != "small-model" || len(review.Errors) != 1 || review.Errors[0] != "command explanation model did not respond within 45 seconds" {
		t.Fatalf("unexpected normalized deadline review: %#v", review)
	}
}

func TestCommandExplainerPersistsAdviceWithoutChangingPolicyRisk(t *testing.T) {
	svc, _, host := newTestService(t)
	explainer := &fakeCommandExplainer{review: domain.CommandReview{
		Status: "completed", DeterministicRisk: domain.RiskReadOnly,
		Explanation: &domain.CommandExplanation{Summary: "重启服务", Mechanism: "由 systemd 停止并重新启动单元"},
		ReviewedAt:  time.Now().UTC(),
	}}
	svc.SetCommandExplainer(explainer)
	result, err := svc.Submit(context.Background(), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"},
		Reason: "recover demo", Rollback: "restart the previous release",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "approval_required" || result.Risk != domain.RiskChange {
		t.Fatalf("explainer changed deterministic approval: %#v", result)
	}
	waitForApproval(t, svc, result.ApprovalID, func(approval domain.Approval) bool {
		return approval.AIReview != nil && approval.AIReview.Status != "pending"
	})
	approvals, err := svc.ListApprovals(context.Background(), "pending", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(approvals) != 1 || approvals[0].AIReview == nil || approvals[0].AIReview.DeterministicRisk != domain.RiskChange || approvals[0].AIReview.Explanation == nil {
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
	svc.SetCommandExplainer(explainer)
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
			Reason: "recover demo", Rollback: "restart the previous release",
		}, "test")
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
	pending := waitForApproval(t, svc, submitted.result.ApprovalID, func(approval domain.Approval) bool {
		return approval.AIReview != nil && approval.AIReview.Status == "pending"
	})
	if pending.Risk != domain.RiskChange {
		t.Fatalf("pending advisory review changed deterministic approval: %#v", pending)
	}

	close(explainer.release)
	released = true
	completed := waitForApproval(t, svc, submitted.result.ApprovalID, func(approval domain.Approval) bool {
		return approval.AIReview != nil && approval.AIReview.Status == "completed"
	})
	if completed.Risk != domain.RiskChange {
		t.Fatalf("completed advisory review unexpectedly changed risk: %#v", completed)
	}
}

func TestApprovalDecisionCancelsCommandExplanation(t *testing.T) {
	svc, transport, host := newTestService(t)
	explainer := &blockingCommandExplainer{
		started: make(chan struct{}), release: make(chan struct{}),
		review: domain.CommandReview{
			Status: "completed", Explanation: &domain.CommandExplanation{Summary: "重启服务", Mechanism: "systemd 重启单元"}, ReviewedAt: time.Now().UTC(),
		},
	}
	svc.SetCommandExplainer(explainer)
	pending, err := svc.Submit(context.Background(), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"},
		Reason: "recover demo", Rollback: "restart the previous release",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-explainer.started:
	case <-time.After(time.Second):
		t.Fatal("background explanation did not start")
	}
	if _, err := svc.Approve(context.Background(), pending.ApprovalID, "deterministic policy reviewed", "operator"); err != nil {
		t.Fatal(err)
	}
	svc.explainWG.Wait()

	approval, err := svc.store.GetApproval(context.Background(), pending.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	if approval.Status != "approved" || approval.Risk != domain.RiskChange {
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
	svc.SetCommandExplainer(explainer)
	results := make([]domain.ExecResult, 0, 3)
	for index := 0; index < 3; index++ {
		result, err := svc.Submit(context.Background(), domain.ExecRequest{
			HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", fmt.Sprintf("demo-%d", index)},
			Reason: "recover demo", Rollback: "restart the previous release",
		}, "test")
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
	svc.SetCommandExplainer(explainer)
	first, err := svc.Submit(context.Background(), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo-one"},
		Reason: "recover demo", Rollback: "restart the previous release",
	}, "test")
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
		Reason: "recover demo", Rollback: "restart the previous release",
	}, "test")
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

func TestRetryApprovalExplanationKeepsPolicyRiskAndDoesNotExecute(t *testing.T) {
	svc, transport, host := newTestService(t)
	ctx := context.Background()
	pending, err := svc.Submit(ctx, domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"},
		Reason: "recover demo", Rollback: "restart the previous release",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if pending.Risk != domain.RiskChange {
		t.Fatalf("unexpected initial approval: %#v", pending)
	}
	explainer := &fakeCommandExplainer{review: domain.CommandReview{
		Status: "completed", Explanation: &domain.CommandExplanation{
			Summary: "重启服务", Mechanism: "systemd 会停止并重新启动服务",
			Risks: []string{"可能短暂中断请求"},
		}, ReviewedAt: time.Now().UTC(),
	}}
	svc.SetCommandExplainer(explainer)

	updated, err := svc.RetryApprovalExplanation(ctx, pending.ApprovalID, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "pending" || updated.Risk != domain.RiskChange || updated.AIReview == nil || updated.AIReview.Explanation == nil {
		t.Fatalf("explanation retry changed the policy-owned approval: %#v", updated)
	}
	if len(transport.calls) != 0 {
		t.Fatalf("explanation retry executed the operation: %#v", transport.calls)
	}
	inputs := explainer.Inputs()
	if len(inputs) != 1 || inputs[0].RequestDigest != updated.RequestDigest {
		t.Fatalf("explanation retry did not receive the exact pending request: %#v", inputs)
	}
	if _, err := svc.Approve(ctx, updated.ID, "reviewed deterministic policy", "operator"); err != nil {
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
		Reason: "recover demo", Rollback: "restart the previous release",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	svc.SetCommandExplainer(&fakeCommandExplainer{err: errors.New("model timed out")})
	updated, err := svc.RetryApprovalExplanation(ctx, pending.ApprovalID, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "pending" || updated.Risk != domain.RiskChange || updated.AIReview == nil || updated.AIReview.Status != "unavailable" {
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
	svc.SetCommandExplainer(explainer)
	pending, err := svc.Submit(context.Background(), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"},
		Reason: "recover demo", Rollback: "restart the previous release",
	}, "test")
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

func TestAgentPlanAdvancesStrictlyOneStepAtATime(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := WithSessionID(context.Background(), "session_plan_test")
	plan, err := svc.CreateAgentPlan(ctx, "Deploy and verify the service", []string{
		"Inspect the project and host", "Deploy the service", "Verify health and rollback readiness",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "active" || len(plan.Steps) != 3 || plan.Steps[0].Status != "in_progress" || plan.Steps[1].Status != "pending" {
		t.Fatalf("unexpected initial plan: %#v", plan)
	}
	if _, err := svc.UpdateAgentPlanStep(ctx, 2, "completed", "not actually current", "test"); err == nil {
		t.Fatal("expected out-of-order step completion to fail")
	} else {
		var transition *store.PlanTransitionError
		if !errors.As(err, &transition) || transition.StepNumber != 2 || transition.Status != "pending" {
			t.Fatalf("out-of-order update did not return a typed transition error: %v", err)
		}
	}
	plan, err = svc.UpdateAgentPlanStep(ctx, 1, "completed", "Inspected README and host facts", "test")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Steps[0].Status != "completed" || plan.Steps[1].Status != "in_progress" || plan.Steps[2].Status != "pending" {
		t.Fatalf("plan did not advance exactly one step: %#v", plan)
	}
	plan, err = svc.UpdateAgentPlanStep(ctx, 2, "blocked", "Package mirror is unavailable", "test")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "blocked" || plan.Steps[1].Status != "blocked" || plan.Steps[2].Status != "pending" {
		t.Fatalf("blocked plan state is inconsistent: %#v", plan)
	}
	loaded, err := svc.GetAgentPlan(context.Background(), "session_plan_test")
	if err != nil || loaded.Status != "blocked" {
		t.Fatalf("plan was not persisted: plan=%#v err=%v", loaded, err)
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
	result, err := svc.Submit(context.Background(), domain.ExecRequest{HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"}, Reason: "recover service", Rollback: "restart previous version"}, "test")
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

func TestFileContentCommandsRequireOneTimeApproval(t *testing.T) {
	for _, req := range []domain.ExecRequest{
		{Mode: domain.ExecProgram, Program: "cat", Args: []string{"/etc/app.conf"}},
		{Mode: domain.ExecScript, Script: "set -e\ngrep token /etc/app.conf"},
		{Mode: domain.ExecRemoteRead, RemotePath: "/etc/app.conf"},
		{Mode: domain.ExecWorkspaceSearch, WorkspaceID: "project", RelativePath: "app.conf", SearchPattern: "token", SearchMatchMode: domain.FileSearchLiteral},
	} {
		if !requiresOneTimeApproval(req) {
			t.Fatalf("file content access permits session approval: %#v", req)
		}
	}
	if requiresOneTimeApproval(domain.ExecRequest{Mode: domain.ExecProgram, Program: "ps", Args: []string{"aux"}}) {
		t.Fatal("ordinary read-only status command was restricted to one-time approval")
	}
}

func TestBlockingApprovalSuspendsToolAndResumesWithExecutionResult(t *testing.T) {
	svc, transport, host := newTestService(t)
	base, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	notifications := make(chan domain.ExecResult, 2)
	ctx := WithSessionID(base, "session_blocking_approval")
	ctx = WithBlockingApprovals(ctx)
	ctx = WithApprovalNotifier(ctx, func(result domain.ExecResult) { notifications <- result })

	type outcome struct {
		result domain.ExecResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := svc.Submit(ctx, domain.ExecRequest{
			HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"},
			Reason: "recover demo service", Rollback: "restart previous version",
		}, "eino-agent")
		done <- outcome{result: result, err: err}
	}()

	var pending domain.ExecResult
	select {
	case pending = <-notifications:
	case <-base.Done():
		t.Fatal("timed out waiting for approval notification")
	}
	if pending.Status != "approval_required" || pending.ApprovalID == "" {
		t.Fatalf("missing pending notification: %#v", pending)
	}
	select {
	case result := <-done:
		t.Fatalf("Tool returned before the human decision: %#v", result)
	case <-time.After(75 * time.Millisecond):
	}

	approved, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed", "operator")
	if err != nil {
		t.Fatal(err)
	}
	var result outcome
	select {
	case result = <-done:
	case <-base.Done():
		t.Fatal("timed out waiting for approved Tool to resume")
	}
	if result.err != nil || result.result.Status != "completed" || result.result.RunID != approved.RunID {
		t.Fatalf("Tool did not resume with execution result: %#v err=%v", result.result, result.err)
	}
	if len(transport.calls) != 1 {
		t.Fatalf("approved operation executed %d times", len(transport.calls))
	}
}

func TestBlockingApprovalReturnsRejectedOperatorInstructionToTool(t *testing.T) {
	svc, transport, host := newTestService(t)
	base, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	notifications := make(chan domain.ExecResult, 1)
	ctx := WithApprovalNotifier(WithBlockingApprovals(WithSessionID(base, "session_rejected_approval")), func(result domain.ExecResult) {
		notifications <- result
	})

	type outcome struct {
		result domain.ExecResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := svc.Submit(ctx, domain.ExecRequest{
			HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"},
			Reason: "recover demo service", Rollback: "restart previous version",
		}, "eino-agent")
		done <- outcome{result: result, err: err}
	}()

	var pending domain.ExecResult
	select {
	case pending = <-notifications:
	case <-base.Done():
		t.Fatal("timed out waiting for approval notification")
	}
	instruction := "不要重启服务，先读取最近 100 行日志并分析。"
	if err := svc.Reject(context.Background(), pending.ApprovalID, instruction, "operator"); err != nil {
		t.Fatal(err)
	}
	var result outcome
	select {
	case result = <-done:
	case <-base.Done():
		t.Fatal("timed out waiting for rejected Tool to resume")
	}
	if result.err != nil || result.result.Status != "rejected" || result.result.OperatorInstruction != instruction {
		t.Fatalf("rejection was not returned to the blocked Tool: %#v err=%v", result.result, result.err)
	}
	select {
	case decision := <-notifications:
		if decision.Status != "rejected" || decision.OperatorInstruction != instruction {
			t.Fatalf("rejection decision notification lost operator context: %#v", decision)
		}
	default:
		t.Fatal("rejection did not notify the active Agent stream")
	}
	if len(transport.calls) != 0 {
		t.Fatalf("rejected operation executed %d times", len(transport.calls))
	}
}

func TestBlockingApprovalAlsoSuspendsMutatingTaskStart(t *testing.T) {
	svc, _, host := newTestService(t)
	base, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	notifications := make(chan domain.ExecResult, 1)
	ctx := WithApprovalNotifier(WithBlockingApprovals(WithSessionID(base, "session_blocking_task")), func(result domain.ExecResult) {
		notifications <- result
	})

	type outcome struct {
		task domain.Task
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		task, err := svc.StartTask(ctx, domain.ExecRequest{
			HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"},
			Reason: "restart demo as a managed task", Rollback: "restart previous version",
		}, "eino-agent")
		done <- outcome{task: task, err: err}
	}()

	var pending domain.ExecResult
	select {
	case pending = <-notifications:
	case <-base.Done():
		t.Fatal("timed out waiting for task approval notification")
	}
	select {
	case result := <-done:
		t.Fatalf("task_start returned before approval decision: %#v", result)
	case <-time.After(75 * time.Millisecond):
	}
	if err := svc.Reject(context.Background(), pending.ApprovalID, "inspect logs instead", "operator"); err != nil {
		t.Fatal(err)
	}
	var result outcome
	select {
	case result = <-done:
	case <-base.Done():
		t.Fatal("timed out waiting for task_start to resume")
	}
	if result.err != nil || result.task.Status != "rejected" || result.task.RunID == "" || result.task.OperatorInstruction != "inspect logs instead" {
		t.Fatalf("unexpected task result after rejection: %#v err=%v", result.task, result.err)
	}
	_, execResult, _, err := svc.GetTask(result.task.ID)
	if err != nil || execResult.OperatorInstruction != "inspect logs instead" {
		t.Fatalf("task result lost operator instruction: %#v err=%v", execResult, err)
	}
}

func TestSessionApprovalGrantMatchesOnlyTheExactOperation(t *testing.T) {
	svc, transport, host := newTestService(t)
	ctx := WithSessionID(context.Background(), "session_grant_test")
	req := domain.ExecRequest{HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"}, Reason: "restart reviewed service", Rollback: "restart previous version"}
	result, err := svc.Submit(ctx, req, "test")
	if err != nil {
		t.Fatal(err)
	}
	approvals, err := svc.ListApprovals(context.Background(), "pending", 10)
	if err != nil || len(approvals) != 1 || approvals[0].SessionID != "session_grant_test" {
		t.Fatalf("approval session association missing: %#v err=%v", approvals, err)
	}
	if _, err := svc.ApproveWithScope(context.Background(), result.ApprovalID, "allow this exact operation in this session", "session", "operator"); err != nil {
		t.Fatal(err)
	}
	req.Reason = "same operation with a different explanation"
	repeated, err := svc.Submit(ctx, req, "test")
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Status != "completed" || len(transport.calls) != 2 {
		t.Fatalf("exact session grant was not reused: %#v calls=%d", repeated, len(transport.calls))
	}
	req.Args = []string{"restart", "different-service"}
	changed, err := svc.Submit(ctx, req, "test")
	if err != nil {
		t.Fatal(err)
	}
	if changed.Status != "approval_required" || len(transport.calls) != 2 {
		t.Fatalf("session grant authorized a different operation: %#v calls=%d", changed, len(transport.calls))
	}
}

func TestCriticalApprovalCannotCreateSessionGrant(t *testing.T) {
	svc, _, host := newTestService(t)
	ctx := WithSessionID(context.Background(), "session_critical_test")
	result, err := svc.Submit(ctx, domain.ExecRequest{HostID: host.ID, Mode: domain.ExecProgram, Program: "rm", Args: []string{"-rf", "/tmp/demo"}, Reason: "critical test", Rollback: "restore snapshot"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApproveWithScope(context.Background(), result.ApprovalID, "reviewed", "session", "operator"); err == nil || !strings.Contains(err.Error(), "cannot be approved") {
		t.Fatalf("critical session grant was accepted: %v", err)
	}
}

func TestCriticalRequiresApprovalReason(t *testing.T) {
	svc, transport, host := newTestService(t)
	result, err := svc.Submit(context.Background(), domain.ExecRequest{HostID: host.ID, Mode: domain.ExecProgram, Program: "rm", Args: []string{"-rf", "/tmp/demo"}, Reason: "clean fixture", Rollback: "restore snapshot"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if result.Risk != domain.RiskCritical || result.Status != "approval_required" {
		t.Fatalf("unexpected critical result %#v", result)
	}
	if _, err := svc.Approve(context.Background(), result.ApprovalID, "", "operator"); err == nil {
		t.Fatal("critical approval without a reason was accepted")
	}
	if len(transport.calls) != 0 {
		t.Fatal("critical command executed before approval")
	}
	approved, err := svc.Approve(context.Background(), result.ApprovalID, "fixture cleanup reviewed", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != "completed" || len(transport.calls) != 1 {
		t.Fatalf("unexpected approved result %#v", approved)
	}
}

func TestForbiddenNeverCreatesApproval(t *testing.T) {
	svc, transport, host := newTestService(t)
	result, err := svc.Submit(context.Background(), domain.ExecRequest{HostID: host.ID, Mode: domain.ExecScript, Script: "cat ~/.ssh/id_ed25519", Reason: "bad request"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "denied" || result.ApprovalID != "" || len(transport.calls) != 0 {
		t.Fatalf("unexpected forbidden result %#v", result)
	}
}

func TestModelProvidersEncryptKeysAndSwitchActiveProvider(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	first, err := svc.SaveModelProvider(ctx, domain.ModelProviderInput{
		Name: "primary", Kind: "openai", Model: "gpt-test", APIKey: "sk-super-secret",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !first.HasAPIKey || first.Active {
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
	if updatedCfg.APIKey != "sk-super-secret" {
		t.Fatal("blank API key update did not preserve the encrypted key")
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
	provider, err := svc.SaveModelProvider(ctx, domain.ModelProviderInput{
		Name: "proxied", Kind: "openai_compatible", BaseURL: "http://model.invalid/v1", Model: "proxied-model",
		ProxyURL: proxy.URL + "/", ProxyUsername: "proxy-user", ProxyPassword: proxyPassword,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if provider.ProxyURL != proxy.URL || provider.ProxyUsername != "proxy-user" || !provider.HasProxyPassword {
		t.Fatalf("unexpected public proxy configuration: %#v", provider)
	}
	serialized, err := json.Marshal(provider)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(serialized), proxyPassword) || strings.Contains(string(serialized), "cipher") {
		t.Fatalf("provider JSON exposed proxy credentials: %s", serialized)
	}
	stored, err := svc.store.GetModelProvider(ctx, provider.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ProxyPasswordCipher == "" || strings.Contains(stored.ProxyPasswordCipher, proxyPassword) {
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
		ProxyURL: provider.ProxyURL, ProxyUsername: provider.ProxyUsername,
	}, "test")
	if err != nil || !preserved.HasProxyPassword {
		t.Fatalf("blank proxy password did not preserve the stored password: provider=%#v err=%v", preserved, err)
	}
	changed, err := svc.SaveModelProvider(ctx, domain.ModelProviderInput{
		ID: provider.ID, Name: provider.Name, Kind: provider.Kind, BaseURL: provider.BaseURL, Model: provider.Model,
		ProxyURL: provider.ProxyURL, ProxyUsername: "different-user",
	}, "test")
	if err != nil || changed.HasProxyPassword {
		t.Fatalf("changed proxy identity reused the stored password: provider=%#v err=%v", changed, err)
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
	if catalog.Count != 2 || strings.Join(catalog.Models, ",") != "a-model,z-model" {
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
		if r.Header.Get("User-Agent") != "OpsPilot-Test/1.0" {
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
		UserAgent: ptr("OpsPilot-Test/1.0"),
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if provider.BaseURL != server.URL {
		t.Fatalf("expected version segment stripped from base URL, got %q", provider.BaseURL)
	}
	if provider.UserAgent != "OpsPilot-Test/1.0" {
		t.Fatalf("user agent was not persisted, got %q", provider.UserAgent)
	}
	renamed, err := svc.SaveModelProvider(ctx, domain.ModelProviderInput{
		ID: provider.ID, Name: "claude renamed", Kind: "anthropic", BaseURL: server.URL, Model: "claude-opus-4-8",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if renamed.UserAgent != "OpsPilot-Test/1.0" {
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
		UserAgent: ptr("OpsPilot-Test/1.0"),
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
	svc, _, _ := newTestService(t)
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
	sessions, err := svc.ListChatSessions(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 || sessions[0].ID != "session-two" || sessions[1].Title != "Investigate disk usage" || sessions[1].MessageCount != 4 {
		t.Fatalf("unexpected sessions %#v", sessions)
	}
	messages, err := svc.ListChatMessages(ctx, "session-one", 10)
	if err != nil || len(messages) != 4 || messages[1].Role != "assistant" || messages[2].Role != "reasoning" || messages[3].Role != "tool" || messages[3].ToolName != "ssh_exec" {
		t.Fatalf("unexpected messages %#v err=%v", messages, err)
	}
	modelMessages, err := svc.store.ListChatModelMessages(ctx, "session-one", 10)
	if err != nil || len(modelMessages) != 2 || modelMessages[0].Role != "user" || modelMessages[1].Role != "assistant" {
		t.Fatalf("reasoning and tool history leaked into model messages: %#v err=%v", modelMessages, err)
	}
	if err := svc.DeleteChatSession(ctx, "session-one", "test"); err != nil {
		t.Fatal(err)
	}
	messages, err = svc.ListChatMessages(ctx, "session-one", 10)
	if err != nil || len(messages) != 0 {
		t.Fatalf("deleted messages still exist: %#v err=%v", messages, err)
	}
	if err := svc.DeleteChatSession(ctx, "session-one", "test"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected not found on second delete, got %v", err)
	}
}

func TestRejectPendingApprovalsForSession(t *testing.T) {
	svc, transport, host := newTestService(t)
	request := domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"},
		Reason: "recover demo service", Rollback: "restart the previous version",
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

	rejected, err := svc.RejectPendingApprovalsForSession(context.Background(), "session_stop", "Agent run stopped by the operator", "operator")
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
