package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/workspacefs"
)

func (s *Service) DeleteWorkspaceEntry(ctx context.Context, workspaceID, relativePath string, recursive bool, reason, actor string) (domain.ExecResult, error) {
	workspace, ok := s.workspaces.Get(workspaceID)
	if !ok {
		return domain.ExecResult{}, fmt.Errorf("workspace %q not found", workspaceID)
	}
	relativePath = strings.TrimSpace(relativePath)
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return domain.ExecResult{}, fmt.Errorf("reason is required")
	}
	if workspace.Access != "read_write" {
		return domain.ExecResult{}, fmt.Errorf("workspace %q is read_only", workspace.ID)
	}
	if err := workspacefs.New(workspace.Root).ValidateDeleteTarget(relativePath, recursive); err != nil {
		return domain.ExecResult{}, workspaceFileError(err)
	}
	host, err := s.workspaceHost(ctx, workspaceID)
	if err != nil {
		return domain.ExecResult{}, err
	}
	return s.Submit(ctx, domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecWorkspaceDelete, WorkspaceID: workspaceID,
		RelativePath: relativePath, Recursive: recursive, Reason: reason,
	}, actor)
}

func (s *Service) deleteWorkspaceEntry(ctx context.Context, workspace config.Workspace, relativePath string, recursive bool, actor string) (WorkspaceDeleteResult, error) {
	if workspace.Access != "read_write" {
		return WorkspaceDeleteResult{}, fmt.Errorf("workspace %q is read_only", workspace.ID)
	}
	deleted, err := workspacefs.New(workspace.Root).Delete(ctx, relativePath, recursive)
	if err != nil {
		return WorkspaceDeleteResult{}, workspaceFileError(err)
	}
	result := WorkspaceDeleteResult{
		WorkspaceID: workspace.ID, Path: deleted.Path, Type: deleted.Type, Size: deleted.Size, SHA256: deleted.SHA256,
	}
	eventType := "workspace_file_deleted"
	if result.Type == "directory" {
		eventType = "workspace_directory_deleted"
	}
	s.audit(ctx, "", eventType, actor, map[string]any{
		"workspace_id": workspace.ID, "path": result.Path, "type": result.Type, "size": result.Size, "sha256": result.SHA256, "permanent": true,
	})
	return result, nil
}
