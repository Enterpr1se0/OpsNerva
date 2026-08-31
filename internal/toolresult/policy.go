package toolresult

import (
	"context"
	"errors"
	"strings"

	"github.com/Enterpr1se0/opsnerva/internal/agenttool"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/service"
	"github.com/Enterpr1se0/opsnerva/internal/skills"
	"github.com/Enterpr1se0/opsnerva/internal/store"
)

type Policy struct{}

func (Policy) NormalizeExec(result domain.ExecResult, err error) (domain.ExecResult, error) {
	return NormalizeExec(result, err)
}

func (Policy) Value(ctx context.Context, toolName string, value any, err error) (any, error) {
	return NormalizeValue(ctx, toolName, value, err)
}

func NormalizeExec(result domain.ExecResult, err error) (domain.ExecResult, error) {
	if err == nil {
		result.ToolMeta = domain.ToolMeta{}
		switch result.Status {
		case "completed", "running", "partial", "approval_required", "cancelled":
			result.OK = true
		}
		return result, nil
	}
	if errors.Is(err, context.Canceled) {
		return result, err
	}
	result.OK = false
	result.Message = err.Error()
	var validationErr *agenttool.InputError
	if errors.As(err, &validationErr) {
		result.Code = "validation_failed"
		result.NextAction = "correct the function tool input using the validation details; do not repeat unchanged input"
		result.Validation = validationErr.Validation()
		if result.Status == "" {
			result.Status = "failed"
		}
		return result, nil
	}
	result.Code, result.Retryable, result.NextAction = ClassifyExecError(err)
	var selectionErr *service.ExecutionToolSelectionError
	if errors.As(err, &selectionErr) {
		result.Validation = &domain.ToolValidationDetails{
			SuggestedTool: selectionErr.SuggestedTool,
			Example:       selectionErr.Example,
		}
	}
	if result.Status == "" {
		result.Status = "failed"
	}
	return result, nil
}

func CompactExec(result domain.ExecResult, err error) (agenttool.ExecResult, error) {
	normalized, normalizedErr := NormalizeExec(result, err)
	return agenttool.ProjectExecResult(normalized), normalizedErr
}

func ClassifyExecError(err error) (string, bool, string) {
	var selectionErr *service.ExecutionToolSelectionError
	var inputValidation *service.InputValidationError
	if errors.As(err, &selectionErr) {
		return "wrong_tool", false, selectionErr.NextAction
	}
	if errors.As(err, &inputValidation) {
		return "validation_failed", false, "correct the tool input using the error message; do not repeat unchanged input"
	}
	if errors.Is(err, service.ErrAgentHostAccessDenied) || errors.Is(err, service.ErrAgentRootAccessDenied) || errors.Is(err, service.ErrHostAgentRootUnavailable) {
		return "denied", false, "respect the host Agent and root access settings; do not retry unchanged input"
	}
	message := strings.ToLower(err.Error())
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "not_found", false, "verify the identifier or list available resources; do not retry the same missing identifier"
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(message, "timed out"), strings.Contains(message, "timeout"):
		return "timeout", true, "narrow the operation or set background=true on ssh_exec or ssh_run_script for a long-running command"
	case strings.Contains(message, "denied"), strings.Contains(message, "forbidden"):
		return "denied", false, "respect the denial and choose a permitted operation"
	case strings.Contains(message, "required"), strings.Contains(message, "invalid"), strings.Contains(message, "unsupported"):
		return "validation_failed", false, "correct the tool input using the error message; do not repeat unchanged input"
	case strings.Contains(message, "changed"), strings.Contains(message, "conflict"):
		return "conflict", true, "read the current state again before proposing another change"
	case strings.Contains(message, "constraint failed"):
		return "internal_error", false, "stop the affected workflow and report the control-plane persistence failure"
	default:
		return "remote_failed", true, "inspect stderr and gather narrower read-only details before retrying"
	}
}

func NormalizeValue(ctx context.Context, toolName string, value any, err error) (any, error) {
	if err == nil {
		return value, nil
	}
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return nil, err
	}
	return FailureFromError(toolName, err), nil
}

func FailureFromError(toolName string, err error) domain.ToolFailure {
	code, message, retryable, nextAction := Classify(toolName, err)
	failure := domain.ToolFailure{
		ToolMeta: domain.ToolMeta{
			ToolVersion: "1.1",
			OK:          false,
			Code:        code,
			Message:     message,
			Retryable:   retryable,
			NextAction:  nextAction,
		},
		Status: "failed",
	}
	var inputValidation *agenttool.InputError
	if errors.As(err, &inputValidation) {
		failure.Validation = inputValidation.Validation()
	}
	return failure
}

