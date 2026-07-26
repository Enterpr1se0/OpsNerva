package domain

import (
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

type RiskLevel string

const (
	RiskReadOnly  RiskLevel = "read_only"
	RiskChange    RiskLevel = "change"
	RiskCritical  RiskLevel = "critical"
	RiskForbidden RiskLevel = "forbidden"
)

type DecisionAction string

const (
	ActionAllow      DecisionAction = "allow"
	ActionApprove    DecisionAction = "approval_required"
	ActionBreakGlass DecisionAction = "break_glass_required"
	ActionDeny       DecisionAction = "denied"
)

type Host struct {
	ID                  string    `json:"id"`
	Name                string    `json:"name"`
	Address             string    `json:"address"`
	Port                int       `json:"port"`
	User                string    `json:"user"`
	AuthType            string    `json:"auth_type"`
	PrivateKeyCipher    string    `json:"-"`
	HasPrivateKey       bool      `json:"has_private_key"`
	KnownHostsFile      string    `json:"known_hosts_file,omitempty"`
	ProxyJumpHostID     string    `json:"proxy_jump_host_id,omitempty"`
	ProxyURL            string    `json:"proxy_url,omitempty"`
	ProxyUsername       string    `json:"proxy_username,omitempty"`
	ProxyPasswordCipher string    `json:"-"`
	HasProxyPassword    bool      `json:"has_proxy_password"`
	PasswordCipher      string    `json:"-"`
	HasPassword         bool      `json:"has_password"`
	SudoMode            string    `json:"sudo_mode"`
	SudoCipher          string    `json:"-"`
	HasSudoPassword     bool      `json:"has_sudo_password"`
	Password            string    `json:"-"`
	SudoPassword        string    `json:"-"`
	PrivateKey          []byte    `json:"-"`
	ProxyPassword       string    `json:"-"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type HostInput struct {
	ID              string `json:"id,omitempty"`
	Name            string `json:"name"`
	Address         string `json:"address"`
	Port            int    `json:"port"`
	User            string `json:"user"`
	AuthType        string `json:"auth_type"`
	PrivateKey      string `json:"private_key,omitempty"`
	KnownHostsFile  string `json:"known_hosts_file,omitempty"`
	ProxyJumpHostID string `json:"proxy_jump_host_id,omitempty"`
	ProxyURL        string `json:"proxy_url,omitempty"`
	ProxyUsername   string `json:"proxy_username,omitempty"`
	ProxyPassword   string `json:"proxy_password,omitempty"`
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
	ID                  string    `json:"id"`
	Name                string    `json:"name"`
	Kind                string    `json:"kind"`
	BaseURL             string    `json:"base_url,omitempty"`
	Model               string    `json:"model"`
	APIKeyCipher        string    `json:"-"`
	HasAPIKey           bool      `json:"has_api_key"`
	ProxyURL            string    `json:"proxy_url,omitempty"`
	ProxyUsername       string    `json:"proxy_username,omitempty"`
	ProxyPasswordCipher string    `json:"-"`
	HasProxyPassword    bool      `json:"has_proxy_password"`
	Active              bool      `json:"active"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type ModelProviderInput struct {
	ID                 string `json:"id,omitempty"`
	Name               string `json:"name"`
	Kind               string `json:"kind"`
	BaseURL            string `json:"base_url,omitempty"`
	Model              string `json:"model"`
	APIKey             string `json:"api_key,omitempty"`
	ProxyURL           string `json:"proxy_url,omitempty"`
	ProxyUsername      string `json:"proxy_username,omitempty"`
	ProxyPassword      string `json:"proxy_password,omitempty"`
	ClearProxyPassword bool   `json:"clear_proxy_password,omitempty"`
}

type ModelDiscoveryInput struct {
	ID                 string  `json:"id,omitempty"`
	Kind               string  `json:"kind,omitempty"`
	BaseURL            *string `json:"base_url,omitempty"`
	APIKey             string  `json:"api_key,omitempty"`
	ProxyURL           *string `json:"proxy_url,omitempty"`
	ProxyUsername      *string `json:"proxy_username,omitempty"`
	ProxyPassword      string  `json:"proxy_password,omitempty"`
	ClearProxyPassword bool    `json:"clear_proxy_password,omitempty"`
}

type ModelTestInput struct {
	ID                 string  `json:"id,omitempty"`
	Kind               string  `json:"kind,omitempty"`
	BaseURL            *string `json:"base_url,omitempty"`
	Model              string  `json:"model"`
	APIKey             string  `json:"api_key,omitempty"`
	ProxyURL           *string `json:"proxy_url,omitempty"`
	ProxyUsername      *string `json:"proxy_username,omitempty"`
	ProxyPassword      string  `json:"proxy_password,omitempty"`
	ClearProxyPassword bool    `json:"clear_proxy_password,omitempty"`
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

var DefaultChatImageAllowedTypes = []string{"image/png", "image/jpeg", "image/webp", "image/gif"}

const (
	WorkspaceShellModeSandbox  = "sandbox"
	WorkspaceShellModeHost     = "host"
	WorkspaceShellModeDisabled = "disabled"
)

type SystemSettings struct {
	AgentMaxIterations          int       `json:"agent_max_iterations"`
	SystemPrompt                string    `json:"system_prompt"`
	DefaultSystemPrompt         string    `json:"default_system_prompt"`
	ApprovalExplanationsEnabled bool      `json:"approval_explanations_enabled"`
	SubagentModelProviderID     string    `json:"subagent_model_provider_id"`
	SubagentTimeoutSeconds      int       `json:"subagent_timeout_seconds"`
	ChatImageAllowedTypes       []string  `json:"chat_image_allowed_types"`
	WorkspaceShellMode          string    `json:"workspace_shell_mode"`
	WorkspaceShellPlatform      string    `json:"workspace_shell_platform,omitempty"`
	WorkspaceShellBackend       string    `json:"workspace_shell_backend,omitempty"`
	WorkspaceShellName          string    `json:"workspace_shell_name,omitempty"`
	WorkspaceSandboxAvailable   bool      `json:"workspace_sandbox_available"`
	WorkspaceHostShellAvailable bool      `json:"workspace_host_shell_available"`
	UpdatedAt                   time.Time `json:"updated_at"`
}

type SystemSettingsInput struct {
	AgentMaxIterations          int      `json:"agent_max_iterations"`
	SystemPrompt                *string  `json:"system_prompt,omitempty"`
	ApprovalExplanationsEnabled *bool    `json:"approval_explanations_enabled,omitempty"`
	SubagentModelProviderID     *string  `json:"subagent_model_provider_id,omitempty"`
	SubagentTimeoutSeconds      *int     `json:"subagent_timeout_seconds,omitempty"`
	ChatImageAllowedTypes       []string `json:"chat_image_allowed_types,omitempty"`
	WorkspaceShellMode          *string  `json:"workspace_shell_mode,omitempty"`
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
	Enabled             bool      `json:"enabled"`
	Provider            string    `json:"provider"`
	BaseURL             string    `json:"base_url"`
	APIKeyCipher        string    `json:"-"`
	HasAPIKey           bool      `json:"has_api_key"`
	ProxyURL            string    `json:"proxy_url,omitempty"`
	ProxyUsername       string    `json:"proxy_username,omitempty"`
	ProxyPasswordCipher string    `json:"-"`
	HasProxyPassword    bool      `json:"has_proxy_password"`
	TimeoutSeconds      int       `json:"timeout_seconds"`
	MaxResults          int       `json:"max_results"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type WebSearchSettingsInput struct {
	Enabled            bool   `json:"enabled"`
	BaseURL            string `json:"base_url"`
	APIKey             string `json:"api_key,omitempty"`
	ClearAPIKey        bool   `json:"clear_api_key,omitempty"`
	ProxyURL           string `json:"proxy_url,omitempty"`
	ProxyUsername      string `json:"proxy_username,omitempty"`
	ProxyPassword      string `json:"proxy_password,omitempty"`
	ClearProxyPassword bool   `json:"clear_proxy_password,omitempty"`
	TimeoutSeconds     int    `json:"timeout_seconds"`
	MaxResults         int    `json:"max_results"`
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
	ID            string       `json:"id"`
	Name          string       `json:"name"`
	Transport     MCPTransport `json:"transport"`
	Command       string       `json:"command,omitempty"`
	Args          []string     `json:"args,omitempty"`
	Cwd           string       `json:"cwd,omitempty"`
	URL           string       `json:"url,omitempty"`
	EnvKeys       []string     `json:"env_keys,omitempty"`
	HeaderKeys    []string     `json:"header_keys,omitempty"`
	SecretsCipher string       `json:"-"`
	Enabled       bool         `json:"enabled"`
	Status        string       `json:"status"`
	LastError     string       `json:"last_error,omitempty"`
	ConnectedAt   *time.Time   `json:"connected_at,omitempty"`
	ToolCount     int          `json:"tool_count"`
	Tools         []MCPTool    `json:"tools,omitempty"`
	CreatedAt     time.Time    `json:"created_at"`
	UpdatedAt     time.Time    `json:"updated_at"`
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

type WebSession struct {
	TokenHash string    `json:"-"`
	CSRFToken string    `json:"csrf_token"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
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
	Evidence  string    `json:"evidence,omitempty"`
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
	ExecRemoteRead             ExecMode = "remote_read"
	ExecRemoteSearch           ExecMode = "remote_search"
	ExecRemoteEdit             ExecMode = "remote_edit"
	ExecWorkspaceUpload        ExecMode = "workspace_upload"
	ExecWorkspaceShell         ExecMode = "workspace_shell"
	ExecSSHFileTransfer        ExecMode = "ssh_file_transfer"
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
	Change                    *FileChange         `json:"change,omitempty"`
	Cwd                       string              `json:"cwd,omitempty" jsonschema:"absolute remote working directory, or a clean workspace-relative directory for workspace_shell"`
	Env                       map[string]string   `json:"env,omitempty" jsonschema:"non-secret environment values"`
	Elevated                  bool                `json:"elevated,omitempty" jsonschema:"request root through the host sudo policy; never pass sudo or a password as a program or argument"`
	TimeoutSeconds            int                 `json:"timeout_seconds,omitempty" jsonschema:"1-600 seconds for synchronous execution"`
	Reason                    string              `json:"reason" jsonschema:"why this command is necessary"`
	ExpectedChanges           string              `json:"expected_changes,omitempty" jsonschema:"expected server changes"`
	Rollback                  string              `json:"rollback,omitempty" jsonschema:"rollback instructions for mutations"`
	RemotePath                string              `json:"remote_path,omitempty" jsonschema:"absolute remote file path for transfers"`
	SourceHostID              string              `json:"source_host_id,omitempty" jsonschema:"registered source host identifier for host-to-host transfers"`
	SourcePath                string              `json:"source_path,omitempty" jsonschema:"absolute source path for host-to-host transfers"`
	Overwrite                 bool                `json:"overwrite,omitempty" jsonschema:"replace an existing transfer destination; defaults to false"`
	ExpectedDestinationSHA256 string              `json:"expected_destination_sha256,omitempty" jsonschema:"destination SHA256 required when overwriting an existing file"`
	WorkspaceID               string              `json:"workspace_id,omitempty" jsonschema:"registered workspace identifier"`
	WorkspaceShellBackend     string              `json:"workspace_shell_backend,omitempty" jsonschema:"control-plane-selected workspace shell backend bound into approval"`
	SSHConnectionDigest       string              `json:"ssh_connection_digest,omitempty" jsonschema:"control-plane-selected SSH connection revision bound into approval"`
	SourceConnectionDigest    string              `json:"source_connection_digest,omitempty" jsonschema:"control-plane-selected source SSH connection revision bound into approval"`
	RelativePath              string              `json:"relative_path,omitempty" jsonschema:"path relative to the workspace root"`
	ExpectedSHA256            string              `json:"expected_sha256,omitempty" jsonschema:"workspace file version observed before mutation"`
	Validator                 string              `json:"validator,omitempty" jsonschema:"allowlisted validator identifier"`
	SearchPattern             string              `json:"search_pattern,omitempty" jsonschema:"file search pattern"`
	SearchMatchMode           FileSearchMatchMode `json:"search_match_mode,omitempty" jsonschema:"file search matching mode: literal or regex"`
	ContextLines              int                 `json:"context_lines,omitempty" jsonschema:"lines around each file search match"`
	MetadataOnly              bool                `json:"metadata_only,omitempty" jsonschema:"return remote file metadata without content"`
	TailLines                 int                 `json:"tail_lines,omitempty" jsonschema:"number of final remote file lines to return"`
	OffsetBytes               int64               `json:"offset_bytes,omitempty" jsonschema:"file read offset; negative values count from the end"`
	MaxBytes                  int                 `json:"max_bytes,omitempty" jsonschema:"bounded file read length"`
	LocalPath                 string              `json:"-"`
}

// SearchText returns the request's human-readable text exactly as submitted,
// without JSON string escaping, so audit search can match what an operator or
// model would type (quotes, redirections, backslashes, newlines).
func (r ExecRequest) SearchText() string {
	parts := []string{r.Program}
	parts = append(parts, r.Args...)
	parts = append(parts, r.Script, r.Cwd, r.Reason, r.ExpectedChanges, r.Rollback,
		r.RemotePath, r.SourcePath, r.RelativePath, r.SearchPattern)
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
	ToolVersion string `json:"tool_version,omitempty"`
	OK          bool   `json:"ok"`
	Code        string `json:"code,omitempty"`
	Message     string `json:"message,omitempty"`
	Retryable   bool   `json:"retryable,omitempty"`
	NextAction  string `json:"next_action,omitempty"`
}

type ToolFailure struct {
	ToolMeta
	Status string `json:"status"`
}

type ExecResult struct {
	ToolMeta
	RunID               string            `json:"run_id"`
	TaskID              string            `json:"task_id,omitempty"`
	Status              string            `json:"status"`
	Risk                RiskLevel         `json:"risk"`
	ApprovalID          string            `json:"approval_id,omitempty"`
	OperatorInstruction string            `json:"operator_instruction,omitempty"`
	ExitCode            int               `json:"exit_code,omitempty"`
	Stdout              string            `json:"stdout,omitempty"`
	Stderr              string            `json:"stderr,omitempty"`
	Duration            time.Duration     `json:"duration,omitempty"`
	PolicyHits          []string          `json:"policy_hits,omitempty"`
	CompletedAt         time.Time         `json:"completed_at,omitempty"`
	File                *FileMetadata     `json:"file,omitempty"`
	Change              *FileChange       `json:"change,omitempty"`
	Search              *FileSearchResult `json:"search,omitempty"`
}

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
}

type FileChange struct {
	Diff      string `json:"diff"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

type Decision struct {
	Risk     RiskLevel      `json:"risk"`
	Action   DecisionAction `json:"action"`
	Reason   string         `json:"reason"`
	RuleHits []string       `json:"rule_hits"`
}

type CommandReviewInput struct {
	Request       ExecRequest    `json:"request"`
	Policy        Decision       `json:"policy"`
	Host          HostCapability `json:"host"`
	PlanStep      string         `json:"plan_step,omitempty"`
	RequestDigest string         `json:"request_digest"`
}

type CommandExplanation struct {
	Summary       string   `json:"summary"`
	Mechanism     string   `json:"mechanism"`
	Effects       []string `json:"effects"`
	Risks         []string `json:"risks"`
	BeginnerTips  []string `json:"beginner_tips"`
	RollbackGuide string   `json:"rollback_guide"`
}

type CommandReview struct {
	Status            string              `json:"status"`
	Model             string              `json:"model,omitempty"`
	DeterministicRisk RiskLevel           `json:"deterministic_risk"`
	Explanation       *CommandExplanation `json:"explanation,omitempty"`
	Errors            []string            `json:"errors,omitempty"`
	ReviewedAt        time.Time           `json:"reviewed_at"`
}

type Run struct {
	ID             string         `json:"id"`
	SessionID      string         `json:"session_id,omitempty"`
	HostID         string         `json:"host_id"`
	RequestJSON    string         `json:"request_json"`
	RequestCipher  string         `json:"-"`
	SearchText     string         `json:"-"`
	RequestDigest  string         `json:"request_digest"`
	Risk           RiskLevel      `json:"risk"`
	Status         string         `json:"status"`
	ExitCode       int            `json:"exit_code"`
	StdoutRedacted string         `json:"stdout_redacted,omitempty"`
	StderrRedacted string         `json:"stderr_redacted,omitempty"`
	StdoutCipher   string         `json:"-"`
	StderrCipher   string         `json:"-"`
	Error          string         `json:"error,omitempty"`
	AIReviewJSON   string         `json:"-"`
	AIReview       *CommandReview `json:"ai_review,omitempty"`
	StartedAt      time.Time      `json:"started_at"`
	CompletedAt    time.Time      `json:"completed_at,omitempty"`
}

type Approval struct {
	ID            string         `json:"id"`
	RunID         string         `json:"run_id"`
	SessionID     string         `json:"session_id,omitempty"`
	HostID        string         `json:"host_id"`
	RequestJSON   string         `json:"request_json"`
	RequestCipher string         `json:"-"`
	RequestDigest string         `json:"request_digest"`
	Risk          RiskLevel      `json:"risk"`
	Status        string         `json:"status"`
	Reason        string         `json:"reason,omitempty"`
	AIReview      *CommandReview `json:"ai_review,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	ExpiresAt     time.Time      `json:"expires_at"`
	DecidedAt     time.Time      `json:"decided_at,omitempty"`
}

type Task struct {
	ToolMeta
	ID                  string    `json:"id"`
	RunID               string    `json:"run_id"`
	HostID              string    `json:"host_id"`
	Status              string    `json:"status"`
	OperatorInstruction string    `json:"operator_instruction,omitempty"`
	StartedAt           time.Time `json:"started_at"`
	EndedAt             time.Time `json:"ended_at,omitempty"`
}

type AuditEvent struct {
	ID        string         `json:"id"`
	RunID     string         `json:"run_id,omitempty"`
	Type      string         `json:"type"`
	Actor     string         `json:"actor"`
	Data      map[string]any `json:"data"`
	CreatedAt time.Time      `json:"created_at"`
}
