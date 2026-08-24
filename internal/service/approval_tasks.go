package service

import (
	"context"
	"time"
)

// updateApprovalTask projects durable run state into the background task that
// owns it. It is called by execution lifecycle events, so approval decisions
// no longer require a database polling goroutine per task.
func (s *Service) updateApprovalTask(runID string) {
	s.taskMu.RLock()
	state := s.approvalTasks[runID]
	s.taskMu.RUnlock()
	if state == nil {
		return
	}
	approval, err := s.store.GetApproval(context.Background(), state.approvalID)
	if err != nil {
		return
	}
	run, err := s.store.GetRun(context.Background(), runID)
	if err != nil {
		return
	}
	result := execResultFromRun(run, approval.ID, "")
	if approval.Status == "rejected" {
		result.OperatorInstruction = approval.Reason
	}
	terminal := terminalExecutionStatus(run.Status)

	s.taskMu.Lock()
	if s.tasks[state.task.ID] != state || s.approvalTasks[runID] != state {
		s.taskMu.Unlock()
		return
	}
	state.task.RunID = run.ID
	state.task.Status = run.Status
	state.task.OperatorInstruction = result.OperatorInstruction
	state.result = result
	if terminal {
		state.task.EndedAt = time.Now().UTC()
		delete(s.tasks, state.task.ID)
		delete(s.approvalTasks, runID)
	}
	taskSnapshot, resultSnapshot, taskErr := state.task, state.result, state.err
	s.taskMu.Unlock()
	_ = s.store.UpsertTask(context.Background(), taskSnapshot, resultSnapshot, taskErr)
	if terminal && state.cancel != nil {
		state.cancel()
	}
}
