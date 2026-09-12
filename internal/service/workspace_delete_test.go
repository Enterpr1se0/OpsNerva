package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestWorkspaceAdminDeletePermanentlyRemovesDirectory(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	directory := filepath.Join(root, "build")
	if err := os.MkdirAll(filepath.Join(directory, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "assets", "app.js"), []byte("console.log('ok')\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	deleted, err := svc.DeleteAdminWorkspaceEntry(context.Background(), "project", "build", "admin-web")
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Path != "build" || deleted.Type != "directory" || deleted.Size != 0 || deleted.SHA256 != "" {
		t.Fatalf("unexpected directory delete result: %#v", deleted)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("deleted directory remains at its original path: %v", err)
	}
	if _, err := svc.DeleteAdminWorkspaceEntry(context.Background(), "project", ".", "admin-web"); err == nil || !strings.Contains(err.Error(), "root cannot be deleted") {
		t.Fatalf("Workspace root deletion was accepted: %v", err)
	}
}

func TestAgentWorkspaceDeleteRequiresApprovalAndRecursiveIntent(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	directory := filepath.Join(root, "generated")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "artifact.txt"), []byte("temporary\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DeleteWorkspaceEntry(context.Background(), "project", "generated", false, "remove generated output", "eino-agent"); err == nil || !strings.Contains(err.Error(), "recursive=true") {
		t.Fatalf("non-empty directory deletion did not require recursive intent: %v", err)
	}
	pending, err := svc.DeleteWorkspaceEntry(context.Background(), "project", "generated", true, "remove generated output", "eino-agent")
	if err != nil || pending.Status != "approval_required" {
		t.Fatalf("Workspace delete did not require approval: result=%#v err=%v", pending, err)
	}
	approved, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed permanent deletion", "operator")
	if err != nil || approved.Status != "completed" || !strings.Contains(approved.Stdout, `"path":"generated"`) {
		t.Fatalf("approved Workspace delete failed: result=%#v err=%v", approved, err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("deleted Workspace directory still exists: %v", err)
	}
	if _, err := svc.DeleteWorkspaceEntry(context.Background(), "project", ".", true, "remove root", "eino-agent"); err == nil || !strings.Contains(err.Error(), "root cannot be deleted") {
		t.Fatalf("Workspace root deletion was accepted: %v", err)
	}
}

func TestWorkspaceDeleteRechecksAccessAndRecursiveIntentAfterApproval(t *testing.T) {
	for _, change := range []string{"access", "directory contents"} {
		t.Run(change, func(t *testing.T) {
			svc, root := newWorkspaceService(t, "read_write")
			ctx := context.Background()
			if err := svc.CreateAdminWorkspaceDirectory(ctx, "project", "generated"); err != nil {
				t.Fatal(err)
			}
			pending, err := svc.DeleteWorkspaceEntry(ctx, "project", "generated", false, "remove empty directory", "eino-agent")
			if err != nil || pending.Status != "approval_required" {
				t.Fatalf("delete skipped approval: %+v err=%v", pending, err)
			}
			if change == "access" {
				if _, err := svc.UpdateAdminWorkspace(ctx, "project", domain.WorkspaceInput{Access: "read_only"}, "operator"); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(filepath.Join(root, "generated", "new.txt"), []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := svc.Approve(ctx, pending.ApprovalID, "reviewed", "operator")
			if err == nil || result.Status != "failed" {
				t.Fatalf("stale permission/intent used: %+v err=%v", result, err)
			}
			if _, err := os.Stat(filepath.Join(root, "generated")); err != nil {
				t.Fatalf("failed deletion removed target: %v", err)
			}
			events, err := svc.ListAudit(ctx, "", 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event.Type == "workspace_directory_deleted" {
					t.Fatalf("failed deletion was audited as success: %+v", event)
				}
			}
		})
	}
}
