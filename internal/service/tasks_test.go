package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"
	"github.com/Enterpr1se0/opsnerva/internal/store"
)

type gatedTaskStreamingTransport struct {
	*fakeTransport
	releaseChunks <-chan struct{}
	chunksDone    chan<- struct{}
	chunks        []fakeStreamChunk
}

func (transport *gatedTaskStreamingTransport) ExecStream(ctx context.Context, connection sshx.ConnectionSpec, request domain.ExecRequest, emit func(string, []byte)) (sshx.RawResult, error) {
	select {
	case <-transport.releaseChunks:
	case <-ctx.Done():
		return sshx.RawResult{}, ctx.Err()
	}
	for _, chunk := range transport.chunks {
		emit(chunk.stream, []byte(chunk.data))
	}
	close(transport.chunksDone)
	return transport.fakeTransport.Exec(ctx, connection, request)
}

func TestTaskAccessIsBoundToSession(t *testing.T) {
	svc, _, host := newTestService(t)
	state := &taskState{
		task: domain.Task{
			ID: "task-session", SessionID: "session-a", HostID: host.ID,
			Status: "running", Revision: 1, StartedAt: time.Now().UTC(),
		},
		result: domain.ExecResult{Status: "running"},
		notify: make(chan struct{}),
	}
	if err := svc.store.UpsertTask(context.Background(), state.task, state.result, ""); err != nil {
		t.Fatal(err)
	}
	svc.taskMu.Lock()
	svc.tasks[state.task.ID] = state
	svc.taskMu.Unlock()
	t.Cleanup(func() {
		svc.taskMu.Lock()
		delete(svc.tasks, state.task.ID)
		svc.taskMu.Unlock()
	})

	wrongSession := WithSessionID(context.Background(), "session-b")
	if _, _, _, err := svc.GetTaskForContext(context.Background(), state.task.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("sessionless task read error = %v, want not found", err)
	}
	if _, _, _, _, err := svc.WaitTask(wrongSession, state.task.ID, 0, 0, 0, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-session task read error = %v, want not found", err)
	}
	if err := svc.CancelTaskForContext(wrongSession, state.task.ID, "eino-agent"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-session task cancel error = %v, want not found", err)
	}
	snapshots, _, unsubscribe := svc.SubscribeTaskEvents(context.Background(), "session-b", []string{state.task.ID})
	defer unsubscribe()
	if snapshots[state.task.ID].Error != store.ErrNotFound.Error() {
		t.Fatalf("cross-session task subscription leaked snapshot: %#v", snapshots[state.task.ID])
	}
	if current, _, _, err := svc.GetTask(state.task.ID); err != nil || current.Status != "running" {
		t.Fatalf("cross-session access changed task: task=%#v error=%v", current, err)
	}
}

