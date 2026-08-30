package agenttool

import (
	"context"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func (ssh *SSH) normalizeTaskResult(task domain.Task, result domain.ExecResult, taskErr string, err error) (domain.ExecResult, error) {
	result.TaskID = task.ID
	if result.RunID == "" {
		result.RunID = task.RunID
	}
	if task.Status != "" {
		result.Status = task.Status
	}
	if result.OperatorInstruction == "" {
		result.OperatorInstruction = task.OperatorInstruction
	}
	if result.CompletedAt.IsZero() && !task.EndedAt.IsZero() {
		result.CompletedAt = task.EndedAt
	}
	result, normalizedErr := ssh.dependencies.Results.NormalizeExec(result, err)
	if normalizedErr != nil {
		return result, normalizedErr
	}
	if taskErr != "" {
		result.OK = false
		result.Message = taskErr
		result.Code = "remote_failed"
	}
	return result, nil
}

func (ssh *SSH) RunTask(ctx context.Context, input TaskInput, actor string) (ExecResult, error) {
	action := strings.ToLower(strings.TrimSpace(input.Action))
	if strings.TrimSpace(input.TaskID) == "" {
		return ssh.execResult(domain.ExecResult{}, InvalidInput("task_id is required"))
	}
	if _, err := ValidateOutputView(input.MaxOutputBytes, input.OutputView); err != nil {
		return ssh.execResult(domain.ExecResult{TaskID: input.TaskID}, InvalidInput("%s", err.Error()))
	}
	if input.WaitSeconds < 0 || input.WaitSeconds > 60 || input.AfterStdoutBytes < 0 || input.AfterStderrBytes < 0 {
		return ssh.execResult(domain.ExecResult{TaskID: input.TaskID}, InvalidInput("wait_seconds must be between 0 and 60 and output byte offsets must be non-negative"))
	}
	blockUntil := strings.ToLower(strings.TrimSpace(input.BlockUntil))
	if input.WaitSeconds == 0 && blockUntil != "" {
		return ssh.execResult(domain.ExecResult{TaskID: input.TaskID}, InvalidInput("block_until requires wait_seconds"))
	}
	if input.WaitSeconds > 0 && blockUntil == "" {
		blockUntil = "terminal"
	}
	if blockUntil != "" && blockUntil != "terminal" && blockUntil != "output" {
		return ssh.execResult(domain.ExecResult{TaskID: input.TaskID}, InvalidInput("block_until must be terminal or output"))
	}
	switch action {
	case "status":
		task, result, taskErr, waitDeadlineReached, err := ssh.dependencies.Tasks.WaitTask(
			ctx, input.TaskID, input.AfterStdoutBytes, input.AfterStderrBytes,
			time.Duration(input.WaitSeconds)*time.Second, blockUntil,
		)
		if task.ID == "" {
			task.ID = input.TaskID
		}
		result, err = ssh.normalizeTaskResult(task, result, taskErr, err)
		if err != nil {
			return ProjectExecResult(result), err
		}
		result.WaitDeadlineReached = waitDeadlineReached
		selected, selectErr := SelectExecOutput(result, input.AfterStdoutBytes, input.AfterStderrBytes, input.MaxOutputBytes, input.OutputView, true)
		if selectErr != nil {
			return ssh.execResult(domain.ExecResult{TaskID: input.TaskID}, InvalidInput("%s", selectErr.Error()))
		}
		return ProjectExecResult(selected), nil
	case "cancel":
		if input.WaitSeconds != 0 || input.BlockUntil != "" || input.AfterStdoutBytes != 0 || input.AfterStderrBytes != 0 || input.MaxOutputBytes != 0 || input.OutputView != "" {
			return ssh.execResult(domain.ExecResult{TaskID: input.TaskID}, InvalidInput("action=cancel accepts only action and task_id"))
		}
		cancelErr := ssh.dependencies.Tasks.CancelTaskForContext(ctx, input.TaskID, actor)
		task, result, taskErr, getErr := ssh.dependencies.Tasks.GetTaskForContext(ctx, input.TaskID)
		if task.ID == "" {
			task.ID = input.TaskID
		}
		if cancelErr != nil {
			normalized, normalizedErr := ssh.normalizeTaskResult(task, result, taskErr, cancelErr)
			return ProjectExecResult(normalized), normalizedErr
		}
		normalized, normalizedErr := ssh.normalizeTaskResult(task, result, taskErr, getErr)
		return ProjectExecResult(normalized), normalizedErr
	default:
		return ssh.execResult(domain.ExecResult{TaskID: input.TaskID}, InvalidInput("invalid action: use status or cancel"))
	}
}

func (ssh *SSH) startedTaskResult(ctx context.Context, task domain.Task, startErr error) (domain.ExecResult, error) {
	if task.ID == "" {
		return ssh.normalizeTaskResult(task, domain.ExecResult{}, "", startErr)
	}
	storedTask, result, taskErr, getErr := ssh.dependencies.Execution.GetTaskForContext(ctx, task.ID)
	if getErr == nil {
		task = storedTask
	} else if startErr == nil {
		startErr = getErr
	}
	return ssh.normalizeTaskResult(task, result, taskErr, startErr)
}

func (ssh *SSH) RunExecution(ctx context.Context, request domain.ExecRequest, actor string) (ExecResult, error) {
	if _, err := ValidateOutputView(request.MaxOutputBytes, request.OutputView); err != nil {
		return ssh.execResult(domain.ExecResult{}, InvalidInput("%s", err.Error()))
	}
	var result domain.ExecResult
	var err error
	if !request.Background {
		result, err = ssh.dependencies.Execution.Submit(ctx, request, actor)
		result, err = ssh.dependencies.Results.NormalizeExec(result, err)
	} else {
		var task domain.Task
		task, err = ssh.dependencies.Execution.StartTask(ctx, request, actor)
		result, err = ssh.startedTaskResult(ctx, task, err)
	}
	selected, selectErr := SelectExecOutput(result, 0, 0, request.MaxOutputBytes, request.OutputView, false)
	if selectErr != nil {
		return ssh.execResult(domain.ExecResult{}, InvalidInput("%s", selectErr.Error()))
	}
	return ProjectExecResult(selected), err
}
