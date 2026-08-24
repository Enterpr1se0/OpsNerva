package service

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fsnotify/fsnotify"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/ids"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"
	"github.com/Enterpr1se0/opsnerva/internal/store"
)

type WorkspaceCapability struct {
	ID           string   `json:"id"`
	Access       string   `json:"access"`
	Shell        bool     `json:"shell"`
	ShellBackend string   `json:"shell_backend,omitempty"`
	ShellName    string   `json:"shell_name,omitempty"`
	Validators   []string `json:"validators,omitempty"`
}

type AdminWorkspaceCapability struct {
	WorkspaceCapability
}

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

const maxAdminWorkspacePreviewBytes int64 = 1 << 20

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

const (
	maxWorkspaceTextFileBytes = 100 << 20
	workspaceWatchDebounce    = 120 * time.Millisecond
)

var workspaceIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

func (s *Service) InitializeWorkspaces(ctx context.Context, workspaceRoot string) error {
	workspaceRoot = filepath.Clean(strings.TrimSpace(workspaceRoot))
	if workspaceRoot == "." || !filepath.IsAbs(workspaceRoot) {
		return fmt.Errorf("workspace root must be absolute")
	}
	if filepath.Dir(workspaceRoot) == workspaceRoot {
		return fmt.Errorf("a filesystem root cannot be used as the workspace directory")
	}
	if s.dataDir != "" {
		dataRoot, err := filepath.Abs(s.dataDir)
		if err != nil {
			return err
		}
		if localPathContains(workspaceRoot, dataRoot) || localPathContains(dataRoot, workspaceRoot) {
			return fmt.Errorf("workspace directory cannot overlap the application data directory")
		}
	}
	if err := ensureWorkspaceDirectory(workspaceRoot); err != nil {
		return fmt.Errorf("prepare workspace directory: %w", err)
	}
	if err := s.store.InitializeWorkspaces(ctx); err != nil {
		return err
	}
	stored, err := s.store.ListWorkspaces(ctx)
	if err != nil {
		return err
	}
	loaded := make(map[string]config.Workspace, len(stored))
	for _, workspace := range stored {
		candidate := config.Workspace{ID: workspace.ID, Root: filepath.Join(workspaceRoot, workspace.ID), Access: workspace.Access}
		if err := validateWorkspaceIdentity(candidate.ID, candidate.Access); err != nil {
			return fmt.Errorf("stored workspace %q is invalid: %w", workspace.ID, err)
		}
		if err := ensureWorkspaceDirectory(candidate.Root); err != nil {
			return fmt.Errorf("prepare workspace %q: %w", candidate.ID, err)
		}
		loaded[candidate.ID] = candidate
	}
	s.workspaceMu.Lock()
	s.workspaceRoot = workspaceRoot
	s.workspaces = loaded
	s.workspaceMu.Unlock()
	return nil
}

func (s *Service) CreateAdminWorkspace(ctx context.Context, input domain.WorkspaceInput, actor string) (AdminWorkspaceCapability, error) {
	workspace := config.Workspace{ID: strings.TrimSpace(input.ID), Access: strings.TrimSpace(input.Access)}
	if workspace.Access == "" {
		workspace.Access = "read_only"
	}
	s.workspaceMu.RLock()
	_, exists := s.workspaces[workspace.ID]
	for id := range s.workspaces {
		exists = exists || strings.EqualFold(id, workspace.ID)
	}
	workspace.Root = filepath.Join(s.workspaceRoot, workspace.ID)
	s.workspaceMu.RUnlock()
	if exists {
		return AdminWorkspaceCapability{}, fmt.Errorf("workspace %q already exists", workspace.ID)
	}
	if err := validateWorkspaceIdentity(workspace.ID, workspace.Access); err != nil {
		return AdminWorkspaceCapability{}, err
	}
	if err := ensureWorkspaceDirectory(workspace.Root); err != nil {
		return AdminWorkspaceCapability{}, err
	}
	now := time.Now().UTC()
	if err := s.store.CreateWorkspace(ctx, domain.Workspace{ID: workspace.ID, Access: workspace.Access, CreatedAt: now, UpdatedAt: now}); err != nil {
		return AdminWorkspaceCapability{}, err
	}
	s.workspaceMu.Lock()
	s.workspaces[workspace.ID] = workspace
	s.workspaceMu.Unlock()
	s.audit(ctx, "", "workspace_created", actor, map[string]any{"workspace_id": workspace.ID, "access": workspace.Access})
	return s.adminWorkspaceCapability(workspace), nil
}

func (s *Service) UpdateAdminWorkspace(ctx context.Context, id string, input domain.WorkspaceInput, actor string) (AdminWorkspaceCapability, error) {
	id = strings.TrimSpace(id)
	workspace := config.Workspace{ID: id, Access: strings.TrimSpace(input.Access)}
	if input.ID != "" && strings.TrimSpace(input.ID) != id {
		return AdminWorkspaceCapability{}, fmt.Errorf("workspace id cannot be changed")
	}
	s.workspaceMu.RLock()
	current, exists := s.workspaces[id]
	s.workspaceMu.RUnlock()
	if !exists {
		return AdminWorkspaceCapability{}, fmt.Errorf("workspace %q not found", id)
	}
	if workspace.Access != current.Access && s.hasActiveWorkspaceShell(id) {
		return AdminWorkspaceCapability{}, fmt.Errorf("workspace %q has an active terminal", id)
	}
	workspace.Root = current.Root
	if err := validateWorkspaceIdentity(workspace.ID, workspace.Access); err != nil {
		return AdminWorkspaceCapability{}, err
	}
	if err := ensureWorkspaceDirectory(workspace.Root); err != nil {
		return AdminWorkspaceCapability{}, err
	}
	if err := s.store.UpdateWorkspace(ctx, domain.Workspace{ID: id, Access: workspace.Access, UpdatedAt: time.Now().UTC()}); err != nil {
		return AdminWorkspaceCapability{}, err
	}
	s.workspaceMu.Lock()
	s.workspaces[id] = workspace
	s.workspaceMu.Unlock()
	s.audit(ctx, "", "workspace_updated", actor, map[string]any{"workspace_id": id, "access": workspace.Access})
	return s.adminWorkspaceCapability(workspace), nil
}

func (s *Service) DeleteAdminWorkspace(ctx context.Context, id, actor string) error {
	id = strings.TrimSpace(id)
	workspace, ok := s.workspaceByID(id)
	if !ok {
		return fmt.Errorf("workspace %q not found", id)
	}
	if s.hasActiveWorkspaceShell(id) {
		return fmt.Errorf("workspace %q has an active terminal", id)
	}
	if err := s.store.DeleteWorkspace(ctx, id); err != nil {
		return err
	}
	s.workspaceMu.Lock()
	delete(s.workspaces, id)
	workspaceRoot := s.workspaceRoot
	s.workspaceMu.Unlock()
	// Unregister first so the agent loses access, then delete the directory.
	// Only ever remove a path strictly inside the managed workspace root.
	var removeErr error
	filesRemoved := false
	if workspace.Root != "" && workspaceRoot != "" && filepath.Clean(workspace.Root) != filepath.Clean(workspaceRoot) && localPathContains(workspace.Root, workspaceRoot) {
		removeErr = os.RemoveAll(workspace.Root)
		filesRemoved = removeErr == nil
	}
	s.audit(ctx, "", "workspace_removed", actor, map[string]any{"workspace_id": id, "root": workspace.Root, "files_removed": filesRemoved})
	if removeErr != nil {
		return fmt.Errorf("workspace %q was unregistered, but its directory could not be fully removed: %w", id, removeErr)
	}
	return nil
}

func validateWorkspaceIdentity(id, access string) error {
	if !workspaceIDPattern.MatchString(id) || id == "." || id == ".." || strings.HasSuffix(id, ".") || isReservedWindowsWorkspaceID(id) {
		return fmt.Errorf("workspace id must use 1-64 letters, numbers, dots, underscores, or hyphens")
	}
	if access != "read_only" && access != "read_write" {
		return fmt.Errorf("workspace access must be read_only or read_write")
	}
	return nil
}

func isReservedWindowsWorkspaceID(id string) bool {
	base := strings.ToUpper(strings.SplitN(id, ".", 2)[0])
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" {
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) {
		return base[3] >= '1' && base[3] <= '9'
	}
	return false
}

func ensureWorkspaceDirectory(path string) error {
	if err := rejectWorkspaceSymlinks(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create directory: %w", err)
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return fmt.Errorf("inspect directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("path must be a real directory, not a file or symbolic link")
	}
	return rejectWorkspaceSymlinks(path)
}

func rejectWorkspaceSymlinks(path string) error {
	clean := filepath.Clean(path)
	volume := filepath.VolumeName(clean)
	current := volume
	remainder := strings.TrimPrefix(clean, volume)
	if filepath.IsAbs(clean) {
		current += string(filepath.Separator)
		remainder = strings.TrimLeft(remainder, `/\\`)
	}
	for _, component := range strings.Split(remainder, string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect path component: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("workspace directories cannot contain symbolic links")
		}
	}
	return nil
}

