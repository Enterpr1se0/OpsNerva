package agent

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/adk"
)

type ApprovalPause struct {
	SessionID     string
	UserMessageID string
	ApprovalID    string
	RunID         string
	CheckpointID  string
	InterruptID   string
}

type ApprovalWait func(context.Context) error
type ApprovalPauseRegistrar func([]ApprovalPause) (ApprovalWait, error)

type approvalPauseRegistrarContextKey struct{}

func WithApprovalPauseRegistrar(ctx context.Context, register ApprovalPauseRegistrar) context.Context {
	if ctx == nil || register == nil {
		return ctx
	}
	return context.WithValue(ctx, approvalPauseRegistrarContextKey{}, register)
}

func registerApprovalPause(ctx context.Context, pauses []ApprovalPause) (ApprovalWait, error) {
	register, _ := ctx.Value(approvalPauseRegistrarContextKey{}).(ApprovalPauseRegistrar)
	if register == nil {
		return nil, fmt.Errorf("Agent approval pause coordinator is unavailable")
	}
	if len(pauses) == 0 {
		return nil, fmt.Errorf("Agent approval pause group is empty")
	}
	return register(pauses)
}

type resumableAgentRunner interface {
	agentRunner
	ResumeWithParams(context.Context, string, *adk.ResumeParams, ...adk.AgentRunOption) (*adk.AsyncIterator[*adk.AgentEvent], error)
}

type approvalInterruptPoint struct {
	State       approvalInterrupt
	InterruptID string
}

func approvalInterruptTargets(interrupted *adk.InterruptInfo) []approvalInterruptPoint {
	if interrupted == nil {
		return nil
	}
	result := make([]approvalInterruptPoint, 0, len(interrupted.InterruptContexts))
	for _, point := range interrupted.InterruptContexts {
		if point == nil || !point.IsRootCause {
			continue
		}
		state, ok := point.Info.(approvalInterrupt)
		if ok && state.ApprovalID != "" && state.RunID != "" && point.ID != "" {
			result = append(result, approvalInterruptPoint{State: state, InterruptID: point.ID})
		}
	}
	return result
}

var _ resumableAgentRunner = (*adk.Runner)(nil)
