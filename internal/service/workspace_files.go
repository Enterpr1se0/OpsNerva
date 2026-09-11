package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"
	"github.com/Enterpr1se0/opsnerva/internal/workspacefs"
)

func (s *Service) DeleteWorkspaceEntry(ctx context.Context, workspaceID, relativePath string, recursive bool, reason, actor string) (domain.ExecResult, error) {
	workspace, ok := s.workspaceByID(workspaceID)
	if !ok {
		return domain.ExecResult{}, fmt.Errorf("workspace %q not found", workspaceID)
	}
	relativePath = strings.TrimSpace(relativePath)
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return domain.ExecResult{}, fmt.Errorf("reason is required")
	}
	if _, _, err := s.validateWorkspaceDeleteTarget(workspace, relativePath, recursive); err != nil {
		return domain.ExecResult{}, err
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

func (s *Service) validateWorkspaceDeleteTarget(workspace config.Workspace, relativePath string, recursive bool) (string, os.FileInfo, error) {
	if workspace.Access != "read_write" {
		return "", nil, fmt.Errorf("workspace %q is read_only", workspace.ID)
	}
	relativePath = strings.TrimSpace(relativePath)
	if relativePath == "" || relativePath == "." {
		return "", nil, fmt.Errorf("Workspace root cannot be deleted")
	}
	path, err := resolveWorkspacePath(workspace, relativePath, false)
	if err != nil {
		return "", nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", nil, err
	}
	if !info.Mode().IsRegular() && !info.IsDir() {
		return "", nil, fmt.Errorf("only regular Workspace files and directories can be deleted")
	}
	if info.IsDir() && !recursive {
		entries, readErr := os.ReadDir(path)
		if readErr != nil {
			return "", nil, readErr
		}
		if len(entries) != 0 {
			return "", nil, fmt.Errorf("workspace directory is not empty; set recursive=true to delete it")
		}
	}
	return path, info, nil
}

func (s *Service) deleteWorkspaceEntry(ctx context.Context, workspace config.Workspace, relativePath string, recursive bool, actor string) (WorkspaceDeleteResult, error) {
	path, info, err := s.validateWorkspaceDeleteTarget(workspace, relativePath, recursive)
	if err != nil {
		return WorkspaceDeleteResult{}, err
	}
	entryType := "directory"
	var size int64
	var sha256Sum string
	if info.Mode().IsRegular() {
		entryType = "file"
		size = info.Size()
		file, err := os.Open(path)
		if err != nil {
			return WorkspaceDeleteResult{}, err
		}
		digest := sha256.New()
		_, copyErr := io.Copy(digest, file)
		closeErr := file.Close()
		if copyErr != nil {
			return WorkspaceDeleteResult{}, copyErr
		}
		if closeErr != nil {
			return WorkspaceDeleteResult{}, closeErr
		}
		sha256Sum = hex.EncodeToString(digest.Sum(nil))
	}
	normalizedPath := filepath.ToSlash(filepath.Clean(relativePath))
	if info.IsDir() {
		if recursive {
			err = os.RemoveAll(path)
		} else {
			err = os.Remove(path)
		}
	} else {
		err = os.Remove(path)
	}
	if err != nil {
		return WorkspaceDeleteResult{}, err
	}
	if err := workspacefs.SyncDirectory(filepath.Dir(path)); err != nil {
		return WorkspaceDeleteResult{}, err
	}
	result := WorkspaceDeleteResult{
		WorkspaceID: workspace.ID, Path: normalizedPath, Type: entryType, Size: size, SHA256: sha256Sum,
	}
	eventType := "workspace_file_deleted"
	if entryType == "directory" {
		eventType = "workspace_directory_deleted"
	}
	s.audit(ctx, "", eventType, actor, map[string]any{
		"workspace_id": workspace.ID, "path": normalizedPath, "type": entryType, "size": size, "sha256": result.SHA256, "permanent": true,
	})
	return result, nil
}

func (s *Service) ReadWorkspaceFile(ctx context.Context, workspaceID, relativePath string, maxBytes int, offset int64, actor string) (domain.ExecResult, error) {
	return s.ReadWorkspaceFileAdvanced(ctx, workspaceID, relativePath, maxBytes, offset, 0, actor)
}

func (s *Service) ReadWorkspaceFileAdvanced(ctx context.Context, workspaceID, relativePath string, maxBytes int, offset int64, tailLines int, actor string) (domain.ExecResult, error) {
	if maxBytes < 0 || tailLines < 0 || (offset != 0 && tailLines != 0) {
		return domain.ExecResult{}, fmt.Errorf("invalid Workspace file read range: max_bytes and tail_lines must be non-negative; tail_lines cannot be combined with offset_bytes")
	}
	workspace, ok := s.workspaceByID(workspaceID)
	if !ok {
		return domain.ExecResult{}, fmt.Errorf("workspace %q not found", workspaceID)
	}
	if _, err := resolveWorkspacePath(workspace, relativePath, false); err != nil {
		return domain.ExecResult{}, err
	}
	host, err := s.workspaceHost(ctx, workspaceID)
	if err != nil {
		return domain.ExecResult{}, err
	}
	result, err := s.Submit(ctx, domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecWorkspaceRead, WorkspaceID: workspaceID, RelativePath: relativePath,
		MaxBytes: maxBytes, OffsetBytes: offset, TailLines: tailLines, Reason: "read a bounded file from an allowlisted workspace",
	}, actor)
	return result, err
}

