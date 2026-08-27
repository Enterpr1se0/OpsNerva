package service

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"
	"github.com/Enterpr1se0/opsnerva/internal/store"
)

type fakeShellSession struct {
	mu          sync.Mutex
	callback    func(string, []byte)
	done        chan struct{}
	once        sync.Once
	cols        int
	rows        int
	inputs      [][]byte
	exit        sshx.ShellExit
	outputDelay time.Duration
}

func (s *fakeShellSession) HostStatus(context.Context) (sshx.HostStatus, error) {
	return sshx.HostStatus{
		CPUTotal: 1000, CPUIdle: 250,
		MemoryUsedBytes: 3 << 30, MemoryTotalBytes: 8 << 30,
		DiskUsedBytes: 10 << 30, DiskTotalBytes: 50 << 30,
		NetworkReceivedBytes: 123456, NetworkSentBytes: 654321,
		UptimeSeconds: 93784, SampledAt: time.Now().UTC(),
	}, nil
}

func (f *fakeTransport) OpenShell(_ context.Context, connection sshx.ConnectionSpec, _ domain.ExecRequest, cols, rows int, callback func(string, []byte)) (sshx.ShellSession, error) {
	f.mu.Lock()
	f.shellConnectionShells = append(f.shellConnectionShells, connection.ShellPath)
	f.mu.Unlock()
	code := 0
	session := &fakeShellSession{
		callback: callback, done: make(chan struct{}), cols: cols, rows: rows,
		exit: sshx.ShellExit{ExitCode: &code},
	}
	callback("stdout", []byte("fixture@test:$ "))
	return session, nil
}

func (s *fakeShellSession) Write(data []byte) (int, error) {
	select {
	case <-s.done:
		return 0, io.ErrClosedPipe
	default:
	}
	s.mu.Lock()
	s.inputs = append(s.inputs, append([]byte(nil), data...))
	delay := s.outputDelay
	s.mu.Unlock()
	output := append([]byte(nil), data...)
	if delay <= 0 {
		s.callback("stdout", output)
	} else {
		go func() {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-s.done:
			case <-timer.C:
				s.callback("stdout", output)
			}
		}()
	}
	return len(data), nil
}

