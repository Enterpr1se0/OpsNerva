package agent

import (
	"context"

	"github.com/Enterpr1se0/opsnerva/internal/agenttool"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/service"
)

type agentToolResultPolicy struct{}

func (agentToolResultPolicy) NormalizeExec(result domain.ExecResult, err error) (domain.ExecResult, error) {
	return normalizeExecResult(result, err)
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

// MCP Server uses these entry points to share the Agent response policy while
// keeping transport registration outside the operational tool package.
func RunExecutionTool(ctx context.Context, svc *service.Service, request domain.ExecRequest, actor string) (agenttool.ExecResult, error) {
	return newSSHTools(svc).RunExecution(ctx, request, actor)
}

func RunTaskTool(ctx context.Context, svc *service.Service, input agenttool.TaskInput, actor string) (agenttool.ExecResult, error) {
	return newSSHTools(svc).RunTask(ctx, input, actor)
}

func RunFileReadTool(ctx context.Context, svc *service.Service, input agenttool.FileReadInput, actor string) (agenttool.ExecResult, error) {
	return newSSHTools(svc).RunFileRead(ctx, input, actor)
}

func RunFileListTool(ctx context.Context, svc *service.Service, input agenttool.FileListInput, actor string) (agenttool.ExecResult, error) {
	return newSSHTools(svc).RunFileList(ctx, input, actor)
}

func RunFileEditTool(ctx context.Context, svc *service.Service, input agenttool.FileEditInput, actor string) (agenttool.ExecResult, error) {
	return newSSHTools(svc).RunFileEdit(ctx, input, actor)
}

func RunFileTransferTool(ctx context.Context, svc *service.Service, input agenttool.SSHFileTransferInput, actor string) (agenttool.ExecResult, error) {
	return newSSHTools(svc).RunFileTransfer(ctx, input, actor)
}

func RunSSHTunnelTool(ctx context.Context, svc *service.Service, input agenttool.SSHTunnelInput, actor string) (any, error) {
	return newSSHTools(svc).RunTunnel(ctx, input, actor)
}

func RunSSHShellTool(ctx context.Context, svc *service.Service, input agenttool.SSHShellInput, actor string) (any, error) {
	return newSSHTools(svc).RunShell(ctx, service.SessionIDFromContext(ctx), input, actor)
}
