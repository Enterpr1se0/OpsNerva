package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/observability"
	"eino-ops-agent/internal/service"
	"eino-ops-agent/internal/skills"
	"eino-ops-agent/internal/store"

	"github.com/cloudwego/eino/compose"
)

type toolActivityContextKey struct{}

type toolCallActivity struct {
	CallID    string
	Name      string
	Arguments string
	Status    string
	Result    string
	Error     string
}

type toolInputValidationError struct {
	message    string
	validation *domain.ToolValidationDetails
}

func (err *toolInputValidationError) Error() string {
	return err.message
}

func invalidToolInput(format string, arguments ...any) error {
	return &toolInputValidationError{message: fmt.Sprintf(format, arguments...)}
}

func invalidStructuredToolInput(message string, validation domain.ToolValidationDetails) error {
	return &toolInputValidationError{message: message, validation: &validation}
}

func withToolActivityNotifier(ctx context.Context, notify func(toolCallActivity)) context.Context {
	if notify == nil {
		return ctx
	}
	return context.WithValue(ctx, toolActivityContextKey{}, notify)
}

func notifyToolActivity(ctx context.Context, activity toolCallActivity) {
	if ctx == nil {
		return
	}
	notify, ok := ctx.Value(toolActivityContextKey{}).(func(toolCallActivity))
	if !ok || notify == nil {
		return
	}
	notify(activity)
}

func notifyToolStarted(ctx context.Context, input *compose.ToolInput) {
	if input == nil {
		return
	}
	notifyToolActivity(ctx, toolCallActivity{
		CallID: input.CallID, Name: input.Name, Arguments: input.Arguments,
		Status: domain.ChatToolCallRunning,
	})
}

type planToolResult struct {
	domain.ToolFailure
	Plan *domain.AgentPlan `json:"plan,omitempty"`
}

func normalizeValueToolResult[T any](ctx context.Context, toolName string, value T, err error) (any, error) {
	if err == nil {
		return value, nil
	}
	failure, fatalErr := normalizeToolFailure(ctx, toolName, err)
	if fatalErr != nil {
		return nil, fatalErr
	}
	return failure, nil
}

func normalizePlanToolResult(ctx context.Context, svc *service.Service, toolName string, plan domain.AgentPlan, err error) (any, error) {
	if err == nil {
		return plan, nil
	}
	failure, fatalErr := normalizeToolFailure(ctx, toolName, err)
	if fatalErr != nil {
		return nil, fatalErr
	}
	result := planToolResult{ToolFailure: failure}
	current, currentErr := svc.GetAgentPlan(ctx, "")
	if currentErr == nil {
		result.Plan = &current
		for _, step := range current.Steps {
			if step.Status == "in_progress" {
				result.NextAction = fmt.Sprintf("update step %d, or revise the remaining plan if its order or scope changed", step.Number)
				break
			}
			if step.Status == "blocked" {
				result.NextAction = fmt.Sprintf("resume, skip, or revise blocked step %d", step.Number)
				break
			}
		}
	}
	return result, nil
}

func normalizeToolFailure(ctx context.Context, toolName string, err error) (domain.ToolFailure, error) {
	if fatalErr := fatalToolError(ctx, err); fatalErr != nil {
		return domain.ToolFailure{}, fatalErr
	}
	return toolFailureFromError(toolName, err), nil
}

func fatalToolError(ctx context.Context, err error) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	_, interrupt := compose.IsInterruptRerunError(err)
	if interrupt {
		return err
	}
	return nil
}

func toolFailureFromError(toolName string, err error) domain.ToolFailure {
	code, message, retryable, nextAction := classifyAgentToolError(toolName, err)
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
	var inputValidation *toolInputValidationError
	if errors.As(err, &inputValidation) {
		failure.Validation = inputValidation.validation
	}
	return failure
}

