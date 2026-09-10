package service

import (
	"context"
	"errors"
	"testing"

	planlogic "github.com/Enterpr1se0/opsnerva/internal/plan"
	"github.com/Enterpr1se0/opsnerva/internal/store"
)

func TestAgentPlanSequentialLifecycle(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := WithSessionID(context.Background(), "plan-session")
	plan, err := svc.CreateAgentPlan(ctx, "Repair service", []string{"Inspect", "Repair", "Verify"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if planlogic.CurrentStep(plan).Number != 1 || len(plan.Steps) != 3 {
		t.Fatalf("initial plan: %#v", plan)
	}
	var validation *InputValidationError
	if _, err := svc.UpdateAgentPlanStep(ctx, 2, "completed", "test"); !errors.As(err, &validation) || !errors.Is(err, planlogic.ErrInvalid) {
		t.Fatalf("out-of-order completion was not classified as validation: %v", err)
	}
	if _, err := svc.UpdateAgentPlanStep(ctx, 1, "blocked", "test"); err == nil {
		t.Fatal("accepted removed blocked status")
	}
	plan, err = svc.UpdateAgentPlanStep(ctx, 1, "completed", "test")
	if err != nil {
		t.Fatal(err)
	}
	finishedAt := plan.Steps[0].UpdatedAt
	if planlogic.CurrentStep(plan).Number != 2 || currentAgentPlanTask(plan) != "Repair service — Repair" {
		t.Fatalf("current task: %#v", plan)
	}
	plan, err = svc.ReviseAgentPlan(ctx, []string{"Adjust", "Verify"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Steps[0].Title != "Inspect" || !plan.Steps[0].UpdatedAt.Equal(finishedAt) || planlogic.CurrentStep(plan).Title != "Adjust" {
		t.Fatalf("revision lost history: %#v", plan)
	}
	if _, err := svc.UpdateAgentPlanStep(ctx, 2, "skipped", "test"); err != nil {
		t.Fatal(err)
	}
	plan, err = svc.UpdateAgentPlanStep(ctx, 3, "completed", "test")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "completed" || planlogic.CurrentStep(plan) != nil || len(plan.Steps) != 3 {
		t.Fatalf("final plan: %#v", plan)
	}
	stored, err := svc.GetAgentPlan(ctx, "")
	if err != nil || stored.Status != "completed" || len(stored.Steps) != 3 {
		t.Fatalf("finished plan disappeared: %#v %v", stored, err)
	}
	if _, err := svc.GetAgentPlan(WithSessionID(context.Background(), "another-session"), ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("plan crossed sessions: %v", err)
	}
	if _, err := svc.ReviseAgentPlan(ctx, []string{"Redo"}, "test"); err == nil {
		t.Fatal("revised completed history")
	}
	plan, err = svc.CreateAgentPlan(ctx, "New work", []string{"Inspect", "Verify"}, "test")
	if err != nil || plan.Goal != "New work" || len(plan.Steps) != 2 {
		t.Fatalf("new plan: %#v %v", plan, err)
	}
}

func TestAgentPlanInputValidation(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := WithSessionID(context.Background(), "plan-input")
	if _, err := svc.CreateAgentPlan(context.Background(), "Goal", []string{"A", "B"}, "test"); err == nil {
		t.Fatal("accepted missing session")
	}
	for _, titles := range [][]string{{"one"}, {"same", " SAME "}, {"", "valid"}} {
		var validation *InputValidationError
		if _, err := svc.CreateAgentPlan(ctx, "Goal", titles, "test"); !errors.As(err, &validation) || !errors.Is(err, planlogic.ErrInvalid) {
			t.Fatalf("expected application/plan validation for %#v: %v", titles, err)
		}
	}
}

func TestAgentPlanErrorClassification(t *testing.T) {
	var validation *InputValidationError
	if err := agentPlanError(planlogic.ErrInvalid); !errors.As(err, &validation) || !errors.Is(err, planlogic.ErrInvalid) {
		t.Fatalf("plan error not mapped to application validation: %v", err)
	}
	for _, original := range []error{nil, store.ErrNotFound, context.Canceled, context.DeadlineExceeded, errors.New("database is closed")} {
		if mapped := agentPlanError(original); mapped != original {
			t.Fatalf("changed non-validation error: %v -> %v", original, mapped)
		}
	}
}
