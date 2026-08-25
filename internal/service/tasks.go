package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/ids"
	"github.com/Enterpr1se0/opsnerva/internal/store"
)

const taskOutputCheckpointInterval = 500 * time.Millisecond

type taskState struct {
	task       domain.Task
	result     domain.ExecResult
	err        string
	cancel     context.CancelFunc
	approvalID string
	notify     chan struct{}
	checkpoint time.Time
}

type executionObserver struct {
	RunStarted func(domain.Run)
	Output     func(string, []byte)
}

func taskStatusEvent(snapshot domain.TaskSnapshot) domain.TaskEvent {
	return domain.TaskEvent{
		Type: "status", TaskID: snapshot.Task.ID, Revision: snapshot.Task.Revision, Snapshot: &snapshot,
	}
}

func appendTaskOutputLocked(state *taskState, streamName, chunk string) (domain.TaskSnapshot, domain.TaskEvent, bool) {
	offset := len(state.result.Stdout)
	if streamName == "stderr" {
		offset = len(state.result.Stderr)
		state.result.Stderr += chunk
	} else {
		streamName = "stdout"
		state.result.Stdout += chunk
	}
	state.task.Revision++
	checkpoint := time.Since(state.checkpoint) >= taskOutputCheckpointInterval
	if checkpoint {
		state.checkpoint = time.Now()
	}
	notifyTaskWaitersLocked(state)
	snapshot := taskSnapshot(state)
	return snapshot, domain.TaskEvent{
		Type: "output", TaskID: snapshot.Task.ID, Revision: snapshot.Task.Revision,
		Stream: streamName, OffsetBytes: offset, TotalBytes: offset + len(chunk), Content: chunk,
	}, checkpoint
}

func requireTaskSession(ctx context.Context, task domain.Task) error {
	sessionID := SessionIDFromContext(ctx)
	if task.SessionID != sessionID {
		return store.ErrNotFound
	}
	return nil
}

func (s *Service) StartTask(ctx context.Context, req domain.ExecRequest, actor string) (domain.Task, error) {
	req.Background = true
	sessionID := SessionIDFromContext(ctx)
	if (actor == "eino-agent" || actor == "mcp-client") && sessionID == "" {
		return domain.Task{}, fmt.Errorf("background tasks require a session context")
	}
	host, err := s.store.GetHost(ctx, strings.TrimSpace(req.HostID))
	if err != nil {
		return domain.Task{}, err
	}
	background := s.executionCtx
	if background == nil {
		background = context.Background()
	}
	if sessionID != "" {
		background = WithSessionID(background, sessionID)
	}
	if owner, ok := executionOwnerFromContext(ctx); ok {
		background = context.WithValue(background, executionOwnerContextKey{}, owner)
	}
	taskCtx, cancel := context.WithCancel(background)
	task := domain.Task{
		ID: ids.New("task"), SessionID: sessionID, HostID: host.ID,
		Status: "running", Revision: 1, StartedAt: time.Now().UTC(),
	}
	state := &taskState{
		task: task, result: domain.ExecResult{Status: "running"}, cancel: cancel, notify: make(chan struct{}), checkpoint: task.StartedAt,
	}
	if err := s.store.UpsertTask(context.Background(), task, state.result, ""); err != nil {
		cancel()
		return domain.Task{}, err
	}
	s.taskMu.Lock()
	s.tasks[task.ID] = state
	s.taskMu.Unlock()

	s.executionMu.Lock()
	if s.executionClosed {
		s.executionMu.Unlock()
		cancel()
		s.taskMu.Lock()
		delete(s.tasks, task.ID)
		s.taskMu.Unlock()
		task.Status = "interrupted"
		task.EndedAt = time.Now().UTC()
		task.Revision++
		result := domain.ExecResult{Status: task.Status, CompletedAt: task.EndedAt}
		_ = s.store.UpsertTask(context.Background(), task, result, "service is shutting down")
		return domain.Task{}, fmt.Errorf("service is shutting down")
	}
	s.executionWG.Add(1)
	s.executionMu.Unlock()
	go func() {
		defer s.executionWG.Done()
		s.runTask(taskCtx, state, req, actor)
	}()
	return task, nil
}

