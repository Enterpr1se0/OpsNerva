package agenttool

import (
	"context"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

type ResultPolicy interface {
	NormalizeExec(domain.ExecResult, error) (domain.ExecResult, error)
	Value(context.Context, string, any, error) (any, error)
}

type TaskReader interface {
	GetTaskForContext(context.Context, string) (domain.Task, domain.ExecResult, string, error)
}

type ExecutionService interface {
	TaskReader
	Submit(context.Context, domain.ExecRequest, string) (domain.ExecResult, error)
	StartTask(context.Context, domain.ExecRequest, string) (domain.Task, error)
}

type TaskService interface {
	TaskReader
	WaitTask(context.Context, string, int, int, time.Duration, string) (domain.Task, domain.ExecResult, string, bool, error)
	CancelTaskForContext(context.Context, string, string) error
}

type FileService interface {
	ReadFileAdvanced(context.Context, string, string, bool, int, int64, int, bool, string) (domain.ExecResult, error)
	SearchFile(context.Context, string, string, string, domain.FileSearchMatchMode, int, bool, string) (domain.ExecResult, error)
	ListFiles(context.Context, string, string, string) (domain.ExecResult, error)
	EditRemoteFile(context.Context, string, string, string, string, string, bool, string, string) (domain.ExecResult, error)
	TransferFileBetweenHosts(context.Context, string, string, string, string, string, string, int, string, string) (domain.ExecResult, error)
}

type TunnelService interface {
	StartSSHTunnel(context.Context, string, domain.SSHTunnelConfig, string, string) (domain.ExecResult, error)
	ListSSHTunnels() domain.SSHTunnelList
	StopSSHTunnel(context.Context, string, string) (domain.SSHTunnel, error)
}

type ShellService interface {
	StartSSHShell(context.Context, string, string, bool, int, int, string, string) (domain.ExecResult, error)
	WriteSSHShellPage(context.Context, string, string, string, time.Duration, int, string, string) (domain.SSHShellOutputPage, error)
	QuerySSHShellOutput(context.Context, string, string, *uint64, time.Duration, int, string, string) (domain.SSHShellOutputPage, error)
	ListSSHShells(context.Context, string, bool, string, string) (domain.SSHShellList, error)
	InterruptSSHShell(context.Context, string, string, string, string) (domain.SSHShell, error)
	CloseSSHShell(context.Context, string, string, string, string) (domain.SSHShell, error)
	ReadableSSHShellSnapshot(context.Context, domain.SSHShellSnapshot, uint64) (domain.SSHShellSnapshot, error)
}

type SSHDependencies struct {
	Execution ExecutionService
	Tasks     TaskService
	Files     FileService
	Tunnels   TunnelService
	Shells    ShellService
	Results   ResultPolicy
}

type SSH struct {
	dependencies SSHDependencies
}

func NewSSH(dependencies SSHDependencies) *SSH {
	return &SSH{dependencies: dependencies}
}

func (ssh *SSH) execResult(result domain.ExecResult, err error) (ExecResult, error) {
	normalized, normalizedErr := ssh.dependencies.Results.NormalizeExec(result, err)
	return ProjectExecResult(normalized), normalizedErr
}
