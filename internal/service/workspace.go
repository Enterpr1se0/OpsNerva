package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

func (s *Service) InitializeWorkspaces(ctx context.Context, workspaceRoot string) error {
	return s.workspaces.Initialize(ctx, workspaceRoot, s.dataDir)
}

func (s *Service) CreateAdminWorkspace(ctx context.Context, input domain.WorkspaceInput, actor string) (AdminWorkspaceCapability, error) {
	workspace, err := s.workspaces.Create(ctx, input.ID, input.Access)
	if err != nil {
		return AdminWorkspaceCapability{}, err
	}
	s.audit(ctx, "", "workspace_created", actor, map[string]any{"workspace_id": workspace.ID, "access": workspace.Access})
	return s.adminWorkspaceCapability(workspace), nil
}

func (s *Service) UpdateAdminWorkspace(ctx context.Context, id string, input domain.WorkspaceInput, actor string) (AdminWorkspaceCapability, error) {
	id = strings.TrimSpace(id)
	if input.ID != "" && strings.TrimSpace(input.ID) != id {
		return AdminWorkspaceCapability{}, fmt.Errorf("workspace id cannot be changed")
	}
	current, exists := s.workspaces.Get(id)
	if !exists {
		return AdminWorkspaceCapability{}, fmt.Errorf("workspace %q not found", id)
	}
	if strings.TrimSpace(input.Access) != current.Access && s.hasActiveWorkspaceShell(id) {
		return AdminWorkspaceCapability{}, fmt.Errorf("workspace %q has an active terminal", id)
	}
	workspace, err := s.workspaces.Update(ctx, id, input.Access)
	if err != nil {
		return AdminWorkspaceCapability{}, err
	}
	s.audit(ctx, "", "workspace_updated", actor, map[string]any{"workspace_id": id, "access": workspace.Access})
	return s.adminWorkspaceCapability(workspace), nil
}

func (s *Service) DeleteAdminWorkspace(ctx context.Context, id, actor string) error {
	id = strings.TrimSpace(id)
	if _, ok := s.workspaces.Get(id); !ok {
		return fmt.Errorf("workspace %q not found", id)
	}
	if s.hasActiveWorkspaceShell(id) {
		return fmt.Errorf("workspace %q has an active terminal", id)
	}
	removed, err := s.workspaces.Delete(ctx, id)
	if removed != nil {
		s.audit(ctx, "", "workspace_removed", actor, map[string]any{
			"workspace_id": id, "root": removed.Workspace.Root, "files_removed": removed.FilesRemoved,
		})
	}
	return err
}

func (s *Service) ListWorkspaceCapabilities() []WorkspaceCapability {
	workspaces := s.workspaces.Snapshot()
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
	_, ok := s.workspaces.Get(workspaceID)
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
