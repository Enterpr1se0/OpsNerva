package service

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type workspaceUploadReadFunc func([]byte) (int, error)

func (read workspaceUploadReadFunc) Read(buffer []byte) (int, error) { return read(buffer) }

func TestWorkspaceMutationAuditIsRecordedOnlyAfterSuccess(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	ctx := context.Background()
	uploaded, err := svc.UploadWorkspaceFile(ctx, "project", "audit.txt", "", strings.NewReader("bytes"), "admin-web")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UploadWorkspaceFile(ctx, "project", "audit.txt", "", strings.NewReader("overwrite"), "admin-web"); err == nil {
		t.Fatal("conflicting upload succeeded")
	}
	deleted, err := svc.DeleteAdminWorkspaceEntry(ctx, "project", "audit.txt", "operator")
	if err != nil || deleted.SHA256 != uploaded.SHA256 {
		t.Fatalf("deletion metadata changed: %+v err=%v", deleted, err)
	}
	if _, err := svc.DeleteAdminWorkspaceEntry(ctx, "project", "audit.txt", "operator"); err == nil {
		t.Fatal("missing target deletion succeeded")
	}
	events, err := svc.ListAudit(ctx, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, event := range events {
		if event.Type != "workspace_file_uploaded" && event.Type != "workspace_file_deleted" {
			continue
		}
		counts[event.Type]++
		if event.Data["workspace_id"] != "project" || event.Data["path"] != "audit.txt" || event.Data["sha256"] != uploaded.SHA256 || event.Data["size"] != float64(5) {
			t.Fatalf("mutation audit data changed: %+v", event)
		}
		if event.Type == "workspace_file_uploaded" && event.Actor != "admin-web" || event.Type == "workspace_file_deleted" && (event.Actor != "operator" || event.Data["permanent"] != true || event.Data["type"] != "file") {
			t.Fatalf("mutation audit owner/operation changed: %+v", event)
		}
	}
	if counts["workspace_file_uploaded"] != 1 || counts["workspace_file_deleted"] != 1 {
		t.Fatalf("missing or duplicate mutation audit: %+v", counts)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("mutations left staging files: %v err=%v", entries, err)
	}
}

func TestWorkspaceCancelledMutationsDoNotChangeFilesOrAudit(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := svc.ListAudit(context.Background(), "", 100)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err = svc.UploadWorkspaceFile(ctx, "project", "cancelled.txt", "", workspaceUploadReadFunc(func(buffer []byte) (int, error) {
		cancel()
		return copy(buffer, "partial"), io.EOF
	}), "admin-web")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled upload = %v", err)
	}
	if _, err := svc.DeleteAdminWorkspaceEntry(ctx, "project", "keep.txt", "admin-web"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled delete = %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "keep.txt" {
		t.Fatalf("cancelled operations changed files: %v err=%v", entries, err)
	}
	content, err := os.ReadFile(filepath.Join(root, "keep.txt"))
	if err != nil || string(content) != "keep" {
		t.Fatalf("cancelled delete changed original: %q err=%v", content, err)
	}
	after, err := svc.ListAudit(context.Background(), "", 100)
	if err != nil || len(after) != len(before) {
		t.Fatalf("cancelled operations wrote success audit: before=%d after=%d err=%v", len(before), len(after), err)
	}
}

func TestWorkspaceMutationsKeepPathErrorClassification(t *testing.T) {
	svc, _ := newWorkspaceService(t, "read_write")
	ctx := context.Background()
	for name, operation := range map[string]func() error{
		"directory": func() error { return svc.CreateAdminWorkspaceDirectory(ctx, "project", "../outside") },
		"upload": func() error {
			_, err := svc.UploadWorkspaceFile(ctx, "project", "../outside", "", strings.NewReader("data"), "test")
			return err
		},
		"delete": func() error {
			_, err := svc.DeleteAdminWorkspaceEntry(ctx, "project", "../outside", "test")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			var invalid *InputValidationError
			if err := operation(); !errors.As(err, &invalid) {
				t.Fatalf("mutation path error lost classification: %v", err)
			}
		})
	}
}

func TestWorkspaceUploadDirectoriesPreserveTree(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	ctx := context.Background()
	for _, path := range []string{"photos", "photos/旅行", "photos/旅行/empty", "photos/旅行"} {
		if err := svc.CreateAdminWorkspaceDirectory(ctx, "project", path); err != nil {
			t.Fatalf("create %q: %v", path, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, "photos", "旅行"))
	if err != nil || len(entries) != 1 || entries[0].Name() != "empty" || !entries[0].IsDir() {
		t.Fatalf("directory tree = %v, err = %v", entries, err)
	}
	empty, err := os.ReadDir(filepath.Join(root, "photos", "旅行", "empty"))
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty directory = %v, err = %v", empty, err)
	}
}

func TestWorkspaceUploadDirectoriesRejectInvalidTargets(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	ctx := context.Background()
	for _, path := range []string{"", ".", "..", "../escape", "/absolute", `nested\windows`, "parent/../escape", ".ssh", ".ssh/keys", "new/.env", "missing/child", "bad\x00name"} {
		if err := svc.CreateAdminWorkspaceDirectory(ctx, "project", path); err == nil {
			t.Errorf("invalid path %q accepted", path)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid requests created directories: %v, err = %v", entries, err)
	}
	if err := os.WriteFile(filepath.Join(root, "existing"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := svc.CreateAdminWorkspaceDirectory(ctx, "project", "existing"); err == nil {
		t.Fatal("file was accepted as directory")
	}
	content, err := os.ReadFile(filepath.Join(root, "existing"))
	if err != nil || string(content) != "keep" {
		t.Fatalf("file conflict changed content: %q, err = %v", content, err)
	}
}

func TestWorkspaceUploadDirectoriesRejectReadOnlyAndCancellation(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_only")
	if err := svc.CreateAdminWorkspaceDirectory(context.Background(), "project", "photos"); err == nil {
		t.Fatal("read-only workspace accepted a directory")
	}
	if err := svc.CreateAdminWorkspaceDirectory(context.Background(), "missing", "photos"); err == nil {
		t.Fatal("unknown workspace accepted a directory")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := svc.CreateAdminWorkspaceDirectory(ctx, "project", "photos"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled directory creation = %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("rejected requests changed workspace: %v, err = %v", entries, err)
	}
}

func TestWorkspaceUploadDirectoriesRejectSymlinks(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	for _, path := range []string{"linked", "linked/child"} {
		if err := svc.CreateAdminWorkspaceDirectory(context.Background(), "project", path); err == nil {
			t.Errorf("symlink path %q accepted", path)
		}
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("directory created outside workspace: %v, err = %v", entries, err)
	}
}
