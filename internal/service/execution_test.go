package service

import (
	"context"
	"strings"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

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
