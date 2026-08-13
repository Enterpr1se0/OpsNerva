package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/service"
	"eino-ops-agent/internal/store"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
	einojsonschema "github.com/eino-contrib/jsonschema"
)

// ToolDescriptor exposes the runtime function schema to the administrator.
// InputSchema is generated from the same Eino ToolInfo used by the model;
// descriptions may be shortened for the Web catalog.
type ToolDescriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Category    string          `json:"category"`
	Guard       string          `json:"guard"`
	Enabled     bool            `json:"enabled"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type ToolCatalog struct {
	Loaded        bool             `json:"loaded"`
	Agent         string           `json:"agent"`
	Framework     string           `json:"framework"`
	ExecutionMode string           `json:"execution_mode"`
	ProviderID    string           `json:"provider_id,omitempty"`
	Model         string           `json:"model,omitempty"`
	LoadedAt      string           `json:"loaded_at,omitempty"`
	Count         int              `json:"count"`
	Total         int              `json:"total"`
	Tools         []ToolDescriptor `json:"tools"`
}

// DescribeTools reads Eino's resolved ToolInfo rather than maintaining a
// second hand-written function schema for the Web control plane.
func DescribeTools(ctx context.Context, tools []tool.BaseTool) ([]ToolDescriptor, error) {
	descriptors := make([]ToolDescriptor, 0, len(tools))
	for _, candidate := range tools {
		info, err := candidate.Info(ctx)
		if err != nil {
			return nil, err
		}
		schemaJSON := json.RawMessage(`{"type":"object","properties":{}}`)
		if info.ParamsOneOf != nil {
			inputSchema, err := info.ParamsOneOf.ToJSONSchema()
			if err != nil {
				return nil, err
			}
			if inputSchema != nil {
				schemaJSON, err = json.Marshal(inputSchema)
				if err != nil {
					return nil, err
				}
			}
		}
		descriptors = append(descriptors, ToolDescriptor{
			Name: info.Name, Description: info.Desc, Category: toolCategory(info.Name), Guard: toolGuard(info.Name), Enabled: true, InputSchema: schemaJSON,
		})
	}
	return descriptors, nil
}

func toolCategory(name string) string {
	switch {
	case isAgentTaskTool(name):
		return "planning"
	case name == "skill":
		return "skills"
	case strings.HasPrefix(name, "mcp__"):
		return "mcp"
	case strings.HasPrefix(name, "workspace_"):
		return "workspace"
	case strings.HasPrefix(name, "web_"):
		return "web"
	case strings.HasPrefix(name, "ssh_host_"):
		return "hosts"
	case name == "ssh_task" || strings.HasPrefix(name, "ssh_task_"):
		return "tasks"
	case strings.HasPrefix(name, "ssh_file_"):
		return "remote_files"
	case name == "ssh_history" || strings.HasPrefix(name, "ssh_history_"):
		return "history"
	default:
		return "execution"
	}
}

func toolGuard(name string) string {
	switch name {
	case "TaskCreate", "TaskGet", "TaskUpdate", "TaskList":
		return "agent_state"
	case "ssh_exec", "ssh_run_script":
		return "approval_required"
	case "ssh_tunnel", "ssh_shell", "ssh_file_read", "workspace_file_read", "ssh_file_edit", "ssh_file_transfer", "workspace_file_edit", "workspace_file_delete", "workspace_file_upload", "workspace_file_download", "workspace_shell":
		return "approval_required"
	case "ssh_task":
		return "audited_control"
	default:
		if strings.HasPrefix(name, "mcp__") {
			return "external_mcp"
		}
		return "read_only"
	}
}

type HostInput struct {
	HostID string `json:"host_id" jsonschema:"registered host identifier"`
}

type HostListOutput struct {
	Hosts []domain.HostCapability `json:"hosts"`
}

type ExecInput struct {
	HostID         string            `json:"host_id" jsonschema:"registered host identifier"`
	Program        string            `json:"program" jsonschema:"remote executable; keep arguments separate"`
	Args           []string          `json:"args,omitempty" jsonschema:"argument vector; no shell quoting"`
	Background     bool              `json:"background,omitempty" jsonschema:"cancellable background task; default false"`
	Cwd            string            `json:"cwd,omitempty" jsonschema:"absolute remote working directory"`
	Env            map[string]string `json:"env,omitempty" jsonschema:"non-secret environment variables"`
	Elevated       bool              `json:"elevated,omitempty" jsonschema:"managed root access; never use sudo or a password"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty" jsonschema:"1-600; default uses configured sync/background timeout"`
	MaxOutputBytes int               `json:"max_output_bytes,omitempty" jsonschema:"per-stream output limit; omit for complete output"`
	OutputView     string            `json:"output_view,omitempty" jsonschema:"with max_output_bytes: head, tail, or head_tail (default)"`
	Reason         string            `json:"reason" jsonschema:"one-sentence purpose"`
}

type ScriptInput struct {
	HostID         string            `json:"host_id" jsonschema:"registered host identifier"`
	Script         string            `json:"script" jsonschema:"complete Bash script"`
	Background     bool              `json:"background,omitempty" jsonschema:"cancellable background task; default false"`
	Cwd            string            `json:"cwd,omitempty" jsonschema:"absolute remote working directory"`
	Env            map[string]string `json:"env,omitempty" jsonschema:"non-secret environment variables"`
	Elevated       bool              `json:"elevated,omitempty" jsonschema:"managed root access; never include sudo or a password"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty" jsonschema:"1-600; default uses configured sync/background timeout"`
	MaxOutputBytes int               `json:"max_output_bytes,omitempty" jsonschema:"per-stream output limit; omit for complete output"`
	OutputView     string            `json:"output_view,omitempty" jsonschema:"with max_output_bytes: head, tail, or head_tail (default)"`
	Reason         string            `json:"reason" jsonschema:"one-sentence purpose"`
}

type SSHTunnelInput struct {
	Action     string `json:"action" jsonschema:"start, list, or stop"`
	HostID     string `json:"host_id,omitempty" jsonschema:"start: SSH host ID"`
	Direction  string `json:"direction,omitempty" jsonschema:"start: local (-L, default) or reverse (-R)"`
	LocalHost  string `json:"local_host,omitempty" jsonschema:"start: local listener bind IP for local forwarding, or client-side target host for reverse forwarding; default 127.0.0.1"`
	LocalPort  int    `json:"local_port,omitempty" jsonschema:"start: local listener port for local forwarding (0 selects one), or client-side target port for reverse forwarding"`
	RemoteHost string `json:"remote_host,omitempty" jsonschema:"start: host-side target for local forwarding, or SSH-server bind IP for reverse forwarding; default 127.0.0.1"`
	RemotePort int    `json:"remote_port,omitempty" jsonschema:"start: host-side target port for local forwarding, or SSH-server listener port for reverse forwarding (0 selects one)"`
	TunnelID   string `json:"tunnel_id,omitempty" jsonschema:"stop: tunnel ID"`
	Reason     string `json:"reason,omitempty" jsonschema:"start: one-sentence purpose"`
}

type SSHShellInput struct {
	Action         string  `json:"action" jsonschema:"start, input, output, list, interrupt, or close"`
	HostID         string  `json:"host_id,omitempty" jsonschema:"start: SSH host ID"`
	ShellID        string  `json:"shell_id,omitempty" jsonschema:"input/output/interrupt/close: shell ID"`
	Input          string  `json:"input,omitempty" jsonschema:"input: exact non-secret bytes"`
	Submit         bool    `json:"submit,omitempty" jsonschema:"input: append carriage return if no newline"`
	Cwd            string  `json:"cwd,omitempty" jsonschema:"start: absolute remote directory"`
	Elevated       bool    `json:"elevated,omitempty" jsonschema:"start: managed root shell"`
	AfterSequence  *uint64 `json:"after_sequence,omitempty" jsonschema:"output: events after next_sequence; omit for cursor, 0 to replay"`
	WaitSeconds    *int    `json:"wait_seconds,omitempty" jsonschema:"input/output: delay before read, 0-600; default 5"`
	MaxOutputBytes *int    `json:"max_output_bytes,omitempty" jsonschema:"input/output: page bytes, 4096-4194304; default 131072"`
	Reason         string  `json:"reason,omitempty" jsonschema:"audit note; required for start"`
}

func sshShellProvidedFields(input SSHShellInput) []string {
	fields := []string{"action"}
	if input.HostID != "" {
		fields = append(fields, "host_id")
	}
	if input.ShellID != "" {
		fields = append(fields, "shell_id")
	}
	if input.Input != "" {
		fields = append(fields, "input")
	}
	if input.Submit {
		fields = append(fields, "submit")
	}
	if input.Cwd != "" {
		fields = append(fields, "cwd")
	}
	if input.Elevated {
		fields = append(fields, "elevated")
	}
	if input.AfterSequence != nil {
		fields = append(fields, "after_sequence")
	}
	if input.WaitSeconds != nil {
		fields = append(fields, "wait_seconds")
	}
	if input.MaxOutputBytes != nil {
		fields = append(fields, "max_output_bytes")
	}
	if input.Reason != "" {
		fields = append(fields, "reason")
	}
	return fields
}

func validateSSHShellActionFields(input SSHShellInput, action string, allowed []string, example map[string]any) error {
	provided := sshShellProvidedFields(input)
	allowedSet := make(map[string]bool, len(allowed))
	for _, field := range allowed {
		allowedSet[field] = true
	}
	unexpected := make([]string, 0)
	for _, field := range provided {
		if !allowedSet[field] {
			unexpected = append(unexpected, field)
		}
	}
	if len(unexpected) == 0 {
		return nil
	}
	return invalidStructuredToolInput(
		fmt.Sprintf("action=%s received unsupported fields: %s", action, strings.Join(unexpected, ", ")),
		domain.ToolValidationDetails{
			Action: action, AllowedFields: allowed, GotFields: provided,
			UnexpectedFields: unexpected, Example: example,
		},
	)
}

func invalidSSHShellValue(input SSHShellInput, action, message string, allowed []string, example map[string]any) error {
	return invalidStructuredToolInput(message, domain.ToolValidationDetails{
		Action: action, AllowedFields: allowed, GotFields: sshShellProvidedFields(input), Example: example,
	})
}

type FileReadInput struct {
	HostID       string                     `json:"host_id" jsonschema:"registered host identifier"`
	Path         string                     `json:"path" jsonschema:"absolute remote file path"`
	MetadataOnly bool                       `json:"metadata_only,omitempty" jsonschema:"metadata and SHA256 only"`
	FullContent  bool                       `json:"full_content,omitempty" jsonschema:"complete file; incompatible with range/search/metadata_only"`
	MaxBytes     int                        `json:"max_bytes,omitempty" jsonschema:"page bytes; default 131072"`
	OffsetBytes  int64                      `json:"offset_bytes,omitempty" jsonschema:"byte offset; negative reads from end; incompatible with tail_lines"`
	TailLines    int                        `json:"tail_lines,omitempty" jsonschema:"final line count; incompatible with offset_bytes"`
	Pattern      string                     `json:"pattern,omitempty" jsonschema:"search pattern; requires match_mode and forbids ranges"`
	MatchMode    domain.FileSearchMatchMode `json:"match_mode,omitempty" jsonschema:"with pattern: literal or POSIX regex"`
	ContextLines int                        `json:"context_lines,omitempty" jsonschema:"search context line count"`
	Elevated     bool                       `json:"elevated,omitempty" jsonschema:"read with managed root access"`
}

type FileListInput struct {
	HostID string `json:"host_id" jsonschema:"registered host identifier"`
	Path   string `json:"path" jsonschema:"absolute remote directory path"`
}

type FileEditInput struct {
	HostID      string `json:"host_id" jsonschema:"registered host identifier"`
	Path        string `json:"path" jsonschema:"existing absolute remote file"`
	OldText     string `json:"old_text" jsonschema:"exact complete lines from latest read; must match once"`
	NewText     string `json:"new_text" jsonschema:"replacement lines; empty deletes old_text"`
	ValidatorID string `json:"validator_id,omitempty" jsonschema:"listed validator ID only; never a command"`
	Elevated    bool   `json:"elevated,omitempty" jsonschema:"edit with managed root access"`
	Reason      string `json:"reason" jsonschema:"one-sentence purpose"`
}

type SSHFileTransferInput struct {
	SourceHostID              string `json:"source_host_id" jsonschema:"registered source SSH host identifier"`
	SourcePath                string `json:"source_path" jsonschema:"absolute source file; no symlinks"`
	ExpectedSHA256            string `json:"expected_sha256" jsonschema:"source SHA256 from ssh_file_read"`
	DestinationHostID         string `json:"destination_host_id" jsonschema:"registered destination SSH host identifier"`
	DestinationPath           string `json:"destination_path" jsonschema:"absolute destination file path"`
	ExpectedDestinationSHA256 string `json:"expected_destination_sha256,omitempty" jsonschema:"omit to create; destination SHA256 to replace"`
	TimeoutSeconds            int    `json:"timeout_seconds,omitempty" jsonschema:"1-600"`
	Reason                    string `json:"reason" jsonschema:"one-sentence purpose"`
}

type WorkspacePathInput struct {
	Path string `json:"path,omitempty" jsonschema:"Workspace-relative directory; default ."`
}

