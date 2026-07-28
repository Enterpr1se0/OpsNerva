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
	Logging              Logging       `yaml:"logging"`
	MasterKey            string        `yaml:"-"`
	PolicyPath           string        `yaml:"policy_path"`
	SSH                  SSH           `yaml:"ssh"`
	Model                Model         `yaml:"model"`
	Limits               Limits        `yaml:"limits"`
	AuditRetention       time.Duration `yaml:"-"`
	WebAuth              WebAuth       `yaml:"web_auth"`
	WorkspaceDir         string        `yaml:"workspace_dir"`
	WorkspaceSandboxPath string        `yaml:"workspace_sandbox_path"`
	Validators           []Validator   `yaml:"validators"`
}

type WebAuth struct {
	SecureCookies bool          `yaml:"secure_cookies"`
	SessionTTL    time.Duration `yaml:"-"`
}

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
	APIKey        string `yaml:"-"`
	Kind          string `yaml:"kind"`
	BaseURL       string `yaml:"base_url"`
	Name          string `yaml:"name"`
	UserAgent     string `yaml:"user_agent"`
	ProxyURL      string `yaml:"-"`
	ProxyUsername string `yaml:"-"`
	ProxyPassword string `yaml:"-"`
}

type Limits struct {
	SyncTimeoutSeconds int `yaml:"sync_timeout_seconds"`
	MaxTimeoutSeconds  int `yaml:"max_timeout_seconds"`
	GlobalConcurrency  int `yaml:"global_concurrency"`
	HostConcurrency    int `yaml:"host_concurrency"`
}

func Default() Config {
	return Config{
		ListenAddress: "0.0.0.0:8080",
		DataDir:       ".data",
		DatabasePath:  ".data/ops-agent.db",
		PolicyPath:    "configs/policy.yaml",
		Logging: Logging{
			Level: "debug", Format: "text", File: ".data/ops-agent.log",
			MaxSizeMB: 20, MaxBackups: 3, RecentLimit: 2000,
		},
		SSH: SSH{
			DefaultKnownHosts: ".data/known_hosts",
		},
		Model: Model{Name: "gpt-4o-mini"},
		Limits: Limits{
			SyncTimeoutSeconds: 60,
			MaxTimeoutSeconds:  600,
			GlobalConcurrency:  8,
			HostConcurrency:    2,
		},
		AuditRetention:       30 * 24 * time.Hour,
		WebAuth:              WebAuth{SessionTTL: 12 * time.Hour},
		WorkspaceDir:         "workspace",
		WorkspaceSandboxPath: "bwrap",
	}
}

func Load(path string) (Config, error) {
	cfg := Default()
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
	if cfg.DatabasePath == "" || (cfg.DataDir != defaultDataDir && cfg.DatabasePath == defaultDatabasePath && os.Getenv("OPS_AGENT_DATABASE") == "") {
		cfg.DatabasePath = filepath.Join(cfg.DataDir, "ops-agent.db")
	}
	if cfg.SSH.DefaultKnownHosts == "" || (cfg.DataDir != defaultDataDir && cfg.SSH.DefaultKnownHosts == defaultKnownHosts) {
		cfg.SSH.DefaultKnownHosts = filepath.Join(cfg.DataDir, "known_hosts")
	}
	if cfg.Logging.File == "" || (cfg.DataDir != defaultDataDir && cfg.Logging.File == defaultLogFile && os.Getenv("OPS_AGENT_LOG_FILE") == "") {
		cfg.Logging.File = filepath.Join(cfg.DataDir, "ops-agent.log")
	}
	workspaceDir := filepath.Clean(strings.TrimSpace(cfg.WorkspaceDir))
	if workspaceDir == "." && strings.TrimSpace(cfg.WorkspaceDir) == "" {
		return Config{}, fmt.Errorf("workspace_dir is required")
	}
	if !filepath.IsAbs(workspaceDir) {
		absolute, err := filepath.Abs(workspaceDir)
		if err != nil {
			return Config{}, fmt.Errorf("resolve workspace_dir: %w", err)
		}
		workspaceDir = absolute
	}
	cfg.WorkspaceDir = workspaceDir
	if err := validateOperationsConfig(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func validateOperationsConfig(cfg *Config) error {
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

func sameOrWithin(path, root string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func applyEnv(cfg *Config) {
	setString(&cfg.ListenAddress, "OPS_AGENT_LISTEN")
	setString(&cfg.DataDir, "OPS_AGENT_DATA_DIR")
	setString(&cfg.DatabasePath, "OPS_AGENT_DATABASE")
	setString(&cfg.PolicyPath, "OPS_AGENT_POLICY")
	setString(&cfg.Logging.Level, "OPS_AGENT_LOG_LEVEL")
	setString(&cfg.Logging.Format, "OPS_AGENT_LOG_FORMAT")
	setString(&cfg.Logging.File, "OPS_AGENT_LOG_FILE")
	setBool(&cfg.Logging.AddSource, "OPS_AGENT_LOG_SOURCE")
	setInt(&cfg.Logging.MaxSizeMB, "OPS_AGENT_LOG_MAX_SIZE_MB")
	setInt(&cfg.Logging.MaxBackups, "OPS_AGENT_LOG_MAX_BACKUPS")
	setInt(&cfg.Logging.RecentLimit, "OPS_AGENT_LOG_RECENT_LIMIT")
	setString(&cfg.MasterKey, "OPS_AGENT_MASTER_KEY")
	setBool(&cfg.WebAuth.SecureCookies, "OPS_AGENT_SECURE_COOKIES")
	setString(&cfg.WorkspaceDir, "OPS_AGENT_WORKSPACE_DIR")
	setString(&cfg.WorkspaceSandboxPath, "OPS_AGENT_WORKSPACE_SANDBOX")
	setString(&cfg.Model.APIKey, "OPENAI_API_KEY")
	setString(&cfg.Model.BaseURL, "OPENAI_BASE_URL")
	setString(&cfg.Model.Name, "OPENAI_MODEL")
	setInt(&cfg.Limits.GlobalConcurrency, "OPS_AGENT_GLOBAL_CONCURRENCY")
	setInt(&cfg.Limits.HostConcurrency, "OPS_AGENT_HOST_CONCURRENCY")
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
