package service

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestSystemSettingsValidatePersistAndReturnDefault(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	settings, err := svc.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settings.AgentMaxIterations != domain.DefaultAgentMaxIterations || settings.ApprovalMode != domain.ApprovalModeManual || !settings.ApprovalExplanationsEnabled || settings.SubagentModelProviderID != "" || settings.AutomaticApprovalModelProviderID != "" || settings.SubagentTimeoutSeconds != domain.DefaultSubagentTimeoutSeconds || !settings.ContextCompressionEnabled || settings.ContextCompressionPercent != domain.DefaultContextCompressionPercent || settings.WorkspaceShellMode != domain.DefaultWorkspaceShellMode(runtime.GOOS) {
		t.Fatalf("unexpected default max iterations: %#v", settings)
	}
	if strings.Join(settings.ChatImageAllowedTypes, ",") != strings.Join(domain.DefaultChatImageAllowedTypes, ",") {
		t.Fatalf("unexpected default chat image formats: %#v", settings.ChatImageAllowedTypes)
	}
	if settings.SystemPrompt != domain.DefaultSystemPrompt || settings.DefaultSystemPrompt != domain.DefaultSystemPrompt {
		t.Fatalf("unexpected default system prompt: %#v", settings)
	}
	if _, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{AgentMaxIterations: 4}, "test"); err == nil {
		t.Fatal("expected lower-bound validation error")
	}
	if _, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{AgentMaxIterations: domain.MaxAgentMaxIterations + 1}, "test"); err == nil {
		t.Fatal("expected upper-bound validation error")
	}
	tooShort := domain.MinSubagentTimeoutSeconds - 1
	if _, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{AgentMaxIterations: 20, SubagentTimeoutSeconds: &tooShort}, "test"); err == nil {
		t.Fatal("expected subagent timeout validation error")
	}
	missingProvider := "model_missing"
	if _, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{AgentMaxIterations: 20, SubagentModelProviderID: &missingProvider}, "test"); err == nil {
		t.Fatal("expected missing subagent provider validation error")
	}
	if _, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{AgentMaxIterations: 20, AutomaticApprovalModelProviderID: &missingProvider}, "test"); err == nil {
		t.Fatal("expected missing Auto approval provider validation error")
	}
	invalidCompressionPercent := domain.MinContextCompressionPercent - 1
	if _, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{AgentMaxIterations: 20, ContextCompressionPercent: &invalidCompressionPercent}, "test"); err == nil {
		t.Fatal("expected context compression threshold validation error")
	}
	provider, err := svc.SaveModelProvider(ctx, domain.ModelProviderInput{
		Name: "subagent", Kind: "ollama", BaseURL: "http://127.0.0.1:11434/v1", Model: "small-model",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	automaticProvider, err := svc.SaveModelProvider(ctx, domain.ModelProviderInput{
		Name: "auto-approval", Kind: "ollama", BaseURL: "http://127.0.0.1:11434/v1", Model: "approval-model",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	explanationsEnabled := false
	timeoutSeconds := 45
	hostShell := domain.WorkspaceShellModeHost
	imageTypes := []string{"image/png", "image/webp"}
	systemPrompt := "You are my personal operations agent."
	approvalMode := domain.ApprovalModeAuto
	compressionEnabled := false
	compressionPercent := 80
	saved, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{
		AgentMaxIterations: 30, ApprovalExplanationsEnabled: &explanationsEnabled,
		ApprovalMode: &approvalMode,
		SystemPrompt: &systemPrompt, SubagentModelProviderID: &provider.ID, AutomaticApprovalModelProviderID: &automaticProvider.ID, SubagentTimeoutSeconds: &timeoutSeconds,
		ChatImageAllowedTypes: imageTypes, WorkspaceShellMode: &hostShell,
		ContextCompressionEnabled: &compressionEnabled, ContextCompressionPercent: &compressionPercent,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if saved.AgentMaxIterations != 30 || saved.SystemPrompt != systemPrompt || saved.ApprovalMode != domain.ApprovalModeAuto || saved.ApprovalExplanationsEnabled || saved.SubagentModelProviderID != provider.ID || saved.AutomaticApprovalModelProviderID != automaticProvider.ID || saved.SubagentTimeoutSeconds != timeoutSeconds || saved.ContextCompressionEnabled || saved.ContextCompressionPercent != compressionPercent || strings.Join(saved.ChatImageAllowedTypes, ",") != strings.Join(imageTypes, ",") || saved.WorkspaceShellMode != domain.WorkspaceShellModeHost || saved.UpdatedAt.IsZero() {
		t.Fatalf("unexpected saved settings: %#v", saved)
	}
	reloaded, err := svc.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.AgentMaxIterations != 30 || reloaded.SystemPrompt != systemPrompt || reloaded.ApprovalMode != domain.ApprovalModeAuto || reloaded.ApprovalExplanationsEnabled || reloaded.SubagentModelProviderID != provider.ID || reloaded.AutomaticApprovalModelProviderID != automaticProvider.ID || reloaded.SubagentTimeoutSeconds != timeoutSeconds || reloaded.ContextCompressionEnabled || reloaded.ContextCompressionPercent != compressionPercent || strings.Join(reloaded.ChatImageAllowedTypes, ",") != strings.Join(imageTypes, ",") || reloaded.WorkspaceShellMode != domain.WorkspaceShellModeHost {
		t.Fatalf("system settings were not persisted: %#v", reloaded)
	}
	if _, err := svc.DeleteModelProvider(ctx, provider.ID, "test"); !errors.Is(err, ErrModelProviderInUse) || !strings.Contains(err.Error(), "selected for the approval Agent") {
		t.Fatalf("selected subagent provider deletion was not blocked: %v", err)
	}
	if _, err := svc.DeleteModelProvider(ctx, automaticProvider.ID, "test"); !errors.Is(err, ErrModelProviderInUse) || !strings.Contains(err.Error(), "selected for the Auto approval Agent") {
		t.Fatalf("selected Auto approval provider deletion was not blocked: %v", err)
	}
	invalidMode := "automatic"
	if _, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{AgentMaxIterations: 30, ApprovalMode: &invalidMode}, "test"); err == nil {
		t.Fatal("invalid approval mode was accepted")
	}
	if _, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{AgentMaxIterations: 30, WorkspaceShellMode: &invalidMode}, "test"); err == nil {
		t.Fatal("invalid workspace shell mode was accepted")
	}
	if _, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{AgentMaxIterations: 30, ChatImageAllowedTypes: []string{}}, "test"); err == nil {
		t.Fatal("empty chat image format selection was accepted")
	}
	if _, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{AgentMaxIterations: 30, ChatImageAllowedTypes: []string{"image/svg+xml"}}, "test"); err == nil {
		t.Fatal("unsupported chat image format was accepted")
	}
}