type WorkspaceReadInput struct {
	Path         string                     `json:"path" jsonschema:"Workspace-relative file"`
	FullContent  bool                       `json:"full_content,omitempty" jsonschema:"complete file; incompatible with range/tail/search"`
	MaxBytes     int                        `json:"max_bytes,omitempty" jsonschema:"page bytes; default 131072"`
	OffsetBytes  int64                      `json:"offset_bytes,omitempty" jsonschema:"byte offset; negative reads from end; incompatible with tail_lines"`
	TailLines    int                        `json:"tail_lines,omitempty" jsonschema:"final line count; incompatible with offset_bytes"`
	Pattern      string                     `json:"pattern,omitempty" jsonschema:"search pattern; requires match_mode and forbids ranges"`
	MatchMode    domain.FileSearchMatchMode `json:"match_mode,omitempty" jsonschema:"with pattern: literal or POSIX regex"`
	ContextLines int                        `json:"context_lines,omitempty" jsonschema:"search context line count"`
}

type WorkspaceFileEditInput struct {
	Path        string `json:"path" jsonschema:"existing Workspace-relative file"`
	OldText     string `json:"old_text" jsonschema:"exact complete lines from latest read; must match once"`
	NewText     string `json:"new_text" jsonschema:"replacement lines; empty deletes old_text"`
	ValidatorID string `json:"validator_id,omitempty" jsonschema:"listed Workspace validator ID only; never a command"`
	Reason      string `json:"reason" jsonschema:"one-sentence purpose"`
}

type WorkspaceFileDeleteInput struct {
	Path      string `json:"path" jsonschema:"existing Workspace-relative path; not root"`
	Recursive bool   `json:"recursive,omitempty" jsonschema:"required only for non-empty directories"`
	Reason    string `json:"reason" jsonschema:"one-sentence deletion purpose"`
}

func fileSearchSchemaOption() toolutils.Option {
	return toolutils.WithSchemaModifier(func(jsonTagName string, _ reflect.Type, _ reflect.StructTag, schema *einojsonschema.Schema) {
		if jsonTagName == "match_mode" {
			schema.Enum = []any{string(domain.FileSearchLiteral), string(domain.FileSearchRegex)}
		}
		if jsonTagName == "query_scope" {
			schema.Enum = []any{"all", "request", "output"}
		}
		if jsonTagName == "output_view" {
			schema.Enum = []any{"head", "tail", "head_tail"}
		}
	})
}

type WorkspaceUploadInput struct {
	HostID         string `json:"host_id" jsonschema:"destination SSH host ID"`
	Path           string `json:"path" jsonschema:"Workspace-relative source file"`
	ExpectedSHA256 string `json:"expected_sha256" jsonschema:"source SHA256 from workspace_file_read"`
	RemotePath     string `json:"remote_path" jsonschema:"absolute remote destination"`
	Reason         string `json:"reason" jsonschema:"one-sentence purpose"`
}

type WorkspaceDownloadInput struct {
	HostID         string `json:"host_id" jsonschema:"source SSH host ID"`
	RemotePath     string `json:"remote_path" jsonschema:"absolute remote source file; no symlinks"`
	ExpectedSHA256 string `json:"expected_sha256" jsonschema:"source SHA256 from ssh_file_read"`
	Path           string `json:"path" jsonschema:"new Workspace-relative destination"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"1-600"`
	Reason         string `json:"reason" jsonschema:"one-sentence purpose"`
}

type WorkspaceShellInput struct {
	Action         string            `json:"action" jsonschema:"run, start, input, output, list, interrupt, or close"`
	Script         string            `json:"script,omitempty" jsonschema:"run: complete non-interactive Bash or PowerShell script"`
	ShellID        string            `json:"shell_id,omitempty" jsonschema:"input/output/interrupt/close: shell ID"`
	Input          string            `json:"input,omitempty" jsonschema:"input: exact non-secret bytes"`
	Submit         bool              `json:"submit,omitempty" jsonschema:"input: append carriage return if no newline"`
	Cwd            string            `json:"cwd,omitempty" jsonschema:"run/start: Workspace-relative directory; default root"`
	Env            map[string]string `json:"env,omitempty" jsonschema:"run/start: non-secret environment variables"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty" jsonschema:"run: 1-600"`
	AfterSequence  *uint64           `json:"after_sequence,omitempty" jsonschema:"output: events after next_sequence; omit for cursor, 0 to replay"`
	WaitSeconds    *int              `json:"wait_seconds,omitempty" jsonschema:"input/output: delay before read, 0-600; default 5"`
	MaxOutputBytes *int              `json:"max_output_bytes,omitempty" jsonschema:"input/output: page bytes, 4096-4194304; default 131072"`
	Reason         string            `json:"reason,omitempty" jsonschema:"audit note; required for run/start"`
}

func workspaceShellProvidedFields(input WorkspaceShellInput) []string {
	fields := []string{"action"}
	if input.Script != "" {
		fields = append(fields, "script")
	}
	if input.ShellID != "" {
		fields = append(fields, "shell_id")
	}
	if input.Input != "" {
		fields = append(fields, "input")
	}
	if input.Submit {
		fields = append(fields, "submit")
	}
	if input.Cwd != "" {
		fields = append(fields, "cwd")
	}
	if len(input.Env) != 0 {
		fields = append(fields, "env")
	}
	if input.TimeoutSeconds != 0 {
		fields = append(fields, "timeout_seconds")
	}
	if input.AfterSequence != nil {
		fields = append(fields, "after_sequence")
	}
	if input.WaitSeconds != nil {
		fields = append(fields, "wait_seconds")
	}
	if input.MaxOutputBytes != nil {
		fields = append(fields, "max_output_bytes")
	}
	if input.Reason != "" {
		fields = append(fields, "reason")
	}
	return fields
}

func validateWorkspaceShellActionFields(input WorkspaceShellInput, action string, allowed []string, example map[string]any) error {
	provided := workspaceShellProvidedFields(input)
	allowedSet := make(map[string]bool, len(allowed))
	for _, field := range allowed {
		allowedSet[field] = true
	}
	unexpected := make([]string, 0)
	for _, field := range provided {
		if !allowedSet[field] {
			unexpected = append(unexpected, field)
		}
	}
	if len(unexpected) == 0 {
		return nil
	}
	return invalidStructuredToolInput(
		fmt.Sprintf("action=%s received unsupported fields: %s", action, strings.Join(unexpected, ", ")),
		domain.ToolValidationDetails{Action: action, AllowedFields: allowed, GotFields: provided, UnexpectedFields: unexpected, Example: example},
	)
}

func invalidWorkspaceShellValue(input WorkspaceShellInput, action, message string, allowed []string, example map[string]any) error {
	return invalidStructuredToolInput(message, domain.ToolValidationDetails{
		Action: action, AllowedFields: allowed, GotFields: workspaceShellProvidedFields(input), Example: example,
	})
}

type HistorySearchInput struct {
	RunID         string                     `json:"run_id,omitempty" jsonschema:"exact run ID; combine with query for bounded excerpts"`
	Query         string                     `json:"query,omitempty" jsonschema:"list search, or bounded matching excerpts when run_id is set"`
	MatchMode     domain.FileSearchMatchMode `json:"match_mode,omitempty" jsonschema:"literal (default) or regex"`
	QueryScope    string                     `json:"query_scope,omitempty" jsonschema:"all (default), request, or output"`
	HostID        string                     `json:"host_id,omitempty" jsonschema:"SSH host ID"`
	ToolName      string                     `json:"tool_name,omitempty" jsonschema:"exact tool name"`
	Status        string                     `json:"status,omitempty" jsonschema:"exact run status"`
	StartedAfter  string                     `json:"started_after,omitempty" jsonschema:"inclusive RFC3339 lower bound"`
	StartedBefore string                     `json:"started_before,omitempty" jsonschema:"inclusive RFC3339 upper bound"`
	Limit         int                        `json:"limit,omitempty" jsonschema:"list results, or run_id query matches per stream; default 20, maximum 100"`
	Cursor        string                     `json:"cursor,omitempty" jsonschema:"search continuation cursor from next_cursor"`
	AfterStdout   int                        `json:"after_stdout_bytes,omitempty" jsonschema:"run_id detail: stdout byte offset"`
	AfterStderr   int                        `json:"after_stderr_bytes,omitempty" jsonschema:"run_id detail: stderr byte offset"`
	MaxOutput     int                        `json:"max_output_bytes,omitempty" jsonschema:"run_id detail/excerpts: per-stream bytes, 1024-65536, default 16384"`
	OutputView    string                     `json:"output_view,omitempty" jsonschema:"run_id detail: head, tail, or head_tail (default)"`
}

type WebSearchInput struct {
	Query           string   `json:"query" jsonschema:"public query; no private data or secrets"`
	MaxResults      int      `json:"max_results,omitempty" jsonschema:"result count; default 5, maximum is configured"`
	Topic           string   `json:"topic,omitempty" jsonschema:"general (default), news, or finance"`
	SearchDepth     string   `json:"search_depth,omitempty" jsonschema:"basic (default), advanced, fast, or ultra-fast; advanced costs more credits"`
	TimeRange       string   `json:"time_range,omitempty" jsonschema:"day, week, month, or year"`
	StartDate       string   `json:"start_date,omitempty" jsonschema:"inclusive YYYY-MM-DD lower bound; do not combine with time_range"`
	EndDate         string   `json:"end_date,omitempty" jsonschema:"inclusive YYYY-MM-DD upper bound; do not combine with time_range"`
	ChunksPerSource int      `json:"chunks_per_source,omitempty" jsonschema:"1-3 relevant chunks per source; only with search_depth=advanced"`
	IncludeDomains  []string `json:"include_domains,omitempty" jsonschema:"public domains to include; no schemes or paths"`
	ExcludeDomains  []string `json:"exclude_domains,omitempty" jsonschema:"public domains to exclude; no schemes or paths"`
}

type WebExtractInput struct {
	URLs            []string `json:"urls" jsonschema:"1-5 public HTTP(S) URLs; no private data or addresses"`
	Query           string   `json:"query,omitempty" jsonschema:"focus extraction on passages relevant to this query"`
	ExtractDepth    string   `json:"extract_depth,omitempty" jsonschema:"basic (default) or advanced; advanced costs more credits"`
	ChunksPerSource int      `json:"chunks_per_source,omitempty" jsonschema:"1-5 relevant chunks per URL; requires query"`
}

type HistoryRunSummary struct {
	ID          string    `json:"id"`
	HostID      string    `json:"host_id"`
	ToolName    string    `json:"tool_name,omitempty"`
	Mode        string    `json:"mode,omitempty"`
	Operation   string    `json:"operation"`
	Status      string    `json:"status"`
	ExitCode    int       `json:"exit_code"`
	DurationMS  int64     `json:"duration_ms,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at,omitempty,omitzero"`
}

type HistoryRunDetail struct {
	HistoryRunSummary
	ToolArguments      any    `json:"tool_arguments,omitempty"`
	Request            any    `json:"request"`
	Stdout             string `json:"stdout_redacted,omitempty"`
	Stderr             string `json:"stderr_redacted,omitempty"`
	Error              string `json:"error,omitempty"`
	OutputView         string `json:"output_view,omitempty"`
	OutputLimited      bool   `json:"output_limited,omitempty"`
	StdoutTotalBytes   int    `json:"stdout_total_bytes,omitempty"`
	StderrTotalBytes   int    `json:"stderr_total_bytes,omitempty"`
	StdoutOmittedBytes int    `json:"stdout_omitted_bytes,omitempty"`
	StderrOmittedBytes int    `json:"stderr_omitted_bytes,omitempty"`
	StdoutOffsetBytes  int    `json:"stdout_offset_bytes,omitempty"`
	StderrOffsetBytes  int    `json:"stderr_offset_bytes,omitempty"`
	StdoutNextOffset   int    `json:"stdout_next_offset_bytes,omitempty"`
	StderrNextOffset   int    `json:"stderr_next_offset_bytes,omitempty"`
	ErrorLimited       bool   `json:"error_limited,omitempty"`
	ErrorTotalBytes    int    `json:"error_total_bytes,omitempty"`
}

type HistoryOutput struct {
	Runs        *[]HistoryRunSummary `json:"runs,omitempty"`
	Run         *HistoryRunDetail    `json:"run,omitempty"`
	Match       *HistoryRunMatch     `json:"match,omitempty"`
	HasMore     bool                 `json:"has_more,omitempty"`
	NextCursor  string               `json:"next_cursor,omitempty"`
	ScanLimited bool                 `json:"scan_limited,omitempty"`
}

type HistoryRunMatch struct {
	HistoryRunSummary
	MatchMode            domain.FileSearchMatchMode `json:"match_mode"`
	QueryScope           string                     `json:"query_scope"`
	Found                bool                       `json:"found"`
	RequestMatched       bool                       `json:"request_matched,omitempty"`
	ToolArgumentsMatched bool                       `json:"tool_arguments_matched,omitempty"`
	StdoutExcerpt        string                     `json:"stdout_excerpt,omitempty"`
	StderrExcerpt        string                     `json:"stderr_excerpt,omitempty"`
	OutputLimited        bool                       `json:"output_limited,omitempty"`
	MatchLimit           int                        `json:"match_limit"`
}

