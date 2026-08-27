package domain

import (
	"strconv"
	"strings"
	"time"
)

type Workspace struct {
	ID        string    `json:"id"`
	Access    string    `json:"access"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type WorkspaceInput struct {
	ID     string `json:"id"`
	Access string `json:"access"`
}

type Host struct {
	ID                  string    `json:"id"`
	Name                string    `json:"name"`
	Address             string    `json:"address"`
	Port                int       `json:"port"`
	User                string    `json:"user"`
	AgentEnabled        bool      `json:"agent_enabled"`
	AgentRootEnabled    bool      `json:"agent_root_enabled"`
	AuthType            string    `json:"auth_type"`
	PrivateKeyCipher    string    `json:"-"`
	HasPrivateKey       bool      `json:"has_private_key"`
	KnownHostsFile      string    `json:"known_hosts_file,omitempty"`
	ProxyJumpHostID     string    `json:"proxy_jump_host_id,omitempty"`
	ProxyID             string    `json:"proxy_id,omitempty"`
	ProxyURL            string    `json:"-"`
	ProxyUsername       string    `json:"-"`
	ProxyPasswordCipher string    `json:"-"`
	ProxyUpdatedAt      time.Time `json:"-"`
	PasswordCipher      string    `json:"-"`
	HasPassword         bool      `json:"has_password"`
	SudoMode            string    `json:"sudo_mode"`
	SudoCipher          string    `json:"-"`
	HasSudoPassword     bool      `json:"has_sudo_password"`
	Password            string    `json:"-"`
	SudoPassword        string    `json:"-"`
	PrivateKey          []byte    `json:"-"`
	ProxyPassword       string    `json:"-"`
	HostKey             *HostKey  `json:"host_key,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type HostKey struct {
	Fingerprint string `json:"fingerprint"`
	Algorithm   string `json:"algorithm,omitempty"`
	Trusted     bool   `json:"trusted"`
}

type HostInput struct {
	ID              string `json:"id,omitempty"`
	Name            string `json:"name"`
	Address         string `json:"address"`
	Port            int    `json:"port"`
	User            string `json:"user"`
	AgentEnabled    *bool  `json:"agent_enabled,omitempty"`
	AuthType        string `json:"auth_type"`
	PrivateKey      string `json:"private_key,omitempty"`
	KnownHostsFile  string `json:"known_hosts_file,omitempty"`
	ProxyJumpHostID string `json:"proxy_jump_host_id,omitempty"`
	ProxyID         string `json:"proxy_id,omitempty"`
	Password        string `json:"password,omitempty"`
	SudoMode        string `json:"sudo_mode"`
	SudoPassword    string `json:"sudo_password,omitempty"`
}

type HostCapability struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	User             string `json:"user"`
	AgentRootEnabled bool   `json:"agent_root_enabled"`
	AuthType         string `json:"auth_type"`
	SudoMode         string `json:"sudo_mode"`
}

const (
	MinModelContextWindow = 1024
	MaxModelContextWindow = 10000000
)

