package service

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/transfer"
	"github.com/Enterpr1se0/opsnerva/internal/workspacefs"
)

func (s *Service) CreateAdminWorkspaceDirectory(ctx context.Context, workspaceID, relativePath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	workspace, ok := s.workspaces.Get(workspaceID)
	if !ok {
		return fmt.Errorf("workspace %q not found", workspaceID)
	}
	if workspace.Access != "read_write" {
		return fmt.Errorf("workspace %q is read_only", workspace.ID)
	}
	return workspaceFileError(workspacefs.New(workspace.Root).CreateDirectory(ctx, relativePath))
}

func (s *Service) UploadWorkspaceFile(ctx context.Context, workspaceID, targetPath, originalFilename string, source io.Reader, actor string) (WorkspaceUploadResult, error) {
	workspace, ok := s.workspaces.Get(workspaceID)
	if !ok {
		return WorkspaceUploadResult{}, fmt.Errorf("workspace %q not found", workspaceID)
	}
	return s.storeWorkspaceFile(ctx, workspace, targetPath, originalFilename, source, "", "workspace_file_uploaded", actor, 0, nil)
}

func (s *Service) validateWorkspaceFileDestination(workspace config.Workspace, targetPath, originalFilename string) (string, error) {
	if workspace.Access != "read_write" {
		return "", fmt.Errorf("workspace %q is read_only", workspace.ID)
	}
	relative, err := workspacefs.New(workspace.Root).ValidateUploadDestination(targetPath, originalFilename)
	return relative, workspaceFileError(err)
}

func (s *Service) storeWorkspaceFile(ctx context.Context, workspace config.Workspace, targetPath, originalFilename string, source io.Reader, expectedSHA256, eventType, actor string, total int64, progress transfer.Reporter) (WorkspaceUploadResult, error) {
	if workspace.Access != "read_write" {
		return WorkspaceUploadResult{}, fmt.Errorf("workspace %q is read_only", workspace.ID)
	}
	stored, err := workspacefs.New(workspace.Root).Upload(ctx, targetPath, originalFilename, source, workspacefs.UploadOptions{
		ExpectedSHA256: expectedSHA256, Total: total, Progress: progress,
	})
	if err != nil {
		var mismatch *workspacefs.SHA256MismatchError
		if errors.As(err, &mismatch) {
			err = fmt.Errorf("remote download %w", err)
		}
		return WorkspaceUploadResult{}, workspaceFileError(err)
	}
	result := WorkspaceUploadResult{WorkspaceID: workspace.ID, Path: stored.Path, Size: stored.Size, SHA256: stored.SHA256}
	s.audit(ctx, "", eventType, actor, map[string]any{
		"workspace_id": workspace.ID, "path": result.Path, "size": result.Size, "sha256": result.SHA256,
	})
	return result, nil
}