const (
	defaultHistorySearchLimit = 20
	maxHistorySearchLimit     = 100
	defaultHistoryOutputBytes = 16 << 10
	maxHistoryOutputBytes     = 64 << 10
	maxHistoryStructuredBytes = 8 << 10
	maxHistoryErrorBytes      = 4 << 10
	maxHistoryRegexBytes      = 512
	maxHistoryQueryBytes      = 4 << 10
	historyRegexScanLimit     = 2000
	maxHistoryOperationBytes  = 512
)

type historySearchCursor struct {
	StartedAt string `json:"started_at"`
	ID        string `json:"id"`
}

func encodeHistoryCursor(startedAt time.Time, id string) string {
	if startedAt.IsZero() || id == "" {
		return ""
	}
	encoded, _ := json.Marshal(historySearchCursor{StartedAt: startedAt.UTC().Format(time.RFC3339Nano), ID: id})
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func decodeHistoryCursor(value string) (time.Time, string, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return time.Time{}, "", fmt.Errorf("invalid history cursor: %w", err)
	}
	var cursor historySearchCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil {
		return time.Time{}, "", fmt.Errorf("invalid history cursor: %w", err)
	}
	startedAt, err := time.Parse(time.RFC3339Nano, cursor.StartedAt)
	if err != nil || strings.TrimSpace(cursor.ID) == "" {
		return time.Time{}, "", fmt.Errorf("invalid history cursor boundary")
	}
	return startedAt.UTC(), strings.TrimSpace(cursor.ID), nil
}

func normalizeHistoryMatch(query string, matchMode domain.FileSearchMatchMode, queryScope string) (domain.FileSearchMatchMode, string, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		if matchMode != "" || strings.TrimSpace(queryScope) != "" {
			return "", "", fmt.Errorf("invalid history input: query is required with match_mode or query_scope")
		}
		return domain.FileSearchLiteral, "all", nil
	}
	if len(query) > maxHistoryQueryBytes {
		return "", "", fmt.Errorf("invalid history input: query must not exceed %d bytes", maxHistoryQueryBytes)
	}
	if matchMode == "" {
		matchMode = domain.FileSearchLiteral
	}
	if matchMode != domain.FileSearchLiteral && matchMode != domain.FileSearchRegex {
		return "", "", fmt.Errorf("invalid history input: match_mode must be literal or regex")
	}
	if matchMode == domain.FileSearchRegex && len(query) > maxHistoryRegexBytes {
		return "", "", fmt.Errorf("invalid history input: regex query must not exceed %d bytes", maxHistoryRegexBytes)
	}
	queryScope = strings.TrimSpace(queryScope)
	if queryScope == "" {
		queryScope = "all"
	}
	if queryScope != "all" && queryScope != "request" && queryScope != "output" {
		return "", "", fmt.Errorf("invalid history input: query_scope must be all, request, or output")
	}
	return matchMode, queryScope, nil
}

func compileHistoryMatcher(query string, matchMode domain.FileSearchMatchMode) (*regexp.Regexp, error) {
	if matchMode == domain.FileSearchRegex {
		expression, err := regexp.CompilePOSIX(query)
		if err != nil {
			return nil, fmt.Errorf("invalid POSIX history regex: %w", err)
		}
		return expression, nil
	}
	return regexp.Compile(regexp.QuoteMeta(query))
}

func historyMatchWindow(value string, matchStart, matchEnd, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	windowStart := matchStart - maxBytes/3
	if windowStart < 0 {
		windowStart = 0
	}
	windowEnd := windowStart + maxBytes
	if windowEnd < matchEnd {
		windowEnd = matchEnd
		windowStart = max(0, windowEnd-maxBytes)
	}
	if windowEnd > len(value) {
		windowEnd = len(value)
		windowStart = max(0, windowEnd-maxBytes)
	}
	for windowStart < len(value) && !utf8.RuneStart(value[windowStart]) {
		windowStart++
	}
	for windowEnd > windowStart && windowEnd < len(value) && !utf8.RuneStart(value[windowEnd]) {
		windowEnd--
	}
	prefix, suffix := "", ""
	if windowStart > 0 {
		prefix = "..."
	}
	if windowEnd < len(value) {
		suffix = "..."
	}
	return prefix + value[windowStart:windowEnd] + suffix
}

func historyMatchExcerpt(value string, expression *regexp.Regexp, maxBytes, maxMatches int) (string, bool, bool) {
	if value == "" {
		return "", false, false
	}
	matches := expression.FindAllStringIndex(value, maxMatches+1)
	if len(matches) == 0 {
		return "", false, false
	}
	var result strings.Builder
	limited := len(matches) > maxMatches
	if limited {
		matches = matches[:maxMatches]
	}
	lastStart, lastEnd := -1, -1
	lastIncludedWholeLine := false
	for _, match := range matches {
		matchStart, matchEnd := match[0], match[1]
		lineStart := strings.LastIndex(value[:matchStart], "\n") + 1
		lineEnd := len(value)
		if newline := strings.IndexByte(value[matchEnd:], '\n'); newline >= 0 {
			lineEnd = matchEnd + newline
		}
		if lineStart != lastStart || lineEnd != lastEnd || !lastIncludedWholeLine {
			remaining := maxBytes - result.Len()
			separator := ""
			if result.Len() > 0 {
				separator = "\n"
				remaining--
			}
			if remaining <= 0 {
				limited = true
				break
			}
			excerpt := historyMatchWindow(value[lineStart:lineEnd], matchStart-lineStart, matchEnd-lineStart, remaining)
			if len(excerpt) > remaining {
				excerpt = validUTF8Prefix(excerpt, remaining)
				limited = true
			}
			result.WriteString(separator)
			result.WriteString(excerpt)
			lastStart, lastEnd = lineStart, lineEnd
			lastIncludedWholeLine = lineEnd-lineStart <= remaining
		}
	}
	return result.String(), true, limited
}

func historyRunMatches(run domain.Run, query string, matchMode domain.FileSearchMatchMode, queryScope string, maxBytes, matchLimit int) (HistoryRunMatch, error) {
	expression, err := compileHistoryMatcher(query, matchMode)
	if err != nil {
		return HistoryRunMatch{}, err
	}
	result := HistoryRunMatch{HistoryRunSummary: historyRunSummary(run), MatchMode: matchMode, QueryScope: queryScope, MatchLimit: matchLimit}
	if queryScope != "output" {
		var request domain.ExecRequest
		_ = json.Unmarshal([]byte(run.RequestJSON), &request)
		result.RequestMatched = expression.MatchString(run.RequestJSON) || expression.MatchString(request.SearchText())
		result.ToolArgumentsMatched = expression.MatchString(run.ToolArgumentsJSON)
	}
	if queryScope != "request" {
		stdout, stdoutFound, stdoutLimited := historyMatchExcerpt(run.StdoutRedacted, expression, maxBytes, matchLimit)
		stderr, stderrFound, stderrLimited := historyMatchExcerpt(run.StderrRedacted, expression, maxBytes, matchLimit)
		result.StdoutExcerpt, result.StderrExcerpt = stdout, stderr
		result.OutputLimited = stdoutLimited || stderrLimited
		result.Found = stdoutFound || stderrFound
	}
	result.Found = result.Found || result.RequestMatched || result.ToolArgumentsMatched
	return result, nil
}

func historyJSON(raw string) any {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	if len(raw) > maxHistoryStructuredBytes {
		return map[string]any{
			"output_limited": true,
			"original_bytes": len(raw),
			"preview":        selectOutputView(raw, maxHistoryStructuredBytes, "head_tail"),
		}
	}
	var value any
	if json.Unmarshal([]byte(raw), &value) == nil {
		return value
	}
	return raw
}

func historyOperation(run domain.Run, request domain.ExecRequest) string {
	switch request.Mode {
	case domain.ExecProgram:
		return strings.Join(append([]string{request.Program}, request.Args...), " ")
	case domain.ExecScript, domain.ExecWorkspaceShell:
		if strings.TrimSpace(request.Reason) != "" {
			return request.Reason
		}
	case domain.ExecRemoteRead:
		return "read " + request.RemotePath
	case domain.ExecRemoteSearch:
		return "search " + request.RemotePath
	case domain.ExecRemoteEdit:
		return "edit " + request.RemotePath
	case domain.ExecWorkspaceRead:
		return "read " + request.WorkspaceID + ":" + request.RelativePath
	case domain.ExecWorkspaceSearch:
		return "search " + request.WorkspaceID + ":" + request.RelativePath
	case domain.ExecWorkspaceEdit:
		return "edit " + request.WorkspaceID + ":" + request.RelativePath
	case domain.ExecWorkspaceDelete:
		return "delete " + request.WorkspaceID + ":" + request.RelativePath
	case domain.ExecWorkspaceDirectoryList:
		return "list " + request.WorkspaceID + ":" + request.RelativePath
	case domain.ExecWorkspaceUpload:
		return request.WorkspaceID + ":" + request.RelativePath + " -> " + request.RemotePath
	case domain.ExecWorkspaceDownload:
		return request.RemotePath + " -> " + request.WorkspaceID + ":" + request.RelativePath
	case domain.ExecSSHFileTransfer:
		return request.SourceHostID + ":" + request.SourcePath + " -> " + request.HostID + ":" + request.RemotePath
	case domain.ExecSSHTunnelStart:
		localHost := request.TunnelLocalHost
		if localHost == "" {
			localHost = "127.0.0.1"
		}
		if request.TunnelDirection == domain.SSHTunnelDirectionReverse {
			return fmt.Sprintf("%s:%d <- %s:%d", localHost, request.TunnelLocalPort, request.TunnelRemoteHost, request.TunnelRemotePort)
		}
		return fmt.Sprintf("%s:%d -> %s:%d", localHost, request.TunnelLocalPort, request.TunnelRemoteHost, request.TunnelRemotePort)
	case domain.ExecSSHShellStart, domain.ExecWorkspaceShellStart:
		return string(request.Mode)
	}
	if request.Mode != "" {
		return string(request.Mode)
	}
	if run.ToolName != "" {
		return run.ToolName
	}
	return "execution"
}

func historyRunSummary(run domain.Run) HistoryRunSummary {
	var request domain.ExecRequest
	_ = json.Unmarshal([]byte(run.RequestJSON), &request)
	duration := int64(0)
	if !run.CompletedAt.IsZero() && !run.StartedAt.IsZero() && !run.CompletedAt.Before(run.StartedAt) {
		duration = run.CompletedAt.Sub(run.StartedAt).Milliseconds()
	}
	operation := historyOperation(run, request)
	if len(operation) > maxHistoryOperationBytes {
		operation = validUTF8Prefix(operation, maxHistoryOperationBytes-3) + "..."
	}
	return HistoryRunSummary{
		ID: run.ID, HostID: run.HostID, ToolName: run.ToolName, Mode: string(request.Mode),
		Operation: operation, Status: run.Status, ExitCode: run.ExitCode,
		DurationMS: duration, StartedAt: run.StartedAt, CompletedAt: run.CompletedAt,
	}
}

func historyRunDetail(run domain.Run, stdoutOffset, stderrOffset, maxBytes int, view string) (HistoryRunDetail, error) {
	selected, err := selectExecResultOutput(domain.ExecResult{Stdout: run.StdoutRedacted, Stderr: run.StderrRedacted},
		stdoutOffset, stderrOffset, maxBytes, view, true)
	if err != nil {
		return HistoryRunDetail{}, err
	}
	errorText := run.Error
	errorLimited := false
	if len(errorText) > maxHistoryErrorBytes {
		errorText = selectOutputView(errorText, maxHistoryErrorBytes, "head_tail")
		errorLimited = true
	}
	detail := HistoryRunDetail{
		HistoryRunSummary: historyRunSummary(run), ToolArguments: historyJSON(run.ToolArgumentsJSON),
		Request: historyJSON(run.RequestJSON), Stdout: selected.Stdout, Stderr: selected.Stderr, Error: errorText,
		OutputView: selected.OutputView, OutputLimited: selected.OutputLimited,
		StdoutTotalBytes: selected.StdoutTotalBytes, StderrTotalBytes: selected.StderrTotalBytes,
		StdoutOmittedBytes: selected.StdoutOmittedBytes, StderrOmittedBytes: selected.StderrOmittedBytes,
		StdoutOffsetBytes: selected.StdoutOffsetBytes, StderrOffsetBytes: selected.StderrOffsetBytes,
		ErrorLimited: errorLimited,
	}
	if errorLimited {
		detail.ErrorTotalBytes = len(run.Error)
	}
	if selected.OutputView == "head" {
		if next := stdoutOffset + len(selected.Stdout); next < selected.StdoutTotalBytes {
			detail.StdoutNextOffset = next
		}
		if next := stderrOffset + len(selected.Stderr); next < selected.StderrTotalBytes {
			detail.StderrNextOffset = next
		}
	}
	return detail, nil
}

func historyRunSummaries(runs []domain.Run) []HistoryRunSummary {
	result := make([]HistoryRunSummary, len(runs))
	for index, run := range runs {
		result[index] = historyRunSummary(run)
	}
	return result
}

