package agenttool

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

type WorkspaceResolver func(context.Context) (string, error)

type WorkspaceFileService interface {
	ReadWorkspaceFileAdvanced(context.Context, string, string, int, int64, int, string) (domain.ExecResult, error)
	ListWorkspaceFiles(context.Context, string, string, string) (domain.ExecResult, error)
	SearchWorkspace(context.Context, string, string, string, domain.FileSearchMatchMode, int, string) (domain.ExecResult, error)
	EditWorkspaceFile(context.Context, string, string, string, string, string, string, string) (domain.ExecResult, error)
	DeleteWorkspaceEntry(context.Context, string, string, bool, string, string) (domain.ExecResult, error)
	UploadWorkspaceFileToHost(context.Context, string, string, string, string, string, string, string) (domain.ExecResult, error)
	DownloadHostFileToWorkspace(context.Context, string, string, string, string, string, int, string, string) (domain.ExecResult, error)
}

type WorkspaceShellService interface {
	ShellSnapshotReader
	RunWorkspaceShell(context.Context, string, string, string, map[string]string, int, string, string) (domain.ExecResult, error)
	StartWorkspaceShell(context.Context, string, string, map[string]string, int, int, string, string) (domain.ExecResult, error)
	WriteWorkspaceShellPage(context.Context, string, string, string, string, time.Duration, int, string, string) (domain.SSHShellOutputPage, error)
	QueryWorkspaceShellOutput(context.Context, string, string, string, *uint64, time.Duration, int, string, string) (domain.SSHShellOutputPage, error)
	ListWorkspaceShells(context.Context, string, string, string, string) (domain.SSHShellList, error)
	InterruptWorkspaceShell(context.Context, string, string, string, string, string) (domain.SSHShell, error)
	CloseWorkspaceShell(context.Context, string, string, string, string, string) (domain.SSHShell, error)
}

type WorkspaceDependencies struct {
	Resolve WorkspaceResolver
	Files   WorkspaceFileService
	Shells  WorkspaceShellService
	Results ResultPolicy
}

type Workspace struct {
	dependencies WorkspaceDependencies
}

func NewWorkspace(dependencies WorkspaceDependencies) *Workspace {
	return &Workspace{dependencies: dependencies}
}

func (workspace *Workspace) resolve(ctx context.Context) (string, error) {
	return workspace.dependencies.Resolve(ctx)
}

func (workspace *Workspace) execResult(result domain.ExecResult, err error) (ExecResult, error) {
	normalized, normalizedErr := workspace.dependencies.Results.NormalizeExec(result, err)
	return ProjectExecResult(normalized), normalizedErr
}

const defaultWorkspaceFileReadBytes = 128 << 10

func (workspace *Workspace) RunFileList(ctx context.Context, input WorkspacePathInput, actor string) (ExecResult, error) {
	workspaceID, err := workspace.resolve(ctx)
	if err != nil {
		return workspace.execResult(domain.ExecResult{}, err)
	}
	result, err := workspace.dependencies.Files.ListWorkspaceFiles(ctx, workspaceID, input.Path, actor)
	return workspace.execResult(result, err)
}

func (workspace *Workspace) RunFileRead(ctx context.Context, input WorkspaceReadInput, actor string) (ExecResult, error) {
	workspaceID, err := workspace.resolve(ctx)
	if err != nil {
		return workspace.execResult(domain.ExecResult{}, err)
	}
	searching := input.Pattern != ""
	if searching && (input.FullContent || input.MaxBytes != 0 || input.OffsetBytes != 0 || input.TailLines != 0) {
		return workspace.execResult(domain.ExecResult{}, fmt.Errorf("invalid Workspace file read input: pattern cannot be combined with full_content, max_bytes, offset_bytes, or tail_lines"))
	}
	if searching && input.MatchMode == "" {
		return workspace.execResult(domain.ExecResult{}, fmt.Errorf("invalid Workspace file read input: match_mode is required with pattern"))
	}
	if !searching && (input.MatchMode != "" || input.ContextLines != 0) {
		return workspace.execResult(domain.ExecResult{}, fmt.Errorf("invalid Workspace file read input: match_mode and context_lines require pattern"))
	}
	if searching {
		result, err := workspace.dependencies.Files.SearchWorkspace(ctx, workspaceID, input.Path, input.Pattern, input.MatchMode, input.ContextLines, actor)
		return workspace.execResult(result, err)
	}
	if input.FullContent && (input.MaxBytes != 0 || input.OffsetBytes != 0 || input.TailLines != 0) {
		return workspace.execResult(domain.ExecResult{}, fmt.Errorf("invalid Workspace file read input: full_content cannot be combined with max_bytes, offset_bytes, or tail_lines"))
	}
	if input.MaxBytes < 0 || input.TailLines < 0 || (input.OffsetBytes != 0 && input.TailLines != 0) {
		return workspace.execResult(domain.ExecResult{}, fmt.Errorf("invalid Workspace file read range: max_bytes and tail_lines must be non-negative; tail_lines cannot be combined with offset_bytes"))
	}
	if !input.FullContent && input.MaxBytes == 0 && input.TailLines == 0 {
		input.MaxBytes = defaultWorkspaceFileReadBytes
	}
	result, err := workspace.dependencies.Files.ReadWorkspaceFileAdvanced(ctx, workspaceID, input.Path, input.MaxBytes, input.OffsetBytes, input.TailLines, actor)
	return workspace.execResult(result, err)
}

