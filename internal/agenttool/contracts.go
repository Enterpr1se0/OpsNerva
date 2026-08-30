package agenttool

import (
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

const (
	SSHExecDescription   = "Run exactly one non-interactive remote executable with separate argv items. Never use bash/sh, shell syntax, a command string, prompts, or terminal UIs; use ssh_run_script for shell syntax and ssh_shell for a PTY. Use top -b -n 1 for a top snapshot."
	SSHScriptDescription = "Run a non-interactive remote shell script without a PTY. Bash is selected when installed, otherwise POSIX sh; use portable syntax unless Bash availability is known. Pass the body directly, never bash -c or sh -c; use ssh_shell for prompts or terminal UIs."
	SSHShellDescription  = "Manage an interactive SSH PTY for prompts and terminal UIs. action=start already opens a login shell: do not input bash; use input/output, continue after next_sequence, and always close it. Never send secrets."
	SSHTaskDescription   = "Read, wait for, or cancel a background SSH task. status returns output after supplied byte offsets without stopping the task."
	SSHTunnelDescription = "Start, list, or stop SSH port forwarding. direction is local or reverse."
)

type HostInput struct {
	HostID string `json:"host_id" jsonschema_description:"registered host identifier"`
}

type HostListOutput struct {
	Hosts []domain.HostCapability `json:"hosts"`
}

type ExecInput struct {
	HostID         string            `json:"host_id" jsonschema_description:"registered host identifier"`
	Program        string            `json:"program" jsonschema_description:"one non-interactive executable; never bash, sh, shell syntax, or a command string"`
	Args           []string          `json:"args,omitempty" jsonschema_description:"argument vector; one argument per item, no shell quoting or command string"`
	Background     bool              `json:"background,omitempty" jsonschema_description:"cancellable background task; default false"`
	Cwd            string            `json:"cwd,omitempty" jsonschema_description:"absolute remote working directory"`
	Env            map[string]string `json:"env,omitempty" jsonschema_description:"non-secret environment variables"`
	Elevated       bool              `json:"elevated,omitempty" jsonschema_description:"managed root access; never use sudo or a password"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty" jsonschema:"minimum=1,maximum=600" jsonschema_description:"1-600; default uses configured sync/background timeout"`
	MaxOutputBytes int               `json:"max_output_bytes,omitempty" jsonschema:"minimum=0,maximum=4194304" jsonschema_description:"per-stream output limit; omit for complete output"`
	OutputView     string            `json:"output_view,omitempty" jsonschema:"enum=head,enum=tail,enum=head_tail" jsonschema_description:"with max_output_bytes: head, tail, or head_tail (default)"`
	Reason         string            `json:"reason" jsonschema_description:"one-sentence purpose"`
}

type ScriptInput struct {
	HostID         string            `json:"host_id" jsonschema_description:"registered host identifier"`
	Script         string            `json:"script" jsonschema_description:"non-interactive shell body; use portable syntax unless Bash is known; do not wrap in bash -c or sh -c"`
	Background     bool              `json:"background,omitempty" jsonschema_description:"cancellable background task; default false"`
	Cwd            string            `json:"cwd,omitempty" jsonschema_description:"absolute remote working directory"`
	Env            map[string]string `json:"env,omitempty" jsonschema_description:"non-secret environment variables"`
	Elevated       bool              `json:"elevated,omitempty" jsonschema_description:"managed root access; never include sudo or a password"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty" jsonschema:"minimum=1,maximum=600" jsonschema_description:"1-600; default uses configured sync/background timeout"`
	MaxOutputBytes int               `json:"max_output_bytes,omitempty" jsonschema:"minimum=0,maximum=4194304" jsonschema_description:"per-stream output limit; omit for complete output"`
	OutputView     string            `json:"output_view,omitempty" jsonschema:"enum=head,enum=tail,enum=head_tail" jsonschema_description:"with max_output_bytes: head, tail, or head_tail (default)"`
	Reason         string            `json:"reason" jsonschema_description:"one-sentence purpose"`
}

type TaskInput struct {
	TaskID           string `json:"task_id" jsonschema_description:"long-running task identifier"`
	Action           string `json:"action" jsonschema:"enum=status,enum=cancel" jsonschema_description:"status or cancel"`
	WaitSeconds      int    `json:"wait_seconds,omitempty" jsonschema:"minimum=0,maximum=60" jsonschema_description:"status only: wait 0-60; deadline leaves task running"`
	BlockUntil       string `json:"block_until,omitempty" jsonschema:"enum=terminal,enum=output" jsonschema_description:"status only: wait condition; terminal (default) or output"`
	AfterStdoutBytes int    `json:"after_stdout_bytes,omitempty" jsonschema:"minimum=0" jsonschema_description:"status only: stdout offset from prior stdout_total_bytes"`
	AfterStderrBytes int    `json:"after_stderr_bytes,omitempty" jsonschema:"minimum=0" jsonschema_description:"status only: stderr offset from prior stderr_total_bytes"`
	MaxOutputBytes   int    `json:"max_output_bytes,omitempty" jsonschema:"minimum=0,maximum=4194304" jsonschema_description:"status only: per-stream output limit"`
	OutputView       string `json:"output_view,omitempty" jsonschema:"enum=head,enum=tail,enum=head_tail" jsonschema_description:"with max_output_bytes: head, tail, or head_tail (default)"`
}

type SSHTunnelInput struct {
	Action     string `json:"action" jsonschema:"enum=start,enum=list,enum=stop" jsonschema_description:"start, list, or stop"`
	HostID     string `json:"host_id,omitempty" jsonschema_description:"start only: SSH host ID"`
	Direction  string `json:"direction,omitempty" jsonschema:"enum=local,enum=reverse" jsonschema_description:"start only: local (default) or reverse"`
	LocalHost  string `json:"local_host,omitempty" jsonschema_description:"start only: local listener for local forwarding, or client-side target for reverse; default 127.0.0.1"`
	LocalPort  int    `json:"local_port,omitempty" jsonschema:"minimum=0,maximum=65535" jsonschema_description:"start only: local listener port for local forwarding (0 selects one), or client-side target port for reverse"`
	RemoteHost string `json:"remote_host,omitempty" jsonschema_description:"start only: host-side target for local forwarding, or SSH-server listener for reverse; default 127.0.0.1"`
	RemotePort int    `json:"remote_port,omitempty" jsonschema:"minimum=0,maximum=65535" jsonschema_description:"start only: host-side target port for local forwarding, or SSH-server listener port for reverse (0 selects one)"`
	TunnelID   string `json:"tunnel_id,omitempty" jsonschema_description:"stop only: tunnel ID"`
	Reason     string `json:"reason,omitempty" jsonschema_description:"start only: one-sentence purpose"`
}

type SSHShellInput struct {
	Action         string  `json:"action" jsonschema:"enum=start,enum=input,enum=output,enum=list,enum=interrupt,enum=close" jsonschema_description:"start, input, output, list, interrupt, or close"`
	HostID         string  `json:"host_id,omitempty" jsonschema_description:"start: SSH host ID"`
	ShellID        string  `json:"shell_id,omitempty" jsonschema_description:"input/output/interrupt/close: shell ID"`
	Input          string  `json:"input,omitempty" jsonschema_description:"input: exact non-secret bytes; the login shell already exists, so do not send bash or sh"`
	Submit         bool    `json:"submit,omitempty" jsonschema_description:"input: append carriage return if no newline"`
	Cwd            string  `json:"cwd,omitempty" jsonschema_description:"start: absolute remote directory"`
	Elevated       bool    `json:"elevated,omitempty" jsonschema_description:"start: managed root shell"`
	AfterSequence  *uint64 `json:"after_sequence,omitempty" jsonschema_description:"output: events after next_sequence; omit for cursor, 0 to replay"`
	WaitSeconds    *int    `json:"wait_seconds,omitempty" jsonschema:"minimum=0,maximum=600" jsonschema_description:"input/output: delay before read, 0-600; default 5"`
	MaxOutputBytes *int    `json:"max_output_bytes,omitempty" jsonschema:"minimum=4096,maximum=4194304" jsonschema_description:"input/output: page bytes, 4096-4194304; default 131072"`
	Reason         string  `json:"reason,omitempty" jsonschema_description:"audit note; required for start"`
}

type FileReadInput struct {
	HostID       string                     `json:"host_id" jsonschema_description:"registered host identifier"`
	Path         string                     `json:"path" jsonschema_description:"absolute remote file path"`
	MetadataOnly bool                       `json:"metadata_only,omitempty" jsonschema_description:"metadata and SHA256 only"`
	FullContent  bool                       `json:"full_content,omitempty" jsonschema_description:"complete file; incompatible with range, search, and metadata_only"`
	MaxBytes     int                        `json:"max_bytes,omitempty" jsonschema:"minimum=1" jsonschema_description:"page bytes; omit for default 131072"`
	OffsetBytes  int64                      `json:"offset_bytes,omitempty" jsonschema_description:"byte offset; negative reads from end; incompatible with tail_lines"`
	TailLines    int                        `json:"tail_lines,omitempty" jsonschema:"minimum=1" jsonschema_description:"final line count; incompatible with offset_bytes"`
	Pattern      string                     `json:"pattern,omitempty" jsonschema:"minLength=1,maxLength=512" jsonschema_description:"search pattern; requires match_mode and forbids ranges"`
	MatchMode    domain.FileSearchMatchMode `json:"match_mode,omitempty" jsonschema:"enum=literal,enum=regex" jsonschema_description:"with pattern: literal or POSIX regex"`
	ContextLines int                        `json:"context_lines,omitempty" jsonschema:"minimum=0" jsonschema_description:"with pattern: search context line count"`
	Elevated     bool                       `json:"elevated,omitempty" jsonschema_description:"read with managed root access"`
}

type FileListInput struct {
	HostID string `json:"host_id" jsonschema_description:"registered host identifier"`
	Path   string `json:"path" jsonschema_description:"absolute remote directory path"`
}

type FileEditInput struct {
	HostID      string `json:"host_id" jsonschema_description:"registered host identifier"`
	Path        string `json:"path" jsonschema_description:"absolute remote file"`
	OldText     string `json:"old_text" jsonschema_description:"exact complete lines from latest read; must match once; empty creates a new file"`
	NewText     string `json:"new_text" jsonschema_description:"replacement lines; empty deletes old_text"`
	ValidatorID string `json:"validator_id,omitempty" jsonschema_description:"listed validator ID only; never a command"`
	Elevated    bool   `json:"elevated,omitempty" jsonschema_description:"edit with managed root access"`
	Reason      string `json:"reason" jsonschema_description:"one-sentence purpose"`
}

type SSHFileTransferInput struct {
	SourceHostID              string `json:"source_host_id" jsonschema_description:"registered source SSH host identifier"`
	SourcePath                string `json:"source_path" jsonschema_description:"absolute source file; no symlinks"`
	ExpectedSHA256            string `json:"expected_sha256" jsonschema_description:"source SHA256 from ssh_file_read"`
	DestinationHostID         string `json:"destination_host_id" jsonschema_description:"registered destination SSH host identifier"`
	DestinationPath           string `json:"destination_path" jsonschema_description:"absolute destination file path"`
	ExpectedDestinationSHA256 string `json:"expected_destination_sha256,omitempty" jsonschema_description:"omit to create; destination SHA256 to replace"`
	TimeoutSeconds            int    `json:"timeout_seconds,omitempty" jsonschema:"minimum=1,maximum=600" jsonschema_description:"1-600"`
	Reason                    string `json:"reason" jsonschema_description:"one-sentence purpose"`
}

type WorkspacePathInput struct {
	Path string `json:"path,omitempty" jsonschema_description:"Workspace-relative directory; default ."`
}

type WorkspaceReadInput struct {
	Path         string                     `json:"path" jsonschema_description:"Workspace-relative file"`
	FullContent  bool                       `json:"full_content,omitempty" jsonschema_description:"complete file; incompatible with range, tail, and search"`
	MaxBytes     int                        `json:"max_bytes,omitempty" jsonschema:"minimum=1" jsonschema_description:"page bytes; omit for default 131072"`
	OffsetBytes  int64                      `json:"offset_bytes,omitempty" jsonschema_description:"byte offset; negative reads from end; incompatible with tail_lines"`
	TailLines    int                        `json:"tail_lines,omitempty" jsonschema:"minimum=1" jsonschema_description:"final line count; incompatible with offset_bytes"`
	Pattern      string                     `json:"pattern,omitempty" jsonschema:"minLength=1,maxLength=512" jsonschema_description:"search pattern; requires match_mode and forbids ranges"`
	MatchMode    domain.FileSearchMatchMode `json:"match_mode,omitempty" jsonschema:"enum=literal,enum=regex" jsonschema_description:"with pattern: literal or POSIX regex"`
	ContextLines int                        `json:"context_lines,omitempty" jsonschema:"minimum=0" jsonschema_description:"with pattern: search context line count"`
}

type WorkspaceFileEditInput struct {
	Path        string `json:"path" jsonschema_description:"Workspace-relative file"`
	OldText     string `json:"old_text" jsonschema_description:"exact complete lines from latest read; must match once; empty creates a new file"`
	NewText     string `json:"new_text" jsonschema_description:"replacement lines; empty deletes old_text"`
	ValidatorID string `json:"validator_id,omitempty" jsonschema_description:"listed Workspace validator ID only; never a command"`
	Reason      string `json:"reason" jsonschema_description:"one-sentence purpose"`
}

type WorkspaceFileDeleteInput struct {
	Path      string `json:"path" jsonschema_description:"existing Workspace-relative path; not root"`
	Recursive bool   `json:"recursive,omitempty" jsonschema_description:"required only for non-empty directories"`
	Reason    string `json:"reason" jsonschema_description:"one-sentence deletion purpose"`
}

type WorkspaceUploadInput struct {
	HostID         string `json:"host_id" jsonschema_description:"destination SSH host ID"`
	Path           string `json:"path" jsonschema_description:"Workspace-relative source file"`
	ExpectedSHA256 string `json:"expected_sha256" jsonschema_description:"source SHA256 from workspace_file_read"`
	RemotePath     string `json:"remote_path" jsonschema_description:"absolute remote destination"`
	Reason         string `json:"reason" jsonschema_description:"one-sentence purpose"`
}

type WorkspaceDownloadInput struct {
	HostID         string `json:"host_id" jsonschema_description:"source SSH host ID"`
	RemotePath     string `json:"remote_path" jsonschema_description:"absolute remote source file; no symlinks"`
	ExpectedSHA256 string `json:"expected_sha256" jsonschema_description:"source SHA256 from ssh_file_read"`
	Path           string `json:"path" jsonschema_description:"new Workspace-relative destination"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"minimum=1,maximum=600" jsonschema_description:"1-600"`
	Reason         string `json:"reason" jsonschema_description:"one-sentence purpose"`
}

type WorkspaceShellInput struct {
	Action         string            `json:"action" jsonschema:"enum=run,enum=start,enum=input,enum=output,enum=list,enum=interrupt,enum=close" jsonschema_description:"run, start, input, output, list, interrupt, or close"`
	Script         string            `json:"script,omitempty" jsonschema_description:"run only: complete non-interactive Bash or PowerShell script"`
	ShellID        string            `json:"shell_id,omitempty" jsonschema_description:"input, output, interrupt, or close: shell ID"`
	Input          string            `json:"input,omitempty" jsonschema_description:"input only: exact non-secret bytes"`
	Submit         bool              `json:"submit,omitempty" jsonschema_description:"input only: append carriage return if no newline"`
	Cwd            string            `json:"cwd,omitempty" jsonschema_description:"run or start: Workspace-relative directory; default root"`
	Env            map[string]string `json:"env,omitempty" jsonschema_description:"run or start: non-secret environment variables"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty" jsonschema:"minimum=1,maximum=600" jsonschema_description:"run only: 1-600"`
	AfterSequence  *uint64           `json:"after_sequence,omitempty" jsonschema_description:"output only: events after next_sequence; omit for cursor, 0 to replay"`
	WaitSeconds    *int              `json:"wait_seconds,omitempty" jsonschema:"minimum=0,maximum=600" jsonschema_description:"input or output: delay before read, 0-600; default 5"`
	MaxOutputBytes *int              `json:"max_output_bytes,omitempty" jsonschema:"minimum=4096,maximum=4194304" jsonschema_description:"input or output: page bytes, 4096-4194304; default 131072"`
	Reason         string            `json:"reason,omitempty" jsonschema_description:"audit note; required for run or start"`
}

type HistorySearchInput struct {
	RunID         string                     `json:"run_id,omitempty" jsonschema_description:"exact run ID; combine with query for bounded excerpts"`
	Query         string                     `json:"query,omitempty" jsonschema:"maxLength=4096" jsonschema_description:"list search, or bounded matching excerpts with run_id"`
	MatchMode     domain.FileSearchMatchMode `json:"match_mode,omitempty" jsonschema:"enum=literal,enum=regex" jsonschema_description:"with query: literal (default) or regex"`
	QueryScope    string                     `json:"query_scope,omitempty" jsonschema:"enum=all,enum=request,enum=output" jsonschema_description:"with query: all (default), request, or output"`
	HostID        string                     `json:"host_id,omitempty" jsonschema_description:"list filter: SSH host ID"`
	ToolName      string                     `json:"tool_name,omitempty" jsonschema_description:"list filter: exact tool name"`
	Status        string                     `json:"status,omitempty" jsonschema_description:"list filter: exact run status"`
	StartedAfter  string                     `json:"started_after,omitempty" jsonschema_description:"list filter: inclusive RFC3339 lower bound"`
	StartedBefore string                     `json:"started_before,omitempty" jsonschema_description:"list filter: inclusive RFC3339 upper bound"`
	Limit         int                        `json:"limit,omitempty" jsonschema:"minimum=1,maximum=100" jsonschema_description:"list results, or run_id query matches per stream; default 20, maximum 100"`
	Cursor        string                     `json:"cursor,omitempty" jsonschema_description:"list continuation cursor from next_cursor"`
	AfterStdout   int                        `json:"after_stdout_bytes,omitempty" jsonschema:"minimum=0" jsonschema_description:"run_id detail: stdout byte offset"`
	AfterStderr   int                        `json:"after_stderr_bytes,omitempty" jsonschema:"minimum=0" jsonschema_description:"run_id detail: stderr byte offset"`
	MaxOutput     int                        `json:"max_output_bytes,omitempty" jsonschema:"minimum=1024,maximum=65536" jsonschema_description:"run_id detail or excerpts: per-stream bytes, 1024-65536; default 16384"`
	OutputView    string                     `json:"output_view,omitempty" jsonschema:"enum=head,enum=tail,enum=head_tail" jsonschema_description:"run_id detail: head, tail, or head_tail (default)"`
}

type WebSearchInput struct {
	Query           string   `json:"query" jsonschema_description:"public query; no private data or secrets"`
	MaxResults      int      `json:"max_results,omitempty" jsonschema:"minimum=1,maximum=20" jsonschema_description:"result count; default 5, maximum is configured"`
	Topic           string   `json:"topic,omitempty" jsonschema:"enum=general,enum=news,enum=finance" jsonschema_description:"general (default), news, or finance"`
	SearchDepth     string   `json:"search_depth,omitempty" jsonschema:"enum=basic,enum=advanced,enum=fast,enum=ultra-fast" jsonschema_description:"basic (default), advanced, fast, or ultra-fast"`
	TimeRange       string   `json:"time_range,omitempty" jsonschema:"enum=day,enum=week,enum=month,enum=year" jsonschema_description:"day, week, month, or year"`
	StartDate       string   `json:"start_date,omitempty" jsonschema_description:"inclusive YYYY-MM-DD lower bound; do not combine with time_range"`
	EndDate         string   `json:"end_date,omitempty" jsonschema_description:"inclusive YYYY-MM-DD upper bound; do not combine with time_range"`
	ChunksPerSource int      `json:"chunks_per_source,omitempty" jsonschema:"minimum=1,maximum=3" jsonschema_description:"1-3 relevant chunks per source; only with search_depth=advanced"`
	IncludeDomains  []string `json:"include_domains,omitempty" jsonschema_description:"public domains to include; no schemes or paths"`
	ExcludeDomains  []string `json:"exclude_domains,omitempty" jsonschema_description:"public domains to exclude; no schemes or paths"`
}

type WebExtractInput struct {
	URLs            []string `json:"urls" jsonschema:"minItems=1,maxItems=5" jsonschema_description:"1-5 public HTTP(S) URLs; no private data or addresses"`
	Query           string   `json:"query,omitempty" jsonschema_description:"focus extraction on passages relevant to this query"`
	ExtractDepth    string   `json:"extract_depth,omitempty" jsonschema:"enum=basic,enum=advanced" jsonschema_description:"basic (default) or advanced"`
	ChunksPerSource int      `json:"chunks_per_source,omitempty" jsonschema:"minimum=1,maximum=5" jsonschema_description:"1-5 relevant chunks per URL; requires query"`
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

// ExecResult is the model-facing projection of a complete execution record.
// Audit and UI-only details stay in the persisted run.
type ExecResult struct {
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
	NextAction          string                        `json:"next_action,omitempty"`
	Validation          *domain.ToolValidationDetails `json:"validation,omitempty"`
}