type TaskInput struct {
	TaskID           string `json:"task_id" jsonschema:"long-running task identifier"`
	Action           string `json:"action" jsonschema:"status or cancel"`
	WaitSeconds      int    `json:"wait_seconds,omitempty" jsonschema:"status: wait 0-60; deadline leaves task running"`
	BlockUntil       string `json:"block_until,omitempty" jsonschema:"status wait condition: terminal (default) or output"`
	AfterStdoutBytes int    `json:"after_stdout_bytes,omitempty" jsonschema:"status: stdout offset from prior stdout_total_bytes"`
	AfterStderrBytes int    `json:"after_stderr_bytes,omitempty" jsonschema:"status: stderr offset from prior stderr_total_bytes"`
	MaxOutputBytes   int    `json:"max_output_bytes,omitempty" jsonschema:"status: per-stream output limit"`
	OutputView       string `json:"output_view,omitempty" jsonschema:"with max_output_bytes: head, tail, or head_tail (default)"`
}

const maxToolOutputViewBytes = 4 << 20

const defaultShellToolOutputBytes = 128 << 10

func shellToolOutputPolicy(waitSeconds, maxOutputBytes *int) (time.Duration, int, error) {
	wait := domain.DefaultShellQueryDelaySeconds
	if waitSeconds != nil {
		wait = *waitSeconds
	}
	if wait < 0 || wait > domain.MaxShellQueryDelaySeconds {
		return 0, 0, fmt.Errorf("wait_seconds must be between 0 and %d", domain.MaxShellQueryDelaySeconds)
	}
	maxBytes := defaultShellToolOutputBytes
	if maxOutputBytes != nil {
		maxBytes = *maxOutputBytes
	}
	if maxBytes < 4<<10 || maxBytes > maxToolOutputViewBytes {
		return 0, 0, fmt.Errorf("max_output_bytes must be between 4096 and %d", maxToolOutputViewBytes)
	}
	return time.Duration(wait) * time.Second, maxBytes, nil
}

func normalizedOutputView(maxBytes int, view string) (string, error) {
	view = strings.ToLower(strings.TrimSpace(view))
	if maxBytes < 0 || maxBytes > maxToolOutputViewBytes {
		return "", fmt.Errorf("max_output_bytes must be between 0 and %d", maxToolOutputViewBytes)
	}
	if maxBytes == 0 {
		if view != "" {
			return "", fmt.Errorf("output_view requires max_output_bytes")
		}
		return "", nil
	}
	if view == "" {
		return "head_tail", nil
	}
	switch view {
	case "head", "tail", "head_tail":
		return view, nil
	default:
		return "", fmt.Errorf("output_view must be head, tail, or head_tail")
	}
}

func validUTF8Prefix(value string, limit int) string {
	if limit >= len(value) {
		return value
	}
	end := limit
	for end > 0 && end < len(value) && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}

func validUTF8Suffix(value string, limit int) string {
	if limit >= len(value) {
		return value
	}
	start := len(value) - limit
	for start < len(value) && !utf8.RuneStart(value[start]) {
		start++
	}
	return value[start:]
}

func selectOutputView(value string, maxBytes int, view string) string {
	if maxBytes == 0 || len(value) <= maxBytes {
		return value
	}
	switch view {
	case "head":
		return validUTF8Prefix(value, maxBytes)
	case "tail":
		return validUTF8Suffix(value, maxBytes)
	default:
		headBytes := maxBytes / 2
		return validUTF8Prefix(value, headBytes) + validUTF8Suffix(value, maxBytes-headBytes)
	}
}

func selectExecResultOutput(result domain.ExecResult, stdoutOffset, stderrOffset, maxBytes int, view string, reportTotals bool) (domain.ExecResult, error) {
	view, err := normalizedOutputView(maxBytes, view)
	if err != nil {
		return result, err
	}
	stdoutTotal, stderrTotal := len(result.Stdout), len(result.Stderr)
	if stdoutOffset < 0 || stdoutOffset > stdoutTotal || stderrOffset < 0 || stderrOffset > stderrTotal {
		return result, fmt.Errorf("output byte offsets must be non-negative and no greater than the current stream totals")
	}
	stdout, stderr := result.Stdout[stdoutOffset:], result.Stderr[stderrOffset:]
	limitedStdout := selectOutputView(stdout, maxBytes, view)
	limitedStderr := selectOutputView(stderr, maxBytes, view)
	result.Stdout, result.Stderr = limitedStdout, limitedStderr
	result.StdoutOffsetBytes, result.StderrOffsetBytes = stdoutOffset, stderrOffset
	result.StdoutOmittedBytes = len(stdout) - len(limitedStdout)
	result.StderrOmittedBytes = len(stderr) - len(limitedStderr)
	result.OutputLimited = result.StdoutOmittedBytes > 0 || result.StderrOmittedBytes > 0
	if reportTotals || maxBytes > 0 || stdoutOffset > 0 || stderrOffset > 0 {
		result.StdoutTotalBytes, result.StderrTotalBytes = stdoutTotal, stderrTotal
	}
	if maxBytes > 0 {
		result.OutputView = view
	}
	return result, nil
}

// ExecToolResult is the model-facing projection of the complete execution
// record. Audit and timing details remain in the Run and UI-only
// display metadata instead of being repeated in every Tool result.
type ExecToolResult struct {
	Status              string                        `json:"status"`
	RunID               string                        `json:"run_id,omitempty"`
	TaskID              string                        `json:"task_id,omitempty"`
	AutoApproved        bool                          `json:"auto_approved,omitempty"`
	ApprovalID          string                        `json:"approval_id,omitempty"`
	OperatorInstruction string                        `json:"operator_instruction,omitempty"`
	ExitCode            int                           `json:"exit_code,omitempty"`
	Stdout              string                        `json:"stdout,omitempty"`
	Stderr              string                        `json:"stderr,omitempty"`
	OutputView          string                        `json:"output_view,omitempty"`
	OutputLimited       bool                          `json:"output_limited,omitempty"`
	StdoutTotalBytes    int                           `json:"stdout_total_bytes,omitempty"`
	StderrTotalBytes    int                           `json:"stderr_total_bytes,omitempty"`
	StdoutOmittedBytes  int                           `json:"stdout_omitted_bytes,omitempty"`
	StderrOmittedBytes  int                           `json:"stderr_omitted_bytes,omitempty"`
	StdoutOffsetBytes   int                           `json:"stdout_offset_bytes,omitempty"`
	StderrOffsetBytes   int                           `json:"stderr_offset_bytes,omitempty"`
	WaitDeadlineReached bool                          `json:"wait_deadline_reached,omitempty"`
	File                *domain.FileMetadata          `json:"file,omitempty"`
	Change              *domain.FileChange            `json:"change,omitempty"`
	Search              *domain.FileSearchResult      `json:"search,omitempty"`
	Tunnel              *domain.SSHTunnel             `json:"tunnel,omitempty"`
	Shell               *domain.SSHShell              `json:"shell,omitempty"`
	ShellUsage          *domain.SSHShellUsage         `json:"shell_usage,omitempty"`
	Code                string                        `json:"code,omitempty"`
	Message             string                        `json:"message,omitempty"`
	Retryable           bool                          `json:"retryable,omitempty"`
	Validation          *domain.ToolValidationDetails `json:"validation,omitempty"`
}

func normalizeToolStatus(meta *domain.ToolMeta, status string) {
	*meta = domain.ToolMeta{}
	switch status {
	case "completed", "running", "partial", "approval_required", "cancelled":
		meta.OK = true
	}
}

func normalizeExecResult(result domain.ExecResult, err error) (domain.ExecResult, error) {
	if err == nil {
		normalizeToolStatus(&result.ToolMeta, result.Status)
		return result, nil
	}
	if errors.Is(err, context.Canceled) {
		return result, err
	}
	result.OK = false
	result.Message = err.Error()
	var validationErr *toolInputValidationError
	if errors.As(err, &validationErr) {
		result.Code = "validation_failed"
		result.Validation = validationErr.validation
		if result.Status == "" {
			result.Status = "failed"
		}
		return result, nil
	}
	result.Code, result.Retryable, _ = classifyToolError(err)
	if result.Status == "" {
		result.Status = "failed"
	}
	return result, nil
}

func compactExecToolResult(result domain.ExecResult) ExecToolResult {
	compact := ExecToolResult{
		Status: result.Status, RunID: result.RunID, TaskID: result.TaskID,
		AutoApproved: result.AutoApproved,
		ApprovalID:   result.ApprovalID, OperatorInstruction: result.OperatorInstruction,
		ExitCode: result.ExitCode, Stdout: result.Stdout, Stderr: result.Stderr,
		OutputView: result.OutputView, OutputLimited: result.OutputLimited,
		StdoutTotalBytes: result.StdoutTotalBytes, StderrTotalBytes: result.StderrTotalBytes,
		StdoutOmittedBytes: result.StdoutOmittedBytes, StderrOmittedBytes: result.StderrOmittedBytes,
		StdoutOffsetBytes: result.StdoutOffsetBytes, StderrOffsetBytes: result.StderrOffsetBytes,
		WaitDeadlineReached: result.WaitDeadlineReached,
		File:                result.File, Change: result.Change, Search: result.Search,
		Tunnel: result.Tunnel, Shell: result.Shell, ShellUsage: result.ShellUsage,
	}
	if result.Code != "" {
		compact.Code = result.Code
		compact.Message = result.Message
		compact.Retryable = result.Retryable
		compact.Validation = result.Validation
	}
	return compact
}

func CompactExecToolResult(result domain.ExecResult, err error) (ExecToolResult, error) {
	normalized, normalizedErr := normalizeExecResult(result, err)
	return compactExecToolResult(normalized), normalizedErr
}

func normalizeTaskResult(task domain.Task, result domain.ExecResult, taskErr string, err error) (domain.ExecResult, error) {
	result.TaskID = task.ID
	if result.RunID == "" {
		result.RunID = task.RunID
	}
	if task.Status != "" {
		result.Status = task.Status
	}
	if result.OperatorInstruction == "" {
		result.OperatorInstruction = task.OperatorInstruction
	}
	if result.CompletedAt.IsZero() && !task.EndedAt.IsZero() {
		result.CompletedAt = task.EndedAt
	}
	result, normalizedErr := normalizeExecResult(result, err)
	if normalizedErr != nil {
		return result, normalizedErr
	}
	if taskErr != "" {
		result.OK = false
		result.Message = taskErr
		result.Code = "remote_failed"
	}
	return result, nil
}

func RunTaskTool(ctx context.Context, svc *service.Service, input TaskInput, actor string) (ExecToolResult, error) {
	action := strings.ToLower(strings.TrimSpace(input.Action))
	if strings.TrimSpace(input.TaskID) == "" {
		return CompactExecToolResult(domain.ExecResult{}, invalidToolInput("task_id is required"))
	}
	if _, err := normalizedOutputView(input.MaxOutputBytes, input.OutputView); err != nil {
		return CompactExecToolResult(domain.ExecResult{TaskID: input.TaskID}, invalidToolInput("%s", err.Error()))
	}
	if input.WaitSeconds < 0 || input.WaitSeconds > 60 || input.AfterStdoutBytes < 0 || input.AfterStderrBytes < 0 {
		return CompactExecToolResult(domain.ExecResult{TaskID: input.TaskID}, invalidToolInput("wait_seconds must be between 0 and 60 and output byte offsets must be non-negative"))
	}
	blockUntil := strings.ToLower(strings.TrimSpace(input.BlockUntil))
	if input.WaitSeconds == 0 && blockUntil != "" {
		return CompactExecToolResult(domain.ExecResult{TaskID: input.TaskID}, invalidToolInput("block_until requires wait_seconds"))
	}
	if input.WaitSeconds > 0 && blockUntil == "" {
		blockUntil = "terminal"
	}
	if blockUntil != "" && blockUntil != "terminal" && blockUntil != "output" {
		return CompactExecToolResult(domain.ExecResult{TaskID: input.TaskID}, invalidToolInput("block_until must be terminal or output"))
	}
	switch action {
	case "status":
		task, result, taskErr, waitDeadlineReached, err := svc.WaitTask(ctx, input.TaskID, input.AfterStdoutBytes, input.AfterStderrBytes, time.Duration(input.WaitSeconds)*time.Second, blockUntil)
		if task.ID == "" {
			task.ID = input.TaskID
		}
		result, err = normalizeTaskResult(task, result, taskErr, err)
		if err != nil {
			return compactExecToolResult(result), err
		}
		result.WaitDeadlineReached = waitDeadlineReached
		selected, selectErr := selectExecResultOutput(result, input.AfterStdoutBytes, input.AfterStderrBytes, input.MaxOutputBytes, input.OutputView, true)
		if selectErr != nil {
			return CompactExecToolResult(domain.ExecResult{TaskID: input.TaskID}, invalidToolInput("%s", selectErr.Error()))
		}
		return compactExecToolResult(selected), nil
	case "cancel":
		if input.WaitSeconds != 0 || input.BlockUntil != "" || input.AfterStdoutBytes != 0 || input.AfterStderrBytes != 0 || input.MaxOutputBytes != 0 || input.OutputView != "" {
			return CompactExecToolResult(domain.ExecResult{TaskID: input.TaskID}, invalidToolInput("action=cancel accepts only action and task_id"))
		}
		cancelErr := svc.CancelTask(input.TaskID, actor)
		task, result, taskErr, getErr := svc.GetTask(input.TaskID)
		if task.ID == "" {
			task.ID = input.TaskID
		}
		if cancelErr != nil {
			normalized, normalizedErr := normalizeTaskResult(task, result, taskErr, cancelErr)
			return compactExecToolResult(normalized), normalizedErr
		}
		normalized, normalizedErr := normalizeTaskResult(task, result, taskErr, getErr)
		return compactExecToolResult(normalized), normalizedErr
	default:
		return CompactExecToolResult(domain.ExecResult{TaskID: input.TaskID}, fmt.Errorf("invalid task action: use status or cancel"))
	}
}

