package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	ListenAddress        string        `yaml:"listen_address"`
	DataDir              string        `yaml:"data_dir"`
	DatabasePath         string        `yaml:"database_path"`
	Auth                 Auth          `yaml:"auth"`
	Logging              Logging       `yaml:"logging"`
	MasterKey            string        `yaml:"-"`
	SSH                  SSH           `yaml:"ssh"`
	Model                Model         `yaml:"model"`
	Limits               Limits        `yaml:"limits"`
	AuditRetention       time.Duration `yaml:"-"`
	WorkspaceDir         string        `yaml:"workspace_dir"`
	WorkspaceSandboxPath string        `yaml:"workspace_sandbox_path"`
	Validators           []Validator   `yaml:"validators"`
}

type Auth struct {
	Username        string `yaml:"username"`
	Password        string `yaml:"password"`
	SessionTTLHours int    `yaml:"session_ttl_hours"`
}

func (a Auth) Enabled() bool { return strings.TrimSpace(a.Password) != "" }

type Workspace struct {
	ID     string
	Root   string
	Access string
}

type Validator struct {
	ID             string   `yaml:"id" json:"id"`
	Scope          string   `yaml:"scope" json:"scope"`
	Program        string   `yaml:"program" json:"-"`
	Args           []string `yaml:"args" json:"-"`
	TimeoutSeconds int      `yaml:"timeout_seconds" json:"timeout_seconds"`
	PathPatterns   []string `yaml:"path_patterns" json:"path_patterns"`
}

type Logging struct {
	Level       string `yaml:"level"`
	Format      string `yaml:"format"`
	File        string `yaml:"file"`
	AddSource   bool   `yaml:"add_source"`
	MaxSizeMB   int    `yaml:"max_size_mb"`
	MaxBackups  int    `yaml:"max_backups"`
	RecentLimit int    `yaml:"recent_limit"`
}

type SSH struct {
	DefaultKnownHosts string `yaml:"default_known_hosts"`
}

type Model struct {
	APIKey          string `yaml:"-"`
	Kind            string `yaml:"kind"`
	BaseURL         string `yaml:"base_url"`
	Name            string `yaml:"name"`
	ContextWindow   int    `yaml:"context_window"`
	ReasoningEffort string `yaml:"reasoning_effort"`
	UserAgent       string `yaml:"user_agent"`
	ProxyURL        string `yaml:"-"`
	ProxyUsername   string `yaml:"-"`
	ProxyPassword   string `yaml:"-"`
}

type Limits struct {
	SyncTimeoutSeconds int `yaml:"sync_timeout_seconds"`
	MaxTimeoutSeconds  int `yaml:"max_timeout_seconds"`
	GlobalConcurrency  int `yaml:"global_concurrency"`
	HostConcurrency    int `yaml:"host_concurrency"`
}

func Default() Config {
	return Config{
		ListenAddress: "127.0.0.1:8080",
		DataDir:       "data",
		DatabasePath:  "data/opsnerva.db",
		Auth:          Auth{Username: "admin", SessionTTLHours: 24},
		Logging: Logging{
			Level: "debug", Format: "text", File: "data/opsnerva.log",
			MaxSizeMB: 20, MaxBackups: 3, RecentLimit: 2000,
		},
		SSH: SSH{
			DefaultKnownHosts: "data/known_hosts",
		},
		Model: Model{Name: "gpt-4o-mini"},
		Limits: Limits{
			SyncTimeoutSeconds: 60,
			MaxTimeoutSeconds:  600,
			GlobalConcurrency:  8,
			HostConcurrency:    2,
		},
		AuditRetention:       30 * 24 * time.Hour,
		WorkspaceDir:         "workspace",
		WorkspaceSandboxPath: "bwrap",
	}
}