func localPathContains(path, root string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		relative = strings.ToLower(relative)
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func cloneWorkspaces(source map[string]config.Workspace) map[string]config.Workspace {
	result := make(map[string]config.Workspace, len(source))
	for id, workspace := range source {
		result[id] = workspace
	}
	return result
}

func (s *Service) workspaceByID(id string) (config.Workspace, bool) {
	s.workspaceMu.RLock()
	defer s.workspaceMu.RUnlock()
	workspace, ok := s.workspaces[strings.TrimSpace(id)]
	return workspace, ok
}

func (s *Service) workspaceSnapshot() map[string]config.Workspace {
	s.workspaceMu.RLock()
	defer s.workspaceMu.RUnlock()
	return cloneWorkspaces(s.workspaces)
}

func (s *Service) ListWorkspaceCapabilities() []WorkspaceCapability {
	workspaces := s.workspaceSnapshot()
	result := make([]WorkspaceCapability, 0, len(workspaces))
	settings, settingsErr := s.SystemSettings(context.Background())
	for _, workspace := range workspaces {
		shellEnabled := settingsErr == nil && settings.WorkspaceShellBackend != ""
		if settings.WorkspaceShellBackend == domain.WorkspaceShellModeHost && workspace.Access != "read_write" {
			shellEnabled = false
		}
		item := WorkspaceCapability{
			ID: workspace.ID, Access: workspace.Access, Shell: shellEnabled,
			ShellBackend: settings.WorkspaceShellBackend, ShellName: settings.WorkspaceShellName,
		}
		for _, validator := range s.validators {
			if validator.Scope == "workspace" {
				item.Validators = append(item.Validators, validator.ID)
			}
		}
		sort.Strings(item.Validators)
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (s *Service) ListAdminWorkspaceCapabilities() []AdminWorkspaceCapability {
	public := s.ListWorkspaceCapabilities()
	result := make([]AdminWorkspaceCapability, 0, len(public))
	for _, capability := range public {
		result = append(result, AdminWorkspaceCapability{WorkspaceCapability: capability})
	}
	return result
}

func (s *Service) adminWorkspaceCapability(workspace config.Workspace) AdminWorkspaceCapability {
	for _, capability := range s.ListWorkspaceCapabilities() {
		if capability.ID == workspace.ID {
			return AdminWorkspaceCapability{WorkspaceCapability: capability}
		}
	}
	return AdminWorkspaceCapability{WorkspaceCapability: WorkspaceCapability{ID: workspace.ID, Access: workspace.Access}}
}

func (s *Service) ListAdminWorkspaceFiles(workspaceID, relativePath string) (WorkspaceFileList, error) {
	workspace, ok := s.workspaceByID(workspaceID)
	if !ok {
		return WorkspaceFileList{}, fmt.Errorf("workspace %q not found", workspaceID)
	}
	relativePath = strings.TrimSpace(relativePath)
	if relativePath == "" {
		relativePath = "."
	}
	directory, err := s.resolveWorkspacePath(workspace, relativePath, false)
	if err != nil {
		return WorkspaceFileList{}, err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return WorkspaceFileList{}, err
	}
	result := WorkspaceFileList{WorkspaceID: workspace.ID, Path: relativePath, Entries: make([]WorkspaceFileEntry, 0, len(entries))}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || isSensitiveWorkspaceComponent(entry.Name()) {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}
		kind := "file"
		if entry.IsDir() {
			kind = "directory"
		} else if !info.Mode().IsRegular() {
			continue
		}
		result.Entries = append(result.Entries, WorkspaceFileEntry{Name: entry.Name(), Type: kind, Size: info.Size()})
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
	directory, err := s.resolveWorkspacePath(workspace, relativePath, false)
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
	relativePath = strings.TrimSpace(relativePath)
	path, err := s.resolveWorkspacePath(workspace, relativePath, false)
	if err != nil {
		return WorkspaceFilePreview{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return WorkspaceFilePreview{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return WorkspaceFilePreview{}, fmt.Errorf("workspace preview target is not a regular file")
	}
	digest := sha256.New()
	hashed := io.TeeReader(file, digest)
	data, err := io.ReadAll(io.LimitReader(hashed, maxAdminWorkspacePreviewBytes+1))
	if err != nil {
		return WorkspaceFilePreview{}, err
	}
	truncated := int64(len(data)) > maxAdminWorkspacePreviewBytes
	if truncated {
		data = data[:maxAdminWorkspacePreviewBytes]
	}
	if _, err := io.Copy(io.Discard, hashed); err != nil {
		return WorkspaceFilePreview{}, err
	}
	binary := bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data)
	result := WorkspaceFilePreview{
		WorkspaceID: workspace.ID, Path: relativePath, Size: info.Size(), SHA256: hex.EncodeToString(digest.Sum(nil)),
		Binary: binary, Truncated: truncated,
	}
	if !binary {
		result.Content = string(data)
	}
	return result, nil
}

func (s *Service) OpenAdminWorkspaceFile(workspaceID, relativePath string) (WorkspaceFileDownload, error) {
	workspace, ok := s.workspaceByID(workspaceID)
	if !ok {
		return WorkspaceFileDownload{}, fmt.Errorf("workspace %q not found", workspaceID)
	}
	relativePath = strings.TrimSpace(relativePath)
	path, err := s.resolveWorkspacePath(workspace, relativePath, false)
	if err != nil {
		return WorkspaceFileDownload{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return WorkspaceFileDownload{}, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return WorkspaceFileDownload{}, err
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return WorkspaceFileDownload{}, fmt.Errorf("workspace download target is not a regular file")
	}
	return WorkspaceFileDownload{
		WorkspaceID: workspace.ID,
		Path:        filepath.ToSlash(filepath.Clean(relativePath)),
		Name:        info.Name(),
		Size:        info.Size(),
		Reader:      file,
	}, nil
}

func (s *Service) SaveAdminWorkspaceTextFile(ctx context.Context, workspaceID, relativePath, content string) (WorkspaceUploadResult, error) {
	workspace, ok := s.workspaceByID(workspaceID)
	if !ok {
		return WorkspaceUploadResult{}, fmt.Errorf("workspace %q not found", workspaceID)
	}
	if workspace.Access != "read_write" {
		return WorkspaceUploadResult{}, fmt.Errorf("workspace %q is read_only", workspace.ID)
	}
	if len(content) > maxWorkspaceTextFileBytes {
		return WorkspaceUploadResult{}, fmt.Errorf("workspace text file exceeds 100 MiB")
	}
	if err := ctx.Err(); err != nil {
		return WorkspaceUploadResult{}, err
	}
	relativePath = strings.TrimSpace(relativePath)
	path, err := s.resolveWorkspacePath(workspace, relativePath, false)
	if err != nil {
		return WorkspaceUploadResult{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return WorkspaceUploadResult{}, err
	}
	if !info.Mode().IsRegular() {
		return WorkspaceUploadResult{}, fmt.Errorf("workspace edit target is not a regular file")
	}
	if info.Size() > maxWorkspaceTextFileBytes {
		return WorkspaceUploadResult{}, fmt.Errorf("workspace text file exceeds 100 MiB")
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return WorkspaceUploadResult{}, err
	}
	if bytes.IndexByte(current, 0) >= 0 || !utf8.Valid(current) {
		return WorkspaceUploadResult{}, fmt.Errorf("workspace edit target is binary")
	}
	suffix := time.Now().UTC().Format("20060102T150405Z") + "-" + ids.New("file")
	temporary := filepath.Join(filepath.Dir(path), ".opsnerva-"+filepath.Base(path)+"-"+suffix+".tmp")
	if err := writeSyncedFile(temporary, []byte(content), info.Mode().Perm()); err != nil {
		return WorkspaceUploadResult{}, err
	}
	defer os.Remove(temporary)
	if err := ctx.Err(); err != nil {
		return WorkspaceUploadResult{}, err
	}
	if err := os.Rename(temporary, path); err != nil {
		return WorkspaceUploadResult{}, err
	}
	if err := syncLocalDirectory(filepath.Dir(path)); err != nil {
		return WorkspaceUploadResult{}, err
	}
	digest := sha256.Sum256([]byte(content))
	return WorkspaceUploadResult{
		WorkspaceID: workspace.ID, Path: relativePath, Size: int64(len(content)), SHA256: hex.EncodeToString(digest[:]),
	}, nil
}

func (s *Service) DeleteAdminWorkspaceEntry(ctx context.Context, workspaceID, relativePath, actor string) (WorkspaceDeleteResult, error) {
	workspace, ok := s.workspaceByID(workspaceID)
	if !ok {
		return WorkspaceDeleteResult{}, fmt.Errorf("workspace %q not found", workspaceID)
	}
	return s.deleteWorkspaceEntry(ctx, workspace, relativePath, true, actor)
}

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
	path, err := s.resolveWorkspacePath(workspace, relativePath, false)
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
	if err := syncLocalDirectory(filepath.Dir(path)); err != nil {
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

func (s *Service) UploadWorkspaceFile(ctx context.Context, workspaceID, targetPath, originalFilename string, source io.Reader, actor string) (WorkspaceUploadResult, error) {
	workspace, ok := s.workspaceByID(workspaceID)
	if !ok {
		return WorkspaceUploadResult{}, fmt.Errorf("workspace %q not found", workspaceID)
	}
	return s.storeWorkspaceFile(ctx, workspace, targetPath, originalFilename, source, "", "workspace_file_uploaded", actor)
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
	target, err := s.resolveWorkspacePath(workspace, targetPath, true)
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

func (s *Service) storeWorkspaceFile(ctx context.Context, workspace config.Workspace, targetPath, originalFilename string, source io.Reader, expectedSHA256, eventType, actor string) (WorkspaceUploadResult, error) {
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
	written, copyErr := io.Copy(io.MultiWriter(temporary, digest), source)
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
	if err := syncLocalDirectory(parent); err != nil {
		_ = os.Remove(target)
		return WorkspaceUploadResult{}, err
	}
	result := WorkspaceUploadResult{WorkspaceID: workspace.ID, Path: targetPath, Size: written, SHA256: actualSHA256}
	s.audit(ctx, "", eventType, actor, map[string]any{
		"workspace_id": workspace.ID, "path": targetPath, "size": written, "sha256": result.SHA256,
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
	if _, err := s.resolveWorkspacePath(workspace, relativePath, false); err != nil {
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
	relativePath = normalizedWorkspaceRelativePath(relativePath)
	if _, err := s.resolveWorkspacePath(workspace, relativePath, false); err != nil {
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
	if _, err := s.resolveWorkspacePath(workspace, relativePath, false); err != nil {
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

func (s *Service) EditWorkspaceFile(ctx context.Context, workspaceID, relativePath, oldText, newText, validatorID, reason, actor string) (domain.ExecResult, error) {
	workspace, ok := s.workspaceByID(workspaceID)
	if !ok {
		return domain.ExecResult{}, fmt.Errorf("workspace %q not found", workspaceID)
	}
	if workspace.Access != "read_write" {
		return domain.ExecResult{}, fmt.Errorf("workspace %q is read_only", workspaceID)
	}
	editContent := oldText + "\n" + newText
	if len(editContent) > 1<<20 || strings.Contains(editContent, "[REDACTED]") || s.redactor.Redact(editContent) != editContent {
		return domain.ExecResult{}, fmt.Errorf("workspace edit is too large or contains sensitive content")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return domain.ExecResult{}, fmt.Errorf("reason is required")
	}
	if _, err := s.workspaceValidator(validatorID, workspace, relativePath); err != nil {
		return domain.ExecResult{}, err
	}
	edit, change, err := buildTextEdit(relativePath, oldText, newText)
	if err != nil {
		return domain.ExecResult{}, err
	}
	host, err := s.workspaceHost(ctx, workspaceID)
	if err != nil {
		return domain.ExecResult{}, err
	}
	result, submitErr := s.Submit(ctx, domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecWorkspaceEdit, WorkspaceID: workspaceID, RelativePath: relativePath,
		Change: &change, TextEdit: &edit, Validator: validatorID, Reason: reason,
	}, actor)
	result.Change = &change
	if result.Stdout != "" {
		metadata := parseFileEditOutput(relativePath, validatorID, result.Stdout, result.Status == "completed")
		result.File = &metadata
	}
	if result.ExitCode == 74 {
		return result, fmt.Errorf("workspace validation failed; the target file was not changed")
	}
	if result.ExitCode == 75 {
		return result, fmt.Errorf("workspace file edit conflict; read the current path and retry")
	}
	return result, submitErr
}

// UploadWorkspaceFileToHost transfers one allowlisted Workspace file directly
// to a registered host. The model provides only the Workspace-relative path;
// the absolute local path is resolved after approval and is never serialized.
func (s *Service) UploadWorkspaceFileToHost(ctx context.Context, hostID, workspaceID, relativePath, expectedSHA256, remotePath, reason, actor string) (domain.ExecResult, error) {
	return s.Submit(ctx, domain.ExecRequest{
		HostID: hostID, Mode: domain.ExecWorkspaceUpload, WorkspaceID: workspaceID, RelativePath: relativePath,
		ExpectedSHA256: strings.ToLower(strings.TrimSpace(expectedSHA256)), RemotePath: remotePath, Reason: reason,
	}, actor)
}

// DownloadHostFileToWorkspace copies one SHA256-bound remote file into a new
// path in the conversation-bound Workspace. The destination is resolved both
// before approval and immediately before the atomic local commit.
func (s *Service) DownloadHostFileToWorkspace(ctx context.Context, hostID, remotePath, expectedSHA256, workspaceID, relativePath string, timeoutSeconds int, reason, actor string) (domain.ExecResult, error) {
	workspace, ok := s.workspaceByID(workspaceID)
	if !ok {
		return domain.ExecResult{}, fmt.Errorf("workspace %q not found", workspaceID)
	}
	if err := validateRemoteFilePath(remotePath); err != nil {
		return domain.ExecResult{}, err
	}
	expectedSHA256 = strings.ToLower(strings.TrimSpace(expectedSHA256))
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(expectedSHA256) {
		return domain.ExecResult{}, fmt.Errorf("workspace download requires expected_sha256 from ssh_file_read")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return domain.ExecResult{}, fmt.Errorf("reason is required")
	}
	if timeoutSeconds < 0 || timeoutSeconds > 600 {
		return domain.ExecResult{}, fmt.Errorf("workspace download timeout_seconds must be between 1 and 600 when provided")
	}
	relativePath, _, err := s.validateWorkspaceFileDestination(workspace, relativePath, filepath.Base(remotePath))
	if err != nil {
		return domain.ExecResult{}, err
	}
	return s.Submit(ctx, domain.ExecRequest{
		HostID: hostID, Mode: domain.ExecWorkspaceDownload, WorkspaceID: workspaceID,
		RemotePath: remotePath, RelativePath: relativePath, ExpectedSHA256: expectedSHA256,
		TimeoutSeconds: timeoutSeconds, Reason: reason,
	}, actor)
}

func (s *Service) executeWorkspaceDownload(ctx context.Context, connection sshx.ConnectionSpec, req domain.ExecRequest, actor string) (sshx.RawResult, error) {
	started := time.Now()
	transport, ok := s.transport.(sshx.SFTPTransport)
	if !ok {
		return sshx.RawResult{ExitCode: -1, Duration: time.Since(started)}, fmt.Errorf("configured SSH transport does not support SFTP")
	}
	workspace, ok := s.workspaceByID(req.WorkspaceID)
	if !ok {
		return sshx.RawResult{ExitCode: -1, Duration: time.Since(started)}, fmt.Errorf("workspace %q not found", req.WorkspaceID)
	}
	if _, _, err := s.validateWorkspaceFileDestination(workspace, req.RelativePath, filepath.Base(req.RemotePath)); err != nil {
		return sshx.RawResult{ExitCode: -1, Duration: time.Since(started)}, err
	}
	timeout := req.TimeoutSeconds
	if timeout <= 0 {
		timeout = s.limits.SyncTimeoutSeconds
	}
	if timeout <= 0 {
		timeout = 60
	}
	downloadCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	download, err := transport.OpenSFTPFile(downloadCtx, connection, req.RemotePath)
	if err != nil {
		return sshx.RawResult{ExitCode: -1, Duration: time.Since(started)}, err
	}
	defer download.Reader.Close()
	if download.Entry.Type != "file" {
		return sshx.RawResult{ExitCode: -1, Duration: time.Since(started)}, fmt.Errorf("remote download source is not a regular file")
	}
	stored, err := s.storeWorkspaceFile(downloadCtx, workspace, req.RelativePath, filepath.Base(req.RemotePath), download.Reader, req.ExpectedSHA256, "workspace_file_downloaded", actor)
	if err != nil {
		return sshx.RawResult{ExitCode: -1, Duration: time.Since(started)}, err
	}
	output, err := json.Marshal(stored)
	return sshx.RawResult{ExitCode: 0, Stdout: output, Duration: time.Since(started)}, err
}

// RunWorkspaceShell resolves the administrator-selected backend before
// submission so the exact host or sandbox boundary is approval-bound.
func (s *Service) RunWorkspaceShell(ctx context.Context, workspaceID, script, cwd string, env map[string]string, timeoutSeconds int, reason, actor string) (domain.ExecResult, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	workspace, ok := s.workspaceByID(workspaceID)
	if !ok {
		return domain.ExecResult{}, fmt.Errorf("workspace %q not found", workspaceID)
	}
	backend, err := s.configuredWorkspaceShellBackend(ctx)
	if err != nil {
		return domain.ExecResult{}, err
	}
	if backend == domain.WorkspaceShellModeHost && workspace.Access != "read_write" {
		return domain.ExecResult{}, fmt.Errorf("host shell is unavailable for read_only workspace %q", workspaceID)
	}
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		cwd = "."
	}
	resolvedCwd, err := s.resolveWorkspacePath(workspace, cwd, false)
	if err != nil {
		return domain.ExecResult{}, err
	}
	if info, statErr := os.Stat(resolvedCwd); statErr != nil || !info.IsDir() {
		return domain.ExecResult{}, fmt.Errorf("workspace shell cwd is not a directory")
	}
	host, err := s.workspaceHost(ctx, workspaceID)
	if err != nil {
		return domain.ExecResult{}, err
	}
	return s.Submit(ctx, domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecWorkspaceShell, WorkspaceID: workspaceID,
		WorkspaceShellBackend: backend,
		Script:                script, Cwd: cwd, Env: env, TimeoutSeconds: timeoutSeconds,
		Reason: reason,
	}, actor)
}

// StartWorkspaceShell opens a persistent PTY in the Workspace bound to an
// Agent conversation. The selected backend is captured in the approved
// request, just like a one-shot workspace_shell execution.
func (s *Service) StartWorkspaceShell(ctx context.Context, workspaceID, cwd string, env map[string]string, cols, rows int, reason, actor string) (domain.ExecResult, error) {
	if SessionIDFromContext(ctx) == "" {
		return domain.ExecResult{}, fmt.Errorf("interactive Workspace shells require an Agent conversation")
	}
	req, err := s.workspaceShellStartRequest(ctx, workspaceID, cwd, env, cols, rows, reason)
	if err != nil {
		return domain.ExecResult{}, err
	}
	return s.Submit(ctx, req, actor)
}

// StartOperatorWorkspaceShell opens a PTY after an authenticated operator
// explicitly selects a Workspace in the Web console.
func (s *Service) StartOperatorWorkspaceShell(ctx context.Context, workspaceID, cwd, actor string) (domain.SSHShell, error) {
	req, err := s.workspaceShellStartRequest(ctx, workspaceID, cwd, nil, 120, 32, webOperatorReason)
	if err != nil {
		return domain.SSHShell{}, err
	}
	req.ShellSurface = domain.WorkspaceShellSurfaceOperator
	host, err := s.workspaceHost(ctx, req.WorkspaceID)
	if err != nil {
		return domain.SSHShell{}, err
	}
	release, err := s.acquire(ctx, host.ID)
	if err != nil {
		return domain.SSHShell{}, err
	}
	defer release()
	return s.openOperatorWorkspaceTerminal(ctx, host, req, actor)
}

func (s *Service) workspaceShellStartRequest(ctx context.Context, workspaceID, cwd string, env map[string]string, cols, rows int, reason string) (domain.ExecRequest, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	workspace, ok := s.workspaceByID(workspaceID)
	if !ok {
		return domain.ExecRequest{}, fmt.Errorf("workspace %q not found", workspaceID)
	}
	backend, err := s.configuredWorkspaceShellBackend(ctx)
	if err != nil {
		return domain.ExecRequest{}, err
	}
	if backend == domain.WorkspaceShellModeHost && workspace.Access != "read_write" {
		return domain.ExecRequest{}, fmt.Errorf("host shell is unavailable for read_only workspace %q", workspaceID)
	}
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		cwd = "."
	}
	resolvedCwd, err := s.resolveWorkspacePath(workspace, cwd, false)
	if err != nil {
		return domain.ExecRequest{}, err
	}
	if info, statErr := os.Stat(resolvedCwd); statErr != nil || !info.IsDir() {
		return domain.ExecRequest{}, fmt.Errorf("workspace shell cwd is not a directory")
	}
	host, err := s.workspaceHost(ctx, workspaceID)
	if err != nil {
		return domain.ExecRequest{}, err
	}
	return domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecWorkspaceShellStart, WorkspaceID: workspaceID,
		WorkspaceShellBackend: backend, Cwd: cwd, Env: env,
		ShellCols: cols, ShellRows: rows, Reason: strings.TrimSpace(reason),
	}, nil
}

func (s *Service) openWorkspaceShell(ctx context.Context, host domain.Host, req domain.ExecRequest, run domain.Run, actor string) (domain.SSHShell, error) {
	return s.openWorkspaceShellRuntime(ctx, host, req, run, actor, false)
}

func (s *Service) openOperatorWorkspaceTerminal(ctx context.Context, host domain.Host, req domain.ExecRequest, actor string) (domain.SSHShell, error) {
	return s.openWorkspaceShellRuntime(ctx, host, req, domain.Run{}, actor, true)
}

func (s *Service) openWorkspaceShellRuntime(ctx context.Context, host domain.Host, req domain.ExecRequest, run domain.Run, actor string, transient bool) (domain.SSHShell, error) {
	if req.Mode != domain.ExecWorkspaceShellStart {
		return domain.SSHShell{}, fmt.Errorf("invalid Workspace shell request mode")
	}
	if err := validateInteractiveShellSize(req); err != nil {
		return domain.SSHShell{}, err
	}
	workspace, ok := s.workspaceByID(req.WorkspaceID)
	if !ok {
		return domain.SSHShell{}, fmt.Errorf("workspace %q not found", req.WorkspaceID)
	}
	configuredBackend, err := s.configuredWorkspaceShellBackend(ctx)
	if err != nil {
		return domain.SSHShell{}, err
	}
	if req.WorkspaceShellBackend == "" || req.WorkspaceShellBackend != configuredBackend {
		return domain.SSHShell{}, fmt.Errorf("approved workspace shell backend %q is no longer enabled", req.WorkspaceShellBackend)
	}
	program, args, directory, environment, err := s.workspacePTYCommand(workspace, req)
	if err != nil {
		return domain.SSHShell{}, err
	}
	user := strings.TrimSpace(os.Getenv("USER"))
	if runtime.GOOS == "windows" {
		user = strings.TrimSpace(os.Getenv("USERNAME"))
	}
	if user == "" {
		user = "local"
	}
	return s.openInteractiveShell(ctx, host, req, run, actor, interactiveShellOptions{
		kind: domain.SSHShellKindWorkspace, workspaceID: workspace.ID,
		backend: req.WorkspaceShellBackend, user: user, transient: transient,
	}, func(shellCtx context.Context, output func(string, []byte)) (sshx.ShellSession, error) {
		return startWorkspacePTY(shellCtx, program, args, directory, environment, req.ShellCols, req.ShellRows, output)
	})
}

func (s *Service) workspaceShellTarget(ctx context.Context, shellID, sessionID, workspaceID string) (domain.SSHShell, error) {
	shell, err := s.store.GetSSHShell(ctx, strings.TrimSpace(shellID))
	if err != nil {
		return domain.SSHShell{}, err
	}
	if shell.Kind != domain.SSHShellKindWorkspace || shell.SessionID != strings.TrimSpace(sessionID) || shell.WorkspaceID != strings.TrimSpace(workspaceID) {
		return domain.SSHShell{}, store.ErrNotFound
	}
	return shell, nil
}

func (s *Service) ListWorkspaceShells(ctx context.Context, sessionID, workspaceID, reason, actor string) (domain.SSHShellList, error) {
	shells, err := s.store.ListSSHShells(ctx, strings.TrimSpace(sessionID), true)
	if err != nil {
		return domain.SSHShellList{}, err
	}
	filtered := shells[:0]
	for _, shell := range shells {
		if shell.Kind == domain.SSHShellKindWorkspace && shell.WorkspaceID == strings.TrimSpace(workspaceID) {
			filtered = append(filtered, shell)
		}
	}
	if reason = strings.TrimSpace(reason); reason != "" {
		if len(reason) > maxSSHShellReasonBytes {
			return domain.SSHShellList{}, fmt.Errorf("reason must not exceed %d bytes", maxSSHShellReasonBytes)
		}
		s.audit(context.WithoutCancel(ctx), "", "workspace_shell_list", actor, map[string]any{
			"session_id": strings.TrimSpace(sessionID), "workspace_id": strings.TrimSpace(workspaceID), "reason": s.redactor.Redact(reason),
		})
	}
	return domain.SSHShellList{Shells: filtered, Count: len(filtered), WorkspaceID: strings.TrimSpace(workspaceID)}, nil
}

func (s *Service) WriteWorkspaceShell(ctx context.Context, shellID, sessionID, workspaceID, input, reason, actor string) (domain.SSHShellSnapshot, error) {
	if _, err := s.workspaceShellTarget(ctx, shellID, sessionID, workspaceID); err != nil {
		return domain.SSHShellSnapshot{}, err
	}
	return s.WriteSSHShell(ctx, shellID, sessionID, input, reason, actor)
}

func (s *Service) WriteWorkspaceShellPage(ctx context.Context, shellID, sessionID, workspaceID, input string, queryDelay time.Duration, maxOutputBytes int, reason, actor string) (domain.SSHShellOutputPage, error) {
	if _, err := s.workspaceShellTarget(ctx, shellID, sessionID, workspaceID); err != nil {
		return domain.SSHShellOutputPage{}, err
	}
	return s.WriteSSHShellPage(ctx, shellID, sessionID, input, queryDelay, maxOutputBytes, reason, actor)
}

func (s *Service) WaitWorkspaceShellOutput(ctx context.Context, shellID, sessionID, workspaceID, reason, actor string) (domain.SSHShellSnapshot, error) {
	if _, err := s.workspaceShellTarget(ctx, shellID, sessionID, workspaceID); err != nil {
		return domain.SSHShellSnapshot{}, err
	}
	return s.WaitSSHShellOutput(ctx, shellID, sessionID, reason, actor)
}

func (s *Service) QueryWorkspaceShellOutput(ctx context.Context, shellID, sessionID, workspaceID string, afterSequence *uint64, queryDelay time.Duration, maxOutputBytes int, reason, actor string) (domain.SSHShellOutputPage, error) {
	if _, err := s.workspaceShellTarget(ctx, shellID, sessionID, workspaceID); err != nil {
		return domain.SSHShellOutputPage{}, err
	}
	return s.QuerySSHShellOutput(ctx, shellID, sessionID, afterSequence, queryDelay, maxOutputBytes, reason, actor)
}

func (s *Service) GetWorkspaceShellSnapshot(ctx context.Context, shellID, sessionID, workspaceID string, after uint64, wait time.Duration, coalesce bool, reason, actor string) (domain.SSHShellSnapshot, error) {
	if _, err := s.workspaceShellTarget(ctx, shellID, sessionID, workspaceID); err != nil {
		return domain.SSHShellSnapshot{}, err
	}
	return s.GetSSHShellSnapshot(ctx, shellID, sessionID, after, wait, coalesce, reason, actor)
}

func (s *Service) InterruptWorkspaceShell(ctx context.Context, shellID, sessionID, workspaceID, reason, actor string) (domain.SSHShell, error) {
	if _, err := s.workspaceShellTarget(ctx, shellID, sessionID, workspaceID); err != nil {
		return domain.SSHShell{}, err
	}
	return s.InterruptSSHShell(ctx, shellID, sessionID, reason, actor)
}

func (s *Service) CloseWorkspaceShell(ctx context.Context, shellID, sessionID, workspaceID, reason, actor string) (domain.SSHShell, error) {
	if _, err := s.workspaceShellTarget(ctx, shellID, sessionID, workspaceID); err != nil {
		return domain.SSHShell{}, err
	}
	return s.CloseSSHShell(ctx, shellID, sessionID, reason, actor)
}

func (s *Service) workspacePTYCommand(workspace config.Workspace, req domain.ExecRequest) (string, []string, string, []string, error) {
	switch req.WorkspaceShellBackend {
	case domain.WorkspaceShellModeSandbox:
		program, args, environment, err := s.workspaceSandboxCommand(workspace, req, true)
		return program, args, "", environment, err
	case domain.WorkspaceShellModeHost:
		if workspace.Access != "read_write" {
			return "", nil, "", nil, fmt.Errorf("host shell is unavailable for read_only workspace %q", workspace.ID)
		}
		shell, _, err := workspaceHostShellExecutable()
		if err != nil {
			return "", nil, "", nil, err
		}
		resolvedCwd, err := s.resolveWorkspacePath(workspace, req.Cwd, false)
		if err != nil {
			return "", nil, "", nil, err
		}
		args := []string{"--noprofile", "--norc", "-i"}
		if runtime.GOOS == "windows" {
			args = []string{"-NoLogo", "-NoProfile", "-NoExit", "-Command", workspacePowerShellUTF8Preamble}
		}
		return shell, args, resolvedCwd, workspaceHostEnvironment(workspace.Root, req.Env), nil
	default:
		return "", nil, "", nil, fmt.Errorf("unsupported workspace shell backend %q", req.WorkspaceShellBackend)
	}
}

func (s *Service) prepareWorkspaceUpload(req domain.ExecRequest) (domain.ExecRequest, error) {
	workspace, ok := s.workspaceByID(req.WorkspaceID)
	if !ok {
		return req, fmt.Errorf("workspace %q not found", req.WorkspaceID)
	}
	expected := strings.ToLower(strings.TrimSpace(req.ExpectedSHA256))
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(expected) {
		return req, fmt.Errorf("workspace upload requires the expected_sha256 returned by workspace_file_read")
	}
	path, err := s.resolveWorkspacePath(workspace, strings.TrimSpace(req.RelativePath), false)
	if err != nil {
		return req, err
	}
	file, err := os.Open(path)
	if err != nil {
		return req, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return req, fmt.Errorf("workspace upload source is not a regular file")
	}
	digest := sha256.New()
	_, copyErr := io.Copy(digest, file)
	closeErr := file.Close()
	if copyErr != nil {
		return req, copyErr
	}
	if closeErr != nil {
		return req, closeErr
	}
	actual := hex.EncodeToString(digest.Sum(nil))
	if actual != expected {
		return req, fmt.Errorf("workspace upload source version conflict: expected SHA256 %s, got %s", expected, actual)
	}
	req.ExpectedSHA256 = expected
	req.LocalPath = path
	return req, nil
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
	path, err := s.resolveWorkspacePath(workspace, req.RelativePath, req.Mode == domain.ExecWorkspaceEdit)
	if err != nil {
		return sshx.RawResult{}, err
	}
	switch req.Mode {
	case domain.ExecWorkspaceRead:
		result.Stdout, err = readWorkspaceFile(path, req.RelativePath, req.MaxBytes, req.OffsetBytes, req.TailLines)
	case domain.ExecWorkspaceDirectoryList:
		result.Stdout, err = listWorkspaceDirectory(path)
	case domain.ExecWorkspaceSearch:
		result.Stdout, err = searchWorkspaceFile(path, req.SearchPattern, req.SearchMatchMode, req.ContextLines)
	case domain.ExecWorkspaceEdit:
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

func (s *Service) decorateWorkspaceShellSettings(settings domain.SystemSettings) domain.SystemSettings {
	if settings.WorkspaceShellMode == "" {
		settings.WorkspaceShellMode = domain.DefaultWorkspaceShellMode(runtime.GOOS)
	}
	settings.WorkspaceShellPlatform = runtime.GOOS
	_, sandboxErr := s.workspaceSandboxExecutable()
	_, hostName, hostErr := workspaceHostShellExecutable()
	settings.WorkspaceSandboxAvailable = sandboxErr == nil
	settings.WorkspaceHostShellAvailable = hostErr == nil
	switch settings.WorkspaceShellMode {
	case domain.WorkspaceShellModeSandbox:
		if sandboxErr == nil {
			settings.WorkspaceShellBackend = domain.WorkspaceShellModeSandbox
			settings.WorkspaceShellName = "bash"
		}
	case domain.WorkspaceShellModeHost:
		if hostErr == nil {
			settings.WorkspaceShellBackend = domain.WorkspaceShellModeHost
			settings.WorkspaceShellName = hostName
		}
	}
	return settings
}

func (s *Service) configuredWorkspaceShellBackend(ctx context.Context) (string, error) {
	settings, err := s.store.GetSystemSettings(ctx)
	if err != nil {
		return "", err
	}
	if settings.WorkspaceShellMode == "" {
		settings.WorkspaceShellMode = domain.DefaultWorkspaceShellMode(runtime.GOOS)
	}
	switch settings.WorkspaceShellMode {
	case domain.WorkspaceShellModeDisabled:
		return "", fmt.Errorf("workspace shell is disabled in System settings")
	case domain.WorkspaceShellModeSandbox:
		if _, err := s.workspaceSandboxExecutable(); err != nil {
			return "", err
		}
		return domain.WorkspaceShellModeSandbox, nil
	case domain.WorkspaceShellModeHost:
		if _, _, err := workspaceHostShellExecutable(); err != nil {
			return "", err
		}
		return domain.WorkspaceShellModeHost, nil
	default:
		return "", fmt.Errorf("invalid workspace shell mode %q", settings.WorkspaceShellMode)
	}
}

func (s *Service) workspaceSandboxExecutable() (string, error) {
	if runtime.GOOS != "linux" {
		return "", fmt.Errorf("workspace sandbox requires Linux; select Host shell or Disabled in System settings")
	}
	configured := strings.TrimSpace(s.workspaceSandboxPath)
	if configured == "" {
		return "", fmt.Errorf("workspace shell sandbox is disabled; configure workspace_sandbox_path")
	}
	path, err := exec.LookPath(configured)
	if err != nil {
		return "", fmt.Errorf("workspace shell sandbox %q is unavailable; install bubblewrap or configure workspace_sandbox_path: %w", configured, err)
	}
	return filepath.Abs(path)
}

func workspaceSandboxSupportsDisableUserns(sandbox string) bool {
	output, err := exec.Command(sandbox, "--help").CombinedOutput()
	return err == nil && bytes.Contains(output, []byte("--disable-userns"))
}

func workspaceHostShellExecutable() (string, string, error) {
	candidates := []string{"bash"}
	if runtime.GOOS == "windows" {
		candidates = []string{"pwsh.exe", "powershell.exe"}
	}
	for _, candidate := range candidates {
		path, err := exec.LookPath(candidate)
		if err == nil {
			absolute, absErr := filepath.Abs(path)
			if absErr != nil {
				return "", "", absErr
			}
			return absolute, strings.TrimSuffix(strings.TrimSuffix(filepath.Base(path), ".exe"), ".EXE"), nil
		}
	}
	return "", "", fmt.Errorf("host shell is unavailable on %s", runtime.GOOS)
}

type workspaceSandboxMask struct {
	path      string
	directory bool
}

func workspaceSandboxMasks(root string) ([]workspaceSandboxMask, error) {
	const maxMasks = 512
	masks := make([]workspaceSandboxMask, 0)
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		isUnsafeSpecialFile := info.Mode()&(os.ModeSocket|os.ModeNamedPipe|os.ModeDevice|os.ModeCharDevice|os.ModeIrregular) != 0
		if path == root || (!isSensitiveWorkspaceComponent(info.Name()) && !isUnsafeSpecialFile) {
			return nil
		}
		if len(masks) >= maxMasks {
			return fmt.Errorf("workspace contains more than %d sensitive paths; sandbox setup refused", maxMasks)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		masks = append(masks, workspaceSandboxMask{
			path:      filepath.Join("/workspace", relative),
			directory: info.IsDir(),
		})
		if info.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	return masks, err
}

func pathsOverlap(first, second string) bool {
	within := func(path, root string) bool {
		relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
		return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
	}
	return within(first, second) || within(second, first)
}

func (s *Service) executeWorkspaceShell(ctx context.Context, workspace config.Workspace, req domain.ExecRequest, stream func(string, []byte)) (sshx.RawResult, error) {
	configuredBackend, err := s.configuredWorkspaceShellBackend(ctx)
	if err != nil {
		return sshx.RawResult{}, err
	}
	if req.WorkspaceShellBackend == "" || req.WorkspaceShellBackend != configuredBackend {
		return sshx.RawResult{}, fmt.Errorf("approved workspace shell backend %q is no longer enabled", req.WorkspaceShellBackend)
	}
	switch req.WorkspaceShellBackend {
	case domain.WorkspaceShellModeSandbox:
		return s.executeWorkspaceSandboxShell(ctx, workspace, req, stream)
	case domain.WorkspaceShellModeHost:
		return s.executeWorkspaceHostShell(ctx, workspace, req, stream)
	default:
		return sshx.RawResult{}, fmt.Errorf("unsupported workspace shell backend %q", req.WorkspaceShellBackend)
	}
}

func (s *Service) executeWorkspaceSandboxShell(ctx context.Context, workspace config.Workspace, req domain.ExecRequest, stream func(string, []byte)) (sshx.RawResult, error) {
	sandbox, args, environment, err := s.workspaceSandboxCommand(workspace, req, false)
	if err != nil {
		return sshx.RawResult{}, err
	}

	timeout := req.TimeoutSeconds
	if timeout <= 0 {
		timeout = s.limits.SyncTimeoutSeconds
	}
	if timeout <= 0 {
		timeout = 60
	}
	execCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	command := exec.CommandContext(execCtx, sandbox, args...)
	command.Env = environment
	command.Stdin = strings.NewReader(req.Script)
	return s.runWorkspaceProcess(execCtx, command, timeout, "shell sandbox", workspace.Root, stream)
}

func (s *Service) workspaceSandboxCommand(workspace config.Workspace, req domain.ExecRequest, interactive bool) (string, []string, []string, error) {
	sandbox, err := s.workspaceSandboxExecutable()
	if err != nil {
		return "", nil, nil, err
	}
	root, err := filepath.EvalSymlinks(workspace.Root)
	if err != nil {
		return "", nil, nil, fmt.Errorf("resolve workspace root: %w", err)
	}
	for _, systemRoot := range []string{"/usr", "/lib", "/lib64"} {
		if pathsOverlap(root, systemRoot) {
			return "", nil, nil, fmt.Errorf("workspace root overlaps sandbox runtime directory %s", systemRoot)
		}
	}
	cwd := req.Cwd
	if cwd == "" {
		cwd = "."
	}
	resolvedCwd, err := s.resolveWorkspacePath(workspace, cwd, false)
	if err != nil {
		return "", nil, nil, err
	}
	info, err := os.Stat(resolvedCwd)
	if err != nil || !info.IsDir() {
		return "", nil, nil, fmt.Errorf("workspace shell cwd is not a directory")
	}
	relativeCwd, err := filepath.Rel(root, resolvedCwd)
	if err != nil {
		return "", nil, nil, err
	}
	masks, err := workspaceSandboxMasks(root)
	if err != nil {
		return "", nil, nil, fmt.Errorf("prepare workspace sandbox masks: %w", err)
	}
	mountMode := "--bind"
	if workspace.Access == "read_only" {
		mountMode = "--ro-bind"
	}
	args := []string{"--die-with-parent"}
	if !interactive {
		args = append(args, "--new-session")
	}
	// Interactive commands already run as the session leader of a dedicated
	// PTY. Starting another session inside Bubblewrap detaches Bash from that
	// controlling terminal and disables job control.
	args = append(args,
		"--unshare-all", "--unshare-user", "--cap-drop", "ALL",
		"--ro-bind", "/usr", "/usr",
		"--ro-bind-try", "/lib", "/lib",
		"--ro-bind-try", "/lib64", "/lib64",
		"--symlink", "usr/bin", "/bin", "--symlink", "usr/sbin", "/sbin",
		"--dir", "/etc", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp",
		"--dir", "/workspace", mountMode, root, "/workspace",
	)
	if workspaceSandboxSupportsDisableUserns(sandbox) {
		args = append([]string{"--disable-userns"}, args...)
	}
	for _, mask := range masks {
		if mask.directory {
			args = append(args, "--tmpfs", mask.path)
		} else {
			args = append(args, "--ro-bind", "/dev/null", mask.path)
		}
	}
	sandboxCwd := "/workspace"
	if relativeCwd != "." {
		sandboxCwd = filepath.ToSlash(filepath.Join("/workspace", relativeCwd))
	}
	args = append(args,
		"--chdir", sandboxCwd,
		"--clearenv",
		"--setenv", "PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"--setenv", "HOME", "/workspace",
		"--setenv", "TMPDIR", "/tmp",
		"--setenv", "TERM", "xterm-256color",
		"--setenv", "COLORTERM", "truecolor",
		"--setenv", "LANG", "C.UTF-8",
		"--setenv", "LC_ALL", "C.UTF-8",
		"--setenv", "PYTHONUTF8", "1",
		"--setenv", "PYTHONIOENCODING", "utf-8",
	)
	keys := make([]string, 0, len(req.Env))
	for key := range req.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--setenv", key, req.Env[key])
	}
	bashArgs := []string{"-se"}
	if interactive {
		bashArgs = []string{"--noprofile", "--norc", "-i"}
	}
	args = append(args, "--", "/usr/bin/bash")
	args = append(args, bashArgs...)
	environment := []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "PYTHONUTF8=1", "PYTHONIOENCODING=utf-8"}
	return sandbox, args, environment, nil
}

func (s *Service) executeWorkspaceHostShell(ctx context.Context, workspace config.Workspace, req domain.ExecRequest, stream func(string, []byte)) (sshx.RawResult, error) {
	if workspace.Access != "read_write" {
		return sshx.RawResult{}, fmt.Errorf("host shell is unavailable for read_only workspace %q", workspace.ID)
	}
	shell, _, err := workspaceHostShellExecutable()
	if err != nil {
		return sshx.RawResult{}, err
	}
	cwd := req.Cwd
	if cwd == "" {
		cwd = "."
	}
	resolvedCwd, err := s.resolveWorkspacePath(workspace, cwd, false)
	if err != nil {
		return sshx.RawResult{}, err
	}
	info, err := os.Stat(resolvedCwd)
	if err != nil || !info.IsDir() {
		return sshx.RawResult{}, fmt.Errorf("workspace shell cwd is not a directory")
	}
	timeout := req.TimeoutSeconds
	if timeout <= 0 {
		timeout = s.limits.SyncTimeoutSeconds
	}
	if timeout <= 0 {
		timeout = 60
	}
	execCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	args := []string{"-se"}
	var cleanupPowerShellScript func() error
	if runtime.GOOS == "windows" {
		scriptPath, cleanup, scriptErr := createWorkspacePowerShellScript(req.Script)
		if scriptErr != nil {
			return sshx.RawResult{}, scriptErr
		}
		cleanupPowerShellScript = cleanup
		args = []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", scriptPath}
	}
	command := exec.CommandContext(execCtx, shell, args...)
	command.Dir = resolvedCwd
	command.Env = workspaceHostEnvironment(workspace.Root, req.Env)
	if runtime.GOOS != "windows" {
		command.Stdin = strings.NewReader(req.Script)
	}
	result, runErr := s.runWorkspaceProcess(execCtx, command, timeout, "host shell", workspace.Root, stream)
	if cleanupPowerShellScript != nil {
		if cleanupErr := cleanupPowerShellScript(); cleanupErr != nil {
			cleanupErr = fmt.Errorf("remove temporary PowerShell script: %w", cleanupErr)
			if runErr != nil {
				return result, errors.Join(runErr, cleanupErr)
			}
			return result, cleanupErr
		}
	}
	return result, runErr
}

const workspacePowerShellUTF8Preamble = `$__opsNervaUtf8Encoding = [System.Text.UTF8Encoding]::new($false)
[System.Console]::InputEncoding = $__opsNervaUtf8Encoding
[System.Console]::OutputEncoding = $__opsNervaUtf8Encoding
$OutputEncoding = $__opsNervaUtf8Encoding
$env:LANG = 'C.UTF-8'
$env:LC_ALL = 'C.UTF-8'
$env:PYTHONUTF8 = '1'
$env:PYTHONIOENCODING = 'utf-8'
`

func workspacePowerShellScript(script string) []byte {
	const utf8BOM = "\xef\xbb\xbf"
	return []byte(utf8BOM + workspacePowerShellUTF8Preamble + script)
}

func createWorkspacePowerShellScript(script string) (string, func() error, error) {
	directory, err := os.MkdirTemp("", "opsnerva-workspace-shell-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temporary PowerShell directory: %w", err)
	}
	cleanup := func() error {
		return os.RemoveAll(directory)
	}
	path := filepath.Join(directory, "script.ps1")
	if err := os.WriteFile(path, workspacePowerShellScript(script), 0o600); err != nil {
		cleanupErr := cleanup()
		if cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove temporary PowerShell directory: %w", cleanupErr))
		}
		return "", nil, fmt.Errorf("write temporary PowerShell script: %w", err)
	}
	return path, cleanup, nil
}

func workspaceHostEnvironment(workspaceRoot string, input map[string]string) []string {
	return workspaceHostEnvironmentForPlatform(runtime.GOOS, workspaceRoot, input)
}

func workspaceHostEnvironmentForPlatform(goos, workspaceRoot string, input map[string]string) []string {
	values := map[string]string{
		"PATH":             os.Getenv("PATH"),
		"TERM":             "xterm-256color",
		"COLORTERM":        "truecolor",
		"LANG":             "C.UTF-8",
		"LC_ALL":           "C.UTF-8",
		"PYTHONUTF8":       "1",
		"PYTHONIOENCODING": "utf-8",
	}
	if goos != "windows" {
		values["HOME"] = workspaceRoot
		values["TMPDIR"] = os.TempDir()
	}
	for key, value := range input {
		values[key] = value
	}
	if goos == "windows" {
		// Keep Windows profile directories outside the Workspace. PowerShell and
		// child processes create AppData and Microsoft trees under USERPROFILE.
		for _, key := range []string{
			"HOME", "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "APPDATA", "LOCALAPPDATA", "PSModulePath",
			"SystemRoot", "WINDIR", "ComSpec", "PATHEXT", "TEMP", "TMP",
		} {
			for existing := range values {
				if strings.EqualFold(existing, key) {
					delete(values, existing)
				}
			}
			if value := os.Getenv(key); value != "" {
				values[key] = value
			}
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+values[key])
	}
	return environment
}

func (s *Service) runWorkspaceProcess(execCtx context.Context, command *exec.Cmd, timeout int, operation, workspaceRoot string, stream func(string, []byte)) (sshx.RawResult, error) {
	stdout := newWorkspaceCaptureBuffer("stdout", workspaceRoot, stream)
	stderr := newWorkspaceCaptureBuffer("stderr", workspaceRoot, stream)
	command.Stdout, command.Stderr = stdout, stderr
	started := time.Now()
	runErr := command.Run()
	stdout.Flush()
	stderr.Flush()
	result := sshx.RawResult{
		ExitCode: workspaceExitCode(runErr), Stdout: stdout.Bytes(), Stderr: stderr.Bytes(),
		Duration: time.Since(started),
	}
	if errors.Is(execCtx.Err(), context.DeadlineExceeded) {
		return result, fmt.Errorf("workspace %s timed out after %s", operation, time.Duration(timeout)*time.Second)
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			return result, fmt.Errorf("start workspace %s: %w", operation, runErr)
		}
	}
	return result, nil
}

type workspaceCaptureBuffer struct {
	buffer  bytes.Buffer
	pending strings.Builder
	stream  string
	emit    func(string, []byte)
	redact  func(string) string
}

func (b *workspaceCaptureBuffer) Write(data []byte) (int, error) {
	written, err := b.buffer.Write(data)
	if written == 0 || b.emit == nil {
		return written, err
	}
	b.pending.Write(data[:written])
	value := b.pending.String()
	consumed := 0
	for consumed < len(value) {
		index := strings.IndexAny(value[consumed:], "\r\n")
		if index < 0 {
			break
		}
		end := consumed + index + 1
		if value[end-1] == '\r' && end < len(value) && value[end] == '\n' {
			end++
		}
		b.emit(b.stream, []byte(b.redact(value[consumed:end])))
		consumed = end
	}
	if consumed > 0 {
		b.pending.Reset()
		b.pending.WriteString(value[consumed:])
	}
	return written, err
}

func (b *workspaceCaptureBuffer) Bytes() []byte { return bytes.Clone(b.buffer.Bytes()) }

func (b *workspaceCaptureBuffer) Flush() {
	if b.emit == nil || b.pending.Len() == 0 {
		return
	}
	b.emit(b.stream, []byte(b.redact(b.pending.String())))
	b.pending.Reset()
}

func newWorkspaceCaptureBuffer(stream, root string, emit func(string, []byte)) *workspaceCaptureBuffer {
	roots := workspaceRedactionRoots(root)
	return &workspaceCaptureBuffer{
		stream: stream,
		emit:   emit,
		redact: func(value string) string { return redactWorkspacePaths(value, roots) },
	}
}

func workspaceExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func (s *Service) workspaceHost(ctx context.Context, workspaceID string) (domain.Host, error) {
	_, ok := s.workspaceByID(workspaceID)
	if !ok {
		return domain.Host{}, fmt.Errorf("workspace %q not found", workspaceID)
	}
	digest := sha256.Sum256([]byte(workspaceID))
	id := "workspace_" + hex.EncodeToString(digest[:8])
	if host, err := s.store.GetHost(ctx, id); err == nil {
		return host, nil
	}
	now := time.Now().UTC()
	return s.store.UpsertHost(ctx, domain.Host{ID: id, Name: "Workspace / " + workspaceID, Address: "local-workspace", Port: 1, User: "opsnerva", AuthType: "workspace", SudoMode: "none", CreatedAt: now})
}

func (s *Service) resolveWorkspacePath(workspace config.Workspace, relative string, allowMissing bool) (string, error) {
	relative = normalizedWorkspaceRelativePath(relative)
	localRelative := filepath.FromSlash(relative)
	if path.IsAbs(relative) || filepath.IsAbs(localRelative) {
		return "", fmt.Errorf(`workspace path must be relative; omit path or use "." for the Workspace root (examples: "src", "src/main.go"); absolute paths such as "/workspace" are invalid`)
	}
	if path.Clean(relative) != relative || relative == ".." || strings.HasPrefix(relative, "../") || strings.ContainsAny(relative, "\\\x00\r\n") {
		return "", fmt.Errorf(`workspace path must be clean and relative (examples: ".", "src", "src/main.go")`)
	}
	for _, component := range strings.Split(relative, "/") {
		if isSensitiveWorkspaceComponent(component) {
			return "", fmt.Errorf("workspace path is sensitive and denied")
		}
	}
	root, err := filepath.EvalSymlinks(workspace.Root)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	target := filepath.Join(root, localRelative)
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		if !allowMissing || !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent, parentErr := filepath.EvalSymlinks(filepath.Dir(target))
		if parentErr != nil {
			return "", parentErr
		}
		resolved = filepath.Join(parent, filepath.Base(target))
	}
	if resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		return "", fmt.Errorf("workspace path escapes its configured root")
	}
	if info, lstatErr := os.Lstat(target); lstatErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("workspace symbolic links are denied")
	}
	return resolved, nil
}

func normalizedWorkspaceRelativePath(relative string) string {
	relative = strings.TrimSpace(relative)
	if relative == "" {
		return "."
	}
	return relative
}

func readWorkspaceFile(path, displayPath string, maxBytes int, offset int64, tailLines int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("workspace target is not a regular file")
	}
	resolvedOffset := resolvedFileOffset(info.Size(), offset)
	if tailLines > 0 {
		resolvedOffset, err = workspaceTailOffset(file, info.Size(), tailLines)
		if err != nil {
			return nil, err
		}
	}
	if _, err := file.Seek(resolvedOffset, io.SeekStart); err != nil {
		return nil, err
	}
	var content []byte
	if maxBytes > 0 {
		content, err = io.ReadAll(io.LimitReader(file, int64(maxBytes)))
	} else {
		content, err = io.ReadAll(file)
	}
	if err != nil {
		return nil, err
	}
	digest := sha256.New()
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if _, err := io.Copy(digest, file); err != nil {
		return nil, err
	}
	metadata := fmt.Sprintf("%s\n%d\t%o\t%s\t%s\t%d\t%d\n%x  %s\n%s\n", fileMetaMarker, info.Size(), info.Mode().Perm(), "local", "local", info.ModTime().Unix(), resolvedOffset, digest.Sum(nil), displayPath, fileContentMarker)
	return append([]byte(metadata), content...), nil
}

func workspaceTailOffset(file *os.File, size int64, lines int) (int64, error) {
	if lines <= 0 || size <= 0 {
		return 0, nil
	}
	const blockSize int64 = 32 << 10
	remaining := lines
	position := size
	ignoreTrailingNewline := true
	buffer := make([]byte, blockSize)
	for position > 0 {
		start := position - blockSize
		if start < 0 {
			start = 0
		}
		length := position - start
		read, err := file.ReadAt(buffer[:length], start)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		for index := read - 1; index >= 0; index-- {
			if buffer[index] != '\n' {
				ignoreTrailingNewline = false
				continue
			}
			if ignoreTrailingNewline {
				ignoreTrailingNewline = false
				continue
			}
			remaining--
			if remaining == 0 {
				return start + int64(index) + 1, nil
			}
		}
		position = start
	}
	return 0, nil
}

func listWorkspaceDirectory(path string) ([]byte, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	type item struct {
		Name string `json:"name"`
		Type string `json:"type"`
		Size int64  `json:"size,omitempty"`
	}
	result := make([]item, 0, len(entries))
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || isSensitiveWorkspaceComponent(entry.Name()) {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}
		kind := "file"
		if entry.IsDir() {
			kind = "directory"
		}
		result = append(result, item{Name: entry.Name(), Type: kind, Size: info.Size()})
	}
	return json.Marshal(map[string]any{"entries": result})
}

func isSensitiveWorkspaceComponent(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasPrefix(lower, ".env") || strings.HasPrefix(lower, ".opsnerva-") || lower == ".ssh" || lower == "data" || lower == "master.key" || strings.Contains(lower, "credential")
}

func searchWorkspaceFile(path, pattern string, matchMode domain.FileSearchMatchMode, contextLines int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var matches func(string) bool
	switch matchMode {
	case domain.FileSearchLiteral:
		matches = func(line string) bool { return strings.Contains(line, pattern) }
	case domain.FileSearchRegex:
		expression, compileErr := regexp.CompilePOSIX(pattern)
		if compileErr != nil {
			return nil, fmt.Errorf("invalid POSIX search regex: %w", compileErr)
		}
		matches = expression.MatchString
	default:
		return nil, fmt.Errorf("invalid search match_mode: use literal or regex")
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), int(^uint(0)>>1))
	var output strings.Builder
	type bufferedLine struct {
		number  int
		content string
		match   bool
	}
	before := make([]bufferedLine, 0, contextLines)
	line, lastOutputLine, afterRemaining := 0, 0, 0
	writeLine := func(item bufferedLine) {
		if item.number <= lastOutputLine {
			return
		}
		separator := "-"
		if item.match {
			separator = ":"
		}
		fmt.Fprintf(&output, "%d%s%s\n", item.number, separator, item.content)
		lastOutputLine = item.number
	}
	for scanner.Scan() {
		line++
		item := bufferedLine{number: line, content: scanner.Text()}
		item.match = matches(item.content)
		if item.match {
			for _, previous := range before {
				writeLine(previous)
			}
			writeLine(item)
			afterRemaining = contextLines
		} else if afterRemaining > 0 {
			writeLine(item)
			afterRemaining--
		}
		if contextLines > 0 {
			before = append(before, item)
			if len(before) > contextLines {
				before = before[len(before)-contextLines:]
			}
		}
	}
	return []byte(output.String()), scanner.Err()
}

func (s *Service) editWorkspaceFile(ctx context.Context, workspace config.Workspace, path string, req domain.ExecRequest) (sshx.RawResult, error) {
	started := time.Now()
	if req.Change == nil {
		return sshx.RawResult{}, fmt.Errorf("workspace file change is missing")
	}
	if req.TextEdit == nil {
		return sshx.RawResult{}, fmt.Errorf("workspace text edit is missing")
	}
	if err := validateTextEditChange(req.RelativePath, *req.TextEdit, *req.Change); err != nil {
		return sshx.RawResult{}, err
	}
	info, statErr := os.Stat(path)
	existed := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return sshx.RawResult{}, statErr
	}
	creating := req.TextEdit.OldText == ""
	if !existed && !creating {
		return sshx.RawResult{ExitCode: 1, Stderr: []byte("workspace edit target does not exist"), Duration: time.Since(started)}, fmt.Errorf("workspace edit target does not exist")
	}
	if existed && creating {
		message := "workspace create target already exists"
		return sshx.RawResult{ExitCode: 75, Stderr: []byte(message), Duration: time.Since(started)}, fmt.Errorf("%s", message)
	}
	if existed && !info.Mode().IsRegular() {
		return sshx.RawResult{}, fmt.Errorf("workspace edit target is not a regular file")
	}
	mode := os.FileMode(0o600)
	var original []byte
	var originalDigest [sha256.Size]byte
	var updated []byte
	var err error
	if existed {
		mode = info.Mode().Perm()
		original, err = os.ReadFile(path)
		if err != nil {
			return sshx.RawResult{}, err
		}
		originalDigest = sha256.Sum256(original)
		updated, err = applyTextEditBytes(original, *req.TextEdit)
		if err != nil {
			return sshx.RawResult{ExitCode: 1, Stderr: []byte(err.Error()), Duration: time.Since(started)}, err
		}
	} else {
		updated = []byte(req.TextEdit.NewText)
	}
	suffix := time.Now().UTC().Format("20060102T150405Z") + "-" + ids.New("file")
	temporary := filepath.Join(filepath.Dir(path), ".opsnerva-"+filepath.Base(path)+"-"+suffix+".tmp")
	if err := writeSyncedFile(temporary, updated, mode); err != nil {
		return sshx.RawResult{}, err
	}
	defer os.Remove(temporary)
	validationOutput, err := s.runWorkspaceValidator(ctx, req.Validator, workspace, req.RelativePath, temporary)
	if err != nil {
		_ = os.Remove(temporary)
		return sshx.RawResult{ExitCode: 74, Stdout: validationOutput, Stderr: []byte(err.Error()), Duration: time.Since(started)}, err
	}
	if existed {
		current, readErr := os.ReadFile(path)
		if readErr != nil {
			return sshx.RawResult{}, readErr
		}
		if sha256.Sum256(current) != originalDigest {
			message := "workspace file edit conflict: target changed during validation"
			return sshx.RawResult{ExitCode: 75, Stderr: []byte(message), Duration: time.Since(started)}, fmt.Errorf("%s", message)
		}
		if err = os.Rename(temporary, path); err != nil {
			return sshx.RawResult{}, err
		}
	} else {
		if err = os.Link(temporary, path); err != nil {
			if errors.Is(err, os.ErrExist) {
				message := "workspace file edit conflict: create target appeared during validation"
				return sshx.RawResult{ExitCode: 75, Stderr: []byte(message), Duration: time.Since(started)}, fmt.Errorf("%s", message)
			}
			return sshx.RawResult{}, err
		}
		if err = os.Remove(temporary); err != nil {
			return sshx.RawResult{}, err
		}
	}
	_ = os.Remove(temporary)
	if err := syncLocalDirectory(filepath.Dir(path)); err != nil {
		return sshx.RawResult{ExitCode: 74, Stderr: []byte(err.Error()), Duration: time.Since(started)}, err
	}
	afterDigest := sha256.Sum256(updated)
	stdout := ""
	if req.Validator != "" {
		stdout = fileValidationMarker + "\n"
	}
	stdout += fmt.Sprintf("%s\n%x  %s\n", fileAfterMarker, afterDigest, req.RelativePath)
	stdout += string(validationOutput)
	return sshx.RawResult{ExitCode: 0, Stdout: []byte(stdout), Duration: time.Since(started)}, nil
}

