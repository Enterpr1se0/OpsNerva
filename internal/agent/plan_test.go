package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/agenttool"
	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/security"
	"github.com/Enterpr1se0/opsnerva/internal/service"
	"github.com/Enterpr1se0/opsnerva/internal/store"
	"github.com/cloudwego/eino/components/tool"
)

func TestPlanToolCatalogAndSessionContext(t *testing.T) {
	st, err := store.Open(context.Background(), t.TempDir()+"/plans.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	encryptor, err := security.NewEncryptor("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(st, nil, encryptor, security.NewRedactor(), config.Default().Limits)
	tools, err := buildPlanTools(svc)
	if err != nil {
		t.Fatal(err)
	}
	descriptors, err := agenttool.Describe(context.Background(), tools)
	if err != nil {
		t.Fatal(err)
	}
	if len(descriptors) != 3 {
		t.Fatalf("plan tool count: %d", len(descriptors))
	}
	byName := make(map[string]tool.InvokableTool)
	for i, candidate := range tools {
		if descriptors[i].Category != "planning" || descriptors[i].Guard != "agent_state" {
			t.Fatalf("descriptor: %#v", descriptors[i])
		}
		byName[descriptors[i].Name] = candidate.(tool.InvokableTool)
	}
	schema := string(descriptors[1].InputSchema)
	if !strings.Contains(schema, `"enum":["completed","skipped"]`) || strings.Contains(schema, "blocked") {
		t.Fatalf("step schema: %s", schema)
	}
	ctx := service.WithSessionID(context.Background(), "plan-tools")
	content, err := byName["ops_plan_create"].InvokableRun(ctx, `{"goal":"Goal","steps":["Inspect","Verify"]}`)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Status string           `json:"status"`
		Plan   domain.AgentPlan `json:"plan"`
	}
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" || result.Plan.Status != "active" || result.Plan.SessionID != "plan-tools" || result.Plan.CurrentStep().Number != 1 {
		t.Fatalf("plan result: %s", content)
	}
	content, err = byName["ops_plan_step_update"].InvokableRun(ctx, `{"step_number":2,"status":"completed"}`)
	if err != nil {
		t.Fatal(err)
	}
	var failure struct {
		Status string            `json:"status"`
		Plan   *domain.AgentPlan `json:"plan"`
	}
	if err := json.Unmarshal([]byte(content), &failure); err != nil {
		t.Fatal(err)
	}
	if failure.Status != "failed" || failure.Plan == nil || failure.Plan.CurrentStep().Number != 1 {
		t.Fatalf("correction context missing: %s", content)
	}
}
