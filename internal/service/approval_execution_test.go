package service

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

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
