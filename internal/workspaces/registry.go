// Package workspaces owns managed Workspace registration and directory lifecycle.
// It does not own sessions, shell runtimes, approval, audit, or tool projections.
package workspaces

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

// Persistence is implemented directly by Store. DeleteWorkspace must also
// remove session bindings in the same database transaction.
type Persistence interface {
	InitializeWorkspaces(context.Context) error
	ListWorkspaces(context.Context) ([]domain.Workspace, error)
	CreateWorkspace(context.Context, domain.Workspace) error
	UpdateWorkspace(context.Context, domain.Workspace) error
	DeleteWorkspace(context.Context, string) error
}

type Registry struct {
	persistence Persistence
	// Serialize persistence, snapshot publication and directory removal. Do not
	// hold the snapshot lock during I/O or synchronous persistence notifications.
	mutationMu sync.Mutex
	mu         sync.RWMutex
	root       string
	entries    map[string]config.Workspace
}

func New(persistence Persistence) *Registry {
	return &Registry{persistence: persistence, entries: make(map[string]config.Workspace)}
}

func (r *Registry) Initialize(ctx context.Context, workspaceRoot, dataDir string) error {
	r.mutationMu.Lock()
	defer r.mutationMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	workspaceRoot = filepath.Clean(strings.TrimSpace(workspaceRoot))
	if workspaceRoot == "." || !filepath.IsAbs(workspaceRoot) {
		return fmt.Errorf("workspace root must be absolute")
	}
	if filepath.Dir(workspaceRoot) == workspaceRoot {
		return fmt.Errorf("a filesystem root cannot be used as the workspace directory")
	}
	if err := ensureDirectory(workspaceRoot); err != nil {
		return fmt.Errorf("prepare workspace directory: %w", err)
	}
	workspaceRoot, err := filepath.EvalSymlinks(workspaceRoot)
	if err != nil {
		return fmt.Errorf("resolve workspace directory: %w", err)
	}
	if dataDir != "" {
		dataRoot, err := filepath.EvalSymlinks(dataDir)
		if err != nil {
			return fmt.Errorf("resolve application data directory: %w", err)
		}
		if pathContains(workspaceRoot, dataRoot) || pathContains(dataRoot, workspaceRoot) {
			return fmt.Errorf("workspace directory cannot overlap the application data directory")
		}
	}
	if err := r.persistence.InitializeWorkspaces(ctx); err != nil {
		return err
	}
	stored, err := r.persistence.ListWorkspaces(ctx)
	if err != nil {
		return err
	}
	loaded := make(map[string]config.Workspace, len(stored))
	for _, workspace := range stored {
		candidate := config.Workspace{ID: workspace.ID, Root: filepath.Join(workspaceRoot, workspace.ID), Access: workspace.Access}
		if err := validateIdentity(candidate.ID, candidate.Access); err != nil {
			return fmt.Errorf("stored workspace %q is invalid: %w", workspace.ID, err)
		}
		if err := ensureDirectory(candidate.Root); err != nil {
			return fmt.Errorf("prepare workspace %q: %w", candidate.ID, err)
		}
		loaded[candidate.ID] = candidate
	}
	r.mu.Lock()
	r.root = workspaceRoot
	r.entries = loaded
	r.mu.Unlock()
	return nil
}

func (r *Registry) Get(id string) (config.Workspace, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	workspace, ok := r.entries[strings.TrimSpace(id)]
	return workspace, ok
}

// Snapshot returns independent values; callers cannot change the live registry.
func (r *Registry) Snapshot() []config.Workspace {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]config.Workspace, 0, len(r.entries))
	for _, workspace := range r.entries {
		result = append(result, workspace)
	}
	return result
}

func (r *Registry) Create(ctx context.Context, id, access string) (config.Workspace, error) {
	r.mutationMu.Lock()
	defer r.mutationMu.Unlock()
	if err := ctx.Err(); err != nil {
		return config.Workspace{}, err
	}
	if r.root == "" {
		return config.Workspace{}, fmt.Errorf("workspace registry is not initialized")
	}
	workspace := config.Workspace{ID: strings.TrimSpace(id), Access: strings.TrimSpace(access)}
	if workspace.Access == "" {
		workspace.Access = "read_only"
	}
	// Other mutations hold mutationMu too, so this check and persistence belong
	// to one ordered change, including case-insensitive duplicate detection.
	for id := range r.entries {
		if strings.EqualFold(id, workspace.ID) {
			return config.Workspace{}, fmt.Errorf("workspace %q already exists", workspace.ID)
		}
	}
	if err := validateIdentity(workspace.ID, workspace.Access); err != nil {
		return config.Workspace{}, err
	}
	workspace.Root = filepath.Join(r.root, workspace.ID)
	if err := ensureDirectory(workspace.Root); err != nil {
		return config.Workspace{}, err
	}
	now := time.Now().UTC()
	if err := r.persistence.CreateWorkspace(ctx, domain.Workspace{ID: workspace.ID, Access: workspace.Access, CreatedAt: now, UpdatedAt: now}); err != nil {
		return config.Workspace{}, err
	}
	r.mu.Lock()
	r.entries[workspace.ID] = workspace
	r.mu.Unlock()
	return workspace, nil
}

func (r *Registry) Update(ctx context.Context, id, access string) (config.Workspace, error) {
	r.mutationMu.Lock()
	defer r.mutationMu.Unlock()
	if err := ctx.Err(); err != nil {
		return config.Workspace{}, err
	}
	id = strings.TrimSpace(id)
	workspace, exists := r.Get(id)
	if !exists {
		return config.Workspace{}, fmt.Errorf("workspace %q not found", id)
	}
	workspace.Access = strings.TrimSpace(access)
	if err := validateIdentity(workspace.ID, workspace.Access); err != nil {
		return config.Workspace{}, err
	}
	if err := ensureDirectory(workspace.Root); err != nil {
		return config.Workspace{}, err
	}
	if err := r.persistence.UpdateWorkspace(ctx, domain.Workspace{ID: id, Access: workspace.Access, UpdatedAt: time.Now().UTC()}); err != nil {
		return config.Workspace{}, err
	}
	r.mu.Lock()
	r.entries[id] = workspace
	r.mu.Unlock()
	return workspace, nil
}

// Removal is returned once unregistration commits, even if directory removal
// subsequently fails. Callers can audit that partial outcome without guessing.
type Removal struct {
	Workspace    config.Workspace
	FilesRemoved bool
}

func (r *Registry) Delete(ctx context.Context, id string) (*Removal, error) {
	r.mutationMu.Lock()
	defer r.mutationMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id = strings.TrimSpace(id)
	workspace, exists := r.Get(id)
	if !exists {
		return nil, fmt.Errorf("workspace %q not found", id)
	}
	if err := r.persistence.DeleteWorkspace(ctx, id); err != nil {
		return nil, err
	}
	r.mu.Lock()
	delete(r.entries, id)
	r.mu.Unlock()
	// Re-creation cannot interleave with removal. Only remove the registered
	// path strictly below the managed root, never the root itself.
	removed := &Removal{Workspace: workspace}
	if workspace.Root != "" && r.root != "" && filepath.Clean(workspace.Root) != filepath.Clean(r.root) && pathContains(workspace.Root, r.root) {
		if err := os.RemoveAll(workspace.Root); err != nil {
			return removed, fmt.Errorf("workspace %q was unregistered, but its directory could not be fully removed: %w", id, err)
		}
		removed.FilesRemoved = true
	}
	return removed, nil
}