func (s *Service) ListWorkspaceFiles(ctx context.Context, workspaceID, relativePath, actor string) (domain.ExecResult, error) {
	workspace, ok := s.workspaceByID(workspaceID)
	if !ok {
		return domain.ExecResult{}, fmt.Errorf("workspace %q not found", workspaceID)
	}
	relativePath = workspacefs.NormalizeRelativePath(relativePath)
	if _, err := resolveWorkspacePath(workspace, relativePath, false); err != nil {
		return domain.ExecResult{}, err
	}
	host, err := s.workspaceHost(ctx, workspaceID)
	if err != nil {
		return domain.ExecResult{}, err
	}
	return s.Submit(ctx, domain.ExecRequest{HostID: host.ID, Mode: domain.ExecWorkspaceDirectoryList, WorkspaceID: workspaceID, RelativePath: relativePath, Reason: "list an allowlisted workspace directory"}, actor)
}

func (s *Service) SearchWorkspace(ctx context.Context, workspaceID, relativePath, pattern string, matchMode domain.FileSearchMatchMode, contextLines int, actor string) (domain.ExecResult, error) {
	workspace, ok := s.workspaceByID(workspaceID)
	if !ok {
		return domain.ExecResult{}, fmt.Errorf("workspace %q not found", workspaceID)
	}
	if _, err := resolveWorkspacePath(workspace, relativePath, false); err != nil {
		return domain.ExecResult{}, err
	}
	host, err := s.workspaceHost(ctx, workspaceID)
	if err != nil {
		return domain.ExecResult{}, err
	}
	if err := validateFileSearchInput(pattern, matchMode, contextLines); err != nil {
		return domain.ExecResult{}, fmt.Errorf("invalid Workspace search: %w", err)
	}
	result, err := s.Submit(ctx, domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecWorkspaceSearch, WorkspaceID: workspaceID, RelativePath: relativePath,
		SearchPattern: pattern, SearchMatchMode: matchMode, ContextLines: contextLines, Reason: "search text in an allowlisted workspace file",
	}, actor)
	decorateFileSearchResult(&result, pattern, matchMode, contextLines)
	return result, err
}

func isWorkspaceMode(mode domain.ExecMode) bool {
	switch mode {
	case domain.ExecWorkspaceRead, domain.ExecWorkspaceDirectoryList, domain.ExecWorkspaceSearch, domain.ExecWorkspaceEdit, domain.ExecWorkspaceDelete, domain.ExecWorkspaceShell, domain.ExecWorkspaceShellStart:
		return true
	default:
		return false
	}
}