func (s *Service) workspaceValidator(id string, workspace config.Workspace, relative string) (config.Validator, error) {
	if id == "" {
		return config.Validator{}, nil
	}
	validator, ok := s.validators[id]
	if !ok || validator.Scope != "workspace" {
		available := s.ValidatorIDs("workspace")
		if len(available) == 0 {
			return config.Validator{}, fmt.Errorf("invalid validator_id %q: no Workspace validator IDs are configured; omit validator_id", id)
		}
		return config.Validator{}, fmt.Errorf("invalid Workspace validator_id %q; available IDs: %s", id, strings.Join(available, ", "))
	}
	if !validatorAllowsPath(validator, filepath.Join(workspace.Root, relative)) && !validatorAllowsPath(validator, relative) {
		return config.Validator{}, fmt.Errorf("validator_id %q is not allowed for Workspace path %s", id, relative)
	}
	return validator, nil
}

func (s *Service) runWorkspaceValidator(ctx context.Context, id string, workspace config.Workspace, relative, path string) ([]byte, error) {
	validator, err := s.workspaceValidator(id, workspace, relative)
	if err != nil || id == "" {
		return nil, err
	}
	args := make([]string, len(validator.Args))
	for index, argument := range validator.Args {
		args[index] = strings.ReplaceAll(argument, "{{path}}", path)
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(validator.TimeoutSeconds)*time.Second)
	defer cancel()
	command := exec.CommandContext(timeoutCtx, validator.Program, args...)
	command.Dir = workspace.Root
	command.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	err = command.Run()
	return output.Bytes(), err
}

