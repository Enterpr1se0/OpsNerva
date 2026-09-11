package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"runtime"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func (s *Store) AgentToolStates(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name,enabled FROM agent_tool_settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]bool)
	for rows.Next() {
		var name string
		var enabled int
		if err := rows.Scan(&name, &enabled); err != nil {
			return nil, err
		}
		result[name] = enabled != 0
	}
	return result, rows.Err()
}

func (s *Store) SetAgentToolEnabled(ctx context.Context, name string, enabled bool) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO agent_tool_settings(name,enabled,updated_at) VALUES(?,?,?)
ON CONFLICT(name) DO UPDATE SET enabled=excluded.enabled,updated_at=excluded.updated_at`, name, boolInt(enabled), formatTime(time.Now().UTC()))
	return err
}

func (s *Store) GetSystemSettings(ctx context.Context) (domain.SystemSettings, error) {
	var settings domain.SystemSettings
	var explanationsEnabled int
	var contextCompressionEnabled int
	var mcpHTTPEnabled int
	var imageTypesJSON string
	var systemPrompt sql.NullString
	var updated string
	err := s.db.QueryRowContext(ctx, `SELECT agent_max_iterations,system_prompt,approval_mode,approval_explanations_enabled,subagent_model_provider_id,subagent_timeout_seconds,
