package agent

import (
	"context"
	"testing"

	"eino-ops-agent/internal/config"
	"eino-ops-agent/internal/security"
	"eino-ops-agent/internal/service"
	"eino-ops-agent/internal/store"

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