type ModelProvider struct {
	ID                    string    `json:"id"`
	Name                  string    `json:"name"`
	Kind                  string    `json:"kind"`
	BaseURL               string    `json:"base_url,omitempty"`
	Model                 string    `json:"model"`
	ContextWindow         int       `json:"context_window"`
	ResolvedContextWindow int       `json:"resolved_context_window,omitempty"`
	ReasoningEffort       string    `json:"reasoning_effort,omitempty"`
	APIKeyCipher          string    `json:"-"`
	HasAPIKey             bool      `json:"has_api_key"`
	ProxyID               string    `json:"proxy_id,omitempty"`
	UserAgent             string    `json:"user_agent,omitempty"`
	Active                bool      `json:"active"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

type ModelProviderInput struct {
	ID              string  `json:"id,omitempty"`
	Name            string  `json:"name"`
	Kind            string  `json:"kind"`
	BaseURL         string  `json:"base_url,omitempty"`
	Model           string  `json:"model"`
	ContextWindow   *int    `json:"context_window,omitempty"`
	ReasoningEffort *string `json:"reasoning_effort,omitempty"`
	APIKey          string  `json:"api_key,omitempty"`
	ProxyID         string  `json:"proxy_id,omitempty"`
	// UserAgent is a pointer so an omitted field keeps the stored value while
	// an explicit empty string clears it, matching the test/discovery inputs.
	UserAgent *string `json:"user_agent,omitempty"`
}

type ModelDiscoveryInput struct {
	ID        string  `json:"id,omitempty"`
	Kind      string  `json:"kind,omitempty"`
	BaseURL   *string `json:"base_url,omitempty"`
	APIKey    string  `json:"api_key,omitempty"`
	ProxyID   *string `json:"proxy_id,omitempty"`
	UserAgent *string `json:"user_agent,omitempty"`
}

type ModelTestInput struct {
	ID              string  `json:"id,omitempty"`
	Kind            string  `json:"kind,omitempty"`
	BaseURL         *string `json:"base_url,omitempty"`
	Model           string  `json:"model"`
	ReasoningEffort *string `json:"reasoning_effort,omitempty"`
	APIKey          string  `json:"api_key,omitempty"`
	ProxyID         *string `json:"proxy_id,omitempty"`
	UserAgent       *string `json:"user_agent,omitempty"`
}

type Proxy struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	URL            string    `json:"url"`
	Username       string    `json:"username,omitempty"`
	PasswordCipher string    `json:"-"`
	HasPassword    bool      `json:"has_password"`
	SSHCompatible  bool      `json:"ssh_compatible"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type ProxyInput struct {
	ID            string `json:"id,omitempty"`
	Name          string `json:"name"`
	URL           string `json:"url"`
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	ClearPassword bool   `json:"clear_password,omitempty"`
}

type ProxyTestResult struct {
	OK         bool   `json:"ok"`
	StatusCode int    `json:"status_code,omitempty"`
	LatencyMS  int64  `json:"latency_ms"`
	Target     string `json:"target"`
}

type ModelCatalog struct {
	Models         []string                 `json:"models"`
	ContextWindows map[string]int           `json:"context_windows,omitempty"`
	Metadata       map[string]ModelMetadata `json:"metadata,omitempty"`
	Count          int                      `json:"count"`
}

type ModelMetadata struct {
	ID               string   `json:"id"`
	Name             string   `json:"name,omitempty"`
	Family           string   `json:"family,omitempty"`
	ContextWindow    int      `json:"context_window,omitempty"`
	InputTokenLimit  int      `json:"input_token_limit,omitempty"`
	OutputTokenLimit int      `json:"output_token_limit,omitempty"`
	Attachment       bool     `json:"attachment"`
	Reasoning        bool     `json:"reasoning"`
	ToolCall         bool     `json:"tool_call"`
	StructuredOutput bool     `json:"structured_output"`
	Temperature      bool     `json:"temperature"`
	Knowledge        string   `json:"knowledge,omitempty"`
	ReleaseDate      string   `json:"release_date,omitempty"`
	LastUpdated      string   `json:"last_updated,omitempty"`
	Status           string   `json:"status,omitempty"`
	InputModalities  []string `json:"input_modalities,omitempty"`
	OutputModalities []string `json:"output_modalities,omitempty"`
}

const (
	DefaultAgentMaxIterations        = 50
	MinAgentMaxIterations            = 5
	MaxAgentMaxIterations            = 100
	DefaultSubagentTimeoutSeconds    = 30
	MinSubagentTimeoutSeconds        = 5
	MaxSubagentTimeoutSeconds        = 120
	DefaultContextCompressionPercent = 70
	MinContextCompressionPercent     = 50
	MaxContextCompressionPercent     = 90
)

const (
	ApprovalModeManual     = "manual"
	ApprovalModeAuto       = "auto"
	ApprovalModeFullAccess = "full_access"
)

const AgentInterruptedMessage = "Agent run stopped by the operator before completion."

var DefaultChatImageAllowedTypes = []string{"image/png", "image/jpeg", "image/webp", "image/gif"}

const (
	WorkspaceShellModeSandbox  = "sandbox"
	WorkspaceShellModeHost     = "host"
	WorkspaceShellModeDisabled = "disabled"
)

func DefaultWorkspaceShellMode(goos string) string {
	if strings.EqualFold(strings.TrimSpace(goos), "linux") {
		return WorkspaceShellModeSandbox
	}
	return WorkspaceShellModeHost
}

type SystemSettings struct {
	AgentMaxIterations               int       `json:"agent_max_iterations"`
	SystemPrompt                     string    `json:"system_prompt"`
	DefaultSystemPrompt              string    `json:"default_system_prompt"`
	ApprovalMode                     string    `json:"approval_mode"`
	ApprovalExplanationsEnabled      bool      `json:"approval_explanations_enabled"`
	SubagentModelProviderID          string    `json:"subagent_model_provider_id"`
	AutomaticApprovalModelProviderID string    `json:"automatic_approval_model_provider_id"`
	SubagentTimeoutSeconds           int       `json:"subagent_timeout_seconds"`
	ContextCompressionEnabled        bool      `json:"context_compression_enabled"`
	ContextCompressionPercent        int       `json:"context_compression_threshold_percent"`
	ChatImageAllowedTypes            []string  `json:"chat_image_allowed_types"`
	WorkspaceShellMode               string    `json:"workspace_shell_mode"`
	WorkspaceShellPlatform           string    `json:"workspace_shell_platform,omitempty"`
	WorkspaceShellBackend            string    `json:"workspace_shell_backend,omitempty"`
	WorkspaceShellName               string    `json:"workspace_shell_name,omitempty"`
	WorkspaceSandboxAvailable        bool      `json:"workspace_sandbox_available"`
	WorkspaceHostShellAvailable      bool      `json:"workspace_host_shell_available"`
	MCPHTTPEnabled                   bool      `json:"mcp_http_enabled"`
	MCPHTTPTokenHash                 string    `json:"-"`
	MCPHTTPTokenConfigured           bool      `json:"mcp_http_token_configured"`
	MCPHTTPToken                     string    `json:"mcp_http_token,omitempty"`
	UpdatedAt                        time.Time `json:"updated_at"`
}

type SystemSettingsInput struct {
	AgentMaxIterations               int      `json:"agent_max_iterations"`
	SystemPrompt                     *string  `json:"system_prompt,omitempty"`
	ApprovalMode                     *string  `json:"approval_mode,omitempty"`
	ApprovalExplanationsEnabled      *bool    `json:"approval_explanations_enabled,omitempty"`
	SubagentModelProviderID          *string  `json:"subagent_model_provider_id,omitempty"`
	AutomaticApprovalModelProviderID *string  `json:"automatic_approval_model_provider_id,omitempty"`
	SubagentTimeoutSeconds           *int     `json:"subagent_timeout_seconds,omitempty"`
	ContextCompressionEnabled        *bool    `json:"context_compression_enabled,omitempty"`
	ContextCompressionPercent        *int     `json:"context_compression_threshold_percent,omitempty"`
	ChatImageAllowedTypes            []string `json:"chat_image_allowed_types,omitempty"`
	WorkspaceShellMode               *string  `json:"workspace_shell_mode,omitempty"`
	MCPHTTPEnabled                   *bool    `json:"mcp_http_enabled,omitempty"`
	RotateMCPHTTPToken               bool     `json:"rotate_mcp_http_token,omitempty"`
}

const (
	DefaultWebSearchBaseURL        = "https://api.tavily.com"
	DefaultWebSearchTimeoutSeconds = 20
	MinWebSearchTimeoutSeconds     = 5
	MaxWebSearchTimeoutSeconds     = 120
	DefaultWebSearchMaxResults     = 10
	MinWebSearchMaxResults         = 1
	MaxWebSearchMaxResults         = 20
)

type WebSearchSettings struct {
	Enabled        bool      `json:"enabled"`
	Provider       string    `json:"provider"`
	BaseURL        string    `json:"base_url"`
	APIKeyCipher   string    `json:"-"`
	HasAPIKey      bool      `json:"has_api_key"`
	ProxyID        string    `json:"proxy_id,omitempty"`
	TimeoutSeconds int       `json:"timeout_seconds"`
	MaxResults     int       `json:"max_results"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type WebSearchSettingsInput struct {
	Enabled        bool   `json:"enabled"`
	BaseURL        string `json:"base_url"`
	APIKey         string `json:"api_key,omitempty"`
	ClearAPIKey    bool   `json:"clear_api_key,omitempty"`
	ProxyID        string `json:"proxy_id,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	MaxResults     int    `json:"max_results"`
}

type WebSearchRequest struct {
	Query           string   `json:"query"`
	MaxResults      int      `json:"max_results,omitempty"`
	Topic           string   `json:"topic,omitempty"`
	SearchDepth     string   `json:"search_depth,omitempty"`
	TimeRange       string   `json:"time_range,omitempty"`
	StartDate       string   `json:"start_date,omitempty"`
	EndDate         string   `json:"end_date,omitempty"`
	ChunksPerSource int      `json:"chunks_per_source,omitempty"`
	IncludeDomains  []string `json:"include_domains,omitempty"`
	ExcludeDomains  []string `json:"exclude_domains,omitempty"`
}

type WebSearchResult struct {
	Title         string  `json:"title"`
	URL           string  `json:"url"`
	Content       string  `json:"content"`
	Score         float64 `json:"score,omitempty"`
	PublishedDate string  `json:"published_date,omitempty"`
	Truncated     bool    `json:"truncated,omitempty"`
	OriginalBytes int     `json:"original_bytes,omitempty"`
	ReturnedBytes int     `json:"returned_bytes,omitempty"`
}

type WebSearchResponse struct {
	ToolMeta
	Query              string            `json:"query"`
	Provider           string            `json:"provider"`
	Results            []WebSearchResult `json:"results"`
	ResponseTime       float64           `json:"response_time,omitempty"`
	RequestID          string            `json:"request_id,omitempty"`
	Credits            float64           `json:"credits,omitempty"`
	Truncated          bool              `json:"truncated,omitempty"`
	OriginalBytes      int               `json:"original_bytes,omitempty"`
	ReturnedBytes      int               `json:"returned_bytes,omitempty"`
	OmittedResults     int               `json:"omitted_results,omitempty"`
	ContentIsUntrusted bool              `json:"content_is_untrusted"`
}

type WebExtractRequest struct {
	URLs            []string `json:"urls"`
	Query           string   `json:"query,omitempty"`
	ExtractDepth    string   `json:"extract_depth,omitempty"`
	ChunksPerSource int      `json:"chunks_per_source,omitempty"`
}

type WebExtractResult struct {
	URL           string `json:"url"`
	RawContent    string `json:"raw_content"`
	Truncated     bool   `json:"truncated,omitempty"`
	OriginalBytes int    `json:"original_bytes,omitempty"`
	ReturnedBytes int    `json:"returned_bytes,omitempty"`
}

type WebExtractFailedResult struct {
	URL   string `json:"url"`
	Error string `json:"error"`
}

type WebExtractResponse struct {
	ToolMeta
	Provider           string                   `json:"provider"`
	Query              string                   `json:"query,omitempty"`
	Results            []WebExtractResult       `json:"results"`
	FailedResults      []WebExtractFailedResult `json:"failed_results,omitempty"`
	ResponseTime       float64                  `json:"response_time,omitempty"`
	RequestID          string                   `json:"request_id,omitempty"`
	Credits            float64                  `json:"credits,omitempty"`
	Truncated          bool                     `json:"truncated,omitempty"`
	OriginalBytes      int                      `json:"original_bytes,omitempty"`
	ReturnedBytes      int                      `json:"returned_bytes,omitempty"`
	OmittedResults     int                      `json:"omitted_results,omitempty"`
	ContentIsUntrusted bool                     `json:"content_is_untrusted"`
}

type MCPTransport string

const (
	MCPTransportStdio          MCPTransport = "stdio"
	MCPTransportStreamableHTTP MCPTransport = "streamable_http"
)

type MCPTool struct {
	Name        string `json:"name"`
	ExposedName string `json:"exposed_name"`
	Description string `json:"description,omitempty"`
}

type MCPServer struct {
	ID              string       `json:"id"`
	Name            string       `json:"name"`
	Transport       MCPTransport `json:"transport"`
	Command         string       `json:"command,omitempty"`
	Args            []string     `json:"args,omitempty"`
	Cwd             string       `json:"cwd,omitempty"`
	URL             string       `json:"url,omitempty"`
	EnvKeys         []string     `json:"env_keys,omitempty"`
	HeaderKeys      []string     `json:"header_keys,omitempty"`
	OAuthConfigured bool         `json:"oauth_configured"`
	OAuthExpiresAt  *time.Time   `json:"oauth_expires_at,omitempty"`
	SecretsCipher   string       `json:"-"`
	Enabled         bool         `json:"enabled"`
	Status          string       `json:"status"`
	LastError       string       `json:"last_error,omitempty"`
	ConnectedAt     *time.Time   `json:"connected_at,omitempty"`
	ToolCount       int          `json:"tool_count"`
	Tools           []MCPTool    `json:"tools,omitempty"`
	CreatedAt       time.Time    `json:"created_at"`
	UpdatedAt       time.Time    `json:"updated_at"`
}

type MCPOAuthStart struct {
	AuthorizationURL string `json:"authorization_url"`
}

type MCPServerInput struct {
	ID        string            `json:"id,omitempty"`
	Name      string            `json:"name"`
	Transport MCPTransport      `json:"transport"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	URL       string            `json:"url,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Enabled   bool              `json:"enabled"`
}

type MCPTestResult struct {
	OK        bool      `json:"ok"`
	LatencyMS int64     `json:"latency_ms"`
	ToolCount int       `json:"tool_count"`
	Tools     []MCPTool `json:"tools"`
}

const (
	MCPCallRunning     = "running"
	MCPCallCompleted   = "completed"
	MCPCallFailed      = "failed"
	MCPCallInterrupted = "interrupted"
)

type MCPClientSession struct {
	ID              string    `json:"id"`
	Transport       string    `json:"transport"`
	ClientName      string    `json:"client_name,omitempty"`
	ClientVersion   string    `json:"client_version,omitempty"`
	ProtocolVersion string    `json:"protocol_version,omitempty"`
	CallCount       int       `json:"call_count"`
	RunningCalls    int       `json:"running_calls"`
	StartedAt       time.Time `json:"started_at"`
	LastSeenAt      time.Time `json:"last_seen_at"`
}

type MCPToolCall struct {
	ID              string    `json:"id"`
	SessionID       string    `json:"session_id"`
	ToolName        string    `json:"tool_name"`
	ArgumentsJSON   string    `json:"arguments_json"`
	Status          string    `json:"status"`
	RunID           string    `json:"run_id,omitempty"`
	ApprovalID      string    `json:"approval_id,omitempty"`
	TaskID          string    `json:"task_id,omitempty"`
	ShellID         string    `json:"shell_id,omitempty"`
	TunnelID        string    `json:"tunnel_id,omitempty"`
	OperationStatus string    `json:"operation_status,omitempty"`
	Error           string    `json:"error,omitempty"`
	StartedAt       time.Time `json:"started_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	CompletedAt     time.Time `json:"completed_at,omitempty,omitzero"`
}

type MCPActivitySnapshot struct {
	Sessions []MCPClientSession `json:"sessions"`
	Calls    []MCPToolCall      `json:"calls"`
}

type MCPActivityEvent struct {
	Sequence         uint64            `json:"sequence"`
	Type             string            `json:"type"`
	SessionID        string            `json:"session_id"`
	CallID           string            `json:"call_id"`
	Session          *MCPClientSession `json:"session,omitempty"`
	Call             *MCPToolCall      `json:"call,omitempty"`
	RunID            string            `json:"run_id,omitempty"`
	Stream           string            `json:"stream,omitempty"`
	Content          string            `json:"content,omitempty"`
	Status           string            `json:"status,omitempty"`
	TransferredBytes int64             `json:"transferred_bytes,omitempty"`
	TotalBytes       int64             `json:"total_bytes,omitempty"`
}

type ChatSession struct {
	ID            string    `json:"id"`
	Title         string    `json:"title"`
	TitleSet      bool      `json:"-"`
	WorkspaceID   string    `json:"workspace_id"`
	ContextTokens int       `json:"context_tokens"`
	ContextWindow int       `json:"context_window"`
	MessageCount  int       `json:"message_count"`
	UpdatedAt     time.Time `json:"updated_at"`
	Active        bool      `json:"active"`
}

type ChatContextSummary struct {
	SessionID        string    `json:"session_id"`
	Summary          string    `json:"summary,omitempty"`
	ThroughMessageID string    `json:"through_message_id"`
	Revision         int       `json:"revision"`
	Trigger          string    `json:"trigger"`
	SourceTokens     int       `json:"source_tokens"`
	SummaryTokens    int       `json:"summary_tokens"`
	Model            string    `json:"model,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type ChatContextCompressionResult struct {
	Summary      ChatContextSummary `json:"summary"`
	BeforeTokens int                `json:"before_tokens"`
	AfterTokens  int                `json:"after_tokens"`
}

type ChatMessage struct {
	ID               string           `json:"id"`
	Role             string           `json:"role"`
	Content          string           `json:"content"`
	ContentTruncated bool             `json:"content_truncated,omitempty"`
	ContentChars     int              `json:"content_chars,omitempty"`
	ModelExtra       map[string]any   `json:"-"`
	TokenUsage       *ChatTokenUsage  `json:"token_usage,omitempty"`
	ToolName         string           `json:"tool_name,omitempty"`
	ToolCallID       string           `json:"tool_call_id,omitempty"`
	ToolArguments    string           `json:"-"`
	RunID            string           `json:"run_id,omitempty"`
	ToolStatus       string           `json:"tool_status,omitempty"`
	Status           string           `json:"status"`
	Attachments      []ChatAttachment `json:"attachments,omitempty"`
	CreatedAt        time.Time        `json:"created_at"`
}

type ChatMessagePage struct {
	Messages      []ChatMessage `json:"messages"`
	HasMore       bool          `json:"has_more"`
	NextCreatedAt string        `json:"next_created_at,omitempty"`
	NextID        string        `json:"next_id,omitempty"`
}

const ChatMessageRoleAssistantProgress = "assistant_progress"

type ChatTokenUsage struct {
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	TotalTokens     int `json:"total_tokens"`
	CachedTokens    int `json:"cached_tokens,omitempty"`
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

const (
	ChatToolCallRunning          = "running"
	ChatToolCallApprovalRequired = "approval_required"
	ChatToolCallCompleted        = "completed"
	ChatToolCallPartial          = "partial"
	ChatToolCallFailed           = "failed"
	ChatToolCallInterrupted      = "interrupted"
	ChatToolCallRejected         = "rejected"
	ChatToolCallExpired          = "expired"
	ChatToolCallUnknown          = "unknown"
)

type ChatToolCall struct {
	SessionID     string    `json:"session_id"`
	UserMessageID string    `json:"user_message_id"`
	MessageID     string    `json:"message_id"`
	ToolCallID    string    `json:"tool_call_id"`
	RunID         string    `json:"run_id,omitempty"`
	ToolName      string    `json:"tool_name"`
	ArgumentsJSON string    `json:"arguments_json"`
	Status        string    `json:"status"`
	ResultJSON    string    `json:"result_json"`
	Error         string    `json:"error,omitempty"`
	StartedAt     time.Time `json:"started_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	CompletedAt   time.Time `json:"completed_at,omitempty,omitzero"`
}

type ChatAttachment struct {
	ID        string `json:"id"`
	MessageID string `json:"-"`
	Name      string `json:"name"`
	MIMEType  string `json:"mime_type"`
	SizeBytes int64  `json:"size_bytes"`
	Data      []byte `json:"-"`
}

type AgentTaskList struct {
	SessionID string      `json:"session_id"`
	Items     []AgentTask `json:"items"`
	UpdatedAt time.Time   `json:"updated_at"`
}

type AgentTask struct {
	ID          string         `json:"id"`
	Subject     string         `json:"subject"`
	Description string         `json:"description"`
	Status      string         `json:"status"`
	Blocks      []string       `json:"blocks"`
	BlockedBy   []string       `json:"blocked_by"`
	ActiveForm  string         `json:"active_form,omitempty"`
	Owner       string         `json:"owner,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

type ExecMode string

const (
	ExecProgram                ExecMode = "program"
	ExecScript                 ExecMode = "script"
	ExecWorkspaceRead          ExecMode = "workspace_read"
	ExecWorkspaceDirectoryList ExecMode = "workspace_directory_list"
	ExecWorkspaceSearch        ExecMode = "workspace_search"
	ExecWorkspaceEdit          ExecMode = "workspace_edit"
	ExecWorkspaceDelete        ExecMode = "workspace_delete"
	ExecRemoteRead             ExecMode = "remote_read"
	ExecRemoteSearch           ExecMode = "remote_search"
	ExecRemoteEdit             ExecMode = "remote_edit"
	ExecWorkspaceUpload        ExecMode = "workspace_upload"
	ExecWorkspaceDownload      ExecMode = "workspace_download"
	ExecWorkspaceShell         ExecMode = "workspace_shell"
	ExecWorkspaceShellStart    ExecMode = "workspace_shell_start"
	ExecSSHFileTransfer        ExecMode = "ssh_file_transfer"
	ExecSSHTunnelStart         ExecMode = "ssh_tunnel_start"
	ExecSSHShellStart          ExecMode = "ssh_shell_start"
)

type FileSearchMatchMode string

const (
	FileSearchLiteral FileSearchMatchMode = "literal"
	FileSearchRegex   FileSearchMatchMode = "regex"
)

type SSHTunnelDirection string

const (
	SSHTunnelDirectionLocal   SSHTunnelDirection = "local"
	SSHTunnelDirectionReverse SSHTunnelDirection = "reverse"
)

type SSHTunnelConfig struct {
	Direction  SSHTunnelDirection `json:"direction,omitempty"`
	LocalHost  string             `json:"local_host,omitempty"`
	LocalPort  int                `json:"local_port,omitempty"`
	RemoteHost string             `json:"remote_host,omitempty"`
	RemotePort int                `json:"remote_port,omitempty"`
}

type ExecRequest struct {
	HostID                    string              `json:"host_id" jsonschema:"registered host identifier; never an address or credential"`
	Mode                      ExecMode            `json:"mode,omitempty" jsonschema:"program for argv execution or script for a reviewed remote shell script"`
	Program                   string              `json:"program,omitempty" jsonschema:"remote executable name for program mode"`
	Args                      []string            `json:"args,omitempty" jsonschema:"separate arguments; do not include shell quoting"`
	Script                    string              `json:"script,omitempty" jsonschema:"remote shell script content for script mode"`
	Background                bool                `json:"background,omitempty" jsonschema:"run as a cancellable background task"`
	Change                    *FileChange         `json:"change,omitempty"`
	TextEdit                  *TextEdit           `json:"text_edit,omitempty"`
	Cwd                       string              `json:"cwd,omitempty" jsonschema:"absolute remote working directory, or a clean workspace-relative directory for workspace_shell"`
	Env                       map[string]string   `json:"env,omitempty" jsonschema:"non-secret environment values"`
	Elevated                  bool                `json:"elevated,omitempty" jsonschema:"request root through the host sudo policy; never pass sudo or a password as a program or argument"`
	TimeoutSeconds            int                 `json:"timeout_seconds,omitempty" jsonschema:"execution timeout in seconds"`
	Reason                    string              `json:"reason" jsonschema:"concise one-sentence purpose of this operation"`
	RemotePath                string              `json:"remote_path,omitempty" jsonschema:"absolute remote file path for transfers"`
	SourceHostID              string              `json:"source_host_id,omitempty" jsonschema:"registered source host identifier for host-to-host transfers"`
	SourcePath                string              `json:"source_path,omitempty" jsonschema:"absolute source path for host-to-host transfers"`
	ExpectedDestinationSHA256 string              `json:"expected_destination_sha256,omitempty" jsonschema:"current destination SHA256; omit to require a new destination, provide it to replace that exact version"`
	WorkspaceID               string              `json:"workspace_id,omitempty" jsonschema:"registered workspace identifier"`
	WorkspaceShellBackend     string              `json:"workspace_shell_backend,omitempty" jsonschema:"control-plane-selected workspace shell backend bound into approval"`
	SSHConnectionDigest       string              `json:"ssh_connection_digest,omitempty" jsonschema:"control-plane-selected SSH connection revision bound into approval"`
	SourceConnectionDigest    string              `json:"source_connection_digest,omitempty" jsonschema:"control-plane-selected source SSH connection revision bound into approval"`
	RelativePath              string              `json:"relative_path,omitempty" jsonschema:"path relative to the workspace root"`
	ExpectedSHA256            string              `json:"expected_sha256,omitempty" jsonschema:"workspace file version observed before mutation"`
	Recursive                 bool                `json:"recursive,omitempty" jsonschema:"allow recursive Workspace directory deletion"`
	Validator                 string              `json:"validator,omitempty" jsonschema:"allowlisted validator identifier"`
	SearchPattern             string              `json:"search_pattern,omitempty" jsonschema:"file search pattern"`
	SearchMatchMode           FileSearchMatchMode `json:"search_match_mode,omitempty" jsonschema:"file search matching mode: literal or regex"`
	ContextLines              int                 `json:"context_lines,omitempty" jsonschema:"lines around each file search match"`
	MetadataOnly              bool                `json:"metadata_only,omitempty" jsonschema:"return remote file metadata without content"`
	TailLines                 int                 `json:"tail_lines,omitempty" jsonschema:"number of final remote file lines to return"`
	OffsetBytes               int64               `json:"offset_bytes,omitempty" jsonschema:"file read offset; negative values count from the end"`
	MaxBytes                  int                 `json:"max_bytes,omitempty" jsonschema:"bounded file read length"`
	TunnelDirection           SSHTunnelDirection  `json:"direction,omitempty" jsonschema:"SSH forwarding direction: local or reverse"`
	TunnelLocalHost           string              `json:"local_host,omitempty" jsonschema:"local bind IP for local forwarding, or client-side target for reverse forwarding"`
	TunnelLocalPort           int                 `json:"local_port,omitempty" jsonschema:"local listener port for local forwarding, or client-side target port for reverse forwarding"`
	TunnelRemoteHost          string              `json:"remote_host,omitempty" jsonschema:"host-side target for local forwarding, or SSH-server bind IP for reverse forwarding"`
	TunnelRemotePort          int                 `json:"remote_port,omitempty" jsonschema:"host-side target port for local forwarding, or SSH-server listener port for reverse forwarding"`
	ShellCols                 int                 `json:"shell_cols,omitempty" jsonschema:"interactive shell terminal columns"`
	ShellRows                 int                 `json:"shell_rows,omitempty" jsonschema:"interactive shell terminal rows"`
	LocalPath                 string              `json:"-"`
	ShellSurface              string              `json:"-"`
	MaxOutputBytes            int                 `json:"-"`
	OutputView                string              `json:"-"`
}

// SearchText returns the request's human-readable text exactly as submitted,
// without JSON string escaping, so audit search can match what an operator or
// model would type (quotes, redirections, backslashes, newlines).
func (r ExecRequest) SearchText() string {
	parts := []string{r.Program}
	parts = append(parts, r.Args...)
	parts = append(parts, r.Script, r.Cwd, r.Reason,
		r.RemotePath, r.SourcePath, r.WorkspaceID, r.RelativePath, r.SearchPattern,
		string(r.TunnelDirection), r.TunnelLocalHost, r.TunnelRemoteHost)
	if r.TunnelRemotePort != 0 {
		parts = append(parts, strconv.Itoa(r.TunnelRemotePort))
	}
	if r.TunnelLocalPort != 0 {
		parts = append(parts, strconv.Itoa(r.TunnelLocalPort))
	}
	if r.Change != nil {
		parts = append(parts, r.Change.Diff)
	}
	filtered := parts[:0]
	for _, part := range parts {
		if part != "" {
			filtered = append(filtered, part)
		}
	}
	return strings.Join(filtered, "\n")
}

type ToolMeta struct {
	ToolVersion string                 `json:"tool_version,omitempty"`
	OK          bool                   `json:"ok"`
	Code        string                 `json:"code,omitempty"`
	Message     string                 `json:"message,omitempty"`
	Retryable   bool                   `json:"retryable,omitempty"`
	NextAction  string                 `json:"next_action,omitempty"`
	Validation  *ToolValidationDetails `json:"validation,omitempty"`
}

type ToolFailure struct {
	ToolMeta
	Status string `json:"status"`
}

type ToolValidationDetails struct {
	Action           string         `json:"action,omitempty"`
	SuggestedTool    string         `json:"suggested_tool,omitempty"`
	AllowedFields    []string       `json:"allowed_fields,omitempty"`
	GotFields        []string       `json:"got_fields,omitempty"`
	UnexpectedFields []string       `json:"unexpected_fields,omitempty"`
	Example          map[string]any `json:"example,omitempty"`
}

type ExecResult struct {
	ToolMeta
	RunID               string            `json:"run_id"`
	TaskID              string            `json:"task_id,omitempty"`
	Status              string            `json:"status"`
	AutoApproved        bool              `json:"auto_approved,omitempty"`
	ApprovalID          string            `json:"approval_id,omitempty"`
	OperatorInstruction string            `json:"operator_instruction,omitempty"`
	ExitCode            int               `json:"exit_code,omitempty"`
	Stdout              string            `json:"stdout,omitempty"`
	Stderr              string            `json:"stderr,omitempty"`
	OutputView          string            `json:"output_view,omitempty"`
	OutputLimited       bool              `json:"output_limited,omitempty"`
	StdoutTotalBytes    int               `json:"stdout_total_bytes,omitempty"`
	StderrTotalBytes    int               `json:"stderr_total_bytes,omitempty"`
	StdoutOmittedBytes  int               `json:"stdout_omitted_bytes,omitempty"`
	StderrOmittedBytes  int               `json:"stderr_omitted_bytes,omitempty"`
	StdoutOffsetBytes   int               `json:"stdout_offset_bytes,omitempty"`
	StderrOffsetBytes   int               `json:"stderr_offset_bytes,omitempty"`
	WaitDeadlineReached bool              `json:"wait_deadline_reached,omitempty"`
	Duration            time.Duration     `json:"duration,omitempty"`
	CompletedAt         time.Time         `json:"completed_at,omitempty,omitzero"`
	File                *FileMetadata     `json:"file,omitempty"`
	Change              *FileChange       `json:"change,omitempty"`
	Search              *FileSearchResult `json:"search,omitempty"`
	Tunnel              *SSHTunnel        `json:"tunnel,omitempty"`
	Shell               *SSHShell         `json:"shell,omitempty"`
	ShellUsage          *SSHShellUsage    `json:"shell_usage,omitempty"`
}

type SSHTunnel struct {
	ID                string             `json:"id"`
	HostID            string             `json:"host_id"`
	HostName          string             `json:"host_name"`
	Direction         SSHTunnelDirection `json:"direction"`
	LocalHost         string             `json:"local_host"`
	LocalPort         int                `json:"local_port"`
	RemoteHost        string             `json:"remote_host"`
	RemotePort        int                `json:"remote_port"`
	Status            string             `json:"status"`
	ProxyUsed         bool               `json:"proxy_used"`
	ActiveConnections int64              `json:"active_connections"`
	TotalConnections  int64              `json:"total_connections"`
	BytesSent         int64              `json:"bytes_sent"`
	BytesReceived     int64              `json:"bytes_received"`
	ReconnectAttempt  int                `json:"reconnect_attempt,omitempty"`
	Error             string             `json:"error,omitempty"`
	StartedAt         time.Time          `json:"started_at"`
}

type SSHTunnelList struct {
	Tunnels []SSHTunnel `json:"tunnels"`
	Count   int         `json:"count"`
}

type SSHShell struct {
	ID                string    `json:"id"`
	RunID             string    `json:"run_id"`
	SessionID         string    `json:"session_id"`
	Kind              string    `json:"kind"`
	Surface           string    `json:"surface"`
	HostID            string    `json:"host_id"`
	HostName          string    `json:"host_name"`
	WorkspaceID       string    `json:"workspace_id,omitempty"`
	Backend           string    `json:"backend,omitempty"`
	User              string    `json:"user"`
	Elevated          bool      `json:"elevated"`
	Cwd               string    `json:"cwd,omitempty"`
	Status            string    `json:"status"`
	Cols              int       `json:"cols"`
	Rows              int       `json:"rows"`
	LastSequence      uint64    `json:"last_sequence"`
	ResponseSequence  uint64    `json:"-"`
	ExitCode          *int      `json:"exit_code,omitempty"`
	TerminationReason string    `json:"termination_reason,omitempty"`
	Error             string    `json:"error,omitempty"`
	StartedAt         time.Time `json:"started_at"`
	EndedAt           time.Time `json:"ended_at,omitempty,omitzero"`
}

type SSHShellEvent struct {
	ShellID         string    `json:"shell_id"`
	FirstSequence   uint64    `json:"first_sequence,omitempty"`
	Sequence        uint64    `json:"sequence"`
	Stream          string    `json:"stream"`
	Source          string    `json:"source,omitempty"`
	Content         string    `json:"content,omitempty"`
	ReadableContent *string   `json:"-"`
	Sensitive       bool      `json:"sensitive,omitempty"`
	InputBytes      int       `json:"input_bytes,omitempty"`
	Status          string    `json:"status,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

type SSHShellSnapshot struct {
	Shell        SSHShell        `json:"shell"`
	Events       []SSHShellEvent `json:"events"`
	RecentOutput string          `json:"recent_output,omitempty"`
	NextSequence uint64          `json:"next_sequence"`
}

type SSHShellOutputPage struct {
	Snapshot SSHShellSnapshot
	HasMore  bool
}

type SSHShellList struct {
	Shells      []SSHShell `json:"shells"`
	Count       int        `json:"count"`
	WorkspaceID string     `json:"workspace_id,omitempty"`
}

type SSHShellUsage struct {
	Input  string `json:"input"`
	Output string `json:"output"`
	Close  string `json:"close"`
}

const (
	DefaultShellQueryDelaySeconds = 5
	MaxShellQueryDelaySeconds     = 600

	SSHShellKindSSH               = "ssh"
	SSHShellKindWorkspace         = "workspace"
	SSHShellSurfaceAgent          = "agent"
	SSHShellSurfaceMCP            = "mcp"
	SSHShellSurfaceQuick          = "quick"
	SSHShellSurfaceWorkspace      = "workspace"
	WorkspaceShellSurfaceAgent    = "workspace_agent"
	WorkspaceShellSurfaceOperator = "workspace_operator"
)

type FileSearchResult struct {
	Found        bool                `json:"found"`
	Pattern      string              `json:"pattern"`
	MatchMode    FileSearchMatchMode `json:"match_mode"`
	ContextLines int                 `json:"context_lines"`
}

type FileMetadata struct {
	Path          string `json:"path"`
	Size          int64  `json:"size,omitempty"`
	Mode          string `json:"mode,omitempty"`
	Owner         string `json:"owner,omitempty"`
	Group         string `json:"group,omitempty"`
	ModifiedUnix  int64  `json:"modified_unix,omitempty"`
	SHA256        string `json:"sha256,omitempty"`
	Validator     string `json:"validator,omitempty"`
	ValidationOK  bool   `json:"validation_ok,omitempty"`
	Sensitive     bool   `json:"sensitive,omitempty"`
	OffsetBytes   int64  `json:"offset_bytes,omitempty"`
	ReturnedBytes int    `json:"returned_bytes,omitempty"`
	HasMore       bool   `json:"has_more,omitempty"`
	NextOffset    int64  `json:"next_offset,omitempty"`
}

type FileChange struct {
	Diff      string `json:"diff"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

type TextEdit struct {
	OldText string `json:"old_text"`
	NewText string `json:"new_text"`
}

type CommandReviewInput struct {
	Request       ExecRequest    `json:"request"`
	Host          HostCapability `json:"host"`
	CurrentTask   string         `json:"current_task,omitempty"`
	RequestDigest string         `json:"request_digest"`
}

type AutomaticApprovalInput struct {
	Request       ExecRequest    `json:"request"`
	Host          HostCapability `json:"host"`
	UserRequest   string         `json:"user_request"`
	CurrentTask   string         `json:"current_task,omitempty"`
	RequestDigest string         `json:"request_digest"`
}

type CommandExplanation struct {
	Summary   string   `json:"summary"`
	Mechanism string   `json:"mechanism"`
	Risks     []string `json:"risks"`
}

const (
	ApprovalContinuationAgent = "agent"

	ApprovalStatusPreparing = "preparing"
	ApprovalStatusPending   = "pending"
	ApprovalStatusApproved  = "approved"
	ApprovalStatusRejected  = "rejected"

	ApprovalAgentAllow  = "allow"
	ApprovalAgentReject = "reject"
	ApprovalAgentManual = "manual"

	CommandReviewKindAutomaticApproval = "automatic_approval"
)

type CommandReview struct {
	Status      string              `json:"status"`
	Kind        string              `json:"kind,omitempty"`
	Model       string              `json:"model,omitempty"`
	Decision    string              `json:"decision,omitempty"`
	Reason      string              `json:"reason,omitempty"`
	Explanation *CommandExplanation `json:"explanation,omitempty"`
	Errors      []string            `json:"errors,omitempty"`
	ReviewedAt  time.Time           `json:"reviewed_at"`
}

type Run struct {
	ID                string         `json:"id"`
	SessionID         string         `json:"session_id,omitempty"`
	HostID            string         `json:"host_id"`
	ToolName          string         `json:"tool_name,omitempty"`
	ToolArgumentsJSON string         `json:"tool_arguments_json,omitempty"`
	RequestJSON       string         `json:"request_json"`
	RequestCipher     string         `json:"-"`
	SearchText        string         `json:"-"`
	RequestDigest     string         `json:"request_digest"`
	Status            string         `json:"status"`
	ExitCode          int            `json:"exit_code"`
	StdoutRedacted    string         `json:"stdout_redacted,omitempty"`
	StderrRedacted    string         `json:"stderr_redacted,omitempty"`
	StdoutCipher      string         `json:"-"`
	StderrCipher      string         `json:"-"`
	Error             string         `json:"error,omitempty"`
	AIReviewJSON      string         `json:"-"`
	AIReview          *CommandReview `json:"ai_review,omitempty"`
	StartedAt         time.Time      `json:"started_at"`
	CompletedAt       time.Time      `json:"completed_at,omitempty,omitzero"`
}

type RunSearchFilter struct {
	Query         string
	QueryScope    string
	HostID        string
	SessionID     string
	ToolName      string
	Status        string
	StartedAfter  time.Time
	StartedBefore time.Time
	CursorStarted time.Time
	CursorID      string
	Limit         int
	ScanLimit     int
}

// RunSearchPage is the bounded, cursor-based projection used by audit and Agent history clients.
type RunSearchPage struct {
	Runs          []Run     `json:"runs"`
	HasMore       bool      `json:"has_more"`
	ScanLimited   bool      `json:"scan_limited,omitempty"`
	NextStartedAt time.Time `json:"next_started_at,omitempty,omitzero"`
	NextID        string    `json:"next_id,omitempty"`
}

type AuditRunDeleteResult struct {
	Deleted  int `json:"deleted"`
	Retained int `json:"retained"`
}

type Approval struct {
	ID               string         `json:"id"`
	RunID            string         `json:"run_id"`
	SessionID        string         `json:"session_id,omitempty"`
	HostID           string         `json:"host_id"`
	RequestJSON      string         `json:"request_json"`
	RequestCipher    string         `json:"-"`
	RequestDigest    string         `json:"request_digest"`
	Status           string         `json:"status"`
	Reason           string         `json:"reason,omitempty"`
	ContinuationKind string         `json:"continuation_kind,omitempty"`
	CheckpointID     string         `json:"-"`
	InterruptID      string         `json:"-"`
	AIReview         *CommandReview `json:"ai_review,omitempty"`
	CreatedAt        time.Time      `json:"created_at"`
	DecidedAt        time.Time      `json:"decided_at,omitempty,omitzero"`
}

type Task struct {
	ToolMeta
	ID                  string    `json:"id"`
	RunID               string    `json:"run_id"`
	SessionID           string    `json:"session_id,omitempty"`
	HostID              string    `json:"host_id"`
	Status              string    `json:"status"`
	Revision            uint64    `json:"revision"`
	OperatorInstruction string    `json:"operator_instruction,omitempty"`
	StartedAt           time.Time `json:"started_at"`
	EndedAt             time.Time `json:"ended_at,omitempty,omitzero"`
}

type TaskSnapshot struct {
	Task   Task       `json:"task"`
	Result ExecResult `json:"result"`
	Error  string     `json:"error,omitempty"`
}

type TaskEvent struct {
	Type        string        `json:"type"`
	TaskID      string        `json:"task_id,omitempty"`
	Revision    uint64        `json:"revision,omitempty"`
	Snapshot    *TaskSnapshot `json:"snapshot,omitempty"`
	Stream      string        `json:"stream,omitempty"`
	OffsetBytes int           `json:"offset_bytes,omitempty"`
	TotalBytes  int           `json:"total_bytes,omitempty"`
	Content     string        `json:"content,omitempty"`
}

type AuditEvent struct {
	ID        string         `json:"id"`
	RunID     string         `json:"run_id,omitempty"`
	Type      string         `json:"type"`
	Actor     string         `json:"actor"`
	Data      map[string]any `json:"data"`
	CreatedAt time.Time      `json:"created_at"`
}

type AuditEventPage struct {
	Events        []AuditEvent `json:"events"`
	HasMore       bool         `json:"has_more"`
	NextCreatedAt time.Time    `json:"next_created_at,omitempty,omitzero"`
	NextID        string       `json:"next_id,omitempty"`
}
