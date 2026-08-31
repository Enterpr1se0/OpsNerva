package agent

import (
	"context"
	"strings"

	"github.com/Enterpr1se0/opsnerva/internal/agenttool"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/service"
	"github.com/Enterpr1se0/opsnerva/internal/toolresult"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
)

type agentToolResultPolicy struct{}

func (agentToolResultPolicy) NormalizeExec(result domain.ExecResult, err error) (domain.ExecResult, error) {
	return toolresult.NormalizeExec(result, err)
}

func (agentToolResultPolicy) Value(ctx context.Context, toolName string, value any, err error) (any, error) {
	return normalizeValueToolResult(ctx, toolName, value, err)
}

func newSSHTools(svc *service.Service) *agenttool.SSH {
	return agenttool.NewSSH(agenttool.SSHDependencies{
		Execution: svc,
		Tasks:     svc,
		Files:     svc,
		Tunnels:   svc,
		Shells:    svc,
		Results:   agentToolResultPolicy{},
	})
}

func newWorkspaceTools(svc *service.Service) *agenttool.Workspace {
	return agenttool.NewWorkspace(agenttool.WorkspaceDependencies{
		Resolve: func(ctx context.Context) (string, error) {
			workspace, err := svc.SessionWorkspace(ctx)
			return workspace.ID, err
		},
		Files:   svc,
		Shells:  svc,
		Results: agentToolResultPolicy{},
	})
}

func newWebTools(svc *service.Service) *agenttool.Web {
	return agenttool.NewWeb(agenttool.WebDependencies{Service: svc, Results: toolresult.WebPolicy{}})
}

func newHistoryTools(svc *service.Service) *agenttool.History {
	return agenttool.NewHistory(svc)
}

