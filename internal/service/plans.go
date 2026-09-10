package service

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Enterpr1se0/opsnerva/internal/agenttool"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func (s *Service) GetAgentPlan(ctx context.Context, sessionID string) (domain.AgentPlan, error) {
	if sessionID == "" {
		sessionID = SessionIDFromContext(ctx)
	}
	if sessionID == "" {
		return domain.AgentPlan{}, agenttool.InvalidInput("plan requires a session context")
	}
	return s.store.GetAgentPlan(ctx, sessionID)
}

func (s *Service) CreateAgentPlan(ctx context.Context, goal string, titles []string, actor string) (domain.AgentPlan, error) {
	sessionID := SessionIDFromContext(ctx)
	goal = strings.TrimSpace(goal)
	if sessionID == "" || goal == "" || utf8.RuneCountInString(goal) > 500 {
		return domain.AgentPlan{}, agenttool.InvalidInput("plan requires a session and a goal of 1-500 characters")
	}
	normalized, err := normalizePlanTitles(titles, 2)
	if err != nil {
		return domain.AgentPlan{}, err
	}
	steps := make([]domain.AgentPlanStep, len(normalized))
	for i, title := range normalized {
		status := "pending"
		if i == 0 {
			status = "in_progress"
		}
		steps[i] = domain.AgentPlanStep{Number: i + 1, Title: title, Status: status}
	}
	plan, err := s.store.ReplaceAgentPlan(ctx, domain.AgentPlan{SessionID: sessionID, Goal: goal, Status: "active", Steps: steps})
	if err == nil {
		s.audit(ctx, "", "agent_plan_created", actor, map[string]any{"session_id": sessionID, "goal": goal, "step_count": len(steps)})
	}
	return plan, err
}

func (s *Service) UpdateAgentPlanStep(ctx context.Context, number int, status, actor string) (domain.AgentPlan, error) {
	sessionID := SessionIDFromContext(ctx)
	if sessionID == "" || number < 1 || (status != "completed" && status != "skipped") {
		return domain.AgentPlan{}, agenttool.InvalidInput("set the current step_number to completed or skipped")
	}
	plan, err := s.store.TransitionAgentPlanStep(ctx, sessionID, number, status)
	if err == nil {
		s.audit(ctx, "", "agent_plan_step_updated", actor, map[string]any{"session_id": sessionID, "step_number": number, "status": status})
	}
	return plan, err
}

func (s *Service) ReviseAgentPlan(ctx context.Context, titles []string, actor string) (domain.AgentPlan, error) {
	sessionID := SessionIDFromContext(ctx)
	if sessionID == "" {
		return domain.AgentPlan{}, agenttool.InvalidInput("plan requires a session context")
	}
	normalized, err := normalizePlanTitles(titles, 1)
	if err != nil {
		return domain.AgentPlan{}, err
	}
	plan, err := s.store.ReviseAgentPlanRemaining(ctx, sessionID, normalized)
	if err == nil {
		s.audit(ctx, "", "agent_plan_revised", actor, map[string]any{"session_id": sessionID, "remaining_steps": len(titles)})
	}
	return plan, err
}

func normalizePlanTitles(titles []string, minimum int) ([]string, error) {
	if len(titles) < minimum || len(titles) > 8 {
		return nil, agenttool.InvalidInput("provide %d-8 ordered steps", minimum)
	}
	result := make([]string, len(titles))
	seen := make(map[string]bool, len(titles))
	for i, title := range titles {
		title = strings.TrimSpace(title)
		if title == "" || utf8.RuneCountInString(title) > 240 || seen[strings.ToLower(title)] {
			return nil, agenttool.InvalidInput("step %d must have a unique title of 1-240 characters", i+1)
		}
		seen[strings.ToLower(title)] = true
		result[i] = title
	}
	return result, nil
}

func currentAgentPlanTask(plan domain.AgentPlan) string {
	if step := plan.CurrentStep(); step != nil {
		return fmt.Sprintf("%s — %s", plan.Goal, step.Title)
	}
	return ""
}