func Load(path string) (Config, error) {
	cfg := Default()
	baseDir, err := configurationBaseDir(path)
	if err != nil {
		return Config{}, err
	}
	defaultDataDir := cfg.DataDir
	defaultDatabasePath := cfg.DatabasePath
	defaultKnownHosts := cfg.SSH.DefaultKnownHosts
	defaultLogFile := cfg.Logging.File
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return Config{}, err
		}
		if len(data) > 0 {
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				return Config{}, err
			}
		}
	}
	applyEnv(&cfg)
	if cfg.DatabasePath == "" || (cfg.DataDir != defaultDataDir && cfg.DatabasePath == defaultDatabasePath && os.Getenv("OPSNERVA_DATABASE") == "") {
		cfg.DatabasePath = filepath.Join(cfg.DataDir, "opsnerva.db")
	}
	if cfg.SSH.DefaultKnownHosts == "" || (cfg.DataDir != defaultDataDir && cfg.SSH.DefaultKnownHosts == defaultKnownHosts) {
		cfg.SSH.DefaultKnownHosts = filepath.Join(cfg.DataDir, "known_hosts")
	}
	if cfg.Logging.File == "" || (cfg.DataDir != defaultDataDir && cfg.Logging.File == defaultLogFile && os.Getenv("OPSNERVA_LOG_FILE") == "") {
		cfg.Logging.File = filepath.Join(cfg.DataDir, "opsnerva.log")
	}
	cfg.DataDir = resolvePath(baseDir, cfg.DataDir)
	if cfg.DatabasePath != ":memory:" && !strings.HasPrefix(cfg.DatabasePath, "file:") {
		cfg.DatabasePath = resolvePath(baseDir, cfg.DatabasePath)
	}
	if cfg.Logging.File != "-" {
		cfg.Logging.File = resolvePath(baseDir, cfg.Logging.File)
	}
	cfg.SSH.DefaultKnownHosts = resolvePath(baseDir, cfg.SSH.DefaultKnownHosts)
	workspaceDir := filepath.Clean(strings.TrimSpace(cfg.WorkspaceDir))
	if workspaceDir == "." && strings.TrimSpace(cfg.WorkspaceDir) == "" {
		return Config{}, fmt.Errorf("workspace_dir is required")
	}
	cfg.WorkspaceDir = resolvePath(baseDir, workspaceDir)
	if err := validateOperationsConfig(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func configurationBaseDir(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		current, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve startup directory: %w", err)
		}
		return filepath.Abs(current)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve configuration path: %w", err)
	}
	return filepath.Dir(absolute), nil
}

func resolvePath(baseDir, path string) string {
	path = filepath.Clean(strings.TrimSpace(path))
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(baseDir, path)
}