const defaultFileReadBytes = 128 << 10

func RunFileReadTool(ctx context.Context, svc *service.Service, input FileReadInput, actor string) (ExecToolResult, error) {
	searching := input.Pattern != ""
	if searching && (input.MetadataOnly || input.FullContent || input.MaxBytes != 0 || input.OffsetBytes != 0 || input.TailLines != 0) {
		return CompactExecToolResult(domain.ExecResult{}, fmt.Errorf("invalid file read input: pattern cannot be combined with metadata_only, full_content, max_bytes, offset_bytes, or tail_lines"))
	}
	if searching && input.MatchMode == "" {
		return CompactExecToolResult(domain.ExecResult{}, fmt.Errorf("invalid file read input: match_mode is required with pattern"))
	}
	if !searching && (input.MatchMode != "" || input.ContextLines != 0) {
		return CompactExecToolResult(domain.ExecResult{}, fmt.Errorf("invalid file read input: match_mode and context_lines require pattern"))
	}
	if searching {
		result, err := svc.SearchFile(ctx, input.HostID, input.Path, input.Pattern, input.MatchMode, input.ContextLines, input.Elevated, actor)
		return CompactExecToolResult(result, err)
	}
	if input.MetadataOnly && (input.FullContent || input.MaxBytes != 0 || input.OffsetBytes != 0 || input.TailLines != 0) {
		return CompactExecToolResult(domain.ExecResult{}, fmt.Errorf("invalid file read input: metadata_only cannot be combined with full_content, max_bytes, offset_bytes, or tail_lines"))
	}
	if input.FullContent && (input.MaxBytes != 0 || input.OffsetBytes != 0 || input.TailLines != 0) {
		return CompactExecToolResult(domain.ExecResult{}, fmt.Errorf("invalid file read input: full_content cannot be combined with max_bytes, offset_bytes, or tail_lines"))
	}
	if !input.MetadataOnly && !input.FullContent && input.MaxBytes == 0 && input.TailLines == 0 {
		input.MaxBytes = defaultFileReadBytes
	}
	result, err := svc.ReadFileAdvanced(ctx, input.HostID, input.Path, input.MetadataOnly, input.MaxBytes, input.OffsetBytes, input.TailLines, input.Elevated, actor)
	return CompactExecToolResult(result, err)
}

func RunWorkspaceFileReadTool(ctx context.Context, svc *service.Service, input WorkspaceReadInput, actor string) (ExecToolResult, error) {
	workspace, err := svc.SessionWorkspace(ctx)
	if err != nil {
		return CompactExecToolResult(domain.ExecResult{}, err)
	}
	searching := input.Pattern != ""
	if searching && (input.FullContent || input.MaxBytes != 0 || input.OffsetBytes != 0 || input.TailLines != 0) {
		return CompactExecToolResult(domain.ExecResult{}, fmt.Errorf("invalid Workspace file read input: pattern cannot be combined with full_content, max_bytes, offset_bytes, or tail_lines"))
	}
	if searching && input.MatchMode == "" {
		return CompactExecToolResult(domain.ExecResult{}, fmt.Errorf("invalid Workspace file read input: match_mode is required with pattern"))
	}
	if !searching && (input.MatchMode != "" || input.ContextLines != 0) {
		return CompactExecToolResult(domain.ExecResult{}, fmt.Errorf("invalid Workspace file read input: match_mode and context_lines require pattern"))
	}
	if searching {
		result, err := svc.SearchWorkspace(ctx, workspace.ID, input.Path, input.Pattern, input.MatchMode, input.ContextLines, actor)
		return CompactExecToolResult(result, err)
	}
	if input.FullContent && (input.MaxBytes != 0 || input.OffsetBytes != 0 || input.TailLines != 0) {
		return CompactExecToolResult(domain.ExecResult{}, fmt.Errorf("invalid Workspace file read input: full_content cannot be combined with max_bytes, offset_bytes, or tail_lines"))
	}
	if input.MaxBytes < 0 || input.TailLines < 0 || (input.OffsetBytes != 0 && input.TailLines != 0) {
		return CompactExecToolResult(domain.ExecResult{}, fmt.Errorf("invalid Workspace file read range: max_bytes and tail_lines must be non-negative; tail_lines cannot be combined with offset_bytes"))
	}
	if !input.FullContent && input.MaxBytes == 0 && input.TailLines == 0 {
		input.MaxBytes = defaultFileReadBytes
	}
	result, err := svc.ReadWorkspaceFileAdvanced(ctx, workspace.ID, input.Path, input.MaxBytes, input.OffsetBytes, input.TailLines, actor)
	return CompactExecToolResult(result, err)
}

func ReadHistoryTool(ctx context.Context, svc *service.Service, input HistorySearchInput) (HistoryOutput, error) {
	runID := strings.TrimSpace(input.RunID)
	if runID != "" {
		if input.HostID != "" || input.ToolName != "" || input.Status != "" || input.StartedAfter != "" || input.StartedBefore != "" || input.Cursor != "" {
			return HistoryOutput{}, fmt.Errorf("invalid history input: run_id cannot be combined with list filters or cursor")
		}
		if input.AfterStdout < 0 || input.AfterStderr < 0 {
			return HistoryOutput{}, fmt.Errorf("invalid history input: output byte offsets must be non-negative")
		}
		maxOutput := input.MaxOutput
		if maxOutput == 0 {
			maxOutput = defaultHistoryOutputBytes
		}
		if maxOutput < 1024 || maxOutput > maxHistoryOutputBytes {
			return HistoryOutput{}, fmt.Errorf("invalid history input: max_output_bytes must be between 1024 and %d", maxHistoryOutputBytes)
		}
		matchMode, queryScope, err := normalizeHistoryMatch(input.Query, input.MatchMode, input.QueryScope)
		if err != nil {
			return HistoryOutput{}, err
		}
		if strings.TrimSpace(input.Query) != "" {
			if input.AfterStdout != 0 || input.AfterStderr != 0 || input.OutputView != "" {
				return HistoryOutput{}, fmt.Errorf("invalid history input: run_id query cannot be combined with output offsets or output_view")
			}
			matchLimit := input.Limit
			if matchLimit == 0 {
				matchLimit = defaultHistorySearchLimit
			}
			if matchLimit < 1 || matchLimit > maxHistorySearchLimit {
				return HistoryOutput{}, fmt.Errorf("invalid history input: limit must be between 1 and %d", maxHistorySearchLimit)
			}
			result, err := svc.GetRun(ctx, runID, false)
			if err != nil {
				return HistoryOutput{}, err
			}
			matched, err := historyRunMatches(result.Run, input.Query, matchMode, queryScope, maxOutput, matchLimit)
			if err != nil {
				return HistoryOutput{}, err
			}
			return HistoryOutput{Match: &matched}, nil
		}
		if input.Limit != 0 {
			return HistoryOutput{}, fmt.Errorf("invalid history input: limit requires query when run_id is set")
		}
		outputView := strings.ToLower(strings.TrimSpace(input.OutputView))
		if outputView == "" && (input.AfterStdout > 0 || input.AfterStderr > 0) {
			outputView = "head"
		}
		if (input.AfterStdout > 0 || input.AfterStderr > 0) && outputView != "head" {
			return HistoryOutput{}, fmt.Errorf("invalid history input: output byte offsets require output_view=head")
		}
		result, err := svc.GetRun(ctx, runID, false)
		if err != nil {
			return HistoryOutput{}, err
		}
		detail, err := historyRunDetail(result.Run, input.AfterStdout, input.AfterStderr, maxOutput, outputView)
		if err != nil {
			return HistoryOutput{}, fmt.Errorf("invalid history input: %w", err)
		}
		return HistoryOutput{Run: &detail}, nil
	}
	if input.AfterStdout != 0 || input.AfterStderr != 0 || input.MaxOutput != 0 || input.OutputView != "" {
		return HistoryOutput{}, fmt.Errorf("invalid history input: output paging fields require run_id")
	}
	limit := input.Limit
	if limit == 0 {
		limit = defaultHistorySearchLimit
	}
	if limit < 1 || limit > maxHistorySearchLimit {
		return HistoryOutput{}, fmt.Errorf("invalid history input: limit must be between 1 and %d", maxHistorySearchLimit)
	}
	matchMode, queryScope, err := normalizeHistoryMatch(input.Query, input.MatchMode, input.QueryScope)
	if err != nil {
		return HistoryOutput{}, err
	}
	parseBound := func(name, value string) (time.Time, error) {
		if strings.TrimSpace(value) == "" {
			return time.Time{}, nil
		}
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid history input: %s must be RFC3339: %w", name, err)
		}
		return parsed.UTC(), nil
	}
	startedAfter, err := parseBound("started_after", input.StartedAfter)
	if err != nil {
		return HistoryOutput{}, err
	}
	startedBefore, err := parseBound("started_before", input.StartedBefore)
	if err != nil {
		return HistoryOutput{}, err
	}
	if !startedAfter.IsZero() && !startedBefore.IsZero() && startedAfter.After(startedBefore) {
		return HistoryOutput{}, fmt.Errorf("invalid history input: started_after must not be later than started_before")
	}
	cursorStarted, cursorID, err := decodeHistoryCursor(input.Cursor)
	if err != nil {
		return HistoryOutput{}, err
	}
	page, err := svc.SearchRunSummariesMatchingPage(ctx, domain.RunSearchFilter{
		Query: input.Query, QueryScope: queryScope, HostID: strings.TrimSpace(input.HostID),
		ToolName: strings.TrimSpace(input.ToolName), Status: strings.TrimSpace(input.Status), StartedAfter: startedAfter,
		StartedBefore: startedBefore, CursorStarted: cursorStarted, CursorID: cursorID,
		Limit: limit, ScanLimit: historyRegexScanLimit,
	}, matchMode)
	if err != nil {
		return HistoryOutput{}, err
	}
	summaries := historyRunSummaries(page.Runs)
	return HistoryOutput{Runs: &summaries, HasMore: page.HasMore,
		NextCursor: encodeHistoryCursor(page.NextStartedAt, page.NextID), ScanLimited: page.ScanLimited}, nil
}

func taskStartToolResult(svc *service.Service, task domain.Task, startErr error) (domain.ExecResult, error) {
	if task.ID == "" {
		return normalizeTaskResult(task, domain.ExecResult{}, "", startErr)
	}
	storedTask, result, taskErr, getErr := svc.GetTask(task.ID)
	if getErr == nil {
		task = storedTask
	} else if startErr == nil {
		startErr = getErr
	}
	return normalizeTaskResult(task, result, taskErr, startErr)
}

func RunExecutionTool(ctx context.Context, svc *service.Service, request domain.ExecRequest, actor string) (ExecToolResult, error) {
	if _, err := normalizedOutputView(request.MaxOutputBytes, request.OutputView); err != nil {
		return CompactExecToolResult(domain.ExecResult{}, invalidToolInput("%s", err.Error()))
	}
	var result domain.ExecResult
	var err error
	if !request.Background {
		result, err = svc.Submit(ctx, request, actor)
	} else {
		var task domain.Task
		task, err = svc.StartTask(ctx, request, actor)
		result, err = taskStartToolResult(svc, task, err)
	}
	if !request.Background {
		result, err = normalizeExecResult(result, err)
	}
	selected, selectErr := selectExecResultOutput(result, 0, 0, request.MaxOutputBytes, request.OutputView, false)
	if selectErr != nil {
		return CompactExecToolResult(domain.ExecResult{}, invalidToolInput("%s", selectErr.Error()))
	}
	return compactExecToolResult(selected), err
}

