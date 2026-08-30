package agenttool

import "github.com/Enterpr1se0/opsnerva/internal/domain"

// ProjectExecResult removes persisted audit and UI-only fields from an
// execution result before it is returned to a model or MCP client.
func ProjectExecResult(result domain.ExecResult) ExecResult {
	compact := ExecResult{
		Status: result.Status, RunID: result.RunID, TaskID: result.TaskID,
		AutoApproved: result.AutoApproved,
		ApprovalID:   result.ApprovalID, OperatorInstruction: result.OperatorInstruction,
		ExitCode: result.ExitCode, Stdout: result.Stdout, Stderr: result.Stderr,
		OutputView: result.OutputView, OutputLimited: result.OutputLimited,
		StdoutTotalBytes: result.StdoutTotalBytes, StderrTotalBytes: result.StderrTotalBytes,
		StdoutOmittedBytes: result.StdoutOmittedBytes, StderrOmittedBytes: result.StderrOmittedBytes,
		StdoutOffsetBytes: result.StdoutOffsetBytes, StderrOffsetBytes: result.StderrOffsetBytes,
		WaitDeadlineReached: result.WaitDeadlineReached,
		File:                result.File, Change: result.Change, Search: result.Search,
		Tunnel: result.Tunnel, Shell: result.Shell, ShellUsage: result.ShellUsage,
	}
	if result.Code != "" {
		compact.Code = result.Code
		compact.Message = result.Message
		compact.Retryable = result.Retryable
		compact.NextAction = result.NextAction
		compact.Validation = result.Validation
	}
	return compact
}