func buildAvailableTools(svc *service.Service) ([]tool.BaseTool, error) {
	var tools []tool.BaseTool
	sshTools := newSSHTools(svc)
	workspaceTools := newWorkspaceTools(svc)
	webTools := newWebTools(svc)
	historyTools := newHistoryTools(svc)
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

	if err := appendTool(toolutils.InferTool("ssh_host_inspect", "Inspect one SSH host's OS, user, shell, and uptime (read-only).", func(ctx context.Context, input agenttool.HostInput) (any, error) {
		capability, err := svc.ProbeHost(ctx, input.HostID, "eino-agent")
		return normalizeValueToolResult(ctx, "ssh_host_inspect", capability, err)
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("ssh_exec", agenttool.SSHExecDescription, func(ctx context.Context, input agenttool.ExecInput) (agenttool.ExecResult, error) {
		return sshTools.RunExecution(ctx, agenttool.ExecutionRequest(input), "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("ssh_run_script", agenttool.SSHScriptDescription, func(ctx context.Context, input agenttool.ScriptInput) (agenttool.ExecResult, error) {
		return sshTools.RunExecution(ctx, agenttool.ScriptRequest(input), "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("ssh_tunnel", agenttool.SSHTunnelDescription, func(ctx context.Context, input agenttool.SSHTunnelInput) (any, error) {
		return sshTools.RunTunnel(ctx, input, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("ssh_shell", agenttool.SSHShellDescription, func(ctx context.Context, input agenttool.SSHShellInput) (any, error) {
		return sshTools.RunShell(ctx, service.SessionIDFromContext(ctx), input, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("ssh_task", agenttool.SSHTaskDescription, func(ctx context.Context, input agenttool.TaskInput) (agenttool.ExecResult, error) {
		return sshTools.RunTask(ctx, input, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("ssh_file_read", "Read, page, tail, inspect metadata, or search one remote file.", func(ctx context.Context, input agenttool.FileReadInput) (agenttool.ExecResult, error) {
		return sshTools.RunFileRead(ctx, input, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("ssh_file_list", "List a remote directory (read-only).", func(ctx context.Context, input agenttool.FileListInput) (agenttool.ExecResult, error) {
		return sshTools.RunFileList(ctx, input, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("ssh_file_edit", "Create a remote text file or replace/delete one exact unique line block; read existing files first."+validatorHint(remoteValidatorIDs), func(ctx context.Context, input agenttool.FileEditInput) (agenttool.ExecResult, error) {
		return sshTools.RunFileEdit(ctx, input, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("ssh_file_transfer", "Transfer one SHA256-bound file between registered SSH hosts.", func(ctx context.Context, input agenttool.SSHFileTransferInput) (agenttool.ExecResult, error) {
		return sshTools.RunFileTransfer(ctx, input, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("workspace_file_list", "List a directory in the current Workspace (read-only).", func(ctx context.Context, input agenttool.WorkspacePathInput) (agenttool.ExecResult, error) {
		return workspaceTools.RunFileList(ctx, input, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("workspace_file_read", "Read, page, tail, or search one file in the current Workspace.", func(ctx context.Context, input agenttool.WorkspaceReadInput) (agenttool.ExecResult, error) {
		return workspaceTools.RunFileRead(ctx, input, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("workspace_file_edit", "Create a text file or replace/delete one exact unique line block in the current Workspace; read existing files first."+validatorHint(workspaceValidatorIDs), func(ctx context.Context, input agenttool.WorkspaceFileEditInput) (agenttool.ExecResult, error) {
		return workspaceTools.RunFileEdit(ctx, input, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("workspace_file_delete", "Permanently delete a path in the current read-write Workspace.", func(ctx context.Context, input agenttool.WorkspaceFileDeleteInput) (agenttool.ExecResult, error) {
		return workspaceTools.RunFileDelete(ctx, input, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("workspace_file_upload", "Upload a SHA256-bound current Workspace file to an SSH host.", func(ctx context.Context, input agenttool.WorkspaceUploadInput) (agenttool.ExecResult, error) {
		return workspaceTools.RunFileUpload(ctx, input, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("workspace_file_download", "Download a SHA256-bound SSH file to a new current Workspace path.", func(ctx context.Context, input agenttool.WorkspaceDownloadInput) (agenttool.ExecResult, error) {
		return workspaceTools.RunFileDownload(ctx, input, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("workspace_shell", "Run a script or manage a PTY in the current Workspace. Use run for one-shot work; wait_seconds delays reads; continue from next_sequence.", func(ctx context.Context, input agenttool.WorkspaceShellInput) (any, error) {
		return workspaceTools.RunShell(ctx, service.SessionIDFromContext(ctx), input, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("web_search", "Search 3-5 public Web sources with Tavily. Prefer official domains and basic depth; use advanced depth only when relevant chunks are necessary. Select result URLs for web_extract and cite source URLs.", func(ctx context.Context, input agenttool.WebSearchInput) (domain.WebSearchResponse, error) {
		return webTools.RunSearch(ctx, input, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("web_extract", "Extract relevant Markdown from up to five selected public URLs. Search first when URLs are unknown, pass query to focus extraction, and cite each source URL.", func(ctx context.Context, input agenttool.WebExtractInput) (domain.WebExtractResponse, error) {
		return webTools.RunExtract(ctx, input, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("ssh_history", "Search this conversation's audited run summaries with literal or POSIX regex matching and cursor pagination. Use run_id for a bounded redacted detail page; combine run_id and query for bounded matching excerpts, with limit as the per-stream match cap.", func(ctx context.Context, input agenttool.HistorySearchInput) (any, error) {
		result, err := historyTools.Read(ctx, input)
		return normalizeValueToolResult(ctx, "ssh_history", result, err)
	})); err != nil {
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

func buildToolSet(ctx context.Context, svc *service.Service) ([]tool.BaseTool, []agenttool.Descriptor, error) {
	available, err := buildAvailableTools(svc)
	if err != nil {
		return nil, nil, err
	}
	descriptors, err := agenttool.Describe(ctx, available)
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
