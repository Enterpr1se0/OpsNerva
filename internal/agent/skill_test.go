package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/security"
	"github.com/Enterpr1se0/opsnerva/internal/service"
	"github.com/Enterpr1se0/opsnerva/internal/store"

	"github.com/cloudwego/eino/adk"
)

func TestDisabledEinoSkillMiddlewareInjectsNeitherPromptNorTool(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/skills-disabled.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	encryptor, err := security.NewEncryptor("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	svc := service.New(st, nil, encryptor, security.NewRedactor(), cfg.Limits, cfg)

	handler, catalogTools, err := newSkillMiddleware(ctx, svc, map[string]bool{"skill": false})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalogTools) != 1 {
		t.Fatalf("catalog Skill tools = %d, want 1", len(catalogTools))
	}
	_, runCtx, err := handler.BeforeAgent(ctx, &adk.ChatModelAgentContext{Instruction: "base"})
	if err != nil {
		t.Fatal(err)
	}
	if runCtx.Instruction != "base" || len(runCtx.Tools) != 0 {
		t.Fatalf("disabled Skill middleware mutated Agent context: %#v", runCtx)
	}
}

func TestEinoSkillMiddlewareInjectsDynamicIndexIntoSystemPrompt(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/skills-dynamic.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	encryptor, err := security.NewEncryptor("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	svc := service.New(st, nil, encryptor, security.NewRedactor(), cfg.Limits, cfg)

	handler, _, err := newSkillMiddleware(ctx, svc, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, first, err := handler.BeforeAgent(ctx, &adk.ChatModelAgentContext{Instruction: "base"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.Instruction, "# Skills System") || !strings.Contains(first.Instruction, "No enabled Skills are available") {
		t.Fatalf("initial Skill prompt = %q", first.Instruction)
	}

	if _, err := svc.SaveAdminSkill(ctx, "powershell-ops", "---\ndescription: Run Windows maintenance with PowerShell\n---\nUse PowerShell.", "test"); err != nil {
		t.Fatal(err)
	}
	_, second, err := handler.BeforeAgent(ctx, &adk.ChatModelAgentContext{Instruction: "base"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Available Skills", "powershell-ops: Run Windows maintenance with PowerShell"} {
		if !strings.Contains(second.Instruction, want) {
			t.Fatalf("updated Skill prompt is missing %q: %s", want, second.Instruction)
		}
	}
	if len(second.Tools) != 1 {
		t.Fatalf("Skill tools = %d, want 1", len(second.Tools))
	}
	info, err := second.Tools[0].Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Desc != "Load one enabled Skill by exact name." || strings.Contains(info.Desc, "powershell-ops") {
		t.Fatalf("Skill Tool description contains dynamic Skill metadata: %q", info.Desc)
	}
}
