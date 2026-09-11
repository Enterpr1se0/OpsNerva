package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/fileedit"
)

func TestWorkspaceEditPathErrorKeepsToolClassification(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	pending, err := svc.EditWorkspaceFile(context.Background(), "project", "../outside", "old", "new", "", "change config", "eino-agent")
	if err != nil || pending.Status != "approval_required" {
		t.Fatalf("prepare edit: %+v err=%v", pending, err)
	}
	result, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed", "operator")
	var invalid *InputValidationError
	if !errors.As(err, &invalid) || result.Status != "failed" {
		t.Fatalf("path rejection lost tool classification: %+v err=%v", result, err)
	}
	assertWorkspaceEditFiles(t, root, false, "")
}

func TestWorkspaceEditValidatorAndResultProtocol(t *testing.T) {
	for _, action := range []string{"accept", "change", "create"} {
		t.Run(action, func(t *testing.T) {
			svc, root := newWorkspaceService(t, "read_write")
			target := filepath.Join(root, "app.conf")
			oldText := "port=8080"
			if action == "create" {
				oldText = ""
			} else if err := os.WriteFile(target, []byte(oldText+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			configureWorkspaceEditValidator(t, svc, target, action)
			pending, err := svc.EditWorkspaceFile(context.Background(), "project", "app.conf", oldText, "port=9090", "fixture", "change port", "eino-agent")
			if err != nil || pending.Status != "approval_required" {
				t.Fatalf("edit skipped approval: %+v err=%v", pending, err)
			}
			assertWorkspaceEditFiles(t, root, action != "create", "port=8080\n")
			result, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed", "operator")
			if action == "accept" {
				digest := sha256.Sum256([]byte("port=9090\n"))
				if err != nil || result.Status != "completed" || result.ExitCode != 0 || result.File == nil || !result.File.ValidationOK || result.File.Validator != "fixture" || result.File.SHA256 != hex.EncodeToString(digest[:]) {
					t.Fatalf("validated edit result: %+v err=%v", result, err)
				}
				if !strings.Contains(result.Stdout, "validator inspected staged content") || !strings.Contains(result.Stdout, fileValidationMarker) || !strings.Contains(result.Stdout, fileAfterMarker) || strings.Contains(result.Stdout, root) {
					t.Fatalf("edit output protocol changed: %q", result.Stdout)
				}
				assertWorkspaceEditFiles(t, root, true, "port=9090\n")
			} else {
				if err == nil || result.ExitCode != 75 || result.Status != "failed" || !strings.Contains(result.Stderr, "during validation") {
					t.Fatalf("validator-time conflict: %+v err=%v", result, err)
				}
				if result.File != nil && result.File.ValidationOK {
					t.Fatalf("conflict reported successful validation: %+v", result.File)
				}
				assertWorkspaceEditFiles(t, root, true, "port=7070\n")
			}
			run, readErr := svc.store.GetRun(context.Background(), result.RunID)
			if readErr != nil || string(run.Status) != result.Status {
				t.Fatalf("persisted edit status diverged: %+v err=%v", run, readErr)
			}
		})
	}
}

func TestWorkspaceEditRejectsCancelledOrTamperedRequest(t *testing.T) {
	for _, action := range []string{"cancelled", "tampered diff", "tampered text"} {
		t.Run(action, func(t *testing.T) {
			svc, root := newWorkspaceService(t, "read_write")
			if err := os.WriteFile(filepath.Join(root, "app.conf"), []byte("old\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			edit, change, err := fileedit.Build("app.conf", "old", "new")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch action {
			case "cancelled":
				cancel()
			case "tampered diff":
				change.Diff += "+unapproved\n"
			case "tampered text":
				edit.NewText = "unapproved"
			}
			result, err := svc.executeWorkspace(ctx, domain.ExecRequest{Mode: domain.ExecWorkspaceEdit, WorkspaceID: "project", RelativePath: "app.conf", TextEdit: &edit, Change: &change}, "test", nil)
			if err == nil || result.ExitCode == 0 {
				t.Fatalf("invalid edit executed: %+v err=%v", result, err)
			}
			if action == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost its error identity: %v", err)
			}
			assertWorkspaceEditFiles(t, root, true, "old\n")
		})
	}
}

// Run the Go test executable as a real validator so the staging/commit boundary
// is exercised on Windows and Linux without relying on Bash or a script fixture.
func configureWorkspaceEditValidator(t *testing.T, svc *Service, target, action string) {
	t.Helper()
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	svc.validators["fixture"] = config.Validator{
		ID: "fixture", Scope: "workspace", Program: program,
		Args:           []string{"-test.run=^TestWorkspaceEditValidatorProcess$", "--", "--workspace-edit-validator", action, "{{path}}", target},
		TimeoutSeconds: 5, PathPatterns: []string{"app.conf"},
	}
}

func TestWorkspaceEditValidatorProcess(t *testing.T) {
	if len(os.Args) < 5 || os.Args[len(os.Args)-4] != "--workspace-edit-validator" {
		return
	}
	action, staged, target := os.Args[len(os.Args)-3], os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]
	content, err := os.ReadFile(staged)
	if err != nil || strings.TrimSuffix(string(content), "\n") != "port=9090" || staged == target || filepath.Dir(staged) != filepath.Dir(target) {
		t.Fatalf("validator did not receive staged replacement: content=%q err=%v", content, err)
	}
	if action != "create" {
		content, err = os.ReadFile(target)
		if err != nil || string(content) != "port=8080\n" {
			t.Fatalf("target changed before validation: content=%q err=%v", content, err)
		}
	}
	fmt.Println("validator inspected staged content")
	switch action {
	case "reject":
		os.Exit(3)
	case "change", "create":
		if err := os.WriteFile(target, []byte("port=7070\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "accept":
	default:
		t.Fatalf("unknown validator action %q", action)
	}
}

func assertWorkspaceEditFiles(t *testing.T, root string, exists bool, want string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		if len(entries) != 0 {
			t.Fatalf("unapproved edit wrote files: %+v", entries)
		}
		return
	}
	if len(entries) != 1 || entries[0].Name() != "app.conf" {
		t.Fatalf("edit left staging files: %+v", entries)
	}
	content, err := os.ReadFile(filepath.Join(root, "app.conf"))
	if err != nil || string(content) != want {
		t.Fatalf("edited content=%q want=%q err=%v", content, want, err)
	}
}

func TestWorkspaceEditRechecksAccessAfterApproval(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	ctx := context.Background()
	target := filepath.Join(root, "app.conf")
	if err := os.WriteFile(target, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pending, err := svc.EditWorkspaceFile(ctx, "project", "app.conf", "original", "updated", "", "update config", "eino-agent")
	if err != nil || pending.Status != "approval_required" {
		t.Fatalf("pending edit: %+v err=%v", pending, err)
	}
	if _, err := svc.UpdateAdminWorkspace(ctx, "project", domain.WorkspaceInput{Access: "read_only"}, "operator"); err != nil {
		t.Fatal(err)
	}
	if result, err := svc.Approve(ctx, pending.ApprovalID, "reviewed", "operator"); err == nil || result.Status == "completed" {
		t.Fatalf("edit used stale access: %+v err=%v", result, err)
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "original\n" {
		t.Fatalf("revoked write changed file: %q err=%v", content, err)
	}
}

func TestWorkspaceReadPatchAndTraversalProtection(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	path := filepath.Join(root, "app.conf")
	if err := os.WriteFile(path, []byte("port=8080\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	read := runApprovedWorkspaceAccess(t, svc, func(ctx context.Context) (domain.ExecResult, error) {
		return svc.ReadWorkspaceFile(ctx, "project", "app.conf", 0, 0, "eino-agent")
	})
	if read.Status != "completed" || read.Stdout != "port=8080\n" || read.File == nil || read.File.SHA256 == "" {
		t.Fatalf("unexpected workspace read: %#v", read)
	}
	pending, err := svc.EditWorkspaceFile(context.Background(), "project", "app.conf", "port=8080", "port=9090", "", "change port", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != "approval_required" {
		t.Fatalf("workspace write skipped approval: %#v", pending)
	}
	approved, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed", "operator")
	if err != nil || approved.Status != "completed" {
		t.Fatalf("workspace patch failed: %#v err=%v", approved, err)
	}
	content, _ := os.ReadFile(path)
	if string(content) != "port=9090\n" {
		t.Fatalf("patch result = %q", content)
	}
	if _, err := svc.ReadWorkspaceFile(context.Background(), "project", "../outside", 100, 0, "test"); err == nil || !strings.Contains(err.Error(), "relative") {
		t.Fatalf("workspace traversal was not rejected: %v", err)
	}
}

func TestWorkspacePatchUsesCurrentContextWithoutSHABinding(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	path := filepath.Join(root, "app.conf")
	_ = os.WriteFile(path, []byte("a\n"), 0o600)
	pending, err := svc.EditWorkspaceFile(context.Background(), "project", "app.conf", "a", "b", "", "change", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed", "operator")
	if err != nil || result.Status != "completed" {
		t.Fatalf("context-matched edit failed: %#v err=%v", result, err)
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "b\n" {
		t.Fatalf("edit result=%q err=%v", content, err)
	}
}

func TestWorkspaceFileEditPreservesMode(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	existingPath := filepath.Join(root, "app.conf")
	if err := os.WriteFile(existingPath, []byte("enabled=false\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	pending, err := svc.EditWorkspaceFile(context.Background(), "project", "app.conf", "enabled=false", "enabled=true", "", "enable app", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed", "operator"); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(existingPath)
	if err != nil || string(content) != "enabled=true\n" {
		t.Fatalf("replacement content=%q err=%v", content, err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(existingPath)
		if err != nil || info.Mode().Perm() != 0o640 {
			t.Fatalf("replacement mode=%v err=%v", info, err)
		}
	}
}

func TestWorkspaceFileEditPreservesUTF8BOMAndCRLF(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	path := filepath.Join(root, "windows.conf")
	original := append([]byte{0xef, 0xbb, 0xbf}, []byte("enabled=false\r\nname=demo\r\n")...)
	if err := os.WriteFile(path, original, 0o640); err != nil {
		t.Fatal(err)
	}
	pending, err := svc.EditWorkspaceFile(context.Background(), "project", "windows.conf", "\ufeffenabled=false\r\nname=demo\r\n", "enabled=true\r\nname=demo\r\n", "", "normalize Windows text", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed", "operator"); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	expected := append([]byte{0xef, 0xbb, 0xbf}, []byte("enabled=true\r\nname=demo\r\n")...)
	if err != nil || !bytes.Equal(content, expected) {
		t.Fatalf("preserved edit bytes=% x want=% x err=%v", content, expected, err)
	}
}

func TestWorkspaceFileEditRejectsInvalidReplacementAndMissingTarget(t *testing.T) {
	svc, _ := newWorkspaceService(t, "read_write")
	if _, err := svc.EditWorkspaceFile(context.Background(), "project", "app.conf", "same", "same", "", "change", "test"); err == nil || !strings.Contains(err.Error(), "different") {
		t.Fatalf("no-op replacement was accepted: %v", err)
	}
	pending, err := svc.EditWorkspaceFile(context.Background(), "project", "missing.conf", "old", "new", "", "change", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed", "operator"); err == nil || !strings.Contains(result.Stderr, "does not exist") {
		t.Fatalf("missing edit target was accepted: result=%#v err=%v", result, err)
	}
}

func TestWorkspaceFileEditCreatesMissingFile(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	pending, err := svc.EditWorkspaceFile(context.Background(), "project", "created.conf", "", "enabled=true", "", "create config", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed", "operator")
	if err != nil || result.Status != "completed" {
		t.Fatalf("create result=%#v err=%v", result, err)
	}
	content, err := os.ReadFile(filepath.Join(root, "created.conf"))
	if err != nil || string(content) != "enabled=true" {
		t.Fatalf("created content=%q err=%v", content, err)
	}
}

func TestWorkspacePreValidationFailureDoesNotTouchOriginal(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	path := filepath.Join(root, "app.conf")
	if err := os.WriteFile(path, []byte("port=8080\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	configureWorkspaceEditValidator(t, svc, path, "reject")
	pending, err := svc.EditWorkspaceFile(context.Background(), "project", "app.conf", "port=8080", "port=9090", "fixture", "change port", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed", "operator")
	if err == nil || result.ExitCode != 74 {
		t.Fatalf("expected pre-validation failure, result=%#v err=%v", result, err)
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil || string(content) != "port=8080\n" {
		t.Fatalf("failed validation touched the original: content=%q err=%v", content, readErr)
	}
	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(path)
		if statErr != nil || info.Mode().Perm() != 0o640 {
			t.Fatalf("failed validation changed file mode: info=%#v err=%v", info, statErr)
		}
	}
}
