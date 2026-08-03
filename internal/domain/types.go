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
	ID       string `json:"id"`
	Name     string `json:"name"`
	AuthType string `json:"auth_type"`
	SudoMode string `json:"sudo_mode"`
}

type ModelProvider struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Kind         string    `json:"kind"`
	BaseURL      string    `json:"base_url,omitempty"`
	Model        string    `json:"model"`
	APIKeyCipher string    `json:"-"`
	HasAPIKey    bool      `json:"has_api_key"`
	ProxyID      string    `json:"proxy_id,omitempty"`
	UserAgent    string    `json:"user_agent,omitempty"`
	Active       bool      `json:"active"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type ModelProviderInput struct {
	ID      string `json:"id,omitempty"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	BaseURL string `json:"base_url,omitempty"`
	Model   string `json:"model"`
	APIKey  string `json:"api_key,omitempty"`
	ProxyID string `json:"proxy_id,omitempty"`
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
	ID        string  `json:"id,omitempty"`
	Kind      string  `json:"kind,omitempty"`
	BaseURL   *string `json:"base_url,omitempty"`
	Model     string  `json:"model"`
	APIKey    string  `json:"api_key,omitempty"`
	ProxyID   *string `json:"proxy_id,omitempty"`
	UserAgent *string `json:"user_agent,omitempty"`
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
	Models []string `json:"models"`
	Count  int      `json:"count"`
}

const (
	DefaultAgentMaxIterations     = 50
	MinAgentMaxIterations         = 5
	MaxAgentMaxIterations         = 100
	DefaultSubagentTimeoutSeconds = 30
	MinSubagentTimeoutSeconds     = 5
	MaxSubagentTimeoutSeconds     = 120
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
	Query          string   `json:"query"`
	MaxResults     int      `json:"max_results,omitempty"`
	TimeRange      string   `json:"time_range,omitempty"`
	IncludeDomains []string `json:"include_domains,omitempty"`
	ExcludeDomains []string `json:"exclude_domains,omitempty"`
}

type WebSearchResult struct {
	Title         string  `json:"title"`
	URL           string  `json:"url"`
	Content       string  `json:"content"`
	Score         float64 `json:"score,omitempty"`
	PublishedDate string  `json:"published_date,omitempty"`
}

type WebSearchResponse struct {
	ToolMeta
	Query              string            `json:"query"`
	Provider           string            `json:"provider"`
	Results            []WebSearchResult `json:"results"`
	ResponseTime       float64           `json:"response_time,omitempty"`
	ContentIsUntrusted bool              `json:"content_is_untrusted"`
}

type WebExtractRequest struct {
	URLs []string `json:"urls"`
}

type WebExtractResult struct {
	URL        string `json:"url"`
	RawContent string `json:"raw_content"`
}

type WebExtractFailedResult struct {
	URL   string `json:"url"`
	Error string `json:"error"`
}

type WebExtractResponse struct {
	ToolMeta
	Provider           string                   `json:"provider"`
	Results            []WebExtractResult       `json:"results"`
	FailedResults      []WebExtractFailedResult `json:"failed_results,omitempty"`
	ResponseTime       float64                  `json:"response_time,omitempty"`
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

type ChatSession struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	WorkspaceID  string    `json:"workspace_id"`
	MessageCount int       `json:"message_count"`
	UpdatedAt    time.Time `json:"updated_at"`
	Active       bool      `json:"active"`
}

type ChatMessage struct {
	ID          string           `json:"id"`
	Role        string           `json:"role"`
	Content     string           `json:"content"`
	ToolName    string           `json:"tool_name,omitempty"`
	Status      string           `json:"status"`
	Attachments []ChatAttachment `json:"attachments,omitempty"`
	CreatedAt   time.Time        `json:"created_at"`
}

type ChatAttachment struct {
	ID        string `json:"id"`
	MessageID string `json:"-"`
	Name      string `json:"name"`
	MIMEType  string `json:"mime_type"`
	SizeBytes int64  `json:"size_bytes"`
	Data      []byte `json:"-"`
}