func TestWriteSSHShellDelaysBeforeReadingOutput(t *testing.T) {
	svc, _, host := newTestService(t)
	ctx := WithSessionID(context.Background(), "session-delayed-shell")
	if _, err := svc.PrepareChatSession(ctx, "session-delayed-shell", "", "test"); err != nil {
		t.Fatal(err)
	}
	pending, err := svc.StartSSHShell(ctx, host.ID, "", false, 80, 24, "test delayed shell output", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	approved, err := svc.Approve(context.Background(), pending.ApprovalID, "approved", "test")
	if err != nil {
		t.Fatal(err)
	}
	shellID := approved.Shell.ID
	svc.shellMu.RLock()
	session := svc.shells[shellID].session.(*fakeShellSession)
	svc.shellMu.RUnlock()
	session.mu.Lock()
	session.outputDelay = 40 * time.Millisecond
	session.mu.Unlock()

	started := time.Now()
	page, err := svc.WriteSSHShellPage(context.Background(), shellID, "session-delayed-shell", "echo delayed\r", 80*time.Millisecond, 0, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 70*time.Millisecond {
		t.Fatalf("shell input returned before its query delay: %s", elapsed)
	}
	snapshot := page.Snapshot
	var output strings.Builder
	for _, event := range snapshot.Events {
		if event.Stream == "stdout" {
			output.WriteString(event.Content)
		}
	}
	if output.String() != "echo delayed\r" {
		t.Fatalf("delayed shell output was not returned with input: %q", output.String())
	}
}

func TestOperatorCanStartShellWithoutAgentConversation(t *testing.T) {
	svc, _, host := newTestService(t)
	ctx := context.Background()
	runsBefore, err := svc.store.SearchRuns(ctx, "", "", "", 500)
	if err != nil {
		t.Fatal(err)
	}
	auditBefore, err := svc.store.ListAudit(ctx, "", 500)
	if err != nil {
		t.Fatal(err)
	}

	shell, err := svc.StartOperatorSSHShell(ctx, host.ID, domain.SSHShellSurfaceWorkspace, "admin-web")
	if err != nil {
		t.Fatal(err)
	}
	if shell.HostID != host.ID || shell.RunID != "" || shell.SessionID != "" || shell.Surface != domain.SSHShellSurfaceWorkspace || shell.Status != "running" || shell.Elevated {
		t.Fatalf("unexpected operator shell: %#v", shell)
	}
	assertNoPendingApprovals(t, svc)
	stored, err := svc.store.ListSSHShells(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 0 {
		t.Fatalf("operator terminal was persisted: %#v", stored)
	}
	svc.shellMu.RLock()
	operatorSession := svc.shells[shell.ID].session.(*fakeShellSession)
	svc.shellMu.RUnlock()
	operatorSession.callback("stdout", []byte("Password:"))
	credentialInput := "password=operator-secret\r"
	if err := svc.SendSSHShellInput(ctx, shell.ID, "", credentialInput, "", "admin-web"); err != nil {
		t.Fatalf("operator terminal reused Agent credential isolation: %v", err)
	}
	if err := svc.SendSSHShellInput(ctx, shell.ID, "", "\x00", "", "admin-web"); err != nil {
		t.Fatalf("operator terminal rejected a PTY control byte: %v", err)
	}
	snapshot, err := svc.GetSSHShellSnapshot(ctx, shell.ID, "", 0, 0, false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	for _, event := range snapshot.Events {
		if event.Stream == "input" {
			t.Fatalf("operator input was retained as an event: %#v", event)
		}
		if event.Stream == "stdout" {
			output.WriteString(event.Content)
		}
	}
	if !strings.Contains(output.String(), credentialInput) {
		t.Fatalf("operator terminal output was filtered: %q", output.String())
	}
	if _, err := svc.CloseSSHShell(ctx, shell.ID, "", "", "admin-web"); err != nil {
		t.Fatal(err)
	}

	quickShell, err := svc.StartOperatorSSHShell(context.Background(), host.ID, "", "admin-web")
	if err != nil {
		t.Fatal(err)
	}
	if quickShell.Surface != domain.SSHShellSurfaceQuick {
		t.Fatalf("quick shell has unexpected surface: %#v", quickShell)
	}
	if _, err := svc.CloseSSHShell(context.Background(), quickShell.ID, "", "", "admin-web"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartOperatorSSHShell(context.Background(), host.ID, "invalid", "admin-web"); err == nil {
		t.Fatal("invalid shell surface was accepted")
	}
	runsAfter, err := svc.store.SearchRuns(ctx, "", "", "", 500)
	if err != nil {
		t.Fatal(err)
	}
	auditAfter, err := svc.store.ListAudit(ctx, "", 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(runsAfter) != len(runsBefore) || len(auditAfter) != len(auditBefore) {
		t.Fatalf("operator terminal changed durable execution history: runs %d -> %d, audit %d -> %d", len(runsBefore), len(runsAfter), len(auditBefore), len(auditAfter))
	}
}

func TestAgentRootShellInputStopsAfterAccessIsRevoked(t *testing.T) {
	svc, _, _ := newTestService(t)
	host, err := svc.AddHost(context.Background(), domain.Host{
		Name: "root shell fixture", Address: "127.0.0.2", Port: 22, User: "root", AgentEnabled: true,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	host, err = svc.SetHostAgentRootEnabled(context.Background(), host.ID, true, "test")
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "session-root-shell"
	ctx := WithSessionID(context.Background(), sessionID)
	if _, err := svc.PrepareChatSession(ctx, sessionID, "", "test"); err != nil {
		t.Fatal(err)
	}
	pending, err := svc.StartSSHShell(ctx, host.ID, "", false, 80, 24, "test root shell revocation", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	approved, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed", "operator")
	if err != nil {
		t.Fatal(err)
	}
	shellID := approved.Shell.ID
	svc.shellMu.RLock()
	session := svc.shells[shellID].session.(*fakeShellSession)
	svc.shellMu.RUnlock()
	if _, err := svc.SetHostAgentRootEnabled(ctx, host.ID, false, "operator"); err != nil {
		t.Fatal(err)
	}
	if err := svc.SendSSHShellInput(ctx, shellID, sessionID, "id\r", "test revoked input", "eino-agent"); !errors.Is(err, ErrAgentRootAccessDenied) {
		t.Fatalf("revoked root shell input error = %v", err)
	}
	session.mu.Lock()
	inputs := len(session.inputs)
	session.mu.Unlock()
	if inputs != 0 {
		t.Fatalf("revoked root shell received %d input writes", inputs)
	}
	if _, err := svc.CloseSSHShell(context.Background(), shellID, sessionID, "test cleanup", "operator"); err != nil {
		t.Fatal(err)
	}
}

func TestGetSSHShellHostStatusUsesActiveConnection(t *testing.T) {
	svc, _, host := newTestService(t)
	shell, err := svc.StartOperatorSSHShell(context.Background(), host.ID, domain.SSHShellSurfaceWorkspace, "admin-web")
	if err != nil {
		t.Fatal(err)
	}
	status, err := svc.GetSSHShellHostStatus(context.Background(), shell.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.CPUTotal != 1000 || status.MemoryTotalBytes != 8<<30 || status.NetworkSentBytes != 654321 || status.UptimeSeconds != 93784 {
		t.Fatalf("unexpected host status: %#v", status)
	}
}

func TestAgentShellOutputStreamsBeforeTheToolResult(t *testing.T) {
	svc, _, host := newTestService(t)
	const sessionID = "session-live-shell"
	ctx := WithSessionID(context.Background(), sessionID)
	if _, err := svc.PrepareChatSession(ctx, sessionID, "", "test"); err != nil {
		t.Fatal(err)
	}
	pending, err := svc.StartSSHShell(ctx, host.ID, "", false, 80, 24, "test live shell output", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	approved, err := svc.Approve(context.Background(), pending.ApprovalID, "approved", "test")
	if err != nil {
		t.Fatal(err)
	}

	events, unsubscribe := svc.SubscribeExecutionEvents(sessionID)
	defer unsubscribe()
	shellEvents, overflow, unsubscribeShell := svc.SubscribeSSHShellEvents(approved.Shell.ID)
	defer unsubscribeShell()
	toolCtx := WithExecutionOwner(context.Background(), "call-shell-input", "ssh_shell", `{"action":"input"}`)
	if _, err := svc.WriteSSHShellPage(toolCtx, approved.Shell.ID, sessionID, "echo live\r", 0, 0, "", "eino-agent"); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event.ToolCallID != "call-shell-input" || event.ToolName != "ssh_shell" || event.RunID != approved.Shell.ID || event.Stream != "stdout" || event.Content != "echo live\r" {
			t.Fatalf("live shell event is not bound to the input tool: %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("live shell output was not published")
	}
	select {
	case event := <-shellEvents:
		if event.ShellID != approved.Shell.ID || event.Stream != "input" || event.Sequence == 0 {
			t.Fatalf("live shell bus event is invalid: %#v", event)
		}
	case <-overflow:
		t.Fatal("live shell subscriber overflowed")
	case <-time.After(time.Second):
		t.Fatal("live shell event was not published")
	}
}

func TestSSHShellOutputBatchesPersistenceUntilFlush(t *testing.T) {
	svc, _, _ := newTestService(t)
	state := &sshShellState{
		shell:   domain.SSHShell{ID: "shell-batched-output", Kind: domain.SSHShellKindSSH, Status: "running", StartedAt: time.Now().UTC()},
		pending: make(map[string]string), notify: make(chan struct{}),
	}
	if err := svc.store.CreateSSHShell(context.Background(), state.shell); err != nil {
		t.Fatal(err)
	}
	state.eventMu.Lock()
	readable := svc.appendSSHShellOutputEventsLocked(state, "stdout", "firstsecond")
	if readable != "firstsecond" || len(state.persistEvents) != 1 {
		state.eventMu.Unlock()
		t.Fatalf("queued output = %q, pending events = %d", readable, len(state.persistEvents))
	}
	storedBefore, err := svc.store.GetSSHShell(context.Background(), state.shell.ID)
	if err != nil {
		state.eventMu.Unlock()
		t.Fatal(err)
	}
	if storedBefore.LastSequence != 0 {
		state.eventMu.Unlock()
		t.Fatalf("output persisted before batch flush: sequence=%d", storedBefore.LastSequence)
	}
	if err := svc.flushSSHShellEventsLocked(state); err != nil {
		state.eventMu.Unlock()
		t.Fatal(err)
	}
	state.eventMu.Unlock()
	storedAfter, err := svc.store.GetSSHShell(context.Background(), state.shell.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedAfter.LastSequence != 1 || len(state.persistEvents) != 0 {
		t.Fatalf("batch flush state: sequence=%d pending=%d", storedAfter.LastSequence, len(state.persistEvents))
	}
}

func TestSSHShellUsageDescribesIncrementalOutput(t *testing.T) {
	usage := sshShellUsage()
	if usage == nil || !strings.Contains(usage.Input, "bounded output page") || !strings.Contains(usage.Output, "next_sequence") || !strings.Contains(usage.Output, "after_sequence") {
		t.Fatalf("shell usage does not describe incremental output: %#v", usage)
	}
}

func TestShellOutputPagePaginatesReadableStreamsWithoutLoss(t *testing.T) {
	svc, _, host := newTestService(t)
	const sessionID = "session-shell-pages"
	ctx := WithSessionID(context.Background(), sessionID)
	if _, err := svc.PrepareChatSession(ctx, sessionID, "", "test"); err != nil {
		t.Fatal(err)
	}
	pending, err := svc.StartSSHShell(ctx, host.ID, "", false, 80, 24, "test paged shell output", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	approved, err := svc.Approve(context.Background(), pending.ApprovalID, "approved", "test")
	if err != nil {
		t.Fatal(err)
	}
	shell, err := svc.store.GetSSHShell(context.Background(), approved.Shell.ID)
	if err != nil {
		t.Fatal(err)
	}
	after := shell.LastSequence
	svc.shellMu.RLock()
	session := svc.shells[shell.ID].session.(*fakeShellSession)
	svc.shellMu.RUnlock()
	session.callback("stdout", []byte("alpha"))
	session.callback("stderr", []byte("warn"))
	session.callback("stdout", []byte("omega"))

	first, err := svc.QuerySSHShellOutput(context.Background(), shell.ID, sessionID, &after, 0, 6, "", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	readableFirst, err := svc.ReadableSSHShellSnapshot(context.Background(), first.Snapshot, after)
	if err != nil {
		t.Fatal(err)
	}
	if !first.HasMore || len(readableFirst.Events) != 1 || readableFirst.Events[0].Stream != "stdout" || readableFirst.Events[0].Content != "alpha" {
		t.Fatalf("first output page = %#v", first)
	}

	secondAfter := first.Snapshot.NextSequence
	second, err := svc.QuerySSHShellOutput(context.Background(), shell.ID, sessionID, &secondAfter, 0, 6, "", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	readableSecond, err := svc.ReadableSSHShellSnapshot(context.Background(), second.Snapshot, secondAfter)
	if err != nil {
		t.Fatal(err)
	}
	if !second.HasMore || len(readableSecond.Events) != 1 || readableSecond.Events[0].Stream != "stderr" || readableSecond.Events[0].Content != "warn" {
		t.Fatalf("second output page = %#v", second)
	}

	thirdAfter := second.Snapshot.NextSequence
	third, err := svc.QuerySSHShellOutput(context.Background(), shell.ID, sessionID, &thirdAfter, 0, 6, "", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	readableThird, err := svc.ReadableSSHShellSnapshot(context.Background(), third.Snapshot, thirdAfter)
	if err != nil {
		t.Fatal(err)
	}
	if third.HasMore || len(readableThird.Events) != 1 || readableThird.Events[0].Content != "omega" {
		t.Fatalf("third output page = %#v", third)
	}
	stored, err := svc.store.GetSSHShell(context.Background(), shell.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ResponseSequence != third.Snapshot.NextSequence {
		t.Fatalf("persisted response sequence = %d, want %d", stored.ResponseSequence, third.Snapshot.NextSequence)
	}
	cursor, err := svc.sshShellResponseCursor(context.Background(), shell.ID, sessionID)
	if err != nil || cursor != third.Snapshot.NextSequence {
		t.Fatalf("restored response cursor = %d, %v", cursor, err)
	}
}

func TestShellOutputQueryDelayIsNotWokenByOutput(t *testing.T) {
	svc, _, host := newTestService(t)
	const sessionID = "session-shell-wait-deadline"
	ctx := WithSessionID(context.Background(), sessionID)
	if _, err := svc.PrepareChatSession(ctx, sessionID, "", "test"); err != nil {
		t.Fatal(err)
	}
	pending, err := svc.StartSSHShell(ctx, host.ID, "", false, 80, 24, "test shell wait deadline", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	approved, err := svc.Approve(context.Background(), pending.ApprovalID, "approved", "test")
	if err != nil {
		t.Fatal(err)
	}
	shell, err := svc.store.GetSSHShell(context.Background(), approved.Shell.ID)
	if err != nil {
		t.Fatal(err)
	}
	after := shell.LastSequence
	svc.shellMu.RLock()
	session := svc.shells[shell.ID].session.(*fakeShellSession)
	svc.shellMu.RUnlock()
	go func() {
		time.Sleep(10 * time.Millisecond)
		session.callback("stdout", []byte("arrived-early\n"))
	}()
	started := time.Now()
	page, err := svc.QuerySSHShellOutput(context.Background(), shell.ID, sessionID, &after, 80*time.Millisecond, 1024, "", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 70*time.Millisecond {
		t.Fatalf("shell output returned before its query delay: %s", elapsed)
	}
	if len(page.Snapshot.Events) != 1 || page.Snapshot.Events[0].Content != "arrived-early\n" {
		t.Fatalf("delayed shell output page = %#v", page)
	}
}

func TestShellOutputQueryDelayHonorsCancellationAndLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := waitShellQueryDelay(ctx, time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled shell query delay = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("canceled shell query delay returned too late: %s", elapsed)
	}
	if err := validateShellQueryDelay(maxShellQueryDelay + time.Second); err == nil {
		t.Fatal("out-of-range shell query delay was accepted")
	}
}

func TestContinuousShellOutputIsReadAfterQueryDelayWithoutStoppingThePTY(t *testing.T) {
	svc, _, host := newTestService(t)
	const sessionID = "session-continuous-shell"
	ctx := WithSessionID(context.Background(), sessionID)
	if _, err := svc.PrepareChatSession(ctx, sessionID, "", "test"); err != nil {
		t.Fatal(err)
	}
	pending, err := svc.StartSSHShell(ctx, host.ID, "", false, 80, 24, "test continuous shell output", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	approved, err := svc.Approve(context.Background(), pending.ApprovalID, "approved", "test")
	if err != nil {
		t.Fatal(err)
	}
	state, before, err := svc.liveSSHShell(approved.Shell.ID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SendSSHShellInput(context.Background(), approved.Shell.ID, sessionID, "top\r", "", "eino-agent"); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				state.session.(*fakeShellSession).callback("stdout", []byte("top-frame\n"))
			}
		}
	}()
	started := time.Now()
	page, err := svc.QuerySSHShellOutput(context.Background(), approved.Shell.ID, sessionID, &before, 120*time.Millisecond, 0, "", "")
	close(stop)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 90*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("continuous output collection duration = %s", elapsed)
	}
	if page.Snapshot.Shell.Status != "running" || len(page.Snapshot.Events) == 0 {
		t.Fatalf("continuous program did not return a live output batch: %#v", page.Snapshot)
	}
}

func TestShellOutputIncludesDataProducedBetweenToolCalls(t *testing.T) {
	svc, _, host := newTestService(t)
	const sessionID = "session-shell-gap"
	ctx := WithSessionID(context.Background(), sessionID)
	if _, err := svc.PrepareChatSession(ctx, sessionID, "", "test"); err != nil {
		t.Fatal(err)
	}
	pending, err := svc.StartSSHShell(ctx, host.ID, "", false, 80, 24, "test shell output gap", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	approved, err := svc.Approve(context.Background(), pending.ApprovalID, "approved", "test")
	if err != nil {
		t.Fatal(err)
	}
	inputCtx := WithExecutionOwner(context.Background(), "call-shell-input", "ssh_shell", `{"action":"input"}`)
	inputPage, err := svc.WriteSSHShellPage(inputCtx, approved.Shell.ID, sessionID, "long-command\r", 0, 0, "", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	inputSnapshot := inputPage.Snapshot
	svc.shellMu.RLock()
	state := svc.shells[approved.Shell.ID]
	svc.shellMu.RUnlock()
	state.session.(*fakeShellSession).callback("stdout", []byte("late-result\n"))

	outputCtx := WithExecutionOwner(context.Background(), "call-shell-output", "ssh_shell", `{"action":"output"}`)
	outputPage, err := svc.QuerySSHShellOutput(outputCtx, approved.Shell.ID, sessionID, nil, 0, 0, "", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	outputSnapshot := outputPage.Snapshot
	var output strings.Builder
	for _, event := range outputSnapshot.Events {
		if event.Stream == "stdout" {
			output.WriteString(event.Content)
		}
	}
	if output.String() != "late-result\n" || outputSnapshot.NextSequence <= inputSnapshot.NextSequence {
		t.Fatalf("output between tool calls was skipped or repeated: input=%d output=%#v", inputSnapshot.NextSequence, outputSnapshot)
	}
}

func (s *fakeShellSession) Resize(cols, rows int) error {
	s.mu.Lock()
	s.cols, s.rows = cols, rows
	s.mu.Unlock()
	return nil
}

func (s *fakeShellSession) Interrupt() error {
	_, err := s.Write([]byte{0x03})
	return err
}

func (s *fakeShellSession) Wait() sshx.ShellExit {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exit
}

func (s *fakeShellSession) Close() error {
	s.once.Do(func() { close(s.done) })
	return nil
}

func (s *fakeShellSession) finish(exit sshx.ShellExit) {
	s.mu.Lock()
	s.exit = exit
	s.mu.Unlock()
	s.once.Do(func() { close(s.done) })
}

func TestInteractiveSSHShellApprovalIsolationCompleteOutputAndSensitiveRedaction(t *testing.T) {
	svc, _, host := newTestService(t)
	ctx := WithSessionID(context.Background(), "session-shell")
	if _, err := svc.PrepareChatSession(ctx, "session-shell", "", "test"); err != nil {
		t.Fatal(err)
	}
	pending, err := svc.StartSSHShell(ctx, host.ID, "/tmp", false, 90, 24, "run an interactive test", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != "approval_required" || pending.ApprovalID == "" {
		t.Fatalf("interactive shell did not require one-time approval: %#v", pending)
	}
	approved, err := svc.Approve(context.Background(), pending.ApprovalID, "approved for test", "test")
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != "completed" || approved.Shell == nil || approved.Shell.Status != "running" {
		t.Fatalf("approved shell was not started: %#v", approved)
	}
	if approved.ShellUsage == nil || !strings.Contains(approved.ShellUsage.Input, "submit=true") {
		t.Fatalf("shell start did not return its minimal usage guide: %#v", approved)
	}
	shellID := approved.Shell.ID

	page, err := svc.WriteSSHShellPage(context.Background(), shellID, "session-shell", "printf hello\r", 0, 0, "test input", "test")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := page.Snapshot
	var output strings.Builder
	for _, event := range snapshot.Events {
		if event.Stream == "stdout" {
			output.WriteString(event.Content)
		}
	}
	if !strings.Contains(output.String(), "printf hello") {
		t.Fatalf("ordinary shell output was not returned completely: %q", output.String())
	}
	if len(snapshot.Events) < 2 || snapshot.Events[0].Stream != "input" || snapshot.Events[0].Source != "operator" {
		t.Fatalf("shell input source was not recorded before output: %#v", snapshot.Events)
	}
	if !strings.Contains(snapshot.RecentOutput, "printf hello") || snapshot.NextSequence == 0 {
		t.Fatalf("readable shell snapshot is incomplete: %#v", snapshot)
	}

	listed, err := svc.ListSSHShells(context.Background(), "session-shell", true, "find the test shell", "test")
	if err != nil {
		t.Fatal(err)
	}
	if listed.Count != 1 || listed.Shells[0].ID != shellID {
		t.Fatalf("session shell list is incomplete: %#v", listed)
	}

	largeInput := strings.Repeat("output-", 8_000) + "\r"
	page, err = svc.WriteSSHShellPage(context.Background(), shellID, "session-shell", largeInput, 0, 0, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	snapshot = page.Snapshot
	output.Reset()
	for _, event := range snapshot.Events {
		if event.Stream == "stdout" {
			output.WriteString(event.Content)
		}
	}
	if output.String() != largeInput {
		t.Fatalf("interactive shell output was truncated: got=%d want=%d", output.Len(), len(largeInput))
	}
	if _, err := svc.WriteSSHShellPage(context.Background(), shellID, "different-session", "whoami\r", 0, 0, "", "test"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-session shell input was not hidden: %v", err)
	}

	svc.shellMu.RLock()
	fakeSession := svc.shells[shellID].session.(*fakeShellSession)
	svc.shellMu.RUnlock()
	shellBeforeCoalesce, err := svc.store.GetSSHShell(context.Background(), shellID)
	if err != nil {
		t.Fatal(err)
	}
	fakeSession.callback("stdout", []byte("first"))
	fakeSession.callback("stdout", []byte("-second"))
	coalesced, err := svc.GetSSHShellSnapshot(context.Background(), shellID, "session-shell", shellBeforeCoalesce.LastSequence, 0, true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(coalesced.Events) != 1 || coalesced.Events[0].Content != "first-second" ||
		coalesced.Events[0].FirstSequence == coalesced.Events[0].Sequence {
		t.Fatalf("adjacent output events were not coalesced losslessly: %#v", coalesced.Events)
	}
	fakeSession.callback("stdout", []byte("before\x1b[?20"))
	insideANSI, err := svc.GetSSHShellSnapshot(context.Background(), shellID, "session-shell", 0, 0, false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	shellInsideANSI := insideANSI.Shell
	fakeSession.callback("stdout", []byte("04lafter"))
	incremental, err := svc.GetSSHShellSnapshot(context.Background(), shellID, "session-shell", shellInsideANSI.LastSequence, 0, false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	readable, err := svc.ReadableSSHShellSnapshot(context.Background(), incremental, shellInsideANSI.LastSequence)
	if err != nil {
		t.Fatal(err)
	}
	if len(readable.Events) != 1 || readable.Events[0].Content != "after" || strings.Contains(readable.Events[0].Content, "2004l") {
		t.Fatalf("incremental ANSI sequence leaked into readable output: %#v", readable.Events)
	}
	if strings.Contains(incremental.RecentOutput, "2004l") || !strings.Contains(incremental.RecentOutput, "beforeafter") {
		t.Fatalf("readable terminal snapshot leaked split ANSI: %q", incremental.RecentOutput)
	}
	fakeSession.callback("stdout", []byte("Password:"))
	if _, err := svc.WriteSSHShellPage(context.Background(), shellID, "session-shell", "should-not-be-sent\r", 0, 0, "", "test"); err == nil || !strings.Contains(err.Error(), "private Web terminal") {
		t.Fatalf("Agent input was accepted at a credential prompt: %v", err)
	}
	beforeSecretSnapshot, err := svc.GetSSHShellSnapshot(context.Background(), shellID, "session-shell", 0, 0, false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	shellBeforeSecret := beforeSecretSnapshot.Shell
	before := shellBeforeSecret.LastSequence
	if err := svc.WriteSensitiveSSHShellInput(context.Background(), shellID, "very-secret-password\r", "test"); err != nil {
		t.Fatal(err)
	}
	snapshot, err = svc.GetSSHShellSnapshot(context.Background(), shellID, "", before, time.Second, false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	output.Reset()
	for _, event := range snapshot.Events {
		output.WriteString(event.Content)
	}
	if strings.Contains(output.String(), "very-secret-password") || !strings.Contains(output.String(), "[REDACTED]") {
		t.Fatalf("sensitive terminal echo was exposed: %q", output.String())
	}

	resized, err := svc.ResizeSSHShell(context.Background(), shellID, 132, 41, "test")
	if err != nil || resized.Cols != 132 || resized.Rows != 41 {
		t.Fatalf("shell resize failed: shell=%#v err=%v", resized, err)
	}
	if _, err := svc.InterruptSSHShell(context.Background(), shellID, "session-shell", "", "test"); err != nil {
		t.Fatal(err)
	}
	fakeSession.mu.Lock()
	lastInput := append([]byte(nil), fakeSession.inputs[len(fakeSession.inputs)-1]...)
	fakeSession.mu.Unlock()
	if len(lastInput) != 1 || lastInput[0] != 0x03 {
		t.Fatalf("interrupt input = %v, want one PTY Ctrl+C byte", lastInput)
	}
	if _, err := svc.CloseSSHShell(context.Background(), shellID, "session-shell", "", "test"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		shell, err := svc.store.GetSSHShell(context.Background(), shellID)
		if err != nil {
			t.Fatal(err)
		}
		if shell.Status == "closed" {
			if shell.ExitCode != nil || shell.TerminationReason != "requested_close" {
				t.Fatalf("closed shell termination metadata is ambiguous: %#v", shell)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("shell did not close: %#v", shell)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestMCPInteractiveSSHShellUsesIsolatedSurface(t *testing.T) {
	svc, _, host := newTestService(t)
	const mcpSessionID = "mcp_sess_test"
	ctx := WithMCPToolCall(context.Background(), mcpSessionID, "mcp_call_shell_start", "ssh_shell", `{"action":"start"}`)
	pending, err := svc.StartSSHShell(ctx, host.ID, "", false, 120, 32, "open an MCP test shell", "mcp-client")
	if err != nil {
		t.Fatal(err)
	}
	approved, err := svc.Approve(context.Background(), pending.ApprovalID, "approved", "test")
	if err != nil {
		t.Fatal(err)
	}
	if approved.Shell == nil || approved.Shell.Surface != domain.SSHShellSurfaceMCP || approved.Shell.SessionID != mcpSessionID {
		t.Fatalf("MCP shell was not isolated: %#v", approved.Shell)
	}
	shellID := approved.Shell.ID
	page, err := svc.WriteSSHShellPage(ctx, shellID, mcpSessionID, "whoami\r", 0, 0, "", "mcp-client")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := page.Snapshot
	if len(snapshot.Events) == 0 || snapshot.Events[0].Stream != "input" || snapshot.Events[0].Source != "agent" {
		t.Fatalf("MCP shell input source is incorrect: %#v", snapshot.Events)
	}
	listed, err := svc.ListSSHShells(ctx, mcpSessionID, true, "", "mcp-client")
	if err != nil || listed.Count != 1 || listed.Shells[0].ID != shellID {
		t.Fatalf("MCP shell list is not isolated: list=%#v err=%v", listed, err)
	}
	if _, err := svc.CloseSSHShell(ctx, shellID, mcpSessionID, "", "mcp-client"); err != nil {
		t.Fatal(err)
	}
}

func TestInteractiveSSHShellTerminationClassification(t *testing.T) {
	testCases := []struct {
		name         string
		exit         sshx.ShellExit
		wantStatus   string
		wantReason   string
		wantExitCode *int
	}{
		{
			name: "remote nonzero exit", exit: shellExit(7, errors.New("remote exit status 7")),
			wantStatus: "failed", wantReason: "remote_exit", wantExitCode: intPointer(7),
		},
		{
			name: "connection loss", exit: sshx.ShellExit{Err: errors.New("connection reset")},
			wantStatus: "failed", wantReason: "connection_lost",
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			svc, _, host := newTestService(t)
			sessionID := "session-" + strings.ReplaceAll(testCase.name, " ", "-")
			ctx := WithSessionID(context.Background(), sessionID)
			if _, err := svc.PrepareChatSession(ctx, sessionID, "", "test"); err != nil {
				t.Fatal(err)
			}
			pending, err := svc.StartSSHShell(ctx, host.ID, "", false, 90, 24, "test shell termination", "eino-agent")
			if err != nil {
				t.Fatal(err)
			}
			approved, err := svc.Approve(context.Background(), pending.ApprovalID, "approved", "test")
			if err != nil {
				t.Fatal(err)
			}
			shellID := approved.Shell.ID
			svc.shellMu.RLock()
			session := svc.shells[shellID].session.(*fakeShellSession)
			svc.shellMu.RUnlock()
			session.finish(testCase.exit)

			deadline := time.Now().Add(time.Second)
			for {
				shell, err := svc.store.GetSSHShell(context.Background(), shellID)
				if err != nil {
					t.Fatal(err)
				}
				if !shellStatusActive(shell.Status) {
					if shell.Status != testCase.wantStatus || shell.TerminationReason != testCase.wantReason {
						t.Fatalf("termination classification = %#v", shell)
					}
					if testCase.wantExitCode == nil {
						if shell.ExitCode != nil {
							t.Fatalf("unexpected exit code: %d", *shell.ExitCode)
						}
					} else if shell.ExitCode == nil || *shell.ExitCode != *testCase.wantExitCode {
						t.Fatalf("exit code = %#v, want %d", shell.ExitCode, *testCase.wantExitCode)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("shell did not finish: %#v", shell)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

func shellExit(code int, err error) sshx.ShellExit {
	return sshx.ShellExit{ExitCode: intPointer(code), Err: err}
}

func intPointer(value int) *int {
	return &value
}
