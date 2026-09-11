package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
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

var workspaceIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

func (s *Service) InitializeWorkspaces(ctx context.Context, workspaceRoot string) error {
	workspaceRoot = filepath.Clean(strings.TrimSpace(workspaceRoot))
	if workspaceRoot == "." || !filepath.IsAbs(workspaceRoot) {
		return fmt.Errorf("workspace root must be absolute")
	}
	if filepath.Dir(workspaceRoot) == workspaceRoot {
		return fmt.Errorf("a filesystem root cannot be used as the workspace directory")
	}
	if err := ensureWorkspaceDirectory(workspaceRoot); err != nil {
		return fmt.Errorf("prepare workspace directory: %w", err)
	}
	workspaceRoot, err := filepath.EvalSymlinks(workspaceRoot)
	if err != nil {
		return fmt.Errorf("resolve workspace directory: %w", err)
	}
	if s.dataDir != "" {
		dataRoot, err := filepath.EvalSymlinks(s.dataDir)
		if err != nil {
			return fmt.Errorf("resolve application data directory: %w", err)
		}
		if localPathContains(workspaceRoot, dataRoot) || localPathContains(dataRoot, workspaceRoot) {
			return fmt.Errorf("workspace directory cannot overlap the application data directory")
		}
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
		return fmt.Errorf("workspace directories cannot contain symbolic links")
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