func RunSSHTunnelTool(ctx context.Context, svc *service.Service, input SSHTunnelInput, actor string) (any, error) {
	switch strings.ToLower(strings.TrimSpace(input.Action)) {
	case "start":
		if input.TunnelID != "" {
			return normalizeValueToolResult(ctx, "ssh_tunnel", domain.SSHTunnel{}, invalidToolInput("tunnel_id is only valid with action=stop"))
		}
		result, err := svc.StartSSHTunnel(ctx, input.HostID, domain.SSHTunnelConfig{
			Direction: domain.SSHTunnelDirection(input.Direction), LocalHost: input.LocalHost, LocalPort: input.LocalPort,
			RemoteHost: input.RemoteHost, RemotePort: input.RemotePort,
		}, input.Reason, actor)
		return CompactExecToolResult(result, err)
	case "list":
		if input.HostID != "" || input.Direction != "" || input.LocalHost != "" || input.LocalPort != 0 || input.RemoteHost != "" || input.RemotePort != 0 || input.TunnelID != "" {
			return normalizeValueToolResult(ctx, "ssh_tunnel", domain.SSHTunnelList{}, invalidToolInput("action=list does not accept host_id, direction, local_host, local_port, remote_host, remote_port, or tunnel_id"))
		}
		return svc.ListSSHTunnels(), nil
	case "stop":
		if input.HostID != "" || input.Direction != "" || input.LocalHost != "" || input.LocalPort != 0 || input.RemoteHost != "" || input.RemotePort != 0 {
			return normalizeValueToolResult(ctx, "ssh_tunnel", domain.SSHTunnel{}, invalidToolInput("action=stop accepts tunnel_id and optional reason only"))
		}
		tunnel, err := svc.StopSSHTunnel(ctx, input.TunnelID, actor)
		return normalizeValueToolResult(ctx, "ssh_tunnel", tunnel, err)
	default:
		return normalizeValueToolResult(ctx, "ssh_tunnel", domain.SSHTunnel{}, invalidToolInput("invalid action: use start, list, or stop"))
	}
}

func readableShellSnapshot(ctx context.Context, svc *service.Service, snapshot domain.SSHShellSnapshot, after uint64) domain.SSHShellSnapshot {
	readable, err := svc.ReadableSSHShellSnapshot(ctx, snapshot, after)
	if err == nil {
		return readable
	}
	// Never expose raw terminal escapes to the model when the readable replay
	// cannot be produced. recent_output remains available as a bounded snapshot.
	for index := range snapshot.Events {
		if snapshot.Events[index].Stream == "stdout" || snapshot.Events[index].Stream == "stderr" {
			snapshot.Events[index].Content = ""
		}
	}
	return snapshot
}

type shellToolOutputChunk struct {
	FirstSequence uint64 `json:"first_sequence,omitempty"`
	Sequence      uint64 `json:"sequence"`
	Stream        string `json:"stream"`
	Content       string `json:"content"`
}

// shellToolResult is the model-facing incremental view of an interactive shell.
type shellToolResult struct {
	ShellID           string                 `json:"shell_id"`
	HostID            string                 `json:"host_id,omitempty"`
	HostName          string                 `json:"host_name,omitempty"`
	WorkspaceID       string                 `json:"workspace_id,omitempty"`
	Status            string                 `json:"status"`
	Chunks            []shellToolOutputChunk `json:"chunks,omitempty"`
	NextSequence      uint64                 `json:"next_sequence"`
	OutputBytes       int                    `json:"output_bytes,omitempty"`
	HasMore           bool                   `json:"has_more,omitempty"`
	ExitCode          *int                   `json:"exit_code,omitempty"`
	TerminationReason string                 `json:"termination_reason,omitempty"`
	Error             string                 `json:"error,omitempty"`
}

func modelShellResult(ctx context.Context, svc *service.Service, page domain.SSHShellOutputPage, after uint64, stripInputEcho bool) shellToolResult {
	readable := readableShellSnapshot(ctx, svc, page.Snapshot, after)
	shell := readable.Shell
	chunks := modelShellChunks(readable.Events, stripInputEcho)
	outputBytes := 0
	for _, chunk := range chunks {
		outputBytes += len(chunk.Content)
	}
	return shellToolResult{
		ShellID: shell.ID, HostID: shell.HostID, HostName: shell.HostName,
		WorkspaceID: shell.WorkspaceID, Status: shell.Status,
		Chunks: chunks, NextSequence: readable.NextSequence, OutputBytes: outputBytes,
		HasMore: page.HasMore, ExitCode: shell.ExitCode,
		TerminationReason: shell.TerminationReason, Error: shell.Error,
	}
}

func modelShellChunks(events []domain.SSHShellEvent, stripInputEcho bool) []shellToolOutputChunk {
	start := 0
	input := ""
	if stripInputEcho {
		for index, event := range events {
			if event.Stream == "input" && event.Source == "agent" {
				start = index + 1
				input = event.Content
			}
		}
	}
	chunks := make([]shellToolOutputChunk, 0)
	for _, event := range events[start:] {
		if (event.Stream != "stdout" && event.Stream != "stderr") || event.Content == "" {
			continue
		}
		content := normalizeShellNewlines(event.Content)
		if len(chunks) > 0 && chunks[len(chunks)-1].Stream == event.Stream {
			chunks[len(chunks)-1].Content += content
			chunks[len(chunks)-1].Sequence = event.Sequence
			continue
		}
		chunks = append(chunks, shellToolOutputChunk{
			FirstSequence: event.Sequence, Sequence: event.Sequence, Stream: event.Stream, Content: content,
		})
	}
	var combined strings.Builder
	for _, chunk := range chunks {
		combined.WriteString(chunk.Content)
	}
	cleaned := cleanModelShellResponse(combined.String(), input)
	removeBytes := combined.Len() - len(cleaned)
	for removeBytes > 0 && len(chunks) > 0 {
		if removeBytes >= len(chunks[0].Content) {
			removeBytes -= len(chunks[0].Content)
			chunks = chunks[1:]
			continue
		}
		chunks[0].Content = chunks[0].Content[removeBytes:]
		removeBytes = 0
	}
	for len(chunks) > 0 && chunks[0].Content == "" {
		chunks = chunks[1:]
	}
	for index := range chunks {
		if chunks[index].FirstSequence == chunks[index].Sequence {
			chunks[index].FirstSequence = 0
		}
	}
	return chunks
}

func modelShellOutput(events []domain.SSHShellEvent) string {
	start := 0
	input := ""
	for index, event := range events {
		if event.Stream == "input" && event.Source == "agent" {
			start = index + 1
			input = event.Content
		}
	}
	var output strings.Builder
	for _, event := range events[start:] {
		if event.Stream == "stdout" || event.Stream == "stderr" {
			output.WriteString(event.Content)
		}
	}
	return cleanModelShellResponse(output.String(), input)
}

// cleanModelShellResponse removes the terminal driver's echo of the input line
// from the model-facing result. Raw PTY events remain unchanged for the Web
// terminal. ConPTY may prefix the echoed command with the PowerShell prompt,
// while Unix PTYs usually echo only the command itself.
func cleanModelShellResponse(output, input string) string {
	output = normalizeShellNewlines(output)
	command := strings.TrimRight(normalizeShellNewlines(input), "\n")
	if output == "" || command == "" || strings.Contains(command, "\n") {
		return output
	}
	lineEnd := strings.IndexByte(output, '\n')
	firstLine := output
	remainder := ""
	if lineEnd >= 0 {
		firstLine = output[:lineEnd]
		remainder = output[lineEnd+1:]
	}
	if firstLine == command || shellPromptEcho(firstLine, command) {
		// PTYs commonly emit an additional carriage return after disabling
		// bracketed paste, and ConPTY commonly emits a blank line after echo.
		return strings.TrimPrefix(remainder, "\n")
	}
	return output
}

func normalizeShellNewlines(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	return strings.ReplaceAll(value, "\r", "\n")
}

func shellPromptEcho(line, command string) bool {
	if !strings.HasSuffix(line, command) {
		return false
	}
	prefix := strings.TrimSpace(strings.TrimSuffix(line, command))
	if prefix == "" {
		return true
	}
	return strings.HasSuffix(prefix, ">") || strings.HasSuffix(prefix, "$") ||
		strings.HasSuffix(prefix, "#") || strings.HasSuffix(prefix, "%")
}

func shellSnapshotAfter(snapshot domain.SSHShellSnapshot) uint64 {
	if len(snapshot.Events) == 0 {
		return snapshot.NextSequence
	}
	sequence := snapshot.Events[0].Sequence
	if snapshot.Events[0].FirstSequence != 0 {
		sequence = snapshot.Events[0].FirstSequence
	}
	if sequence == 0 {
		return 0
	}
	return sequence - 1
}

func RunSSHShellTool(ctx context.Context, svc *service.Service, input SSHShellInput, actor string) (any, error) {
	action := strings.ToLower(strings.TrimSpace(input.Action))
	sessionID := service.SessionIDFromContext(ctx)
	if sessionID == "" {
		return normalizeValueToolResult(ctx, "ssh_shell", domain.SSHShell{}, invalidToolInput("ssh_shell requires an Agent or MCP session"))
	}
	switch action {
	case "start":
		allowed := []string{"action", "host_id", "cwd", "elevated", "reason"}
		example := map[string]any{"action": "start", "host_id": "host_xxx", "reason": "open an interactive diagnostic shell"}
		if err := validateSSHShellActionFields(input, action, allowed, example); err != nil {
			return normalizeValueToolResult(ctx, "ssh_shell", domain.ExecResult{}, err)
		}
		if strings.TrimSpace(input.HostID) == "" || strings.TrimSpace(input.Reason) == "" {
			return normalizeValueToolResult(ctx, "ssh_shell", domain.ExecResult{}, invalidSSHShellValue(input, action, "action=start requires host_id and reason", allowed, example))
		}
		if len(input.Reason) > 500 {
			return normalizeValueToolResult(ctx, "ssh_shell", domain.ExecResult{}, invalidSSHShellValue(input, action, "reason must not exceed 500 bytes", allowed, example))
		}
		result, err := svc.StartSSHShell(ctx, input.HostID, input.Cwd, input.Elevated, 120, 32, input.Reason, actor)
		return CompactExecToolResult(result, err)
	case "input":
		allowed := []string{"action", "shell_id", "input", "submit", "wait_seconds", "max_output_bytes", "reason"}
		example := map[string]any{"action": "input", "shell_id": "shell_xxx", "input": "whoami", "submit": true}
		if err := validateSSHShellActionFields(input, action, allowed, example); err != nil {
			return normalizeValueToolResult(ctx, "ssh_shell", domain.SSHShellSnapshot{}, err)
		}
		if strings.TrimSpace(input.ShellID) == "" || input.Input == "" {
			return normalizeValueToolResult(ctx, "ssh_shell", domain.SSHShellSnapshot{}, invalidSSHShellValue(input, action, "action=input requires shell_id and input", allowed, example))
		}
		shellInput := input.Input
		if input.Submit && !strings.HasSuffix(shellInput, "\r") && !strings.HasSuffix(shellInput, "\n") {
			shellInput += "\r"
		}
		if len(shellInput) > 64<<10 || len(input.Reason) > 500 {
			return normalizeValueToolResult(ctx, "ssh_shell", domain.SSHShellSnapshot{}, invalidSSHShellValue(input, action, "input must not exceed 65536 bytes and reason must not exceed 500 bytes", allowed, example))
		}
		queryDelay, maxBytes, policyErr := shellToolOutputPolicy(input.WaitSeconds, input.MaxOutputBytes)
		if policyErr != nil {
			return normalizeValueToolResult(ctx, "ssh_shell", domain.SSHShellSnapshot{}, invalidSSHShellValue(input, action, policyErr.Error(), allowed, example))
		}
		page, err := svc.WriteSSHShellPage(ctx, input.ShellID, sessionID, shellInput, queryDelay, maxBytes, input.Reason, actor)
		return normalizeValueToolResult(ctx, "ssh_shell", modelShellResult(ctx, svc, page, shellSnapshotAfter(page.Snapshot), true), err)
	case "output":
		allowed := []string{"action", "shell_id", "after_sequence", "wait_seconds", "max_output_bytes", "reason"}
		example := map[string]any{"action": "output", "shell_id": "shell_xxx", "wait_seconds": 10}
		if err := validateSSHShellActionFields(input, action, allowed, example); err != nil {
			return normalizeValueToolResult(ctx, "ssh_shell", domain.SSHShellSnapshot{}, err)
		}
		if strings.TrimSpace(input.ShellID) == "" {
			return normalizeValueToolResult(ctx, "ssh_shell", domain.SSHShellSnapshot{}, invalidSSHShellValue(input, action, "action=output requires shell_id", allowed, example))
		}
		if len(input.Reason) > 500 {
			return normalizeValueToolResult(ctx, "ssh_shell", domain.SSHShellSnapshot{}, invalidSSHShellValue(input, action, "reason must not exceed 500 bytes", allowed, example))
		}
		queryDelay, maxBytes, policyErr := shellToolOutputPolicy(input.WaitSeconds, input.MaxOutputBytes)
		if policyErr != nil {
			return normalizeValueToolResult(ctx, "ssh_shell", domain.SSHShellSnapshot{}, invalidSSHShellValue(input, action, policyErr.Error(), allowed, example))
		}
		page, err := svc.QuerySSHShellOutput(ctx, input.ShellID, sessionID, input.AfterSequence, queryDelay, maxBytes, input.Reason, actor)
		return normalizeValueToolResult(ctx, "ssh_shell", modelShellResult(ctx, svc, page, shellSnapshotAfter(page.Snapshot), false), err)
	case "list":
		allowed := []string{"action", "reason"}
		example := map[string]any{"action": "list"}
		if err := validateSSHShellActionFields(input, action, allowed, example); err != nil {
			return normalizeValueToolResult(ctx, "ssh_shell", domain.SSHShellList{}, err)
		}
		if len(input.Reason) > 500 {
			return normalizeValueToolResult(ctx, "ssh_shell", domain.SSHShellList{}, invalidSSHShellValue(input, action, "reason must not exceed 500 bytes", allowed, example))
		}
		result, err := svc.ListSSHShells(ctx, sessionID, true, input.Reason, actor)
		return normalizeValueToolResult(ctx, "ssh_shell", result, err)
	case "interrupt":
		allowed := []string{"action", "shell_id", "reason"}
		example := map[string]any{"action": "interrupt", "shell_id": "shell_xxx"}
		if err := validateSSHShellActionFields(input, action, allowed, example); err != nil {
			return normalizeValueToolResult(ctx, "ssh_shell", domain.SSHShell{}, err)
		}
		if strings.TrimSpace(input.ShellID) == "" {
			return normalizeValueToolResult(ctx, "ssh_shell", domain.SSHShell{}, invalidSSHShellValue(input, action, "action=interrupt requires shell_id", allowed, example))
		}
		if len(input.Reason) > 500 {
			return normalizeValueToolResult(ctx, "ssh_shell", domain.SSHShell{}, invalidSSHShellValue(input, action, "reason must not exceed 500 bytes", allowed, example))
		}
		result, err := svc.InterruptSSHShell(ctx, input.ShellID, sessionID, input.Reason, actor)
		return normalizeValueToolResult(ctx, "ssh_shell", result, err)
	case "close":
		allowed := []string{"action", "shell_id", "reason"}
		example := map[string]any{"action": "close", "shell_id": "shell_xxx"}
		if err := validateSSHShellActionFields(input, action, allowed, example); err != nil {
			return normalizeValueToolResult(ctx, "ssh_shell", domain.SSHShell{}, err)
		}
		if strings.TrimSpace(input.ShellID) == "" {
			return normalizeValueToolResult(ctx, "ssh_shell", domain.SSHShell{}, invalidSSHShellValue(input, action, "action=close requires shell_id", allowed, example))
		}
		if len(input.Reason) > 500 {
			return normalizeValueToolResult(ctx, "ssh_shell", domain.SSHShell{}, invalidSSHShellValue(input, action, "reason must not exceed 500 bytes", allowed, example))
		}
		result, err := svc.CloseSSHShell(ctx, input.ShellID, sessionID, input.Reason, actor)
		return normalizeValueToolResult(ctx, "ssh_shell", result, err)
	default:
		return normalizeValueToolResult(ctx, "ssh_shell", domain.SSHShell{}, invalidStructuredToolInput(
			"invalid action: use start, input, output, list, interrupt, or close",
			domain.ToolValidationDetails{
				Action: action, AllowedFields: []string{"action"}, GotFields: sshShellProvidedFields(input),
				Example: map[string]any{"action": "list"},
			},
		))
	}
}