func TestTaskSubscriptionReceivesOrderedOutputDelta(t *testing.T) {
	svc, _, host := newTestService(t)
	state := &taskState{
		task: domain.Task{
			ID: "task-events", SessionID: "session-events", HostID: host.ID,
			Status: "running", Revision: 4, StartedAt: time.Now().UTC(),
		},
		result: domain.ExecResult{Status: "running", Stdout: "old"},
		notify: make(chan struct{}),
	}
	svc.taskMu.Lock()
	svc.tasks[state.task.ID] = state
	svc.taskMu.Unlock()
	t.Cleanup(func() {
		svc.taskMu.Lock()
		delete(svc.tasks, state.task.ID)
		svc.taskMu.Unlock()
	})

	snapshots, events, unsubscribe := svc.SubscribeTaskEvents(context.Background(), state.task.SessionID, []string{state.task.ID})
	defer unsubscribe()
	if snapshots[state.task.ID].Task.Revision != 4 || snapshots[state.task.ID].Result.Stdout != "old" {
		t.Fatalf("unexpected initial task snapshot: %#v", snapshots[state.task.ID])
	}

	svc.taskMu.Lock()
	state.result.Stdout += "-new"
	state.task.Revision++
	notifyTaskWaitersLocked(state)
	revision := state.task.Revision
	svc.taskMu.Unlock()
	svc.publishTaskEvent(domain.TaskEvent{
		Type: "output", TaskID: state.task.ID, Revision: revision,
		Stream: "stdout", OffsetBytes: len("old"), TotalBytes: len("old-new"), Content: "-new",
	})

	select {
	case event := <-events:
		if event.Type != "output" || event.Revision != 5 || event.OffsetBytes != len("old") || event.TotalBytes != len("old-new") || event.Content != "-new" {
			t.Fatalf("unexpected task event: %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("task output event was not pushed")
	}
}

func TestApprovedBackgroundTaskPushesOutputWithoutExecutionSubscriber(t *testing.T) {
	svc, transport, host := newTestService(t)
	transport.mu.Lock()
	transport.stdout = []byte("password=split-secret\napproved output\n")
	transport.mu.Unlock()
	svc.transport = &streamingFakeTransport{
		fakeTransport: transport,
		chunks: []fakeStreamChunk{
			{stream: "stdout", data: "password=split-"},
			{stream: "stdout", data: "secret\napproved output\n"},
		},
	}

	const sessionID = "approved-task-events"
	task, err := svc.StartTask(WithSessionID(context.Background(), sessionID), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"},
		Reason: "restart demo as a managed task",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	_, pending := waitForBackgroundTaskApproval(t, svc, task.ID)
	_, events, unsubscribe := svc.SubscribeTaskEvents(context.Background(), sessionID, []string{task.ID})
	defer unsubscribe()
	if _, err := svc.ApproveAsync(context.Background(), pending.ApprovalID, "reviewed", "operator"); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(2 * time.Second)
	var stdout string
	for {
		select {
		case event := <-events:
			if event.Type == "output" {
				if event.Stream != "stdout" || event.OffsetBytes != len(stdout) {
					t.Fatalf("unexpected approved task output offset: %#v", event)
				}
				stdout += event.Content
				if strings.Contains(stdout, "split-secret") {
					t.Fatalf("approved task event exposed a split secret: %q", stdout)
				}
			}
			if event.Type == "status" && event.Snapshot != nil && event.Snapshot.Task.Status == "completed" {
				if stdout != "password=[REDACTED]\napproved output\n" {
					t.Fatalf("unexpected approved task output: %q", stdout)
				}
				return
			}
		case <-deadline:
			t.Fatal("approved task output was not pushed")
		}
	}
}

func TestTaskOutputIsCheckpointedInsteadOfPersistedPerChunk(t *testing.T) {
	svc, transport, host := newTestService(t)
	saveApprovalMode(t, svc, domain.ApprovalModeFullAccess)
	releaseChunks := make(chan struct{})
	chunksDone := make(chan struct{})
	executionStarted := make(chan struct{})
	executionRelease := make(chan struct{})
	transport.mu.Lock()
	transport.stdout = []byte("one\ntwo\nthree\n")
	transport.execStarted = executionStarted
	transport.execRelease = executionRelease
	transport.mu.Unlock()
	svc.transport = &gatedTaskStreamingTransport{
		fakeTransport: transport,
		releaseChunks: releaseChunks,
		chunksDone:    chunksDone,
		chunks: []fakeStreamChunk{
			{stream: "stdout", data: "one\n"},
			{stream: "stdout", data: "two\n"},
			{stream: "stdout", data: "three\n"},
		},
	}

	const sessionID = "task-checkpoint"
	task, err := svc.StartTask(WithSessionID(context.Background(), sessionID), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "printf", Args: []string{"one\\ntwo\\nthree\\n"},
		Reason: "verify task output checkpoints",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	svc.taskMu.Lock()
	svc.tasks[task.ID].checkpoint = time.Now()
	svc.taskMu.Unlock()
	close(releaseChunks)
	select {
	case <-chunksDone:
	case <-time.After(time.Second):
		t.Fatal("stream chunks were not emitted")
	}
	select {
	case <-executionStarted:
	case <-time.After(time.Second):
		t.Fatal("execution did not reach the controlled transport")
	}

	liveTask, liveResult, _, err := svc.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	persistedTask, persistedResult, _, err := svc.store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if liveResult.Stdout != "one\ntwo\nthree\n" || liveTask.Revision <= persistedTask.Revision {
		t.Fatalf("live task did not advance beyond its checkpoint: live=%#v result=%#v persisted=%#v", liveTask, liveResult, persistedTask)
	}
	if persistedResult.Stdout != "" {
		t.Fatalf("output was persisted before the checkpoint interval: %q", persistedResult.Stdout)
	}

	close(executionRelease)
	ctx, cancel := context.WithTimeout(WithSessionID(context.Background(), sessionID), time.Second)
	defer cancel()
	completed, result, _, _, err := svc.WaitTask(ctx, task.ID, 0, 0, time.Second, "terminal")
	if err != nil || completed.Status != "completed" || result.Stdout != "one\ntwo\nthree\n" {
		t.Fatalf("terminal checkpoint = task=%#v result=%#v error=%v", completed, result, err)
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