type AgentPlan struct {
	SessionID string          `json:"session_id"`
	Goal      string          `json:"goal"`
	Status    string          `json:"status"`
	Steps     []AgentPlanStep `json:"steps"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

type AgentPlanStep struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updated_at"`
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

type ExecRequest struct {
	HostID                    string              `json:"host_id" jsonschema:"registered host identifier; never an address or credential"`
	Mode                      ExecMode            `json:"mode,omitempty" jsonschema:"program for argv execution or script for a reviewed bash script"`
	Program                   string              `json:"program,omitempty" jsonschema:"remote executable name for program mode"`
	Args                      []string            `json:"args,omitempty" jsonschema:"separate arguments; do not include shell quoting"`
	Script                    string              `json:"script,omitempty" jsonschema:"bash script content for script mode"`
	Background                bool                `json:"background,omitempty" jsonschema:"run as a cancellable background task"`
	Change                    *FileChange         `json:"change,omitempty"`
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
	TunnelRemoteHost          string              `json:"remote_host,omitempty" jsonschema:"host reached from the SSH target"`
	TunnelRemotePort          int                 `json:"remote_port,omitempty" jsonschema:"port reached from the SSH target"`
	TunnelLocalPort           int                 `json:"local_port,omitempty" jsonschema:"local loopback port; zero selects an available port"`
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
		r.RemotePath, r.SourcePath, r.WorkspaceID, r.RelativePath, r.SearchPattern, r.TunnelRemoteHost)
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
	ID                string    `json:"id"`
	HostID            string    `json:"host_id"`
	HostName          string    `json:"host_name"`
	LocalHost         string    `json:"local_host"`
	LocalPort         int       `json:"local_port"`
	RemoteHost        string    `json:"remote_host"`
	RemotePort        int       `json:"remote_port"`
	Status            string    `json:"status"`
	ProxyUsed         bool      `json:"proxy_used"`
	ActiveConnections int64     `json:"active_connections"`
	TotalConnections  int64     `json:"total_connections"`
	BytesSent         int64     `json:"bytes_sent"`
	BytesReceived     int64     `json:"bytes_received"`
	Error             string    `json:"error,omitempty"`
	StartedAt         time.Time `json:"started_at"`
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
	ExitCode          *int      `json:"exit_code,omitempty"`
	TerminationReason string    `json:"termination_reason,omitempty"`
	Error             string    `json:"error,omitempty"`
	StartedAt         time.Time `json:"started_at"`
	EndedAt           time.Time `json:"ended_at,omitempty,omitzero"`
}

type SSHShellEvent struct {
	ShellID       string    `json:"shell_id"`
	FirstSequence uint64    `json:"first_sequence,omitempty"`
	Sequence      uint64    `json:"sequence"`
	Stream        string    `json:"stream"`
	Source        string    `json:"source,omitempty"`
	Content       string    `json:"content,omitempty"`
	Sensitive     bool      `json:"sensitive,omitempty"`
	InputBytes    int       `json:"input_bytes,omitempty"`
	Status        string    `json:"status,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

type SSHShellSnapshot struct {
	Shell        SSHShell        `json:"shell"`
	Events       []SSHShellEvent `json:"events"`
	RecentOutput string          `json:"recent_output,omitempty"`
	NextSequence uint64          `json:"next_sequence"`
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

type CommandReviewInput struct {
	Request       ExecRequest    `json:"request"`
	Host          HostCapability `json:"host"`
	PlanStep      string         `json:"plan_step,omitempty"`
	RequestDigest string         `json:"request_digest"`
}

type AutomaticApprovalInput struct {
	Request       ExecRequest    `json:"request"`
	Host          HostCapability `json:"host"`
	UserRequest   string         `json:"user_request"`
	PlanGoal      string         `json:"plan_goal,omitempty"`
	PlanStep      string         `json:"plan_step,omitempty"`
	RequestDigest string         `json:"request_digest"`
}

type CommandExplanation struct {
	Summary   string   `json:"summary"`
	Mechanism string   `json:"mechanism"`
	Risks     []string `json:"risks"`
}

const (
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
	Limit         int
}

type Approval struct {
	ID            string         `json:"id"`
	RunID         string         `json:"run_id"`
	SessionID     string         `json:"session_id,omitempty"`
	HostID        string         `json:"host_id"`
	RequestJSON   string         `json:"request_json"`
	RequestCipher string         `json:"-"`
	RequestDigest string         `json:"request_digest"`
	Status        string         `json:"status"`
	Reason        string         `json:"reason,omitempty"`
	AIReview      *CommandReview `json:"ai_review,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	DecidedAt     time.Time      `json:"decided_at,omitempty,omitzero"`
}

type Task struct {
	ToolMeta
	ID                  string    `json:"id"`
	RunID               string    `json:"run_id"`
	HostID              string    `json:"host_id"`
	Status              string    `json:"status"`
	OperatorInstruction string    `json:"operator_instruction,omitempty"`
	StartedAt           time.Time `json:"started_at"`
	EndedAt             time.Time `json:"ended_at,omitempty,omitzero"`
}

type AuditEvent struct {
	ID        string         `json:"id"`
	RunID     string         `json:"run_id,omitempty"`
	Type      string         `json:"type"`
	Actor     string         `json:"actor"`
	Data      map[string]any `json:"data"`
	CreatedAt time.Time      `json:"created_at"`
}