func RunWorkspaceShellTool(ctx context.Context, svc *service.Service, input WorkspaceShellInput, actor string) (any, error) {
	action := strings.ToLower(strings.TrimSpace(input.Action))
	sessionID := service.SessionIDFromContext(ctx)
	if sessionID == "" {
		return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShell{}, invalidToolInput("workspace_shell requires an Agent conversation"))
	}
	workspace, err := svc.SessionWorkspace(ctx)
	if err != nil {
		return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShell{}, err)
	}
	switch action {
	case "run":
		allowed := []string{"action", "script", "cwd", "env", "timeout_seconds", "reason"}
		example := map[string]any{"action": "run", "script": "go test ./...", "reason": "run the project tests"}
		if err := validateWorkspaceShellActionFields(input, action, allowed, example); err != nil {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.ExecResult{}, err)
		}
		if strings.TrimSpace(input.Script) == "" || strings.TrimSpace(input.Reason) == "" {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.ExecResult{}, invalidWorkspaceShellValue(input, action, "action=run requires script and reason", allowed, example))
		}
		result, err := svc.RunWorkspaceShell(ctx, workspace.ID, input.Script, input.Cwd, input.Env, input.TimeoutSeconds, input.Reason, actor)
		return CompactExecToolResult(result, err)
	case "start":
		allowed := []string{"action", "cwd", "env", "reason"}
		example := map[string]any{"action": "start", "reason": "open an interactive project shell"}
		if err := validateWorkspaceShellActionFields(input, action, allowed, example); err != nil {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.ExecResult{}, err)
		}
		if strings.TrimSpace(input.Reason) == "" {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.ExecResult{}, invalidWorkspaceShellValue(input, action, "action=start requires reason", allowed, example))
		}
		result, err := svc.StartWorkspaceShell(ctx, workspace.ID, input.Cwd, input.Env, 120, 32, input.Reason, actor)
		return CompactExecToolResult(result, err)
	case "input":
		allowed := []string{"action", "shell_id", "input", "submit", "wait_seconds", "max_output_bytes", "reason"}
		example := map[string]any{"action": "input", "shell_id": "shell_xxx", "input": "go test ./...", "submit": true}
		if err := validateWorkspaceShellActionFields(input, action, allowed, example); err != nil {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShellSnapshot{}, err)
		}
		if strings.TrimSpace(input.ShellID) == "" || input.Input == "" {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShellSnapshot{}, invalidWorkspaceShellValue(input, action, "action=input requires shell_id and input", allowed, example))
		}
		shellInput := input.Input
		if input.Submit && !strings.HasSuffix(shellInput, "\r") && !strings.HasSuffix(shellInput, "\n") {
			shellInput += "\r"
		}
		if len(shellInput) > 64<<10 || len(input.Reason) > 500 {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShellSnapshot{}, invalidWorkspaceShellValue(input, action, "input must not exceed 65536 bytes and reason must not exceed 500 bytes", allowed, example))
		}
		queryDelay, maxBytes, policyErr := shellToolOutputPolicy(input.WaitSeconds, input.MaxOutputBytes)
		if policyErr != nil {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShellSnapshot{}, invalidWorkspaceShellValue(input, action, policyErr.Error(), allowed, example))
		}
		page, err := svc.WriteWorkspaceShellPage(ctx, input.ShellID, sessionID, workspace.ID, shellInput, queryDelay, maxBytes, input.Reason, actor)
		return normalizeValueToolResult(ctx, "workspace_shell", modelShellResult(ctx, svc, page, shellSnapshotAfter(page.Snapshot), true), err)
	case "output":
		allowed := []string{"action", "shell_id", "after_sequence", "wait_seconds", "max_output_bytes", "reason"}
		example := map[string]any{"action": "output", "shell_id": "shell_xxx", "wait_seconds": 10}
		if err := validateWorkspaceShellActionFields(input, action, allowed, example); err != nil {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShellSnapshot{}, err)
		}
		if strings.TrimSpace(input.ShellID) == "" {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShellSnapshot{}, invalidWorkspaceShellValue(input, action, "action=output requires shell_id", allowed, example))
		}
		if len(input.Reason) > 500 {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShellSnapshot{}, invalidWorkspaceShellValue(input, action, "reason must not exceed 500 bytes", allowed, example))
		}
		queryDelay, maxBytes, policyErr := shellToolOutputPolicy(input.WaitSeconds, input.MaxOutputBytes)
		if policyErr != nil {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShellSnapshot{}, invalidWorkspaceShellValue(input, action, policyErr.Error(), allowed, example))
		}
		page, err := svc.QueryWorkspaceShellOutput(ctx, input.ShellID, sessionID, workspace.ID, input.AfterSequence, queryDelay, maxBytes, input.Reason, actor)
		return normalizeValueToolResult(ctx, "workspace_shell", modelShellResult(ctx, svc, page, shellSnapshotAfter(page.Snapshot), false), err)
	case "list":
		allowed := []string{"action", "reason"}
		example := map[string]any{"action": "list"}
		if err := validateWorkspaceShellActionFields(input, action, allowed, example); err != nil {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShellList{}, err)
		}
		if len(input.Reason) > 500 {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShellList{}, invalidWorkspaceShellValue(input, action, "reason must not exceed 500 bytes", allowed, example))
		}
		result, err := svc.ListWorkspaceShells(ctx, sessionID, workspace.ID, input.Reason, actor)
		return normalizeValueToolResult(ctx, "workspace_shell", result, err)
	case "interrupt":
		allowed := []string{"action", "shell_id", "reason"}
		example := map[string]any{"action": "interrupt", "shell_id": "shell_xxx"}
		if err := validateWorkspaceShellActionFields(input, action, allowed, example); err != nil {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShell{}, err)
		}
		if strings.TrimSpace(input.ShellID) == "" || len(input.Reason) > 500 {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShell{}, invalidWorkspaceShellValue(input, action, "action=interrupt requires shell_id and reason must not exceed 500 bytes", allowed, example))
		}
		result, err := svc.InterruptWorkspaceShell(ctx, input.ShellID, sessionID, workspace.ID, input.Reason, actor)
		return normalizeValueToolResult(ctx, "workspace_shell", result, err)
	case "close":
		allowed := []string{"action", "shell_id", "reason"}
		example := map[string]any{"action": "close", "shell_id": "shell_xxx"}
		if err := validateWorkspaceShellActionFields(input, action, allowed, example); err != nil {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShell{}, err)
		}
		if strings.TrimSpace(input.ShellID) == "" || len(input.Reason) > 500 {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShell{}, invalidWorkspaceShellValue(input, action, "action=close requires shell_id and reason must not exceed 500 bytes", allowed, example))
		}
		result, err := svc.CloseWorkspaceShell(ctx, input.ShellID, sessionID, workspace.ID, input.Reason, actor)
		return normalizeValueToolResult(ctx, "workspace_shell", result, err)
	default:
		return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShell{}, invalidStructuredToolInput(
			"invalid action: use run, start, input, output, list, interrupt, or close",
			domain.ToolValidationDetails{Action: action, AllowedFields: []string{"action"}, GotFields: workspaceShellProvidedFields(input), Example: map[string]any{"action": "list"}},
		))
	}
}

func classifyToolError(err error) (string, bool, string) {
	message := strings.ToLower(err.Error())
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "not_found", false, "verify the identifier or list available resources; do not retry the same missing identifier"
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(message, "timed out"), strings.Contains(message, "timeout"):
		return "timeout", true, "narrow the operation or set background=true on ssh_exec or ssh_run_script for a long-running command"
	case strings.Contains(message, "denied"), strings.Contains(message, "forbidden"):
		return "denied", false, "respect the denial and choose a permitted operation"
	case strings.Contains(message, "required"), strings.Contains(message, "invalid"), strings.Contains(message, "unsupported"):
		return "validation_failed", false, "correct the tool input using the error message; do not repeat unchanged input"
	case strings.Contains(message, "changed"), strings.Contains(message, "conflict"):
		return "conflict", true, "read the current state again before proposing another change"
	case strings.Contains(message, "constraint failed"):
		return "internal_error", false, "stop the affected workflow and report the control-plane persistence failure"
	default:
		return "remote_failed", true, "inspect stderr and gather narrower read-only details before retrying"
	}
}

func NormalizeWebSearchToolResult(result domain.WebSearchResponse, err error) (domain.WebSearchResponse, error) {
	result.ToolVersion = "1.1"
	result.ContentIsUntrusted = true
	if err == nil {
		result.OK = true
		result.Code = "completed"
		return result, nil
	}
	if errors.Is(err, context.Canceled) {
		return result, err
	}
	result.OK = false
	result.Message = err.Error()
	switch {
	case errors.Is(err, service.ErrWebSearchDisabled):
		result.Code = "configuration_required"
		result.NextAction = "tell the operator that Tavily Web must be enabled and configured in Settings; do not retry"
	case errors.Is(err, context.DeadlineExceeded):
		result.Code = "timeout"
		result.Retryable = true
		result.NextAction = "retry once with a narrower query or fewer results"
	case errors.Is(err, service.ErrWebSearchUpstream):
		result.Code, result.Retryable, result.NextAction = classifyWebProviderToolError(err)
	case strings.Contains(strings.ToLower(err.Error()), "timeout"):
		result.Code = "timeout"
		result.Retryable = true
		result.NextAction = "retry once with a narrower query or fewer results"
	default:
		result.Code, result.Retryable, result.NextAction = classifyToolError(err)
	}
	return result, nil
}

func NormalizeWebExtractToolResult(result domain.WebExtractResponse, err error) (domain.WebExtractResponse, error) {
	result.ToolVersion = "1.1"
	result.ContentIsUntrusted = true
	if err == nil {
		result.OK = true
		result.Code = "completed"
		if len(result.FailedResults) > 0 {
			result.Code = "partial"
			result.Message = "some URLs could not be extracted"
			result.NextAction = "use the successful pages and retry only failed URLs when they are still necessary"
		}
		return result, nil
	}
	if errors.Is(err, context.Canceled) {
		return result, err
	}
	result.OK = false
	result.Message = err.Error()
	switch {
	case errors.Is(err, service.ErrWebSearchDisabled):
		result.Code = "configuration_required"
		result.NextAction = "tell the operator that Tavily Web must be enabled and configured in Settings; do not retry"
	case errors.Is(err, context.DeadlineExceeded):
		result.Code = "timeout"
		result.Retryable = true
		result.NextAction = "retry once with fewer URLs"
	case errors.Is(err, service.ErrWebSearchUpstream):
		result.Code, result.Retryable, result.NextAction = classifyWebProviderToolError(err)
	case strings.Contains(strings.ToLower(err.Error()), "timeout"):
		result.Code = "timeout"
		result.Retryable = true
		result.NextAction = "retry once with fewer URLs"
	default:
		result.Code, result.Retryable, result.NextAction = classifyToolError(err)
	}
	return result, nil
}