func (s *Service) runTask(ctx context.Context, state *taskState, req domain.ExecRequest, actor string) {
	observer := executionObserver{
		RunStarted: func(run domain.Run) {
			s.taskMu.Lock()
			if s.tasks[state.task.ID] != state || terminalExecutionStatus(state.task.Status) {
				s.taskMu.Unlock()
				return
			}
			state.task.RunID = run.ID
			state.result.RunID = run.ID
			state.task.Revision++
			notifyTaskWaitersLocked(state)
			snapshot := taskSnapshot(state)
			s.taskMu.Unlock()
			_ = s.store.UpsertTask(context.Background(), snapshot.Task, snapshot.Result, snapshot.Error)
			s.publishTaskEvent(taskStatusEvent(snapshot))
		},
		Output: func(streamName string, data []byte) {
			chunk := string(data)
			if chunk == "" {
				return
			}
			s.taskMu.Lock()
			if s.tasks[state.task.ID] != state || state.task.Status != "running" {
				s.taskMu.Unlock()
				return
			}
			snapshot, event, checkpoint := appendTaskOutputLocked(state, streamName, chunk)
			s.taskMu.Unlock()
			if checkpoint {
				_ = s.store.UpsertTask(context.Background(), snapshot.Task, snapshot.Result, snapshot.Error)
			}
			s.publishTaskEvent(event)
		},
	}
	result, err := s.submit(ctx, req, actor, observer)
	if err == nil && result.Status == "approval_required" && result.ApprovalID != "" {
		s.taskMu.Lock()
		if s.tasks[state.task.ID] != state || terminalExecutionStatus(state.task.Status) {
			s.taskMu.Unlock()
			_ = s.Reject(context.Background(), result.ApprovalID, "background task cancelled", actor)
			return
		}
		state.result = result
		state.task.RunID = result.RunID
		state.task.Status = "approval_required"
		state.task.Revision++
		state.approvalID = result.ApprovalID
		s.approvalTasks[result.RunID] = state
		snapshot := taskSnapshot(state)
		notifyTaskWaitersLocked(state)
		s.taskMu.Unlock()
		_ = s.store.UpsertTask(context.Background(), snapshot.Task, snapshot.Result, snapshot.Error)
		s.publishTaskEvent(taskStatusEvent(snapshot))
		return
	}

	s.taskMu.Lock()
	if s.tasks[state.task.ID] != state || terminalExecutionStatus(state.task.Status) {
		s.taskMu.Unlock()
		return
	}
	state.result = result
	state.task.RunID = result.RunID
	state.task.EndedAt = time.Now().UTC()
	state.task.Status = result.Status
	if err != nil {
		state.err = err.Error()
		if state.task.Status == "" {
			state.task.Status = "failed"
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				state.task.Status = "interrupted"
			}
			state.result.Status = state.task.Status
		}
	}
	state.task.Revision++
	notifyTaskWaitersLocked(state)
	snapshot := taskSnapshot(state)
	s.taskMu.Unlock()
	_ = s.store.UpsertTask(context.Background(), snapshot.Task, snapshot.Result, snapshot.Error)
	s.publishTaskEvent(taskStatusEvent(snapshot))
	s.taskMu.Lock()
	if s.tasks[state.task.ID] == state {
		delete(s.tasks, state.task.ID)
	}
	s.taskMu.Unlock()
}

func (s *Service) GetTask(id string) (domain.Task, domain.ExecResult, string, error) {
	s.taskMu.RLock()
	state := s.tasks[id]
	if state != nil {
		snapshot := taskSnapshot(state)
		s.taskMu.RUnlock()
		return snapshot.Task, snapshot.Result, snapshot.Error, nil
	}
	s.taskMu.RUnlock()
	return s.store.GetTask(context.Background(), id)
}

func (s *Service) GetTaskForContext(ctx context.Context, id string) (domain.Task, domain.ExecResult, string, error) {
	snapshot, _, err := s.taskForContext(ctx, id)
	return snapshot.Task, snapshot.Result, snapshot.Error, err
}

