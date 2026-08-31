package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/observability"
	"github.com/Enterpr1se0/opsnerva/internal/service"
	"github.com/Enterpr1se0/opsnerva/internal/toolresult"

	einotool "github.com/cloudwego/eino/components/tool"
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

type approvalInterrupt struct {
	ApprovalID string
	RunID      string
}

type approvalResumeDecision struct {
	ApprovalID string
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

func normalizeToolFailure(ctx context.Context, toolName string, err error) (domain.ToolFailure, error) {
	if fatalErr := fatalToolError(ctx, err); fatalErr != nil {
		return domain.ToolFailure{}, fatalErr
	}
	return toolresult.FailureFromError(toolName, err), nil
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

func normalizeToolCallErrors(svc *service.Service, next compose.InvokableToolEndpoint) compose.InvokableToolEndpoint {
	return func(ctx context.Context, input *compose.ToolInput) (output *compose.ToolOutput, err error) {
		started := time.Now()
		var paused *approvalInterrupt
		logger := observability.FromContext(ctx).With("component", "agent", "tool_name", input.Name, "tool_call_id", input.CallID)
		ctx, release := scopedToolContext(ctx, input.CallID)
		defer release()
		notifyToolStarted(ctx, input)
		ctx = service.WithExecutionOwner(ctx, input.CallID, input.Name, input.Arguments)
		defer func() {
			if paused != nil {
				result := ""
				if output != nil {
					result = output.Result
				}
				notifyToolActivity(ctx, toolCallActivity{
					CallID: input.CallID, Name: input.Name, Arguments: input.Arguments,
					Status: domain.ChatToolCallApprovalRequired, Result: result,
				})
				return
			}
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

		wasInterrupted, hasState, interrupt := einotool.GetInterruptState[approvalInterrupt](ctx)
		if wasInterrupted {
			if !hasState || interrupt.ApprovalID == "" || interrupt.RunID == "" {
				return nil, fmt.Errorf("Agent approval checkpoint is missing its tool state")
			}
			isTarget, hasDecision, decision := einotool.GetResumeContext[approvalResumeDecision](ctx)
			if !isTarget {
				paused = &interrupt
				return nil, einotool.StatefulInterrupt(ctx, interrupt, interrupt)
			}
			if !hasDecision || decision.ApprovalID != interrupt.ApprovalID {
				return nil, fmt.Errorf("Agent approval resume target does not match its checkpoint")
			}
			if svc == nil {
				return nil, fmt.Errorf("Agent approval service is unavailable")
			}
			result, resumeErr := svc.ResumeAgentApproval(ctx, interrupt.ApprovalID)
			compact, normalizedErr := toolresult.CompactExec(result, resumeErr)
			if normalizedErr != nil {
				return nil, normalizedErr
			}
			encoded, marshalErr := json.Marshal(compact)
			if marshalErr != nil {
				return nil, fmt.Errorf("encode resumed Agent approval result: %w", marshalErr)
			}
			output = &compose.ToolOutput{Result: string(encoded)}
			logStructuredToolFailure(ctx, logger, output, time.Since(started))
			return output, nil
		}
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
		if interrupt, ok := approvalInterruptFromOutput(output); ok {
			paused = &interrupt
			return output, einotool.StatefulInterrupt(ctx, interrupt, interrupt)
		}
		return output, nil
	}
}

func approvalInterruptFromOutput(output *compose.ToolOutput) (approvalInterrupt, bool) {
	if output == nil || strings.TrimSpace(output.Result) == "" {
		return approvalInterrupt{}, false
	}
	var result struct {
		Status     string `json:"status"`
		ApprovalID string `json:"approval_id"`
		RunID      string `json:"run_id"`
	}
	if json.Unmarshal([]byte(output.Result), &result) != nil || result.Status != "approval_required" || result.ApprovalID == "" || result.RunID == "" {
		return approvalInterrupt{}, false
	}
	return approvalInterrupt{ApprovalID: result.ApprovalID, RunID: result.RunID}, true
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
