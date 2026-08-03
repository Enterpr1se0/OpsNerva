package service

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/sshx"
	"eino-ops-agent/internal/store"
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

func (f *fakeTransport) OpenShell(_ context.Context, _ sshx.ConnectionSpec, _ domain.ExecRequest, cols, rows int, callback func(string, []byte)) (sshx.ShellSession, error) {
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

func TestWriteSSHShellWaitsForDelayedOutput(t *testing.T) {
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

	snapshot, err := svc.WriteSSHShell(context.Background(), shellID, "session-delayed-shell", "echo delayed\r", "", "test")
	if err != nil {
		t.Fatal(err)
	}
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
	toolCtx := WithExecutionOwner(context.Background(), "call-shell-input", "ssh_shell", `{"action":"input"}`)
	if _, err := svc.WriteSSHShell(toolCtx, approved.Shell.ID, sessionID, "echo live\r", "", "eino-agent"); err != nil {
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

func TestShellOutputPageReportsWaitDeadline(t *testing.T) {
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
	started := time.Now()
	page, err := svc.QuerySSHShellOutput(context.Background(), shell.ID, sessionID, &after, 40*time.Millisecond, 1024, "", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if !page.WaitDeadlineReached || len(page.Snapshot.Events) != 0 || time.Since(started) < 30*time.Millisecond {
		t.Fatalf("wait deadline page = %#v after %s", page, time.Since(started))
	}
}

func TestContinuousShellOutputReturnsABatchWithoutStoppingThePTY(t *testing.T) {
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
	snapshot, err := svc.collectSSHShellResponseWithPolicy(context.Background(), approved.Shell.ID, sessionID, before, 120*time.Millisecond, 30*time.Millisecond, "", "")
	close(stop)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 90*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("continuous output collection duration = %s", elapsed)
	}
	if snapshot.Shell.Status != "running" || !shellSnapshotHasOutput(snapshot) {
		t.Fatalf("continuous program did not return a live output batch: %#v", snapshot)
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
	inputSnapshot, err := svc.WriteSSHShell(inputCtx, approved.Shell.ID, sessionID, "long-command\r", "", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	svc.shellMu.RLock()
	state := svc.shells[approved.Shell.ID]
	svc.shellMu.RUnlock()
	state.session.(*fakeShellSession).callback("stdout", []byte("late-result\n"))

	outputCtx := WithExecutionOwner(context.Background(), "call-shell-output", "ssh_shell", `{"action":"output"}`)
	outputSnapshot, err := svc.WaitSSHShellOutput(outputCtx, approved.Shell.ID, sessionID, "", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
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

	snapshot, err := svc.WriteSSHShell(context.Background(), shellID, "session-shell", "printf hello\r", "test input", "test")
	if err != nil {
		t.Fatal(err)
	}
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
	snapshot, err = svc.WriteSSHShell(context.Background(), shellID, "session-shell", largeInput, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	output.Reset()
	for _, event := range snapshot.Events {
		if event.Stream == "stdout" {
			output.WriteString(event.Content)
		}
	}
	if output.String() != largeInput {
		t.Fatalf("interactive shell output was truncated: got=%d want=%d", output.Len(), len(largeInput))
	}
	if _, err := svc.WriteSSHShell(context.Background(), shellID, "different-session", "whoami\r", "", "test"); !errors.Is(err, store.ErrNotFound) {
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
	shellInsideANSI, err := svc.store.GetSSHShell(context.Background(), shellID)
	if err != nil {
		t.Fatal(err)
	}
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
	if _, err := svc.WriteSSHShell(context.Background(), shellID, "session-shell", "should-not-be-sent\r", "", "test"); err == nil || !strings.Contains(err.Error(), "private Web terminal") {
		t.Fatalf("Agent input was accepted at a credential prompt: %v", err)
	}
	shellBeforeSecret, err := svc.store.GetSSHShell(context.Background(), shellID)
	if err != nil {
		t.Fatal(err)
	}
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
	ctx := WithMCPClientSession(context.Background())
	pending, err := svc.StartSSHShell(ctx, host.ID, "", false, 120, 32, "open an MCP test shell", "mcp-client")
	if err != nil {
		t.Fatal(err)
	}
	approved, err := svc.Approve(context.Background(), pending.ApprovalID, "approved", "test")
	if err != nil {
		t.Fatal(err)
	}
	if approved.Shell == nil || approved.Shell.Surface != domain.SSHShellSurfaceMCP || approved.Shell.SessionID != mcpClientSessionID {
		t.Fatalf("MCP shell was not isolated: %#v", approved.Shell)
	}
	shellID := approved.Shell.ID
	snapshot, err := svc.WriteSSHShell(ctx, shellID, mcpClientSessionID, "whoami\r", "", "mcp-client")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Events) == 0 || snapshot.Events[0].Stream != "input" || snapshot.Events[0].Source != "agent" {
		t.Fatalf("MCP shell input source is incorrect: %#v", snapshot.Events)
	}
	listed, err := svc.ListSSHShells(ctx, mcpClientSessionID, true, "", "mcp-client")
	if err != nil || listed.Count != 1 || listed.Shells[0].ID != shellID {
		t.Fatalf("MCP shell list is not isolated: list=%#v err=%v", listed, err)
	}
	if _, err := svc.CloseSSHShell(ctx, shellID, mcpClientSessionID, "", "mcp-client"); err != nil {
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