func (s *Service) taskForContext(ctx context.Context, id string) (domain.TaskSnapshot, <-chan struct{}, error) {
	s.taskMu.Lock()
	state := s.tasks[id]
	if state != nil {
		if err := requireTaskSession(ctx, state.task); err != nil {
			s.taskMu.Unlock()
			return domain.TaskSnapshot{}, nil, err
		}
		if state.notify == nil {
			state.notify = make(chan struct{})
		}
		snapshot, notify := taskSnapshot(state), (<-chan struct{})(state.notify)
		s.taskMu.Unlock()
		return snapshot, notify, nil
	}
	s.taskMu.Unlock()
	task, result, taskErr, err := s.store.GetTask(ctx, id)
	if err != nil {
		return domain.TaskSnapshot{}, nil, err
	}
	if err := requireTaskSession(ctx, task); err != nil {
		return domain.TaskSnapshot{}, nil, err
	}
	return domain.TaskSnapshot{Task: task, Result: result, Error: taskErr}, nil, nil
}

// WaitTask blocks on task state changes instead of polling the task store.
func (s *Service) WaitTask(ctx context.Context, id string, afterStdout, afterStderr int, wait time.Duration, blockUntil string) (domain.Task, domain.ExecResult, string, bool, error) {
	var deadline <-chan time.Time
	var timer *time.Timer
	if wait > 0 {
		timer = time.NewTimer(wait)
		defer timer.Stop()
		deadline = timer.C
	}
	for {
		snapshot, notify, err := s.taskForContext(ctx, id)
		if err != nil {
			return domain.Task{}, domain.ExecResult{}, "", false, err
		}
		terminal := terminalExecutionStatus(snapshot.Task.Status)
		outputReady := len(snapshot.Result.Stdout) > afterStdout || len(snapshot.Result.Stderr) > afterStderr
		if wait <= 0 || terminal || (blockUntil == "output" && outputReady) || notify == nil {
			return snapshot.Task, snapshot.Result, snapshot.Error, false, nil
		}
		select {
		case <-ctx.Done():
			return domain.Task{}, domain.ExecResult{}, "", false, ctx.Err()
		case <-deadline:
			return snapshot.Task, snapshot.Result, snapshot.Error, true, nil
		case <-notify:
		}
	}
}

func (s *Service) CancelTask(id, actor string) error {
	return s.cancelTask(context.Background(), id, actor, false)
}

func (s *Service) CancelTaskForContext(ctx context.Context, id, actor string) error {
	if (actor == "eino-agent" || actor == "mcp-client") && SessionIDFromContext(ctx) == "" {
		return fmt.Errorf("background tasks require a session context")
	}
	return s.cancelTask(ctx, id, actor, true)
}

func (s *Service) cancelTask(ctx context.Context, id, actor string, enforceSession bool) error {
	s.taskMu.Lock()
	state := s.tasks[id]
	if state == nil {
		s.taskMu.Unlock()
		task, _, _, err := s.store.GetTask(ctx, id)
		if err != nil {
			return err
		}
		if enforceSession {
			if err := requireTaskSession(ctx, task); err != nil {
				return err
			}
		}
		return fmt.Errorf("task is not running and cannot be cancelled")
	}
	if enforceSession {
		if err := requireTaskSession(ctx, state.task); err != nil {
			s.taskMu.Unlock()
			return err
		}
	}
	if state.task.Status != "running" && state.task.Status != "approval_required" {
		s.taskMu.Unlock()
		return fmt.Errorf("task is not running and cannot be cancelled")
	}
	cancel := state.cancel
	approvalID := state.approvalID
	runID := state.task.RunID
	state.task.Status = "cancelled"
	state.task.EndedAt = time.Now().UTC()
	state.task.Revision++
	state.result.Status = "cancelled"
	state.result.CompletedAt = state.task.EndedAt
	notifyTaskWaitersLocked(state)
	snapshot := taskSnapshot(state)
	delete(s.approvalTasks, runID)
	s.taskMu.Unlock()

	if cancel != nil {
		cancel()
	}
	if approvalID != "" {
		approval, err := s.store.GetApproval(context.Background(), approvalID)
		if err == nil && approval.Status == "pending" {
			if rejectErr := s.Reject(context.Background(), approvalID, "background task cancelled", actor); rejectErr != nil {
				s.cancelApprovedExecution(runID)
			}
		} else {
			s.cancelApprovedExecution(runID)
		}
	}
	s.audit(context.Background(), runID, "task_cancelled", actor, map[string]any{"task_id": id})
	_ = s.store.UpsertTask(context.Background(), snapshot.Task, snapshot.Result, snapshot.Error)
	s.publishTaskEvent(taskStatusEvent(snapshot))
	s.taskMu.Lock()
	if s.tasks[id] == state {
		delete(s.tasks, id)
	}
	s.taskMu.Unlock()
	return nil
}

