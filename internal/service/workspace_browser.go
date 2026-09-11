package service

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/workspacefs"
	"github.com/fsnotify/fsnotify"
)

type WorkspaceUploadResult struct {
	WorkspaceID string `json:"workspace_id"`
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
}

type WorkspaceFileEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Size int64  `json:"size,omitempty"`
}

type WorkspaceFileList struct {
	WorkspaceID string               `json:"workspace_id"`
	Path        string               `json:"path"`
	Entries     []WorkspaceFileEntry `json:"entries"`
}

type WorkspaceFileChange struct {
	WorkspaceID string `json:"workspace_id"`
	Path        string `json:"path"`
}

type WorkspaceFileWatch struct {
	Changes <-chan WorkspaceFileChange
	Errors  <-chan error
}

type WorkspaceFilePreview struct {
	WorkspaceID string `json:"workspace_id"`
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	Content     string `json:"content,omitempty"`
	Binary      bool   `json:"binary,omitempty"`
	Truncated   bool   `json:"truncated,omitempty"`
}

type WorkspaceFileDownload struct {
	WorkspaceID string
	Path        string
	Name        string
	Size        int64
	Reader      io.ReadCloser
}

type WorkspaceDeleteResult struct {
	WorkspaceID string `json:"workspace_id"`
	Path        string `json:"path"`
	Type        string `json:"type"`
	Size        int64  `json:"size,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
}

const workspaceWatchDebounce = 120 * time.Millisecond

func (s *Service) ListAdminWorkspaceFiles(workspaceID, relativePath string) (WorkspaceFileList, error) {
	workspace, ok := s.workspaceByID(workspaceID)
	if !ok {
		return WorkspaceFileList{}, fmt.Errorf("workspace %q not found", workspaceID)
	}
	relativePath = strings.TrimSpace(relativePath)
	if relativePath == "" {
		relativePath = "."
	}
	entries, err := workspacefs.New(workspace.Root).ReadDir(relativePath)
	if err != nil {
		return WorkspaceFileList{}, workspaceFileError(err)
	}
	result := WorkspaceFileList{WorkspaceID: workspace.ID, Path: relativePath, Entries: make([]WorkspaceFileEntry, 0, len(entries))}
	for _, info := range entries {
		kind := "file"
		if info.IsDir() {
			kind = "directory"
		} else if !info.Mode().IsRegular() {
			continue
		}
		result.Entries = append(result.Entries, WorkspaceFileEntry{Name: info.Name(), Type: kind, Size: info.Size()})
	}
	sort.Slice(result.Entries, func(i, j int) bool {
		if result.Entries[i].Type != result.Entries[j].Type {
			return result.Entries[i].Type == "directory"
		}
		return strings.ToLower(result.Entries[i].Name) < strings.ToLower(result.Entries[j].Name)
	})
	return result, nil
}

// WatchAdminWorkspaceFiles subscribes to operating-system file notifications for
// one visible Workspace directory. It deliberately watches only that directory:
// changes below a child directory cannot alter the current listing, and avoiding
// recursive watches keeps large projects from consuming one watch per folder.
func (s *Service) WatchAdminWorkspaceFiles(ctx context.Context, workspaceID, relativePath string) (WorkspaceFileWatch, error) {
	workspace, ok := s.workspaceByID(workspaceID)
	if !ok {
		return WorkspaceFileWatch{}, fmt.Errorf("workspace %q not found", workspaceID)
	}
	relativePath = strings.TrimSpace(relativePath)
	if relativePath == "" {
		relativePath = "."
	}
	directory, err := resolveWorkspacePath(workspace, relativePath, false)
	if err != nil {
		return WorkspaceFileWatch{}, err
	}
	info, err := os.Stat(directory)
	if err != nil {
		return WorkspaceFileWatch{}, err
	}
	if !info.IsDir() {
		return WorkspaceFileWatch{}, fmt.Errorf("workspace watch target is not a directory")
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return WorkspaceFileWatch{}, fmt.Errorf("create workspace file watcher: %w", err)
	}
	if err := watcher.Add(directory); err != nil {
		_ = watcher.Close()
		return WorkspaceFileWatch{}, fmt.Errorf("watch workspace directory: %w", err)
	}

	changes := make(chan WorkspaceFileChange, 1)
	errors := make(chan error, 1)
	go func() {
		defer close(changes)
		defer close(errors)
		defer watcher.Close()

		var debounce *time.Timer
		var debounceC <-chan time.Time
		stopDebounce := func() {
			if debounce != nil && !debounce.Stop() {
				select {
				case <-debounce.C:
				default:
				}
			}
		}
		defer stopDebounce()
		schedule := func() {
			if debounce == nil {
				debounce = time.NewTimer(workspaceWatchDebounce)
			} else {
				stopDebounce()
				debounce.Reset(workspaceWatchDebounce)
			}
			debounceC = debounce.C
		}

		for {
			select {
			case <-ctx.Done():
				return
			case event, open := <-watcher.Events:
				if !open {
					return
				}
				if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) != 0 {
					schedule()
				}
			case watchErr, open := <-watcher.Errors:
				if !open {
					return
				}
				select {
				case errors <- fmt.Errorf("workspace file watcher failed: %w", watchErr):
				default:
				}
				return
			case <-debounceC:
				debounceC = nil
				select {
				case changes <- WorkspaceFileChange{WorkspaceID: workspace.ID, Path: relativePath}:
				default:
				}
			}
		}
	}()

	return WorkspaceFileWatch{Changes: changes, Errors: errors}, nil
}

func (s *Service) PreviewAdminWorkspaceFile(workspaceID, relativePath string) (WorkspaceFilePreview, error) {
	workspace, ok := s.workspaceByID(workspaceID)
	if !ok {
		return WorkspaceFilePreview{}, fmt.Errorf("workspace %q not found", workspaceID)
	}
	preview, err := workspacefs.New(workspace.Root).Preview(relativePath)
	if err != nil {
		return WorkspaceFilePreview{}, workspaceFileError(err)
	}
	return WorkspaceFilePreview{WorkspaceID: workspace.ID, Path: strings.TrimSpace(relativePath),
		Size: preview.Size, SHA256: preview.SHA256, Content: preview.Content,
		Binary: preview.Binary, Truncated: preview.Truncated}, nil
}

func (s *Service) OpenAdminWorkspaceFile(workspaceID, relativePath string) (WorkspaceFileDownload, error) {
	workspace, ok := s.workspaceByID(workspaceID)
	if !ok {
		return WorkspaceFileDownload{}, fmt.Errorf("workspace %q not found", workspaceID)
	}
	download, err := workspacefs.New(workspace.Root).OpenDownload(relativePath)
	if err != nil {
		return WorkspaceFileDownload{}, workspaceFileError(err)
	}
	return WorkspaceFileDownload{WorkspaceID: workspace.ID, Path: filepath.ToSlash(filepath.Clean(strings.TrimSpace(relativePath))),
		Name: download.Info.Name(), Size: download.Info.Size(), Reader: download.Reader}, nil
}

func (s *Service) SaveAdminWorkspaceTextFile(ctx context.Context, workspaceID, relativePath, content string) (WorkspaceUploadResult, error) {
	workspace, ok := s.workspaceByID(workspaceID)
	if !ok {
		return WorkspaceUploadResult{}, fmt.Errorf("workspace %q not found", workspaceID)
	}
	if workspace.Access != "read_write" {
		return WorkspaceUploadResult{}, fmt.Errorf("workspace %q is read_only", workspace.ID)
	}
	written, err := workspacefs.New(workspace.Root).SaveText(ctx, relativePath, content)
	if err != nil {
		return WorkspaceUploadResult{}, workspaceFileError(err)
	}
	return WorkspaceUploadResult{WorkspaceID: workspace.ID, Path: strings.TrimSpace(relativePath), Size: written.Size, SHA256: written.SHA256}, nil
}

func (s *Service) DeleteAdminWorkspaceEntry(ctx context.Context, workspaceID, relativePath, actor string) (WorkspaceDeleteResult, error) {
	workspace, ok := s.workspaceByID(workspaceID)
	if !ok {
		return WorkspaceDeleteResult{}, fmt.Errorf("workspace %q not found", workspaceID)
	}
	return s.deleteWorkspaceEntry(ctx, workspace, relativePath, true, actor)
}
