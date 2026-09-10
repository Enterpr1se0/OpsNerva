package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	planlogic "github.com/Enterpr1se0/opsnerva/internal/plan"
)

func (s *Service) GetAgentPlan(ctx context.Context, sessionID string) (domain.AgentPlan, error) {
	if sessionID == "" {
		sessionID = SessionIDFromContext(ctx)
	}
	if sessionID == "" {
		return domain.AgentPlan{}, asInputValidationError(errors.New("plan requires a session context"))
	}
	return s.store.GetAgentPlan(ctx, sessionID)
}

func (s *Service) CreateAgentPlan(ctx context.Context, goal string, titles []string, actor string) (domain.AgentPlan, error) {
	sessionID := SessionIDFromContext(ctx)
	if sessionID == "" {
		return domain.AgentPlan{}, asInputValidationError(errors.New("plan requires a session context"))
	}
	plan, err := planlogic.New(goal, titles)
	if err != nil {
		return plan, asInputValidationError(err)
	}
	plan.SessionID = sessionID
	plan, err = s.store.ReplaceAgentPlan(ctx, plan)
	if err == nil {
		s.audit(ctx, "", "agent_plan_created", actor, map[string]any{"session_id": sessionID, "goal": plan.Goal, "step_count": len(plan.Steps)})
	}
	return plan, agentPlanError(err)
}

func (s *Service) UpdateAgentPlanStep(ctx context.Context, number int, status, actor string) (domain.AgentPlan, error) {
	sessionID := SessionIDFromContext(ctx)
	if sessionID == "" {
		return domain.AgentPlan{}, asInputValidationError(errors.New("plan requires a session context"))
	}
	plan, err := s.store.TransitionAgentPlanStep(ctx, sessionID, number, status)
	if err == nil {
		s.audit(ctx, "", "agent_plan_step_updated", actor, map[string]any{"session_id": sessionID, "step_number": number, "status": status})
	}
	return plan, agentPlanError(err)
}

func (s *Service) ReviseAgentPlan(ctx context.Context, titles []string, actor string) (domain.AgentPlan, error) {
	sessionID := SessionIDFromContext(ctx)
	if sessionID == "" {
		return domain.AgentPlan{}, asInputValidationError(errors.New("plan requires a session context"))
	}
	plan, err := s.store.ReviseAgentPlanRemaining(ctx, sessionID, titles)
	if err == nil {
		s.audit(ctx, "", "agent_plan_revised", actor, map[string]any{"session_id": sessionID, "remaining_steps": len(titles)})
	}
	return plan, agentPlanError(err)
}

// Plan validation is translated at the application boundary. Database and
// cancellation errors must keep their original classification.
func agentPlanError(err error) error {
	if errors.Is(err, planlogic.ErrInvalid) {
		return asInputValidationError(err)
	}
	return err
}

func currentAgentPlanTask(plan domain.AgentPlan) string {
	if step := planlogic.CurrentStep(plan); step != nil {
		return fmt.Sprintf("%s — %s", plan.Goal, step.Title)
	}
	return ""
}
