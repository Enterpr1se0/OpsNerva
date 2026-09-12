package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/store"
)

func (s *Service) SystemSettings(ctx context.Context) (domain.SystemSettings, error) {
	settings, err := s.store.GetSystemSettings(ctx)
	if err != nil {
		return domain.SystemSettings{}, err
	}
	return s.decorateWorkspaceShellSettings(settings), nil
}

func (s *Service) SaveSystemSettings(ctx context.Context, input domain.SystemSettingsInput, actor string) (domain.SystemSettings, error) {
	if input.AgentMaxIterations < domain.MinAgentMaxIterations || input.AgentMaxIterations > domain.MaxAgentMaxIterations {
		return domain.SystemSettings{}, fmt.Errorf("agent_max_iterations must be between %d and %d", domain.MinAgentMaxIterations, domain.MaxAgentMaxIterations)
	}
	current, err := s.store.GetSystemSettings(ctx)
	if err != nil {
		return domain.SystemSettings{}, err
	}
	current.AgentMaxIterations = input.AgentMaxIterations
	systemPromptChanged := false
	if input.SystemPrompt != nil {
		systemPromptChanged = current.SystemPrompt != *input.SystemPrompt
		current.SystemPrompt = *input.SystemPrompt
	}
	if input.ApprovalMode != nil {
		mode := strings.ToLower(strings.TrimSpace(*input.ApprovalMode))
		switch mode {
		case domain.ApprovalModeManual, domain.ApprovalModeAuto, domain.ApprovalModeFullAccess:
			current.ApprovalMode = mode
		default:
			return domain.SystemSettings{}, fmt.Errorf("approval_mode must be manual, auto, or full_access")
		}
	}
	if input.ApprovalExplanationsEnabled != nil {
		current.ApprovalExplanationsEnabled = *input.ApprovalExplanationsEnabled
	}
	if input.SubagentModelProviderID != nil {
		providerID := strings.TrimSpace(*input.SubagentModelProviderID)
		if providerID != "" {
			if _, err := s.store.GetModelProvider(ctx, providerID); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return domain.SystemSettings{}, fmt.Errorf("subagent model provider %q not found", providerID)
				}
				return domain.SystemSettings{}, err
			}
		}
		current.SubagentModelProviderID = providerID
	}
	if input.AutomaticApprovalModelProviderID != nil {
		providerID := strings.TrimSpace(*input.AutomaticApprovalModelProviderID)
		if providerID != "" {
			if _, err := s.store.GetModelProvider(ctx, providerID); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return domain.SystemSettings{}, fmt.Errorf("Auto approval model provider %q not found", providerID)
				}
				return domain.SystemSettings{}, err
			}
		}
		current.AutomaticApprovalModelProviderID = providerID
	}
	if input.SubagentTimeoutSeconds != nil {
		if *input.SubagentTimeoutSeconds < domain.MinSubagentTimeoutSeconds || *input.SubagentTimeoutSeconds > domain.MaxSubagentTimeoutSeconds {
			return domain.SystemSettings{}, fmt.Errorf("subagent_timeout_seconds must be between %d and %d", domain.MinSubagentTimeoutSeconds, domain.MaxSubagentTimeoutSeconds)
		}
		current.SubagentTimeoutSeconds = *input.SubagentTimeoutSeconds
	}
	if input.ContextCompressionEnabled != nil {
		current.ContextCompressionEnabled = *input.ContextCompressionEnabled
	}
	if input.ContextCompressionPercent != nil {
		if *input.ContextCompressionPercent < domain.MinContextCompressionPercent || *input.ContextCompressionPercent > domain.MaxContextCompressionPercent {
			return domain.SystemSettings{}, fmt.Errorf("context_compression_threshold_percent must be between %d and %d", domain.MinContextCompressionPercent, domain.MaxContextCompressionPercent)
		}
		current.ContextCompressionPercent = *input.ContextCompressionPercent
	}
	if input.ChatImageAllowedTypes != nil {
		allowed := map[string]struct{}{
			"image/png": {}, "image/jpeg": {}, "image/webp": {}, "image/gif": {},
		}
		seen := make(map[string]struct{}, len(input.ChatImageAllowedTypes))
		normalized := make([]string, 0, len(input.ChatImageAllowedTypes))
		for _, value := range input.ChatImageAllowedTypes {
			value = strings.ToLower(strings.TrimSpace(value))
			if _, ok := allowed[value]; !ok {
				return domain.SystemSettings{}, fmt.Errorf("unsupported chat image type %q", value)
			}
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			normalized = append(normalized, value)
		}
		if len(normalized) == 0 {
			return domain.SystemSettings{}, fmt.Errorf("at least one chat image type is required")
		}
		current.ChatImageAllowedTypes = normalized
	}
	if input.WorkspaceShellMode != nil {
		mode := strings.ToLower(strings.TrimSpace(*input.WorkspaceShellMode))
		switch mode {
		case domain.WorkspaceShellModeSandbox, domain.WorkspaceShellModeHost, domain.WorkspaceShellModeDisabled:
			if mode != current.WorkspaceShellMode && s.shells.hasActive(func(shell domain.SSHShell) bool {
				return shell.Kind == domain.SSHShellKindWorkspace
			}) {
				return domain.SystemSettings{}, fmt.Errorf("close active Workspace terminals before changing workspace_shell_mode")
			}
			current.WorkspaceShellMode = mode
		default:
			return domain.SystemSettings{}, fmt.Errorf("workspace_shell_mode must be sandbox, host, or disabled")
		}
	}
	rotatedMCPHTTPToken := false
	var mcpHTTPToken string
	if input.MCPHTTPEnabled != nil {
		wasEnabled := current.MCPHTTPEnabled
		current.MCPHTTPEnabled = *input.MCPHTTPEnabled
		if current.MCPHTTPEnabled && (!wasEnabled || current.MCPHTTPTokenHash == "") {
			input.RotateMCPHTTPToken = true
		}
	}
	if input.RotateMCPHTTPToken {
		if !current.MCPHTTPEnabled {
			return domain.SystemSettings{}, fmt.Errorf("MCP HTTP server must be enabled before rotating its token")
		}
		mcpHTTPToken, err = generateMCPHTTPToken()
		if err != nil {
			return domain.SystemSettings{}, err
		}
		current.MCPHTTPTokenHash = hashMCPHTTPToken(mcpHTTPToken)
		rotatedMCPHTTPToken = true
	}
	saved, err := s.store.SaveSystemSettings(ctx, current)
	if err != nil {
		return domain.SystemSettings{}, err
	}
	s.audit(ctx, "", "system_settings_updated", actor, map[string]any{
		"agent_max_iterations": saved.AgentMaxIterations, "approval_mode": saved.ApprovalMode,
		"approval_explanations_enabled": saved.ApprovalExplanationsEnabled,
		"system_prompt_changed":         systemPromptChanged, "system_prompt_bytes": len(saved.SystemPrompt),
		"subagent_model_provider_id": saved.SubagentModelProviderID, "subagent_timeout_seconds": saved.SubagentTimeoutSeconds,
		"automatic_approval_model_provider_id":  saved.AutomaticApprovalModelProviderID,
		"context_compression_enabled":           saved.ContextCompressionEnabled,
		"context_compression_threshold_percent": saved.ContextCompressionPercent,
		"chat_image_allowed_types":              saved.ChatImageAllowedTypes,
		"workspace_shell_mode":                  saved.WorkspaceShellMode,
		"mcp_http_enabled":                      saved.MCPHTTPEnabled,
		"mcp_http_token_rotated":                rotatedMCPHTTPToken,
	})
	saved.MCPHTTPToken = mcpHTTPToken
	return s.decorateWorkspaceShellSettings(saved), nil
}

func generateMCPHTTPToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate MCP HTTP token: %w", err)
	}
	return "opsnerva_mcp_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

func hashMCPHTTPToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Service) MCPHTTPAccess(ctx context.Context, token string) (enabled bool, authorized bool, err error) {
	settings, err := s.store.GetSystemSettings(ctx)
	if err != nil {
		return false, false, err
	}
	if !settings.MCPHTTPEnabled {
		return false, false, nil
	}
	token = strings.TrimSpace(token)
	if token == "" || settings.MCPHTTPTokenHash == "" {
		return true, false, nil
	}
	expected, decodeErr := hex.DecodeString(settings.MCPHTTPTokenHash)
	if decodeErr != nil || len(expected) != sha256.Size {
		return true, false, nil
	}
	actual := sha256.Sum256([]byte(token))
	return true, subtle.ConstantTimeCompare(expected, actual[:]) == 1, nil
}
