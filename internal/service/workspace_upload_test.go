package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

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
