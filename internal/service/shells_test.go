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
	mu       sync.Mutex
	callback func(string, []byte)
	done     chan struct{}
	once     sync.Once
	cols     int
	rows     int
	inputs   [][]byte
	exit     sshx.ShellExit
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
	s.mu.Unlock()
	s.callback("stdout", append([]byte(nil), data...))
	return len(data), nil
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
	pending, err := svc.StartSSHShell(ctx, host.ID, "/tmp", false, 90, 24, "run an interactive test", "test")
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != "approval_required" || pending.ApprovalID == "" || pending.Risk != domain.RiskChange {
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
			pending, err := svc.StartSSHShell(ctx, host.ID, "", false, 90, 24, "test shell termination", "test")
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