func (s *Service) executeWorkspace(ctx context.Context, req domain.ExecRequest, actor string, stream func(string, []byte)) (sshx.RawResult, error) {
	started := time.Now()
	workspace, ok := s.workspaceByID(req.WorkspaceID)
	if !ok {
		return sshx.RawResult{}, fmt.Errorf("workspace %q not found", req.WorkspaceID)
	}
	result := sshx.RawResult{ExitCode: 0}
	if req.Mode == domain.ExecWorkspaceShell {
		result, err := s.executeWorkspaceShell(ctx, workspace, req, stream)
		result.Duration = time.Since(started)
		return redactWorkspaceResult(result, err, workspace.Root)
	}
	files := workspacefs.New(workspace.Root)
	var err error
	switch req.Mode {
	case domain.ExecWorkspaceRead:
		result.Stdout, err = readWorkspaceFile(files, req.RelativePath, req.MaxBytes, req.OffsetBytes, req.TailLines)
	case domain.ExecWorkspaceDirectoryList:
		result.Stdout, err = listWorkspaceDirectory(files, req.RelativePath)
	case domain.ExecWorkspaceSearch:
		result.Stdout, err = files.Search(req.RelativePath, req.SearchPattern, req.SearchMatchMode, req.ContextLines)
	case domain.ExecWorkspaceEdit:
		path, pathErr := resolveWorkspacePath(workspace, req.RelativePath, true)
		if pathErr != nil {
			return sshx.RawResult{}, pathErr
		}
		if workspace.Access != "read_write" {
			err = fmt.Errorf("workspace %q is read_only", workspace.ID)
			break
		}
		result, err = s.editWorkspaceFile(ctx, workspace, path, req)
	case domain.ExecWorkspaceDelete:
		deleted, deleteErr := s.deleteWorkspaceEntry(ctx, workspace, req.RelativePath, req.Recursive, actor)
		if deleteErr != nil {
			err = deleteErr
			break
		}
		result.Stdout, err = json.Marshal(deleted)
	default:
		err = fmt.Errorf("unsupported workspace operation %q", req.Mode)
	}
	result.Duration = time.Since(started)
	return redactWorkspaceResult(result, err, workspace.Root)
}

func redactWorkspaceResult(result sshx.RawResult, err error, root string) (sshx.RawResult, error) {
	roots := workspaceRedactionRoots(root)
	result.Stdout = []byte(redactWorkspacePaths(string(result.Stdout), roots))
	result.Stderr = []byte(redactWorkspacePaths(string(result.Stderr), roots))
	if err != nil && result.ExitCode == 0 {
		result.ExitCode = 1
		result.Stderr = []byte(redactWorkspacePaths(err.Error(), roots))
	}
	if err != nil {
		err = fmt.Errorf("%s", redactWorkspacePaths(err.Error(), roots))
	}
	return result, err
}

func workspaceRedactionRoots(root string) []string {
	roots := []string{root}
	if resolved, resolveErr := filepath.EvalSymlinks(root); resolveErr == nil && resolved != root {
		roots = append(roots, resolved)
	}
	return roots
}

func redactWorkspacePaths(value string, roots []string) string {
	for _, candidate := range roots {
		if candidate != "" {
			value = strings.ReplaceAll(value, candidate, "$WORKSPACE")
		}
	}
	return value
}

// workspaceFileError translates filesystem input errors into the shared tool contract.
// Filesystem I/O and access-denied errors keep their original classification.
func workspaceFileError(err error) error {
	var pathErr *workspacefs.InvalidPathError
	if errors.As(err, &pathErr) {
		return asInputValidationError(err)
	}
	return err
}

func resolveWorkspacePath(workspace config.Workspace, relative string, allowMissing bool) (string, error) {
	resolved, err := workspacefs.New(workspace.Root).Resolve(relative, allowMissing)
	return resolved, workspaceFileError(err)
}

func readWorkspaceFile(files *workspacefs.FS, relative string, maxBytes int, offset int64, tailLines int) ([]byte, error) {
	result, err := files.Read(relative, maxBytes, offset, tailLines)
	if err != nil {
		return nil, workspaceFileError(err)
	}
	info := result.Info
	metadata := fmt.Sprintf("%s\n%d\t%o\t%s\t%s\t%d\t%d\n%s  %s\n%s\n",
		fileMetaMarker, info.Size(), info.Mode().Perm(), "local", "local", info.ModTime().Unix(), result.Offset, result.SHA256, relative, fileContentMarker)
	return append([]byte(metadata), result.Content...), nil
}

func listWorkspaceDirectory(files *workspacefs.FS, relative string) ([]byte, error) {
	entries, err := files.ReadDir(relative)
	if err != nil {
		return nil, workspaceFileError(err)
	}
	result := make([]WorkspaceFileEntry, 0, len(entries))
	for _, entry := range entries {
		kind := "file"
		if entry.IsDir() {
			kind = "directory"
		}
		result = append(result, WorkspaceFileEntry{Name: entry.Name(), Type: kind, Size: entry.Size()})
	}
	return json.Marshal(map[string]any{"entries": result})
}
