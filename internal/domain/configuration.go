package domain

import "time"

const (
	ConfigurationSchema        = "opsnerva.configuration"
	ConfigurationSchemaVersion = 1
)

type ConfigurationPackage struct {
	Schema             string                       `json:"schema"`
	SchemaVersion      int                          `json:"schema_version"`
	ApplicationVersion string                       `json:"application_version,omitempty"`
	ExportedAt         time.Time                    `json:"exported_at"`
	SecretsIncluded    bool                         `json:"secrets_included"`
	Proxies            []ConfigurationProxy         `json:"proxies"`
	Hosts              []ConfigurationHost          `json:"hosts"`
	ModelProviders     []ConfigurationModelProvider `json:"model_providers"`
}

type ConfigurationProxy struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	URL                string `json:"url"`
	Username           string `json:"username,omitempty"`
	PasswordConfigured bool   `json:"password_configured"`
	Password           string `json:"password,omitempty"`
}

type ConfigurationHost struct {
	ID                     string `json:"id"`
	Name                   string `json:"name"`
	Address                string `json:"address"`
	Port                   int    `json:"port"`
	User                   string `json:"user"`
	AgentEnabled           bool   `json:"agent_enabled"`
	AuthType               string `json:"auth_type"`
	PrivateKeyConfigured   bool   `json:"private_key_configured"`
	PrivateKey             string `json:"private_key,omitempty"`
	KnownHostsFile         string `json:"known_hosts_file,omitempty"`
	ProxyJumpHostID        string `json:"proxy_jump_host_id,omitempty"`
	ProxyID                string `json:"proxy_id,omitempty"`
	PasswordConfigured     bool   `json:"password_configured"`
	Password               string `json:"password,omitempty"`
	SudoMode               string `json:"sudo_mode"`
	SudoPasswordConfigured bool   `json:"sudo_password_configured"`
	SudoPassword           string `json:"sudo_password,omitempty"`
}

type ConfigurationModelProvider struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Kind             string `json:"kind"`
	BaseURL          string `json:"base_url,omitempty"`
	Model            string `json:"model"`
	ContextWindow    int    `json:"context_window"`
	ReasoningEffort  string `json:"reasoning_effort,omitempty"`
	APIKeyConfigured bool   `json:"api_key_configured"`
	APIKey           string `json:"api_key,omitempty"`
	ProxyID          string `json:"proxy_id,omitempty"`
	UserAgent        string `json:"user_agent,omitempty"`
	Active           bool   `json:"active"`
}

type ConfigurationImportResult struct {
	Proxies         int  `json:"proxies"`
	Hosts           int  `json:"hosts"`
	ModelProviders  int  `json:"model_providers"`
	SecretsImported bool `json:"secrets_imported"`
	RuntimeReloaded bool `json:"runtime_reloaded"`
}
