package agenttool

import (
	"context"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

type FileService interface {
	ReadFileAdvanced(context.Context, string, string, bool, int, int64, int, bool, string) (domain.ExecResult, error)
	SearchFile(context.Context, string, string, string, domain.FileSearchMatchMode, int, bool, string) (domain.ExecResult, error)
	ListFiles(context.Context, string, string, string) (domain.ExecResult, error)
	EditRemoteFile(context.Context, string, string, string, string, string, bool, string, string) (domain.ExecResult, error)
	TransferFileBetweenHosts(context.Context, string, string, string, string, string, string, int, string, string) (domain.ExecResult, error)
}

const defaultFileReadBytes = 128 << 10

func (ssh *SSH) RunFileRead(ctx context.Context, input FileReadInput, actor string) (ExecResult, error) {
	searching := input.Pattern != ""
	if searching && (input.MetadataOnly || input.FullContent || input.MaxBytes != 0 || input.OffsetBytes != 0 || input.TailLines != 0) {
		return ssh.execResult(domain.ExecResult{}, InvalidInput("invalid file read input: pattern cannot be combined with metadata_only, full_content, max_bytes, offset_bytes, or tail_lines"))
	}
	if searching && input.MatchMode == "" {
		return ssh.execResult(domain.ExecResult{}, InvalidInput("invalid file read input: match_mode is required with pattern"))
	}
	if !searching && (input.MatchMode != "" || input.ContextLines != 0) {
		return ssh.execResult(domain.ExecResult{}, InvalidInput("invalid file read input: match_mode and context_lines require pattern"))
	}
	if searching {
		result, err := ssh.dependencies.Files.SearchFile(ctx, input.HostID, input.Path, input.Pattern, input.MatchMode, input.ContextLines, input.Elevated, actor)
		return ssh.execResult(result, err)
	}
	if input.MetadataOnly && (input.FullContent || input.MaxBytes != 0 || input.OffsetBytes != 0 || input.TailLines != 0) {
		return ssh.execResult(domain.ExecResult{}, InvalidInput("invalid file read input: metadata_only cannot be combined with full_content, max_bytes, offset_bytes, or tail_lines"))
	}
	if input.FullContent && (input.MaxBytes != 0 || input.OffsetBytes != 0 || input.TailLines != 0) {
		return ssh.execResult(domain.ExecResult{}, InvalidInput("invalid file read input: full_content cannot be combined with max_bytes, offset_bytes, or tail_lines"))
	}
	if !input.MetadataOnly && !input.FullContent && input.MaxBytes == 0 && input.TailLines == 0 {
		input.MaxBytes = defaultFileReadBytes
	}
	result, err := ssh.dependencies.Files.ReadFileAdvanced(ctx, input.HostID, input.Path, input.MetadataOnly, input.MaxBytes, input.OffsetBytes, input.TailLines, input.Elevated, actor)
	return ssh.execResult(result, err)
}

func (ssh *SSH) RunFileList(ctx context.Context, input FileListInput, actor string) (ExecResult, error) {
	result, err := ssh.dependencies.Files.ListFiles(ctx, input.HostID, input.Path, actor)
	return ssh.execResult(result, err)
}

func (ssh *SSH) RunFileEdit(ctx context.Context, input FileEditInput, actor string) (ExecResult, error) {
	result, err := ssh.dependencies.Files.EditRemoteFile(
		ctx, input.HostID, input.Path, input.OldText, input.NewText, input.ValidatorID,
		input.Elevated, input.Reason, actor,
	)
	return ssh.execResult(result, err)
}

func (ssh *SSH) RunFileTransfer(ctx context.Context, input SSHFileTransferInput, actor string) (ExecResult, error) {
	result, err := ssh.dependencies.Files.TransferFileBetweenHosts(
		ctx, input.SourceHostID, input.SourcePath, input.ExpectedSHA256,
		input.DestinationHostID, input.DestinationPath, input.ExpectedDestinationSHA256,
		input.TimeoutSeconds, input.Reason, actor,
	)
	return ssh.execResult(result, err)
}