context_compression_enabled,context_compression_threshold_percent,chat_image_allowed_types_json,workspace_shell_mode,mcp_http_enabled,mcp_http_token_hash,updated_at FROM system_settings WHERE id=1`).Scan(
		&settings.AgentMaxIterations, &systemPrompt, &settings.ApprovalMode, &explanationsEnabled, &settings.SubagentModelProviderID, &settings.SubagentTimeoutSeconds,
		&contextCompressionEnabled, &settings.ContextCompressionPercent, &imageTypesJSON, &settings.WorkspaceShellMode, &mcpHTTPEnabled, &settings.MCPHTTPTokenHash, &updated,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.SystemSettings{
			AgentMaxIterations: domain.DefaultAgentMaxIterations, ApprovalExplanationsEnabled: true,
			ApprovalMode: domain.ApprovalModeManual,
			SystemPrompt: domain.DefaultSystemPrompt, DefaultSystemPrompt: domain.DefaultSystemPrompt,
			SubagentTimeoutSeconds: domain.DefaultSubagentTimeoutSeconds, WorkspaceShellMode: domain.DefaultWorkspaceShellMode(runtime.GOOS),
			ContextCompressionEnabled: true, ContextCompressionPercent: domain.DefaultContextCompressionPercent,
			ChatImageAllowedTypes: append([]string(nil), domain.DefaultChatImageAllowedTypes...),
		}, nil
	}
	if err != nil {
		return domain.SystemSettings{}, err
	}
	settings.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	settings.DefaultSystemPrompt = domain.DefaultSystemPrompt
	if systemPrompt.Valid {
		settings.SystemPrompt = systemPrompt.String
	} else {
		settings.SystemPrompt = domain.DefaultSystemPrompt
	}
	settings.ApprovalExplanationsEnabled = explanationsEnabled != 0
	settings.ContextCompressionEnabled = contextCompressionEnabled != 0
	if settings.ContextCompressionPercent < domain.MinContextCompressionPercent || settings.ContextCompressionPercent > domain.MaxContextCompressionPercent {
		settings.ContextCompressionPercent = domain.DefaultContextCompressionPercent
	}
	settings.MCPHTTPEnabled = mcpHTTPEnabled != 0
	settings.MCPHTTPTokenConfigured = settings.MCPHTTPTokenHash != ""
	switch settings.ApprovalMode {
	case domain.ApprovalModeManual, domain.ApprovalModeAuto, domain.ApprovalModeFullAccess:
	default:
		settings.ApprovalMode = domain.ApprovalModeManual
	}
	if err := json.Unmarshal([]byte(imageTypesJSON), &settings.ChatImageAllowedTypes); err != nil || len(settings.ChatImageAllowedTypes) == 0 {
		settings.ChatImageAllowedTypes = append([]string(nil), domain.DefaultChatImageAllowedTypes...)
	}
	if settings.SubagentTimeoutSeconds < domain.MinSubagentTimeoutSeconds || settings.SubagentTimeoutSeconds > domain.MaxSubagentTimeoutSeconds {
		settings.SubagentTimeoutSeconds = domain.DefaultSubagentTimeoutSeconds
	}
	err = s.db.QueryRowContext(ctx, `SELECT model_provider_id FROM automatic_approval_settings WHERE id=1`).Scan(&settings.AutomaticApprovalModelProviderID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return domain.SystemSettings{}, err
	}
	return settings, nil
}

func (s *Store) SaveSystemSettings(ctx context.Context, settings domain.SystemSettings) (domain.SystemSettings, error) {
	settings.UpdatedAt = time.Now().UTC()
	imageTypesJSON, err := json.Marshal(settings.ChatImageAllowedTypes)
	if err != nil {
		return domain.SystemSettings{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.SystemSettings{}, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO system_settings(id,agent_max_iterations,system_prompt,approval_mode,approval_explanations_enabled,subagent_model_provider_id,subagent_timeout_seconds,context_compression_enabled,context_compression_threshold_percent,chat_image_allowed_types_json,workspace_shell_mode,mcp_http_enabled,mcp_http_token_hash,updated_at) VALUES(1,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET agent_max_iterations=excluded.agent_max_iterations,
system_prompt=excluded.system_prompt,
approval_mode=excluded.approval_mode,
approval_explanations_enabled=excluded.approval_explanations_enabled,
subagent_model_provider_id=excluded.subagent_model_provider_id,
subagent_timeout_seconds=excluded.subagent_timeout_seconds,
context_compression_enabled=excluded.context_compression_enabled,
context_compression_threshold_percent=excluded.context_compression_threshold_percent,
chat_image_allowed_types_json=excluded.chat_image_allowed_types_json,
workspace_shell_mode=excluded.workspace_shell_mode,
mcp_http_enabled=excluded.mcp_http_enabled,
mcp_http_token_hash=excluded.mcp_http_token_hash,
updated_at=excluded.updated_at`,
		settings.AgentMaxIterations, settings.SystemPrompt, settings.ApprovalMode, boolInt(settings.ApprovalExplanationsEnabled), settings.SubagentModelProviderID,
		settings.SubagentTimeoutSeconds, boolInt(settings.ContextCompressionEnabled), settings.ContextCompressionPercent, string(imageTypesJSON), settings.WorkspaceShellMode, boolInt(settings.MCPHTTPEnabled), settings.MCPHTTPTokenHash, formatTime(settings.UpdatedAt))
	if err != nil {
		return domain.SystemSettings{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO automatic_approval_settings(id,model_provider_id,updated_at) VALUES(1,?,?)
ON CONFLICT(id) DO UPDATE SET model_provider_id=excluded.model_provider_id,updated_at=excluded.updated_at`,
		settings.AutomaticApprovalModelProviderID, formatTime(settings.UpdatedAt))
	if err != nil {
		return domain.SystemSettings{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.SystemSettings{}, err
	}
	settings.MCPHTTPTokenConfigured = settings.MCPHTTPTokenHash != ""
	return settings, nil
}

func (s *Store) GetWebSearchSettings(ctx context.Context) (domain.WebSearchSettings, error) {
	var settings domain.WebSearchSettings
	var enabled int
	var updated string
	err := s.db.QueryRowContext(ctx, `SELECT enabled,provider,base_url,api_key_cipher,proxy_id,timeout_seconds,max_results,updated_at
FROM web_search_settings WHERE id=1`).Scan(
		&enabled, &settings.Provider, &settings.BaseURL, &settings.APIKeyCipher, &settings.ProxyID,
		&settings.TimeoutSeconds, &settings.MaxResults, &updated,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.WebSearchSettings{
			Provider: "tavily", BaseURL: domain.DefaultWebSearchBaseURL,
			TimeoutSeconds: domain.DefaultWebSearchTimeoutSeconds, MaxResults: domain.DefaultWebSearchMaxResults,
		}, nil
	}
	if err != nil {
		return domain.WebSearchSettings{}, err
	}
	settings.Enabled = enabled != 0
	settings.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return settings, nil
}

func (s *Store) SaveWebSearchSettings(ctx context.Context, settings domain.WebSearchSettings) (domain.WebSearchSettings, error) {
	settings.UpdatedAt = time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `INSERT INTO web_search_settings(id,enabled,provider,base_url,api_key_cipher,proxy_id,timeout_seconds,max_results,updated_at)
VALUES(1,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET enabled=excluded.enabled,provider=excluded.provider,base_url=excluded.base_url,
api_key_cipher=excluded.api_key_cipher,proxy_id=excluded.proxy_id,
timeout_seconds=excluded.timeout_seconds,max_results=excluded.max_results,
updated_at=excluded.updated_at`,
		boolInt(settings.Enabled), settings.Provider, settings.BaseURL, settings.APIKeyCipher, settings.ProxyID,
		settings.TimeoutSeconds, settings.MaxResults, formatTime(settings.UpdatedAt))
	if err != nil {
		return domain.WebSearchSettings{}, err
	}
	return settings, nil
}