func TestMCPHTTPSettingsGenerateRotateAndAuthorizeToken(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	enabled := true
	started, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{
		AgentMaxIterations: domain.DefaultAgentMaxIterations,
		MCPHTTPEnabled:     &enabled,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !started.MCPHTTPEnabled || started.MCPHTTPToken == "" || !started.MCPHTTPTokenConfigured || started.MCPHTTPTokenHash == "" {
		t.Fatalf("MCP HTTP start did not generate a token: %#v", started)
	}
	firstToken := started.MCPHTTPToken
	reloaded, err := svc.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.MCPHTTPToken != "" || !reloaded.MCPHTTPTokenConfigured {
		t.Fatalf("stored MCP HTTP token was exposed or lost: %#v", reloaded)
	}
	accessEnabled, authorized, err := svc.MCPHTTPAccess(ctx, firstToken)
	if err != nil || !accessEnabled || !authorized {
		t.Fatalf("generated MCP HTTP token was not authorized: enabled=%v authorized=%v err=%v", accessEnabled, authorized, err)
	}
	if _, authorized, err := svc.MCPHTTPAccess(ctx, "wrong-token"); err != nil || authorized {
		t.Fatalf("invalid MCP HTTP token was accepted: authorized=%v err=%v", authorized, err)
	}
	rotated, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{
		AgentMaxIterations: domain.DefaultAgentMaxIterations,
		MCPHTTPEnabled:     &enabled,
		RotateMCPHTTPToken: true,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.MCPHTTPToken == "" || rotated.MCPHTTPToken == firstToken {
		t.Fatal("MCP HTTP token was not rotated")
	}
	if _, authorized, err := svc.MCPHTTPAccess(ctx, firstToken); err != nil || authorized {
		t.Fatalf("rotated MCP HTTP token remained valid: authorized=%v err=%v", authorized, err)
	}
	disabled := false
	if _, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{
		AgentMaxIterations: domain.DefaultAgentMaxIterations,
		MCPHTTPEnabled:     &disabled,
	}, "test"); err != nil {
		t.Fatal(err)
	}
	accessEnabled, authorized, err = svc.MCPHTTPAccess(ctx, rotated.MCPHTTPToken)
	if err != nil || accessEnabled || authorized {
		t.Fatalf("disabled MCP HTTP endpoint remained accessible: enabled=%v authorized=%v err=%v", accessEnabled, authorized, err)
	}
}