func Classify(toolName string, err error) (code, message string, retryable bool, nextAction string) {
	messageLower := strings.ToLower(err.Error())
	rootMessage := rootError(err).Error()
	var inputValidation *agenttool.InputError
	var serviceValidation *service.InputValidationError
	var selectionErr *service.ExecutionToolSelectionError
	switch {
	case errors.As(err, &inputValidation):
		return "validation_failed", inputValidation.Error(), false, "correct the function tool input using this error; do not repeat unchanged input"
	case errors.As(err, &serviceValidation):
		return "validation_failed", rootMessage, false, "correct the function tool input using this error; do not repeat unchanged input"
	case errors.As(err, &selectionErr):
		return "wrong_tool", rootMessage, false, selectionErr.NextAction
	case errors.Is(err, service.ErrAgentHostAccessDenied), errors.Is(err, service.ErrAgentRootAccessDenied), errors.Is(err, service.ErrHostAgentRootUnavailable):
		return "denied", rootMessage, false, "respect the host Agent and root access settings; do not retry unchanged input"
	case errors.Is(err, store.ErrNotFound), errors.Is(err, skills.ErrNotFound):
		return "not_found", rootMessage, false, "list or read the available resources and use a valid identifier"
	case errors.Is(err, skills.ErrDisabled):
		return "configuration_required", rootMessage, false, "tell the operator that the requested skill is disabled; do not retry it"
	case strings.Contains(messageLower, "failed to unmarshal arguments"), strings.Contains(messageLower, "invalid type, toolname="):
		return "validation_failed", "the function tool arguments are not valid for its input schema", false, "correct the arguments using the function tool schema before trying again"
	case strings.Contains(messageLower, "failed to marshal output"):
		return "internal_error", "the function tool could not encode its result", false, "stop this workflow and report the function tool failure to the operator"
	case errors.Is(err, context.DeadlineExceeded):
		if strings.HasPrefix(toolName, "mcp__") {
			return "outcome_unknown", "the external MCP call timed out and may have taken effect", false, "inspect the external system state before deciding whether another call is safe"
		}
		return "timeout", "the function tool did not finish before its timeout", true, "retry only after narrowing the operation or increasing its configured timeout"
	case toolName == "ssh_shell" &&
		(strings.Contains(messageLower, "requesting a credential") || strings.Contains(messageLower, "private web terminal")):
		return "operator_input_required", rootMessage, false, "wait for the operator to enter the credential in the private Web terminal; do not send, request, or retry the credential"
	case toolName == "ssh_shell" &&
		(strings.Contains(messageLower, "shell limit reached") || strings.Contains(messageLower, "service is shutting down")):
		return "unavailable", rootMessage, false, "close an active shell or wait until the service is available before starting another one"
	case strings.Contains(messageLower, "required"), strings.Contains(messageLower, "invalid"), strings.Contains(messageLower, "unsupported"):
		return "validation_failed", rootMessage, false, "correct the function tool input using this error; do not repeat unchanged input"
	case strings.Contains(messageLower, "changed"), strings.Contains(messageLower, "conflict"):
		return "conflict", rootMessage, true, "read the current state again before proposing another change"
	case strings.Contains(messageLower, "denied"), strings.Contains(messageLower, "forbidden"):
		return "denied", rootMessage, false, "respect the denial and choose a permitted operation"
	case strings.HasPrefix(toolName, "mcp__") && strings.Contains(messageLower, "not ready"):
		return "provider_failed", "the external MCP server is not ready", false, "tell the operator to check or reconnect the MCP server"
	case strings.HasPrefix(toolName, "mcp__"):
		return "outcome_unknown", "the external MCP call failed and its side effects are unknown", false, "inspect the external system state before deciding whether another call is safe"
	case toolName == "ssh_host_inspect":
		return "remote_failed", "the SSH host inspection failed", true, "check the registered host state and retry once only if the failure appears transient"
	default:
		return "internal_error", "the function tool failed internally", false, "stop the affected workflow and report the function tool failure to the operator"
	}
}

func rootError(err error) error {
	for {
		next := errors.Unwrap(err)
		if next == nil {
			return err
		}
		err = next
	}
}
