package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestEnsureDefaultFileCreatesLoadableConfigWithoutOverwriting(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, DefaultFileName)
	created, err := EnsureDefaultFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("default configuration was not created")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "listen_address: 127.0.0.1:8080") || strings.Contains(string(data), "password:") {
		t.Fatalf("unexpected generated configuration:\n%s", data)
	}
	var loaded Config
	if err := yaml.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("parse generated configuration: %v", err)
	}
	if loaded.ListenAddress != Default().ListenAddress || loaded.DatabasePath != Default().DatabasePath || loaded.Logging.Level != "debug" {
		t.Fatalf("generated defaults were not preserved: %#v", loaded)
	}

	const replacement = "listen_address: 127.0.0.1:9090\n"
	if err := os.WriteFile(path, []byte(replacement), 0o600); err != nil {
		t.Fatal(err)
	}
	created, err = EnsureDefaultFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("existing configuration was reported as created")
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != replacement {
		t.Fatalf("existing configuration was overwritten: %q", data)
	}
}

func TestLoadResolvesRuntimePathsRelativeToConfigurationDirectory(t *testing.T) {
	configRoot := t.TempDir()
	startupRoot := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(startupRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })

	cfg, err := Load(filepath.Join(configRoot, "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != filepath.Join(configRoot, "data") {
		t.Fatalf("data directory = %q", cfg.DataDir)
	}
	if cfg.DatabasePath != filepath.Join(configRoot, "data", "opsnerva.db") {
		t.Fatalf("database path = %q", cfg.DatabasePath)
	}
	if cfg.Logging.File != filepath.Join(configRoot, "data", "opsnerva.log") {
		t.Fatalf("log path = %q", cfg.Logging.File)
	}
	if cfg.SSH.DefaultKnownHosts != filepath.Join(configRoot, "data", "known_hosts") {
		t.Fatalf("known hosts path = %q", cfg.SSH.DefaultKnownHosts)
	}
	if cfg.WorkspaceDir != filepath.Join(configRoot, "workspace") {
		t.Fatalf("workspace directory = %q", cfg.WorkspaceDir)
	}
	if cfg.WorkspaceSandboxPath != "bwrap" {
		t.Fatalf("default workspace sandbox = %q", cfg.WorkspaceSandboxPath)
	}
}

func TestLoadWithoutConfigurationUsesStartupDirectory(t *testing.T) {
	root := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != filepath.Join(root, "data") || cfg.WorkspaceDir != filepath.Join(root, "workspace") {
		t.Fatalf("runtime roots = data %q workspace %q", cfg.DataDir, cfg.WorkspaceDir)
	}
}

func TestLoadRejectsWorkspaceDirectoryOverlappingDataDirectory(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte("data_dir: runtime\nworkspace_dir: runtime/workspaces\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	if _, err := Load(path); err == nil {
		t.Fatal("overlapping workspace directory was accepted")
	}
}

func TestLoadNormalizesAndValidatesReasoningEffort(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte("model:\n  reasoning_effort: XHIGH\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model.ReasoningEffort != "xhigh" {
		t.Fatalf("reasoning effort = %q, want xhigh", cfg.Model.ReasoningEffort)
	}
	if err := os.WriteFile(path, []byte("model:\n  reasoning_effort: maximum\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("invalid reasoning effort was accepted")
	}
}

func TestModelContextWindowAllowsAutoOrManualValue(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte("model:\n  context_window: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil || cfg.Model.ContextWindow != 0 {
		t.Fatalf("automatic context window = %d, %v", cfg.Model.ContextWindow, err)
	}
	if err := os.WriteFile(path, []byte("model:\n  context_window: 200000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(path)
	if err != nil || cfg.Model.ContextWindow != 200000 {
		t.Fatalf("manual context window = %d, %v", cfg.Model.ContextWindow, err)
	}
	if err := os.WriteFile(path, []byte("model:\n  context_window: 100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("invalid context window was accepted")
	}
}

func TestOptionalAuthenticationLoadsFromYAMLAndEnvironment(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte("auth:\n  username: operator\n  password: yaml-password\n  session_ttl_hours: 12\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Auth.Enabled() || cfg.Auth.Username != "operator" || cfg.Auth.Password != "yaml-password" || cfg.Auth.SessionTTLHours != 12 {
		t.Fatalf("YAML authentication = %#v", cfg.Auth)
	}
	t.Setenv("OPSNERVA_AUTH_USERNAME", "environment-operator")
	t.Setenv("OPSNERVA_AUTH_PASSWORD", "environment-password")
	t.Setenv("OPSNERVA_AUTH_SESSION_TTL_HOURS", "36")
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.Username != "environment-operator" || cfg.Auth.Password != "environment-password" || cfg.Auth.SessionTTLHours != 36 {
		t.Fatalf("environment authentication = %#v", cfg.Auth)
	}
	t.Setenv("OPSNERVA_AUTH_PASSWORD", "short")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "at least 8") {
		t.Fatalf("short authentication password was accepted: %v", err)
	}
}
