package store

import (
	"context"
	"database/sql"
	"runtime"
	"testing"

	"eino-ops-agent/internal/domain"
)

func TestLegacyReviewSettingsMigrateToOneExplanationToggle(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/legacy-settings.db"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE system_settings (
  id INTEGER PRIMARY KEY CHECK(id=1),
  agent_max_iterations INTEGER NOT NULL,
  subagent_reviews_enabled INTEGER NOT NULL DEFAULT 1,
  beginner_explanations_enabled INTEGER NOT NULL DEFAULT 1,
  updated_at TEXT NOT NULL
)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO system_settings(id,agent_max_iterations,subagent_reviews_enabled,beginner_explanations_enabled,updated_at)
VALUES(1,20,1,0,'2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE session_approval_grants (
session_id TEXT NOT NULL, request_fingerprint TEXT NOT NULL, created_at TEXT NOT NULL, expires_at TEXT NOT NULL,
PRIMARY KEY(session_id,request_fingerprint));
INSERT INTO session_approval_grants VALUES('session','fingerprint','2026-01-01T00:00:00Z','2027-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := st.GetSystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settings.ApprovalExplanationsEnabled {
		t.Fatalf("legacy disabled explanation was not preserved: %#v", settings)
	}
	if settings.WorkspaceShellMode != domain.DefaultWorkspaceShellMode(runtime.GOOS) {
		t.Fatalf("legacy settings did not receive the platform Workspace Shell mode: %#v", settings)
	}
	if settings.SubagentTimeoutSeconds != domain.DefaultSubagentTimeoutSeconds || settings.SubagentModelProviderID != "" {
		t.Fatalf("legacy settings did not receive subagent defaults: %#v", settings)
	}
	if len(settings.ChatImageAllowedTypes) != len(domain.DefaultChatImageAllowedTypes) {
		t.Fatalf("legacy settings did not receive chat image formats: %#v", settings)
	}
	if settings.SystemPrompt != domain.DefaultSystemPrompt || settings.DefaultSystemPrompt != domain.DefaultSystemPrompt {
		t.Fatalf("legacy settings did not receive the default system prompt: %#v", settings)
	}
	if settings.ApprovalMode != domain.ApprovalModeManual {
		t.Fatalf("legacy settings did not default to manual approval: %#v", settings)
	}
	var legacyGrantTables int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='session_approval_grants'`).Scan(&legacyGrantTables); err != nil {
		t.Fatal(err)
	}
	if legacyGrantTables != 0 {
		t.Fatal("legacy session approval grants survived migration")
	}
	settings.ApprovalExplanationsEnabled = true
	settings.SubagentModelProviderID = "model_fixture"
	settings.SubagentTimeoutSeconds = 45
	settings.ApprovalMode = domain.ApprovalModeAuto
	settings.WorkspaceShellMode = domain.WorkspaceShellModeDisabled
	settings.MCPHTTPEnabled = true
	settings.MCPHTTPTokenHash = "fixture-token-hash"
	if _, err := st.SaveSystemSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	settings, err = reopened.GetSystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !settings.ApprovalExplanationsEnabled || settings.AgentMaxIterations != 20 || settings.ApprovalMode != domain.ApprovalModeAuto || settings.SubagentModelProviderID != "model_fixture" || settings.SubagentTimeoutSeconds != 45 || settings.WorkspaceShellMode != domain.WorkspaceShellModeDisabled || !settings.MCPHTTPEnabled || !settings.MCPHTTPTokenConfigured || settings.MCPHTTPTokenHash != "fixture-token-hash" {
		// Existing installations retain their explicitly stored iteration value.
		t.Fatalf("migrated explanation setting did not persist: %#v", settings)
	}
}

func TestWorkspaceShellPlatformDefaultUsesHostOnWindows(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/settings.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.db.ExecContext(ctx, `UPDATE system_settings SET workspace_shell_mode='sandbox' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := st.applyWorkspaceShellPlatformDefault(ctx, "windows"); err != nil {
		t.Fatal(err)
	}
	settings, err := st.GetSystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settings.WorkspaceShellMode != domain.WorkspaceShellModeHost {
		t.Fatalf("Windows default did not select Host Shell: %#v", settings)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE system_settings SET workspace_shell_mode='disabled' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := st.applyWorkspaceShellPlatformDefault(ctx, "windows"); err != nil {
		t.Fatal(err)
	}
	settings, err = st.GetSystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settings.WorkspaceShellMode != domain.WorkspaceShellModeDisabled {
		t.Fatalf("Windows migration overwrote an explicit disabled mode: %#v", settings)
	}
}

func TestSystemSettingsPersistExplicitEmptySystemPrompt(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/settings.db"
	st, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := st.GetSystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settings.SystemPrompt != domain.DefaultSystemPrompt || settings.DefaultSystemPrompt != domain.DefaultSystemPrompt {
		t.Fatalf("unexpected initial prompt settings: %#v", settings)
	}
	settings.SystemPrompt = ""
	if _, err := st.SaveSystemSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	settings, err = reopened.GetSystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settings.SystemPrompt != "" {
		t.Fatalf("explicit empty system prompt was replaced: %q", settings.SystemPrompt)
	}
	if settings.DefaultSystemPrompt != domain.DefaultSystemPrompt {
		t.Fatalf("default system prompt was not returned separately: %q", settings.DefaultSystemPrompt)
	}
}