func (s *Service) hasApprovalTask(runID string) bool {
	s.taskMu.RLock()
	defer s.taskMu.RUnlock()
	return s.approvalTasks[runID] != nil
}

// updateApprovalTask projects approved execution events into the background
// task that owns the run. Output events stay in memory and are checkpointed;
// terminal events reload the durable run once to build the complete result.
func (s *Service) updateApprovalTask(event ExecutionEvent) {
	s.taskMu.Lock()
	state := s.approvalTasks[event.RunID]
	if state == nil || s.tasks[state.task.ID] != state || terminalExecutionStatus(state.task.Status) {
		s.taskMu.Unlock()
		return
	}
	if (event.Stream == "stdout" || event.Stream == "stderr") && event.Content != "" {
		snapshot, taskEvent, checkpoint := appendTaskOutputLocked(state, event.Stream, event.Content)
		s.taskMu.Unlock()
		if checkpoint {
			_ = s.store.UpsertTask(context.Background(), snapshot.Task, snapshot.Result, snapshot.Error)
		}
		s.publishTaskEvent(taskEvent)
		return
	}
	if event.Status == "running" {
		if state.task.Status == event.Status {
			s.taskMu.Unlock()
			return
		}
		state.task.Status = event.Status
		state.result.Status = event.Status
		state.task.Revision++
		notifyTaskWaitersLocked(state)
		snapshot := taskSnapshot(state)
		s.taskMu.Unlock()
		_ = s.store.UpsertTask(context.Background(), snapshot.Task, snapshot.Result, snapshot.Error)
		s.publishTaskEvent(taskStatusEvent(snapshot))
		return
	}
	approvalID := state.approvalID
	s.taskMu.Unlock()
	if !terminalExecutionStatus(event.Status) {
		return
	}
	approval, err := s.store.GetApproval(context.Background(), approvalID)
	if err != nil {
		return
	}
	run, err := s.store.GetRun(context.Background(), event.RunID)
	if err != nil {
		return
	}
	result := execResultFromRun(run, approval.ID, "")
	if approval.Status == "rejected" {
		result.OperatorInstruction = approval.Reason
	}
	terminal := terminalExecutionStatus(run.Status)

	s.taskMu.Lock()
	if s.tasks[state.task.ID] != state || s.approvalTasks[event.RunID] != state || terminalExecutionStatus(state.task.Status) {
		s.taskMu.Unlock()
		return
	}
	state.task.RunID = run.ID
	state.task.Status = run.Status
	state.task.OperatorInstruction = result.OperatorInstruction
	state.result = result
	state.task.Revision++
	if terminal {
		state.task.EndedAt = run.CompletedAt
		if state.task.EndedAt.IsZero() {
			state.task.EndedAt = time.Now().UTC()
		}
		delete(s.approvalTasks, event.RunID)
	}
	notifyTaskWaitersLocked(state)
	snapshot := taskSnapshot(state)
	s.taskMu.Unlock()
	_ = s.store.UpsertTask(context.Background(), snapshot.Task, snapshot.Result, snapshot.Error)
	s.publishTaskEvent(taskStatusEvent(snapshot))
	if terminal {
		s.taskMu.Lock()
		if s.tasks[state.task.ID] == state {
			delete(s.tasks, state.task.ID)
		}
		s.taskMu.Unlock()
		if state.cancel != nil {
			state.cancel()
		}
	}
}
