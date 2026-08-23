package service

import (
	"context"
	"strings"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

type sessionContextKey struct{}
type blockingApprovalContextKey struct{}
type approvalNotifierContextKey struct{}
type executionOwnerContextKey struct{}
type approvalUserRequestContextKey struct{}

type executionOwner struct {
	ToolCallID string
	ToolName   string
	Arguments  string
	Source     string
}

// WithSessionID binds an Agent conversation to all audited runs created by
// tools below this context. Session IDs never come from model tool arguments.
func WithSessionID(ctx context.Context, sessionID string) context.Context {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionContextKey{}, sessionID)
}

func SessionIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(sessionContextKey{}).(string)
	return value
}

// WithApprovalUserRequest binds the current user turn to any approval review
// started by that Agent run. It is control-plane context, never a tool field.
func WithApprovalUserRequest(ctx context.Context, request string) context.Context {
	request = strings.TrimSpace(request)
	if request == "" {
		return ctx
	}
	return context.WithValue(ctx, approvalUserRequestContextKey{}, request)
}

func approvalUserRequestFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(approvalUserRequestContextKey{}).(string)
	return strings.TrimSpace(value)
}

// WithExecutionOwner binds a service run to the Agent tool card that started
// it. The binding is copied into approved and background execution events.
func WithExecutionOwner(ctx context.Context, toolCallID, toolName, arguments string) context.Context {
	if ctx == nil || (strings.TrimSpace(toolCallID) == "" && strings.TrimSpace(toolName) == "" && strings.TrimSpace(arguments) == "") {
		return ctx
	}
	return context.WithValue(ctx, executionOwnerContextKey{}, executionOwner{
		ToolCallID: strings.TrimSpace(toolCallID),
		ToolName:   strings.TrimSpace(toolName),
		Arguments:  strings.TrimSpace(arguments),
	})
}

// WithMCPToolCall binds one server-assigned MCP session and call to every
// service operation below it. Neither identifier is accepted from tool input.
func WithMCPToolCall(ctx context.Context, sessionID, callID, toolName, arguments string) context.Context {
	ctx = WithSessionID(ctx, sessionID)
	return context.WithValue(ctx, executionOwnerContextKey{}, executionOwner{
		ToolCallID: strings.TrimSpace(callID),
		ToolName:   strings.TrimSpace(toolName),
		Arguments:  strings.TrimSpace(arguments),
		Source:     "mcp",
	})
}

func executionOwnerFromContext(ctx context.Context) (executionOwner, bool) {
	if ctx == nil {
		return executionOwner{}, false
	}
	owner, ok := ctx.Value(executionOwnerContextKey{}).(executionOwner)
	return owner, ok && (owner.ToolCallID != "" || owner.ToolName != "" || owner.Arguments != "")
}

// WithBlockingApprovals makes approval-producing Submit calls wait for the
// human decision. It is set only for the Eino Agent run: CLI, MCP and direct
// API callers keep the non-blocking approval_required contract.
func WithBlockingApprovals(ctx context.Context) context.Context {
	return context.WithValue(ctx, blockingApprovalContextKey{}, true)
}

func blockingApprovalsFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	value, _ := ctx.Value(blockingApprovalContextKey{}).(bool)
	return value
}

// WithApprovalNotifier lets the Agent transport surface a pending approval
// immediately over its existing SSE stream while the Tool call remains
// blocked. The callback receives only already-redacted result metadata.
func WithApprovalNotifier(ctx context.Context, notify func(domain.ExecResult)) context.Context {
	if notify == nil {
		return ctx
	}
	return context.WithValue(ctx, approvalNotifierContextKey{}, notify)
}

func notifyApproval(ctx context.Context, result domain.ExecResult) {
	if ctx == nil {
		return
	}
	if notify, ok := ctx.Value(approvalNotifierContextKey{}).(func(domain.ExecResult)); ok && notify != nil {
		notify(result)
	}
}