func writeSyncedFile(path string, content []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	succeeded := false
	defer func() {
		if !succeeded {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(content); err != nil {
		file.Close()
		return err
	}
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	succeeded = true
	return nil
}

type editableTextLine struct {
	text              string
	start, contentEnd int
	afterEnd          int
	eol               []byte
}

func editableTextLines(original []byte) ([]editableTextLine, error) {
	if bytes.HasPrefix(original, []byte{0xff, 0xfe}) || bytes.HasPrefix(original, []byte{0xfe, 0xff}) {
		return nil, fmt.Errorf("UTF-16 file editing is unsupported; convert the file to UTF-8 first")
	}
	if bytes.IndexByte(original, 0) >= 0 {
		return nil, fmt.Errorf("binary file editing is unsupported")
	}
	start := 0
	if bytes.HasPrefix(original, []byte{0xef, 0xbb, 0xbf}) {
		start = 3
	}
	lines := make([]editableTextLine, 0, bytes.Count(original[start:], []byte{'\n'})+1)
	for start < len(original) {
		newlineOffset := bytes.IndexByte(original[start:], '\n')
		if newlineOffset < 0 {
			lines = append(lines, editableTextLine{text: string(original[start:]), start: start, contentEnd: len(original), afterEnd: len(original)})
			break
		}
		newline := start + newlineOffset
		contentEnd := newline
		if contentEnd > start && original[contentEnd-1] == '\r' {
			contentEnd--
		}
		lines = append(lines, editableTextLine{
			text: string(original[start:contentEnd]), start: start, contentEnd: contentEnd, afterEnd: newline + 1,
			eol: append([]byte(nil), original[contentEnd:newline+1]...),
		})
		start = newline + 1
	}
	return lines, nil
}

func applyTextEditBytes(original []byte, edit domain.TextEdit) ([]byte, error) {
	originalLines, err := editableTextLines(original)
	if err != nil {
		return nil, err
	}
	oldLines := strings.Split(edit.OldText, "\n")
	var newLines []string
	if edit.NewText != "" {
		newLines = strings.Split(edit.NewText, "\n")
	}
	matches := make([]int, 0, 2)
	for start := 0; start+len(oldLines) <= len(originalLines); start++ {
		matched := true
		for offset := range oldLines {
			if originalLines[start+offset].text != oldLines[offset] {
				matched = false
				break
			}
		}
		if matched {
			matches = append(matches, start)
		}
	}
	if len(matches) != 1 {
		return nil, fmt.Errorf("file edit conflict: old_text matched %d blocks; read the current file and retry with a unique block", len(matches))
	}
	start := matches[0]
	first := originalLines[start]
	last := originalLines[start+len(oldLines)-1]
	spliceStart, spliceEnd := first.start, last.contentEnd
	if len(newLines) == 0 {
		if last.afterEnd > last.contentEnd {
			spliceEnd = last.afterEnd
		} else if start > 0 {
			spliceStart = originalLines[start-1].contentEnd
		}
		updated := make([]byte, 0, len(original)-(spliceEnd-spliceStart))
		updated = append(updated, original[:spliceStart]...)
		updated = append(updated, original[spliceEnd:]...)
		return updated, nil
	}
	eol := []byte{'\n'}
	for index := start; index <= start+len(oldLines)-1; index++ {
		if len(originalLines[index].eol) > 0 {
			eol = originalLines[index].eol
			break
		}
	}
	if len(last.eol) == 0 && start > 0 && len(originalLines[start-1].eol) > 0 {
		eol = originalLines[start-1].eol
	}
	var replacement bytes.Buffer
	for index, line := range newLines {
		if index > 0 {
			replacement.Write(eol)
		}
		replacement.WriteString(line)
	}
	updated := make([]byte, 0, len(original)-(spliceEnd-spliceStart)+replacement.Len())
	updated = append(updated, original[:spliceStart]...)
	updated = append(updated, replacement.Bytes()...)
	updated = append(updated, original[spliceEnd:]...)
	return updated, nil
}

func applyTextEdit(original string, edit domain.TextEdit) (string, error) {
	updated, err := applyTextEditBytes([]byte(original), edit)
	return string(updated), err
}