func classifyWebProviderToolError(err error) (string, bool, string) {
	var providerError *service.WebSearchProviderError
	if !errors.As(err, &providerError) {
		return "provider_failed", true, "retry once only when the provider failure appears transient"
	}
	switch providerError.Code {
	case service.WebSearchErrorInvalidRequest:
		return providerError.Code, false, "correct the search or extraction parameters; do not repeat unchanged input"
	case service.WebSearchErrorAuthenticationFailed:
		return providerError.Code, false, "tell the operator to verify the Tavily API key in Settings; do not retry"
	case service.WebSearchErrorQuotaExhausted:
		return providerError.Code, false, "tell the operator that Tavily quota is exhausted; do not retry"
	case service.WebSearchErrorRateLimited:
		if providerError.Retryable {
			return providerError.Code, true, "retry once after a short delay with fewer results or URLs"
		}
		return providerError.Code, false, "do not retry in this turn; continue with sources already available"
	case service.WebSearchErrorTimeout:
		return providerError.Code, providerError.Retryable, "retry once with fewer results or URLs only when the operation is still necessary"
	case service.WebSearchErrorProviderUnavailable:
		return providerError.Code, providerError.Retryable, "retry once only when the operation is still necessary; otherwise report the provider outage"
	default:
		return "provider_failed", providerError.Retryable, "retry once only when the provider failure appears transient"
	}
}

func buildAvailableTools(svc *service.Service) ([]tool.BaseTool, error) {
	var tools []tool.BaseTool
	remoteValidatorIDs := svc.ValidatorIDs("remote")
	workspaceValidatorIDs := svc.ValidatorIDs("workspace")
	validatorHint := func(ids []string) string {
		if len(ids) == 0 {
			return " No validators; omit validator_id."
		}
		return " validator_id: " + strings.Join(ids, ", ") + "."
	}
	appendTool := func(created tool.InvokableTool, err error) error {
		if err != nil {
			return err
		}
		tools = append(tools, created)
		return nil
	}

	if err := appendTool(toolutils.InferTool("ssh_host_inspect", "Inspect one SSH host's OS, user, and uptime (read-only).", func(ctx context.Context, input HostInput) (any, error) {
		capability, err := svc.ProbeHost(ctx, input.HostID, "eino-agent")
		return normalizeValueToolResult(ctx, "ssh_host_inspect", capability, err)
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("ssh_host_list", "List registered SSH host IDs and capabilities; excludes connection data and secrets.", func(ctx context.Context, _ struct{}) (any, error) {
		hosts, err := svc.ListHostCapabilities(ctx)
		return normalizeValueToolResult(ctx, "ssh_host_list", HostListOutput{Hosts: hosts}, err)
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("ssh_exec", "Run one remote executable with separate arguments. Use background only for long, pollable work.", func(ctx context.Context, input ExecInput) (ExecToolResult, error) {
		request := domain.ExecRequest{HostID: input.HostID, Mode: domain.ExecProgram, Program: input.Program, Args: input.Args, Background: input.Background, Cwd: input.Cwd, Env: input.Env, Elevated: input.Elevated, TimeoutSeconds: input.TimeoutSeconds, MaxOutputBytes: input.MaxOutputBytes, OutputView: input.OutputView, Reason: input.Reason}
		return RunExecutionTool(ctx, svc, request, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("ssh_run_script", "Run a remote Bash script. Use background only for long, pollable work.", func(ctx context.Context, input ScriptInput) (ExecToolResult, error) {
		request := domain.ExecRequest{HostID: input.HostID, Mode: domain.ExecScript, Script: input.Script, Background: input.Background, Cwd: input.Cwd, Env: input.Env, Elevated: input.Elevated, TimeoutSeconds: input.TimeoutSeconds, MaxOutputBytes: input.MaxOutputBytes, OutputView: input.OutputView, Reason: input.Reason}
		return RunExecutionTool(ctx, svc, request, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("ssh_tunnel", "Start, list, or stop local (-L) and reverse (-R) SSH port forwarding with configurable listener addresses.", func(ctx context.Context, input SSHTunnelInput) (any, error) {
		return RunSSHTunnelTool(ctx, svc, input, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("ssh_shell", "Manage an interactive SSH PTY. Use only for prompts; wait_seconds delays reads; continue from next_sequence. Never send secrets.", func(ctx context.Context, input SSHShellInput) (any, error) {
		return RunSSHShellTool(ctx, svc, input, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("ssh_task", "Wait for, read, or cancel a background SSH task. Status returns output after supplied byte offsets without stopping the task.", func(ctx context.Context, input TaskInput) (ExecToolResult, error) {
		return RunTaskTool(ctx, svc, input, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("ssh_file_read", "Read, page, tail, inspect metadata, or search one remote file.", func(ctx context.Context, input FileReadInput) (ExecToolResult, error) {
		return RunFileReadTool(ctx, svc, input, "eino-agent")
	}, fileSearchSchemaOption())); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("ssh_file_list", "List a remote directory (read-only).", func(ctx context.Context, input FileListInput) (ExecToolResult, error) {
		result, err := svc.ListFiles(ctx, input.HostID, input.Path, "eino-agent")
		return CompactExecToolResult(result, err)
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("ssh_file_edit", "Replace an exact unique line block in an existing remote file; read it first."+validatorHint(remoteValidatorIDs), func(ctx context.Context, input FileEditInput) (ExecToolResult, error) {
		result, err := svc.EditRemoteFile(ctx, input.HostID, input.Path, input.OldText, input.NewText, input.ValidatorID, input.Elevated, input.Reason, "eino-agent")
		return CompactExecToolResult(result, err)
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("ssh_file_transfer", "Transfer one SHA256-bound file between registered SSH hosts.", func(ctx context.Context, input SSHFileTransferInput) (ExecToolResult, error) {
		result, err := svc.TransferFileBetweenHosts(ctx, input.SourceHostID, input.SourcePath, input.ExpectedSHA256, input.DestinationHostID, input.DestinationPath, input.ExpectedDestinationSHA256, input.TimeoutSeconds, input.Reason, "eino-agent")
		return CompactExecToolResult(result, err)
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("workspace_file_list", "List a directory in the current Workspace (read-only).", func(ctx context.Context, input WorkspacePathInput) (ExecToolResult, error) {
		workspace, err := svc.SessionWorkspace(ctx)
		if err != nil {
			return CompactExecToolResult(domain.ExecResult{}, err)
		}
		result, err := svc.ListWorkspaceFiles(ctx, workspace.ID, input.Path, "eino-agent")
		return CompactExecToolResult(result, err)
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("workspace_file_read", "Read, page, tail, or search one file in the current Workspace.", func(ctx context.Context, input WorkspaceReadInput) (ExecToolResult, error) {
		return RunWorkspaceFileReadTool(ctx, svc, input, "eino-agent")
	}, fileSearchSchemaOption())); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("workspace_file_edit", "Replace an exact unique line block in an existing current Workspace file; read it first."+validatorHint(workspaceValidatorIDs), func(ctx context.Context, input WorkspaceFileEditInput) (ExecToolResult, error) {
		workspace, err := svc.SessionWorkspace(ctx)
		if err != nil {
			return CompactExecToolResult(domain.ExecResult{}, err)
		}
		result, err := svc.EditWorkspaceFile(ctx, workspace.ID, input.Path, input.OldText, input.NewText, input.ValidatorID, input.Reason, "eino-agent")
		return CompactExecToolResult(result, err)
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("workspace_file_delete", "Permanently delete a path in the current read-write Workspace.", func(ctx context.Context, input WorkspaceFileDeleteInput) (ExecToolResult, error) {
		workspace, err := svc.SessionWorkspace(ctx)
		if err != nil {
			return CompactExecToolResult(domain.ExecResult{}, err)
		}
		result, err := svc.DeleteWorkspaceEntry(ctx, workspace.ID, input.Path, input.Recursive, input.Reason, "eino-agent")
		return CompactExecToolResult(result, err)
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("workspace_file_upload", "Upload a SHA256-bound current Workspace file to an SSH host.", func(ctx context.Context, input WorkspaceUploadInput) (ExecToolResult, error) {
		workspace, err := svc.SessionWorkspace(ctx)
		if err != nil {
			return CompactExecToolResult(domain.ExecResult{}, err)
		}
		result, err := svc.UploadWorkspaceFileToHost(ctx, input.HostID, workspace.ID, input.Path, input.ExpectedSHA256, input.RemotePath, input.Reason, "eino-agent")
		return CompactExecToolResult(result, err)
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("workspace_file_download", "Download a SHA256-bound SSH file to a new current Workspace path.", func(ctx context.Context, input WorkspaceDownloadInput) (ExecToolResult, error) {
		workspace, err := svc.SessionWorkspace(ctx)
		if err != nil {
			return CompactExecToolResult(domain.ExecResult{}, err)
		}
		result, err := svc.DownloadHostFileToWorkspace(ctx, input.HostID, input.RemotePath, input.ExpectedSHA256, workspace.ID, input.Path, input.TimeoutSeconds, input.Reason, "eino-agent")
		return CompactExecToolResult(result, err)
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("workspace_shell", "Run a script or manage a PTY in the current Workspace. Use run for one-shot work; wait_seconds delays reads; continue from next_sequence.", func(ctx context.Context, input WorkspaceShellInput) (any, error) {
		return RunWorkspaceShellTool(ctx, svc, input, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("web_search", "Search 3-5 public Web sources with Tavily. Prefer official domains and basic depth; use advanced depth only when relevant chunks are necessary. Select result URLs for web_extract and cite source URLs.", func(ctx context.Context, input WebSearchInput) (domain.WebSearchResponse, error) {
		result, err := svc.SearchWeb(ctx, domain.WebSearchRequest{
			Query: input.Query, MaxResults: input.MaxResults, Topic: input.Topic, SearchDepth: input.SearchDepth,
			TimeRange: input.TimeRange, StartDate: input.StartDate, EndDate: input.EndDate, ChunksPerSource: input.ChunksPerSource,
			IncludeDomains: input.IncludeDomains, ExcludeDomains: input.ExcludeDomains,
		}, "eino-agent")
		if result.Query == "" {
			result.Query = input.Query
		}
		if result.Provider == "" {
			result.Provider = "tavily"
		}
		return NormalizeWebSearchToolResult(result, err)
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("web_extract", "Extract relevant Markdown from up to five selected public URLs. Search first when URLs are unknown, pass query to focus extraction, and cite each source URL.", func(ctx context.Context, input WebExtractInput) (domain.WebExtractResponse, error) {
		result, err := svc.ExtractWeb(ctx, domain.WebExtractRequest{
			URLs: input.URLs, Query: input.Query, ExtractDepth: input.ExtractDepth, ChunksPerSource: input.ChunksPerSource,
		}, "eino-agent")
		if result.Provider == "" {
			result.Provider = "tavily"
		}
		if result.Query == "" {
			result.Query = input.Query
		}
		return NormalizeWebExtractToolResult(result, err)
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("ssh_history", "Search this conversation's audited run summaries with literal or POSIX regex matching and cursor pagination. Use run_id for a bounded redacted detail page; combine run_id and query for bounded matching excerpts, with limit as the per-stream match cap.", func(ctx context.Context, input HistorySearchInput) (any, error) {
		result, err := ReadHistoryTool(ctx, svc, input)
		return normalizeValueToolResult(ctx, "ssh_history", result, err)
	}, fileSearchSchemaOption())); err != nil {
		return nil, err
	}
	tools = append(tools, svc.MCPTools()...)
	return tools, nil
}

func BuildTools(svc *service.Service) ([]tool.BaseTool, error) {
	ctx := context.Background()
	available, err := buildAvailableTools(svc)
	if err != nil {
		return nil, err
	}
	states, err := svc.AgentToolStates(ctx)
	if err != nil {
		return nil, err
	}
	_, skillTools, err := newSkillMiddleware(ctx, svc, states)
	if err != nil {
		return nil, err
	}
	available = append(available, skillTools...)
	enabled := make([]tool.BaseTool, 0, len(available))
	for _, candidate := range available {
		info, err := candidate.Info(ctx)
		if err != nil {
			return nil, err
		}
		if value, configured := states[info.Name]; !configured || value {
			enabled = append(enabled, candidate)
		}
	}
	return enabled, nil
}

func buildToolSet(ctx context.Context, svc *service.Service) ([]tool.BaseTool, []ToolDescriptor, error) {
	available, err := buildAvailableTools(svc)
	if err != nil {
		return nil, nil, err
	}
	descriptors, err := DescribeTools(ctx, available)
	if err != nil {
		return nil, nil, err
	}
	states, err := svc.AgentToolStates(ctx)
	if err != nil {
		return nil, nil, err
	}
	enabled := make([]tool.BaseTool, 0, len(available))
	for index, candidate := range available {
		if value, configured := states[descriptors[index].Name]; configured {
			descriptors[index].Enabled = value
		}
		if descriptors[index].Enabled {
			enabled = append(enabled, candidate)
		}
	}
	return enabled, descriptors, nil
}