func validateOperationsConfig(cfg *Config) error {
	cfg.Auth.Username = strings.TrimSpace(cfg.Auth.Username)
	if cfg.Auth.SessionTTLHours == 0 {
		cfg.Auth.SessionTTLHours = 24
	}
	if cfg.Auth.Enabled() {
		if cfg.Auth.Username == "" {
			return fmt.Errorf("auth.username is required when auth.password is configured")
		}
		if len(cfg.Auth.Username) > 128 || containsControl(cfg.Auth.Username) {
			return fmt.Errorf("auth.username is invalid")
		}
		if len(cfg.Auth.Password) < 8 {
			return fmt.Errorf("auth.password must contain at least 8 characters")
		}
		if len(cfg.Auth.Password) > 1024 || containsControl(cfg.Auth.Password) {
			return fmt.Errorf("auth.password is invalid")
		}
	}
	if cfg.Auth.SessionTTLHours < 1 || cfg.Auth.SessionTTLHours > 24*30 {
		return fmt.Errorf("auth.session_ttl_hours must be between 1 and 720")
	}
	if cfg.Model.ContextWindow != 0 && (cfg.Model.ContextWindow < 1024 || cfg.Model.ContextWindow > 10000000) {
		return fmt.Errorf("model.context_window must be between 1024 and 10000000")
	}
	cfg.Model.ReasoningEffort = strings.ToLower(strings.TrimSpace(cfg.Model.ReasoningEffort))
	switch cfg.Model.ReasoningEffort {
	case "", "low", "medium", "high", "xhigh", "max":
	default:
		return fmt.Errorf("model.reasoning_effort must be low, medium, high, xhigh, max, or empty")
	}
	dataRoot, _ := filepath.Abs(cfg.DataDir)
	if sameOrWithin(cfg.WorkspaceDir, dataRoot) || sameOrWithin(dataRoot, cfg.WorkspaceDir) {
		return fmt.Errorf("workspace_dir cannot overlap the application data directory")
	}
	seenValidators := make(map[string]struct{}, len(cfg.Validators))
	for index := range cfg.Validators {
		validator := &cfg.Validators[index]
		validator.ID = strings.TrimSpace(validator.ID)
		validator.Scope = strings.TrimSpace(validator.Scope)
		validator.Program = strings.TrimSpace(validator.Program)
		if !regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`).MatchString(validator.ID) || validator.Program == "" {
			return fmt.Errorf("validator %d is invalid", index+1)
		}
		if _, exists := seenValidators[validator.ID]; exists {
			return fmt.Errorf("duplicate validator id %q", validator.ID)
		}
		seenValidators[validator.ID] = struct{}{}
		if validator.Scope != "remote" && validator.Scope != "workspace" {
			return fmt.Errorf("validator %q scope must be remote or workspace", validator.ID)
		}
		if validator.TimeoutSeconds <= 0 || validator.TimeoutSeconds > 60 {
			return fmt.Errorf("validator %q timeout_seconds must be between 1 and 60", validator.ID)
		}
	}
	return nil
}

func containsControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func sameOrWithin(path, root string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func applyEnv(cfg *Config) {
	setString(&cfg.ListenAddress, "OPSNERVA_LISTEN")
	setString(&cfg.DataDir, "OPSNERVA_DATA_DIR")
	setString(&cfg.DatabasePath, "OPSNERVA_DATABASE")
	setString(&cfg.Auth.Username, "OPSNERVA_AUTH_USERNAME")
	setString(&cfg.Auth.Password, "OPSNERVA_AUTH_PASSWORD")
	setInt(&cfg.Auth.SessionTTLHours, "OPSNERVA_AUTH_SESSION_TTL_HOURS")
	setString(&cfg.Logging.Level, "OPSNERVA_LOG_LEVEL")
	setString(&cfg.Logging.Format, "OPSNERVA_LOG_FORMAT")
	setString(&cfg.Logging.File, "OPSNERVA_LOG_FILE")
	setBool(&cfg.Logging.AddSource, "OPSNERVA_LOG_SOURCE")
	setInt(&cfg.Logging.MaxSizeMB, "OPSNERVA_LOG_MAX_SIZE_MB")
	setInt(&cfg.Logging.MaxBackups, "OPSNERVA_LOG_MAX_BACKUPS")
	setInt(&cfg.Logging.RecentLimit, "OPSNERVA_LOG_RECENT_LIMIT")
	setString(&cfg.MasterKey, "OPSNERVA_MASTER_KEY")
	setString(&cfg.WorkspaceDir, "OPSNERVA_WORKSPACE_DIR")
	setString(&cfg.WorkspaceSandboxPath, "OPSNERVA_WORKSPACE_SANDBOX")
	setString(&cfg.Model.APIKey, "OPENAI_API_KEY")
	setString(&cfg.Model.BaseURL, "OPENAI_BASE_URL")
	setString(&cfg.Model.Name, "OPENAI_MODEL")
	setInt(&cfg.Model.ContextWindow, "OPENAI_CONTEXT_WINDOW")
	setString(&cfg.Model.ReasoningEffort, "OPENAI_REASONING_EFFORT")
	setInt(&cfg.Limits.GlobalConcurrency, "OPSNERVA_GLOBAL_CONCURRENCY")
	setInt(&cfg.Limits.HostConcurrency, "OPSNERVA_HOST_CONCURRENCY")
}

func setBool(dst *bool, name string) {
	if value := os.Getenv(name); value != "" {
		if parsed, err := strconv.ParseBool(value); err == nil {
			*dst = parsed
		}
	}
}

func setString(dst *string, name string) {
	if value := os.Getenv(name); value != "" {
		*dst = value
	}
}

func setInt(dst *int, name string) {
	if value := os.Getenv(name); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			*dst = parsed
		}
	}
}
