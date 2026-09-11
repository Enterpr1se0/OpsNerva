package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/transfer"
	"github.com/Enterpr1se0/opsnerva/internal/workspacefs"
)

// CreateAdminWorkspaceDirectory creates one directory. An existing directory is
// accepted so folder uploads can merge trees without overwriting existing files.
func (s *Service) CreateAdminWorkspaceDirectory(ctx context.Context, workspaceID, relativePath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	workspace, ok := s.workspaceByID(workspaceID)
	if !ok {
		return fmt.Errorf("workspace %q not found", workspaceID)
	}
	if workspace.Access != "read_write" {
		return fmt.Errorf("workspace %q is read_only", workspace.ID)
	}
	relativePath = strings.TrimSpace(relativePath)
	if relativePath == "" || relativePath == "." || len(relativePath) > 1024 {
		return fmt.Errorf("invalid workspace directory path")
	}
	target, err := resolveWorkspacePath(workspace, relativePath, true)
	if err != nil {
		return err
	}
	if err := os.Mkdir(target, 0o755); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		info, statErr := os.Stat(target)
		if statErr != nil {
			return statErr
		}
		if !info.IsDir() {
			return fmt.Errorf("workspace directory conflicts with an existing file")
		}
		return nil
	}
	return workspacefs.SyncDirectory(filepath.Dir(target))
}

func (s *Service) UploadWorkspaceFile(ctx context.Context, workspaceID, targetPath, originalFilename string, source io.Reader, actor string) (WorkspaceUploadResult, error) {
	workspace, ok := s.workspaceByID(workspaceID)
	if !ok {
		return WorkspaceUploadResult{}, fmt.Errorf("workspace %q not found", workspaceID)
	}
	return s.storeWorkspaceFile(ctx, workspace, targetPath, originalFilename, source, "", "workspace_file_uploaded", actor, 0, nil)
}

func (s *Service) validateWorkspaceFileDestination(workspace config.Workspace, targetPath, originalFilename string) (string, string, error) {
	if workspace.Access != "read_write" {
		return "", "", fmt.Errorf("workspace %q is read_only", workspace.ID)
	}
	targetPath = strings.TrimSpace(targetPath)
	if targetPath == "" {
		targetPath = filepath.Base(strings.ReplaceAll(originalFilename, "\\", "/"))
	}
	if targetPath == "" || targetPath == "." || len(targetPath) > 1024 {
		return "", "", fmt.Errorf("invalid workspace destination path")
	}
	target, err := resolveWorkspacePath(workspace, targetPath, true)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", fmt.Errorf("workspace destination parent directory does not exist")
		}
		return "", "", err
	}
	if _, err := os.Lstat(target); err == nil {
		return "", "", fmt.Errorf("workspace file already exists; choose a new path instead of overwriting it")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}
	parent := filepath.Dir(target)
	parentInfo, err := os.Stat(parent)
	if err != nil || !parentInfo.IsDir() {
		return "", "", fmt.Errorf("workspace destination parent directory does not exist")
	}
	return targetPath, target, nil
}

func (s *Service) storeWorkspaceFile(ctx context.Context, workspace config.Workspace, targetPath, originalFilename string, source io.Reader, expectedSHA256, eventType, actor string, total int64, progress transfer.Reporter) (WorkspaceUploadResult, error) {
	targetPath, target, err := s.validateWorkspaceFileDestination(workspace, targetPath, originalFilename)
	if err != nil {
		return WorkspaceUploadResult{}, err
	}
	parent := filepath.Dir(target)
	temporary, err := os.CreateTemp(parent, ".opsnerva-upload-*")
	if err != nil {
		return WorkspaceUploadResult{}, err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	digest := sha256.New()
	progressWriter := transfer.NewWriter(io.MultiWriter(temporary, digest), total, progress)
	written, copyErr := io.Copy(progressWriter, source)
	progressWriter.Finish()
	if copyErr != nil {
		temporary.Close()
		return WorkspaceUploadResult{}, copyErr
	}
	actualSHA256 := hex.EncodeToString(digest.Sum(nil))
	if expectedSHA256 != "" && actualSHA256 != expectedSHA256 {
		temporary.Close()
		return WorkspaceUploadResult{}, fmt.Errorf("remote download source version conflict: expected SHA256 %s, got %s", expectedSHA256, actualSHA256)
	}
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return WorkspaceUploadResult{}, err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return WorkspaceUploadResult{}, err
	}
	if err := temporary.Close(); err != nil {
		return WorkspaceUploadResult{}, err
	}
	if err := os.Link(temporaryPath, target); err != nil {
		if errors.Is(err, os.ErrExist) {
			return WorkspaceUploadResult{}, fmt.Errorf("workspace file already exists; choose a new path instead of overwriting it")
		}
		return WorkspaceUploadResult{}, err
	}
	if err := workspacefs.SyncDirectory(parent); err != nil {
		_ = os.Remove(target)
		return WorkspaceUploadResult{}, err
	}
	result := WorkspaceUploadResult{WorkspaceID: workspace.ID, Path: targetPath, Size: written, SHA256: actualSHA256}
	s.audit(ctx, "", eventType, actor, map[string]any{
		"workspace_id": workspace.ID, "path": targetPath, "size": written, "sha256": result.SHA256,
	})
	return result, nil
}
