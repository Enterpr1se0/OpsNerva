package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if _, ok := svc.workspaceByID("docs"); ok {
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
	if !sameWorkspaceFile(svc.workspaceRoot, filepath.Join(parentTarget, "managed")) {
		t.Fatalf("Workspace root was not canonicalized: %q", svc.workspaceRoot)
	}
}
