package agent

import (
	"context"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/service"
	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
)

type PlanCreateInput struct {
	Goal  string   `json:"goal" jsonschema:"required,minLength=1,maxLength=500" jsonschema_description:"concrete user goal"`
	Steps []string `json:"steps" jsonschema:"required,minItems=2,maxItems=8" jsonschema_description:"ordered, verifiable step titles; first step starts automatically"`
}

type PlanStepUpdateInput struct {
	StepNumber int    `json:"step_number" jsonschema:"required,minimum=1" jsonschema_description:"current step number from the plan"`
	Status     string `json:"status" jsonschema:"required,enum=completed,enum=skipped" jsonschema_description:"complete verified work, or skip work that is no longer needed; next step starts automatically"`
}

type PlanReviseInput struct {
	Steps []string `json:"steps" jsonschema:"required,minItems=1,maxItems=8" jsonschema_description:"replace current and pending steps; completed and skipped steps stay unchanged"`
}

func buildPlanTools(svc *service.Service) ([]tool.BaseTool, error) {
	create, err := toolutils.InferTool("ops_plan_create", "Create a persistent ordered plan for complex work. Step 1 starts; a new plan replaces the previous plan.", func(ctx context.Context, input PlanCreateInput) (any, error) {
		plan, err := svc.CreateAgentPlan(ctx, input.Goal, input.Steps, "eino-agent")
		return planToolResult(ctx, svc, "ops_plan_create", plan, err)
	})
	if err != nil {
		return nil, err
	}
	update, err := toolutils.InferTool("ops_plan_step_update", "Complete or skip the current step. The next pending step starts automatically; finished plans remain available.", func(ctx context.Context, input PlanStepUpdateInput) (any, error) {
		plan, err := svc.UpdateAgentPlanStep(ctx, input.StepNumber, input.Status, "eino-agent")
		return planToolResult(ctx, svc, "ops_plan_step_update", plan, err)
	})
	if err != nil {
		return nil, err
	}
	revise, err := toolutils.InferTool("ops_plan_revise", "Replace the unfinished sequence when scope changes, preserving finished steps.", func(ctx context.Context, input PlanReviseInput) (any, error) {
		plan, err := svc.ReviseAgentPlan(ctx, input.Steps, "eino-agent")
		return planToolResult(ctx, svc, "ops_plan_revise", plan, err)
	})
	if err != nil {
		return nil, err
	}
	return []tool.BaseTool{create, update, revise}, nil
}

func planToolResult(ctx context.Context, svc *service.Service, name string, plan domain.AgentPlan, err error) (any, error) {
	if err == nil {
		// A successful tool call is finished even while its plan remains active.
		return struct {
			Status string           `json:"status"`
			Plan   domain.AgentPlan `json:"plan"`
		}{"completed", plan}, nil
	}
	failure, fatalErr := normalizeToolFailure(ctx, name, err)
	if fatalErr != nil {
		return nil, fatalErr
	}
	var current *domain.AgentPlan
	if stored, readErr := svc.GetAgentPlan(ctx, ""); readErr == nil {
		current = &stored
	}
	return struct {
		domain.ToolFailure
		Plan *domain.AgentPlan `json:"plan,omitempty"`
	}{failure, current}, nil
}