func (workspace *Workspace) RunFileEdit(ctx context.Context, input WorkspaceFileEditInput, actor string) (ExecResult, error) {
	workspaceID, err := workspace.resolve(ctx)
	if err != nil {
		return workspace.execResult(domain.ExecResult{}, err)
	}
	result, err := workspace.dependencies.Files.EditWorkspaceFile(
		ctx, workspaceID, input.Path, input.OldText, input.NewText, input.ValidatorID, input.Reason, actor,
	)
	return workspace.execResult(result, err)
}

func (workspace *Workspace) RunFileDelete(ctx context.Context, input WorkspaceFileDeleteInput, actor string) (ExecResult, error) {
	workspaceID, err := workspace.resolve(ctx)
	if err != nil {
		return workspace.execResult(domain.ExecResult{}, err)
	}
	result, err := workspace.dependencies.Files.DeleteWorkspaceEntry(ctx, workspaceID, input.Path, input.Recursive, input.Reason, actor)
	return workspace.execResult(result, err)
}

func (workspace *Workspace) RunFileUpload(ctx context.Context, input WorkspaceUploadInput, actor string) (ExecResult, error) {
	workspaceID, err := workspace.resolve(ctx)
	if err != nil {
		return workspace.execResult(domain.ExecResult{}, err)
	}
	result, err := workspace.dependencies.Files.UploadWorkspaceFileToHost(
		ctx, input.HostID, workspaceID, input.Path, input.ExpectedSHA256, input.RemotePath, input.Reason, actor,
	)
	return workspace.execResult(result, err)
}

func (workspace *Workspace) RunFileDownload(ctx context.Context, input WorkspaceDownloadInput, actor string) (ExecResult, error) {
	workspaceID, err := workspace.resolve(ctx)
	if err != nil {
		return workspace.execResult(domain.ExecResult{}, err)
	}
	result, err := workspace.dependencies.Files.DownloadHostFileToWorkspace(
		ctx, input.HostID, input.RemotePath, input.ExpectedSHA256, workspaceID, input.Path,
		input.TimeoutSeconds, input.Reason, actor,
	)
	return workspace.execResult(result, err)
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
	return ValidateActionFields(action, workspaceShellProvidedFields(input), allowed, example)
}

func invalidWorkspaceShellValue(input WorkspaceShellInput, action, message string, allowed []string, example map[string]any) error {
	return StructuredInputError(message, domain.ToolValidationDetails{
		Action: action, AllowedFields: allowed, GotFields: workspaceShellProvidedFields(input), Example: example,
	})
}

