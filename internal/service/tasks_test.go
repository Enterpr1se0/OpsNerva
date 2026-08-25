package service

import (
	"context"
	"errors"
	"strings"
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
