package agent

import (
	"context"
	"fmt"
	"strings"

	"eino-ops-agent/internal/service"

	"github.com/cloudwego/eino/adk"
	skillmw "github.com/cloudwego/eino/adk/middlewares/skill"
	"github.com/cloudwego/eino/components/tool"
)

type managedSkillBackend struct {
	service *service.Service
}

var _ skillmw.Backend = (*managedSkillBackend)(nil)

func (b *managedSkillBackend) List(_ context.Context) ([]skillmw.FrontMatter, error) {
	items, err := b.service.ListEnabledSkills()
	if err != nil {
		return nil, err
	}
	result := make([]skillmw.FrontMatter, 0, len(items))
	for _, item := range items {
		result = append(result, skillmw.FrontMatter{
			Name:        item.Name,
			Description: item.Summary,
		})
	}
	return result, nil
}

func (b *managedSkillBackend) Get(ctx context.Context, name string) (skillmw.Skill, error) {
	item, err := b.service.LoadSkill(ctx, name, "eino-agent")
	if err != nil {
		return skillmw.Skill{}, err
	}
	return skillmw.Skill{
		FrontMatter: skillmw.FrontMatter{
			Name:        item.Name,
			Description: item.Summary,
			// Context, Agent and Model intentionally remain empty. Managed Skills
			// execute inline until fork execution is explicitly enabled.
		},
		Content:       item.RuntimeContent,
		BaseDirectory: item.BaseDirectory,
	}, nil
}

func managedSkillToolDescription(_ context.Context, _ []skillmw.FrontMatter) string {
	return "Load one enabled Skill by exact name."
}

func managedSkillSystemPrompt(_ context.Context, toolName string) string {
	return fmt.Sprintf(`# Skills System
Skills are administrator-managed instructions for specialized tasks. Match the request against the available Skill index below. When a Skill applies, call the %q tool with its exact name before acting, then follow the loaded instructions. Load only Skills relevant to the request.`, toolName)
}

func managedSkillIndexPrompt(items []skillmw.FrontMatter) string {
	if len(items) == 0 {
		return "## Available Skills\nNo enabled Skills are available."
	}
	summaries := make([]string, 0, len(items))
	for _, item := range items {
		summary := strings.Join(strings.Fields(item.Description), " ")
		if summary == "" {
			summaries = append(summaries, "- "+item.Name)
			continue
		}
		if runes := []rune(summary); len(runes) > 120 {
			summary = string(runes[:120]) + "…"
		}
		summaries = append(summaries, "- "+item.Name+": "+summary)
	}
	return "## Available Skills\n" + strings.Join(summaries, "\n")
}

type configuredSkillMiddleware struct {
	adk.ChatModelAgentMiddleware
	backend skillmw.Backend
	enabled bool
}

func (m *configuredSkillMiddleware) BeforeAgent(ctx context.Context, runCtx *adk.ChatModelAgentContext) (context.Context, *adk.ChatModelAgentContext, error) {
	if !m.enabled {
		return ctx, runCtx, nil
	}
	items, err := m.backend.List(ctx)
	if err != nil {
		return ctx, runCtx, fmt.Errorf("list enabled Skills: %w", err)
	}
	ctx, runCtx, err = m.ChatModelAgentMiddleware.BeforeAgent(ctx, runCtx)
	if err != nil {
		return ctx, runCtx, err
	}
	runCtx.Instruction = strings.TrimRight(runCtx.Instruction, "\n") + "\n\n" + managedSkillIndexPrompt(items)
	return ctx, runCtx, nil
}

func newSkillMiddleware(ctx context.Context, svc *service.Service, states map[string]bool) (adk.ChatModelAgentMiddleware, []tool.BaseTool, error) {
	backend := &managedSkillBackend{service: svc}
	framework, err := skillmw.NewMiddleware(ctx, &skillmw.Config{
		Backend:               backend,
		CustomSystemPrompt:    managedSkillSystemPrompt,
		CustomToolDescription: managedSkillToolDescription,
	})
	if err != nil {
		return nil, nil, err
	}
	_, runCtx, err := framework.BeforeAgent(ctx, &adk.ChatModelAgentContext{})
	if err != nil {
		return nil, nil, fmt.Errorf("inspect Eino skill middleware: %w", err)
	}
	enabled := true
	if state, configured := states["skill"]; configured {
		enabled = state
	}
	configured := &configuredSkillMiddleware{ChatModelAgentMiddleware: framework, backend: backend, enabled: enabled}
	return configured, runCtx.Tools, nil
}