func classifyAgentToolError(toolName string, err error) (code, message string, retryable bool, nextAction string) {
	messageLower := strings.ToLower(err.Error())
	rootMessage := rootToolError(err).Error()
	var transition *store.PlanTransitionError
	var inputValidation *toolInputValidationError
	switch {
	case errors.As(err, &transition):
		return "invalid_state", transition.Error(), false, "update the current step or revise the remaining plan"
	case errors.As(err, &inputValidation):
		return "validation_failed", inputValidation.Error(), false, "correct the function tool input using this error; do not repeat unchanged input"
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

func rootToolError(err error) error {
	for {
		next := errors.Unwrap(err)
		if next == nil {
			return err
		}
		err = next
	}
}

// normalizeEmptyToolArguments repairs tool calls whose argument payload
// arrived empty. The Claude model component deliberately rewrites "{}"
// streaming arguments to "" to keep chunk concatenation stable, which would
// otherwise fail JSON unmarshalling for every parameterless tool call.
func normalizeEmptyToolArguments(next compose.InvokableToolEndpoint) compose.InvokableToolEndpoint {
	return func(ctx context.Context, input *compose.ToolInput) (*compose.ToolOutput, error) {
		if strings.TrimSpace(input.Arguments) == "" {
			input.Arguments = "{}"
		}
		return next(ctx, input)
	}
}

func normalizeToolCallErrors(next compose.InvokableToolEndpoint) compose.InvokableToolEndpoint {
	return func(ctx context.Context, input *compose.ToolInput) (output *compose.ToolOutput, err error) {
		started := time.Now()
		logger := observability.FromContext(ctx).With("component", "agent", "tool_name", input.Name, "tool_call_id", input.CallID)
		ctx, release := scopedToolContext(ctx, input.CallID)
		defer release()
		notifyToolStarted(ctx, input)
		ctx = service.WithExecutionOwner(ctx, input.CallID, input.Name, input.Arguments)
		defer func() {
			activityErr := err
			if errors.Is(activityErr, context.Canceled) && errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
				activityErr = context.DeadlineExceeded
			}
			status, result, errorText := completedToolActivity(output, activityErr)
			notifyToolActivity(ctx, toolCallActivity{
				CallID: input.CallID, Name: input.Name, Arguments: input.Arguments,
				Status: status, Result: result, Error: errorText,
			})
		}()
		defer func() {
			if recovered := recover(); recovered != nil {
				if ctx.Err() != nil {
					output = nil
					err = ctx.Err()
					return
				}
				failure := domain.ToolFailure{
					ToolMeta: domain.ToolMeta{ToolVersion: "1.1", OK: false, Code: "internal_error", Message: "the function tool failed internally", NextAction: "stop the affected workflow and report the function tool failure to the operator"},
					Status:   "failed",
				}
				output = &compose.ToolOutput{Result: marshalToolFailure(failure)}
				err = nil
				logger.ErrorContext(ctx, "function tool panicked", "panic_type", fmt.Sprintf("%T", recovered), "stack", string(debug.Stack()), "duration_ms", time.Since(started).Milliseconds())
			}
		}()

		output, err = next(ctx, input)
		if err != nil {
			failure, fatalErr := normalizeToolFailure(ctx, input.Name, err)
			if fatalErr != nil {
				return nil, fatalErr
			}
			logger.WarnContext(ctx, "function tool error returned to Agent", "code", failure.Code, "message", failure.Message, "duration_ms", time.Since(started).Milliseconds())
			return &compose.ToolOutput{Result: marshalToolFailure(failure)}, nil
		}
		logStructuredToolFailure(ctx, logger, output, time.Since(started))
		return output, nil
	}
}

// Execution ownership is bound when the service creates a run. Result payloads
// can reference runs owned by other tool calls and only determine status here.
func completedToolActivity(output *compose.ToolOutput, err error) (status, result, errorText string) {
	status = domain.ChatToolCallCompleted
	if output != nil {
		result = output.Result
	}
	if err != nil {
		errorText = err.Error()
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			status = domain.ChatToolCallExpired
		case errors.Is(err, context.Canceled):
			status = domain.ChatToolCallInterrupted
		default:
			status = domain.ChatToolCallFailed
		}
		return status, result, errorText
	}
	var payload map[string]any
	if json.Unmarshal([]byte(result), &payload) != nil {
		return status, result, ""
	}
	resultStatus := stringField(payload, "status")
	code := stringField(payload, "code")
	for _, key := range []string{"result", "task"} {
		if nested, ok := payload[key].(map[string]any); ok {
			if resultStatus == "" {
				resultStatus = stringField(nested, "status")
			}
			if code == "" {
				code = stringField(nested, "code")
			}
		}
	}
	switch resultStatus {
	case domain.ChatToolCallPartial:
		status = domain.ChatToolCallPartial
	case domain.ChatToolCallFailed:
		status = domain.ChatToolCallFailed
	case domain.ChatToolCallInterrupted, "cancelled":
		status = domain.ChatToolCallInterrupted
	case domain.ChatToolCallRejected, "denied":
		status = domain.ChatToolCallRejected
	case domain.ChatToolCallExpired, "timeout":
		status = domain.ChatToolCallExpired
	case domain.ChatToolCallUnknown:
		status = domain.ChatToolCallUnknown
	}
	if code == "outcome_unknown" {
		status = domain.ChatToolCallUnknown
	} else if okValue, exists := payload["ok"].(bool); exists && !okValue && status == domain.ChatToolCallCompleted {
		status = domain.ChatToolCallFailed
	}
	return status, result, ""
}

func stringField(payload map[string]any, key string) string {
	value, _ := payload[key].(string)
	return strings.TrimSpace(value)
}

func logStructuredToolFailure(ctx context.Context, logger interface {
	WarnContext(context.Context, string, ...any)
}, output *compose.ToolOutput, duration time.Duration) {
	if output == nil || strings.TrimSpace(output.Result) == "" {
		return
	}
	var meta struct {
		OK      *bool  `json:"ok"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(output.Result), &meta) != nil || meta.OK == nil || *meta.OK {
		return
	}
	logger.WarnContext(ctx, "function tool completed with failure", "code", meta.Code, "message", meta.Message, "duration_ms", duration.Milliseconds())
}

func unknownToolResult(ctx context.Context, name, _ string) (string, error) {
	failure := domain.ToolFailure{
		ToolMeta: domain.ToolMeta{
			ToolVersion: "1.1", OK: false, Code: "unknown_tool",
			Message:    "the requested function tool is not available",
			NextAction: "use one of the function tools provided in the current tool list",
		},
		Status: "failed",
	}
	observability.FromContext(ctx).WarnContext(ctx, "Agent requested an unknown function tool", "component", "agent", "tool_name", name)
	return marshalToolFailure(failure), nil
}

func marshalToolFailure(failure domain.ToolFailure) string {
	payload, err := json.Marshal(failure)
	if err == nil {
		return string(payload)
	}
	return `{"tool_version":"1.1","ok":false,"status":"failed","code":"internal_error","message":"the function tool failed internally"}`
}
