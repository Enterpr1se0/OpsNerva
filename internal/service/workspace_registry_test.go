package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestConversationWorkspaceBindingIsAuthoritative(t *testing.T) {
	svc, _ := newWorkspaceService(t, "read_write")
	ctx := context.Background()
	if _, err := svc.CreateAdminWorkspace(ctx, domain.WorkspaceInput{ID: "other", Access: "read_only"}, "web-user"); err != nil {
		t.Fatal(err)
	}
	session, err := svc.PrepareChatSession(ctx, "session-workspace", "project", "web-user")
	if err != nil {
		t.Fatal(err)
	}
	if session.WorkspaceID != "project" {
		t.Fatalf("bound Workspace = %q", session.WorkspaceID)
	}
	capability, err := svc.SessionWorkspace(WithSessionID(ctx, session.ID))
	if err != nil {
		t.Fatal(err)
	}
	if capability.ID != "project" || capability.Access != "read_write" {
		t.Fatalf("session capability = %#v", capability)
	}
	if _, err := svc.PrepareChatSession(ctx, session.ID, "other", "web-user"); err == nil || !strings.Contains(err.Error(), "switch it before sending") {
		t.Fatalf("mismatched request Workspace was accepted: %v", err)
	}
	unbound, err := svc.SetChatSessionWorkspace(ctx, session.ID, "", "web-user")
	if err != nil {
		t.Fatal(err)
	}
	if unbound.WorkspaceID != "" {
		t.Fatalf("unbound Workspace = %q", unbound.WorkspaceID)
	}
	if _, err := svc.SessionWorkspace(WithSessionID(ctx, session.ID)); err == nil || !strings.Contains(err.Error(), "no Workspace is bound") {
		t.Fatalf("unbound conversation resolved a Workspace: %v", err)
	}
	if _, err := svc.SetChatSessionWorkspace(ctx, session.ID, "missing", "web-user"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing Workspace was accepted: %v", err)
	}
	if _, err := svc.SetChatSessionWorkspace(ctx, session.ID, "project", "web-user"); err != nil {
		t.Fatal(err)
	}
	events, _, unsubscribe := svc.SubscribeStateEvents()
	defer unsubscribe()
	if err := svc.DeleteAdminWorkspace(ctx, "project", "web-user"); err != nil {
		t.Fatal(err)
	}
	afterDelete, err := svc.GetChatSession(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterDelete.WorkspaceID != "" {
		t.Fatalf("deleted Workspace remains bound: %q", afterDelete.WorkspaceID)
	}
	seen := map[string]bool{}
	for len(events) > 0 {
		seen[(<-events).Topic] = true
	}
	if !seen[StateTopicSessions] || !seen[StateTopicChatState] || !seen[StateTopicAudit] {
		t.Fatalf("Workspace deletion lost committed state notifications: %+v", seen)
	}
}

func TestWorkspaceRegistryReloadKeepsPersistedAccessAndAudit(t *testing.T) {
	svc, projectRoot := newWorkspaceService(t, "read_write")
	ctx := context.Background()
	if _, err := svc.CreateAdminWorkspace(ctx, domain.WorkspaceInput{ID: "persisted"}, "operator"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateAdminWorkspace(ctx, "persisted", domain.WorkspaceInput{Access: "read_write"}, "operator"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateAdminWorkspace(ctx, "persisted", domain.WorkspaceInput{ID: "renamed", Access: "read_only"}, "operator"); err == nil {
		t.Fatal("immutable Workspace ID was changed")
	}
	cfg := config.Default()
	cfg.DataDir = svc.dataDir
	reloaded := New(svc.store, svc.transport, svc.encryptor, svc.redactor, svc.limits, cfg)
	t.Cleanup(func() {
		if err := reloaded.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if err := reloaded.InitializeWorkspaces(ctx, filepath.Dir(projectRoot)); err != nil {
		t.Fatal(err)
	}
	workspace, ok := reloaded.workspaces.Get("persisted")
	if !ok || workspace.Access != "read_write" || filepath.Base(workspace.Root) != "persisted" {
		t.Fatalf("registry reload lost persisted configuration: %+v", workspace)
	}
	if _, ok := reloaded.workspaces.Get("default"); ok {
		t.Fatal("registry reload resurrected deleted default Workspace")
	}
	events, err := svc.ListAudit(ctx, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, event := range events {
		if event.Data["workspace_id"] == "persisted" {
			counts[event.Type]++
			if event.Actor != "operator" {
				t.Fatalf("registry audit owner changed: %+v", event)
			}
		}
	}
	if counts["workspace_created"] != 1 || counts["workspace_updated"] != 1 || len(counts) != 2 {
		t.Fatalf("registry emitted missing or duplicate audit: %+v", counts)
	}
}

func TestWorkspaceRegistryRetainsActiveTerminalGuard(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	// Keep this boundary test independent of local Bash/PTY availability.
	state := &sshShellState{shell: domain.SSHShell{
		ID: "workspace-fixture", Kind: domain.SSHShellKindWorkspace, WorkspaceID: "project", Status: "running",
	}}
	state.history = newMemoryShellHistory(state.shell)
	if err := svc.shells.add(state); err != nil {
		t.Fatal(err)
	}
	defer svc.shells.remove(state)
	ctx := context.Background()
	if _, err := svc.UpdateAdminWorkspace(ctx, "project", domain.WorkspaceInput{Access: "read_only"}, "operator"); err == nil || !strings.Contains(err.Error(), "active terminal") {
		t.Fatalf("active terminal allowed access change: %v", err)
	}
	if err := svc.DeleteAdminWorkspace(ctx, "project", "operator"); err == nil || !strings.Contains(err.Error(), "active terminal") {
		t.Fatalf("active terminal allowed unregistration: %v", err)
	}
	if workspace, ok := svc.workspaces.Get("project"); !ok || workspace.Access != "read_write" {
		t.Fatalf("rejected mutation changed registration: %+v", workspace)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("rejected mutation removed Workspace directory: %v", err)
	}
}

func runApprovedWorkspaceAccess(t *testing.T, svc *Service, invoke func(context.Context) (domain.ExecResult, error)) domain.ExecResult {
	t.Helper()
	base, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	pending, err := invoke(base)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != "approval_required" || pending.ApprovalID == "" {
		t.Fatalf("Workspace file access skipped approval: %#v", pending)
	}
	completed, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed file access", "operator")
	if err != nil {
		t.Fatal(err)
	}
	return completed
}

func TestWorkspaceAdminCreateUpdateAndRemove(t *testing.T) {
	svc, projectRoot := newWorkspaceService(t, "read_write")
	created, err := svc.CreateAdminWorkspace(context.Background(), domain.WorkspaceInput{ID: "docs", Access: "read_only"}, "admin-web")
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != "docs" || created.Access != "read_only" {
		t.Fatalf("unexpected created workspace: %#v", created)
	}
	docsRoot := filepath.Join(filepath.Dir(projectRoot), "docs")
	if info, err := os.Stat(docsRoot); err != nil || !info.IsDir() {
		t.Fatalf("managed Workspace directory was not created: %v", err)
	}
	if err := os.WriteFile(filepath.Join(docsRoot, "preserved.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(docsRoot, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docsRoot, "nested", "inner.txt"), []byte("deep"), 0o600); err != nil {
		t.Fatal(err)
	}
	updated, err := svc.UpdateAdminWorkspace(context.Background(), "project", domain.WorkspaceInput{ID: "project", Access: "read_only"}, "admin-web")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Access != "read_only" {
		t.Fatalf("workspace access was not updated: %#v", updated)
	}
	if _, err := svc.UploadWorkspaceFile(context.Background(), "project", "blocked.txt", "blocked.txt", strings.NewReader("x"), "admin-web"); err == nil || !strings.Contains(err.Error(), "read_only") {
		t.Fatalf("updated read-only permission was not enforced: %v", err)
	}
	if err := svc.DeleteAdminWorkspace(context.Background(), "docs", "admin-web"); err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.workspaces.Get("docs"); ok {
		t.Fatal("removed workspace remains active")
	}
	if _, err := os.Lstat(docsRoot); !os.IsNotExist(err) {
		t.Fatalf("removing the Workspace did not delete its directory: %v", err)
	}
	if _, err := svc.CreateAdminWorkspace(context.Background(), domain.WorkspaceInput{ID: "docs", Access: "read_write"}, "admin-web"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(docsRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("re-added Workspace directory is not empty: %d entries", len(entries))
	}
}

func TestWorkspaceManagedDirectoriesRejectUnsafeNamesAndSymlinks(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	for _, id := range []string{".", "..", "CON", "com1.txt", "trailing."} {
		if _, err := svc.CreateAdminWorkspace(context.Background(), domain.WorkspaceInput{ID: id, Access: "read_write"}, "admin-web"); err == nil {
			t.Fatalf("unsafe Workspace id %q was accepted", id)
		}
	}
	if _, err := svc.CreateAdminWorkspace(context.Background(), domain.WorkspaceInput{ID: "PROJECT", Access: "read_write"}, "admin-web"); err == nil {
		t.Fatal("case-insensitive duplicate Workspace id was accepted")
	}

	target := t.TempDir()
	linkedRoot := filepath.Join(filepath.Dir(root), "linked")
	if err := os.Symlink(target, linkedRoot); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}
	if err := svc.InitializeWorkspaces(context.Background(), linkedRoot); err == nil || !strings.Contains(err.Error(), "symbolic links") {
		t.Fatalf("symlinked managed root was accepted: %v", err)
	}
	if _, err := svc.CreateAdminWorkspace(context.Background(), domain.WorkspaceInput{ID: "linked", Access: "read_write"}, "admin-web"); err == nil || !strings.Contains(err.Error(), "symbolic links") {
		t.Fatalf("symlinked Workspace directory was accepted: %v", err)
	}

	parentTarget := t.TempDir()
	parentLink := filepath.Join(t.TempDir(), "parent-link")
	if err := os.Symlink(parentTarget, parentLink); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}
	configuredRoot := filepath.Join(parentLink, "managed")
	if err := svc.InitializeWorkspaces(context.Background(), configuredRoot); err != nil {
		t.Fatalf("Workspace root below a symlinked system parent was rejected: %v", err)
	}
	workspace, ok := svc.workspaces.Get("project")
	if !ok || !sameWorkspaceFile(filepath.Dir(workspace.Root), filepath.Join(parentTarget, "managed")) {
		t.Fatalf("Workspace root was not canonicalized: %+v", workspace)
	}
}