func (workspace *Workspace) RunShell(ctx context.Context, sessionID string, input WorkspaceShellInput, actor string) (any, error) {
	action := strings.ToLower(strings.TrimSpace(input.Action))
	if sessionID == "" {
		return workspace.dependencies.Results.Value(ctx, "workspace_shell", domain.SSHShell{}, InvalidInput("workspace_shell requires an Agent conversation"))
	}
	workspaceID, err := workspace.resolve(ctx)
	if err != nil {
		return workspace.dependencies.Results.Value(ctx, "workspace_shell", domain.SSHShell{}, err)
	}
	switch action {
	case "run":
		allowed := []string{"action", "script", "cwd", "env", "timeout_seconds", "reason"}
		example := map[string]any{"action": "run", "script": "go test ./...", "reason": "run the project tests"}
		if err := validateWorkspaceShellActionFields(input, action, allowed, example); err != nil {
			return workspace.dependencies.Results.Value(ctx, "workspace_shell", domain.ExecResult{}, err)
		}
		if strings.TrimSpace(input.Script) == "" || strings.TrimSpace(input.Reason) == "" {
			return workspace.dependencies.Results.Value(ctx, "workspace_shell", domain.ExecResult{}, invalidWorkspaceShellValue(input, action, "action=run requires script and reason", allowed, example))
		}
		result, err := workspace.dependencies.Shells.RunWorkspaceShell(ctx, workspaceID, input.Script, input.Cwd, input.Env, input.TimeoutSeconds, input.Reason, actor)
		return workspace.execResult(result, err)
	case "start":
		allowed := []string{"action", "cwd", "env", "reason"}
		example := map[string]any{"action": "start", "reason": "open an interactive project shell"}
		if err := validateWorkspaceShellActionFields(input, action, allowed, example); err != nil {
			return workspace.dependencies.Results.Value(ctx, "workspace_shell", domain.ExecResult{}, err)
		}
		if strings.TrimSpace(input.Reason) == "" {
			return workspace.dependencies.Results.Value(ctx, "workspace_shell", domain.ExecResult{}, invalidWorkspaceShellValue(input, action, "action=start requires reason", allowed, example))
		}
		result, err := workspace.dependencies.Shells.StartWorkspaceShell(ctx, workspaceID, input.Cwd, input.Env, 120, 32, input.Reason, actor)
		return workspace.execResult(result, err)
	case "input":
		allowed := []string{"action", "shell_id", "input", "submit", "wait_seconds", "max_output_bytes", "reason"}
		example := map[string]any{"action": "input", "shell_id": "shell_xxx", "input": "go test ./...", "submit": true}
		if err := validateWorkspaceShellActionFields(input, action, allowed, example); err != nil {
			return workspace.dependencies.Results.Value(ctx, "workspace_shell", domain.SSHShellSnapshot{}, err)
		}
		if strings.TrimSpace(input.ShellID) == "" || input.Input == "" {
			return workspace.dependencies.Results.Value(ctx, "workspace_shell", domain.SSHShellSnapshot{}, invalidWorkspaceShellValue(input, action, "action=input requires shell_id and input", allowed, example))
		}
		shellInput := input.Input
		if input.Submit && !strings.HasSuffix(shellInput, "\r") && !strings.HasSuffix(shellInput, "\n") {
			shellInput += "\r"
		}
		if len(shellInput) > 64<<10 || len(input.Reason) > 500 {
			return workspace.dependencies.Results.Value(ctx, "workspace_shell", domain.SSHShellSnapshot{}, invalidWorkspaceShellValue(input, action, "input must not exceed 65536 bytes and reason must not exceed 500 bytes", allowed, example))
		}
		queryDelay, maxBytes, policyErr := ShellOutputPolicy(input.WaitSeconds, input.MaxOutputBytes)
		if policyErr != nil {
			return workspace.dependencies.Results.Value(ctx, "workspace_shell", domain.SSHShellSnapshot{}, invalidWorkspaceShellValue(input, action, policyErr.Error(), allowed, example))
		}
		page, err := workspace.dependencies.Shells.WriteWorkspaceShellPage(ctx, input.ShellID, sessionID, workspaceID, shellInput, queryDelay, maxBytes, input.Reason, actor)
		return workspace.dependencies.Results.Value(ctx, "workspace_shell", formatShellPage(ctx, workspace.dependencies.Shells, page, ShellSnapshotAfter(page.Snapshot), true), err)
	case "output":
		allowed := []string{"action", "shell_id", "after_sequence", "wait_seconds", "max_output_bytes", "reason"}
		example := map[string]any{"action": "output", "shell_id": "shell_xxx", "wait_seconds": 10}
		if err := validateWorkspaceShellActionFields(input, action, allowed, example); err != nil {
			return workspace.dependencies.Results.Value(ctx, "workspace_shell", domain.SSHShellSnapshot{}, err)
		}
		if strings.TrimSpace(input.ShellID) == "" {
			return workspace.dependencies.Results.Value(ctx, "workspace_shell", domain.SSHShellSnapshot{}, invalidWorkspaceShellValue(input, action, "action=output requires shell_id", allowed, example))
		}
		if len(input.Reason) > 500 {
			return workspace.dependencies.Results.Value(ctx, "workspace_shell", domain.SSHShellSnapshot{}, invalidWorkspaceShellValue(input, action, "reason must not exceed 500 bytes", allowed, example))
		}
		queryDelay, maxBytes, policyErr := ShellOutputPolicy(input.WaitSeconds, input.MaxOutputBytes)
		if policyErr != nil {
			return workspace.dependencies.Results.Value(ctx, "workspace_shell", domain.SSHShellSnapshot{}, invalidWorkspaceShellValue(input, action, policyErr.Error(), allowed, example))
		}
		page, err := workspace.dependencies.Shells.QueryWorkspaceShellOutput(ctx, input.ShellID, sessionID, workspaceID, input.AfterSequence, queryDelay, maxBytes, input.Reason, actor)
		return workspace.dependencies.Results.Value(ctx, "workspace_shell", formatShellPage(ctx, workspace.dependencies.Shells, page, ShellSnapshotAfter(page.Snapshot), false), err)
	case "list":
		allowed := []string{"action", "reason"}
		example := map[string]any{"action": "list"}
		if err := validateWorkspaceShellActionFields(input, action, allowed, example); err != nil {
			return workspace.dependencies.Results.Value(ctx, "workspace_shell", domain.SSHShellList{}, err)
		}
		if len(input.Reason) > 500 {
			return workspace.dependencies.Results.Value(ctx, "workspace_shell", domain.SSHShellList{}, invalidWorkspaceShellValue(input, action, "reason must not exceed 500 bytes", allowed, example))
		}
		result, err := workspace.dependencies.Shells.ListWorkspaceShells(ctx, sessionID, workspaceID, input.Reason, actor)
		return workspace.dependencies.Results.Value(ctx, "workspace_shell", result, err)
	case "interrupt":
		allowed := []string{"action", "shell_id", "reason"}
		example := map[string]any{"action": "interrupt", "shell_id": "shell_xxx"}
		if err := validateWorkspaceShellActionFields(input, action, allowed, example); err != nil {
			return workspace.dependencies.Results.Value(ctx, "workspace_shell", domain.SSHShell{}, err)
		}
		if strings.TrimSpace(input.ShellID) == "" || len(input.Reason) > 500 {
			return workspace.dependencies.Results.Value(ctx, "workspace_shell", domain.SSHShell{}, invalidWorkspaceShellValue(input, action, "action=interrupt requires shell_id and reason must not exceed 500 bytes", allowed, example))
		}
		result, err := workspace.dependencies.Shells.InterruptWorkspaceShell(ctx, input.ShellID, sessionID, workspaceID, input.Reason, actor)
		return workspace.dependencies.Results.Value(ctx, "workspace_shell", result, err)
	case "close":
		allowed := []string{"action", "shell_id", "reason"}
		example := map[string]any{"action": "close", "shell_id": "shell_xxx"}
		if err := validateWorkspaceShellActionFields(input, action, allowed, example); err != nil {
			return workspace.dependencies.Results.Value(ctx, "workspace_shell", domain.SSHShell{}, err)
		}
		if strings.TrimSpace(input.ShellID) == "" || len(input.Reason) > 500 {
			return workspace.dependencies.Results.Value(ctx, "workspace_shell", domain.SSHShell{}, invalidWorkspaceShellValue(input, action, "action=close requires shell_id and reason must not exceed 500 bytes", allowed, example))
		}
		result, err := workspace.dependencies.Shells.CloseWorkspaceShell(ctx, input.ShellID, sessionID, workspaceID, input.Reason, actor)
		return workspace.dependencies.Results.Value(ctx, "workspace_shell", result, err)
	default:
		return workspace.dependencies.Results.Value(ctx, "workspace_shell", domain.SSHShell{}, StructuredInputError(
			"invalid action: use run, start, input, output, list, interrupt, or close",
			domain.ToolValidationDetails{
				Action: action, AllowedFields: []string{"action"}, GotFields: workspaceShellProvidedFields(input),
				Example: map[string]any{"action": "list"},
			},
		))
	}
}
