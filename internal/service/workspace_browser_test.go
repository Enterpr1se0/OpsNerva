package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/workspacefs"
)

func TestWorkspaceAdminUploadIsAtomicAndNeverOverwrites(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	content := []byte("package main\n")
	result, err := svc.UploadWorkspaceFile(context.Background(), "project", "main.go", "ignored.txt", bytes.NewReader(content), "admin-web")
	if err != nil {
		t.Fatal(err)
	}
	wantSHA := fmt.Sprintf("%x", sha256.Sum256(content))
	if result.Path != "main.go" || result.Size != int64(len(content)) || result.SHA256 != wantSHA {
		t.Fatalf("unexpected upload result: %#v", result)
	}
	stored, err := os.ReadFile(filepath.Join(root, "main.go"))
	if err != nil || !bytes.Equal(stored, content) {
		t.Fatalf("uploaded content mismatch: %q err=%v", stored, err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(root, "main.go"))
		if err != nil || info.Mode().Perm() != 0o644 {
			t.Fatalf("uploaded mode = %v err=%v", info.Mode().Perm(), err)
		}
	}
	if _, err := svc.UploadWorkspaceFile(context.Background(), "project", "main.go", "main.go", bytes.NewBufferString("overwrite\n"), "admin-web"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing file was overwritten: %v", err)
	}
	stored, _ = os.ReadFile(filepath.Join(root, "main.go"))
	if !bytes.Equal(stored, content) {
		t.Fatalf("failed overwrite changed existing content: %q", stored)
	}
	listing, err := svc.ListAdminWorkspaceFiles("project", ".")
	if err != nil || len(listing.Entries) != 1 || listing.Entries[0].Name != "main.go" || listing.Entries[0].Type != "file" {
		t.Fatalf("uploaded file was not visible in the admin listing: %#v err=%v", listing, err)
	}
	for _, path := range []string{"../escape", ".env.production", `nested\windows.txt`} {
		if _, err := svc.UploadWorkspaceFile(context.Background(), "project", path, "file", bytes.NewBufferString("x"), "admin-web"); err == nil {
			t.Fatalf("unsafe upload path %q was accepted", path)
		}
	}
	capabilities := svc.ListAdminWorkspaceCapabilities()
	if len(capabilities) != 1 || capabilities[0].ID != "project" {
		t.Fatalf("unexpected admin capabilities: %#v", capabilities)
	}
	preview, err := svc.PreviewAdminWorkspaceFile("project", "main.go")
	if err != nil {
		t.Fatal(err)
	}
	if preview.Path != "main.go" || preview.Content != string(content) || preview.SHA256 != wantSHA || preview.Binary {
		t.Fatalf("unexpected workspace preview: %#v", preview)
	}
	deleted, err := svc.DeleteAdminWorkspaceEntry(context.Background(), "project", "main.go", "admin-web")
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Path != "main.go" || deleted.Type != "file" || deleted.SHA256 != wantSHA {
		t.Fatalf("unexpected delete result: %#v", deleted)
	}
	if _, err := os.Stat(filepath.Join(root, "main.go")); !os.IsNotExist(err) {
		t.Fatalf("deleted file remains at its original path: %v", err)
	}
	listing, err = svc.ListAdminWorkspaceFiles("project", ".")
	if err != nil || len(listing.Entries) != 0 {
		t.Fatalf("deleted file remains in listing: %#v err=%v", listing, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".opsnerva-trash")); !os.IsNotExist(err) {
		t.Fatalf("delete created a recovery directory: %v", err)
	}
}

func TestWorkspaceAdminPreviewIsBoundedAndHashesWholeFile(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	content := bytes.Repeat([]byte("a"), int(workspacefs.MaxPreviewBytes)+4096)
	if err := os.WriteFile(filepath.Join(root, "large.txt"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	preview, err := svc.PreviewAdminWorkspaceFile("project", "large.txt")
	if err != nil {
		t.Fatal(err)
	}
	wantSHA := fmt.Sprintf("%x", sha256.Sum256(content))
	if !preview.Truncated || len(preview.Content) != int(workspacefs.MaxPreviewBytes) || preview.Size != int64(len(content)) || preview.SHA256 != wantSHA {
		t.Fatalf("unexpected bounded workspace preview: %#v", preview)
	}
}

func TestWorkspaceAdminUploadAcceptsFilesLargerThanLegacyLimit(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	const size = int64(100<<20) + 1

	result, err := svc.UploadWorkspaceFile(context.Background(), "project", "large.bin", "large.bin", io.LimitReader(zeroReader{}, size), "admin-web")
	if err != nil {
		t.Fatal(err)
	}
	if result.Size != size {
		t.Fatalf("uploaded size = %d, want %d", result.Size, size)
	}
	info, err := os.Stat(filepath.Join(root, "large.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != size {
		t.Fatalf("stored size = %d, want %d", info.Size(), size)
	}
}

func TestWorkspaceAdminTextEditorPreservesModeAndRejectsBinaryFiles(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	path := filepath.Join(root, "config.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	result, err := svc.SaveAdminWorkspaceTextFile(context.Background(), "project", "config.txt", "after\n")
	if err != nil {
		t.Fatal(err)
	}
	wantSHA := fmt.Sprintf("%x", sha256.Sum256([]byte("after\n")))
	if result.Path != "config.txt" || result.Size != int64(len("after\n")) || result.SHA256 != wantSHA {
		t.Fatalf("unexpected text edit result: %#v", result)
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "after\n" {
		t.Fatalf("edited content = %q, err = %v", content, err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o640 {
			t.Fatalf("edited mode = %v, err = %v", info.Mode().Perm(), err)
		}
	}
	binaryPath := filepath.Join(root, "binary.dat")
	if err := os.WriteFile(binaryPath, []byte{0, 1, 2}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SaveAdminWorkspaceTextFile(context.Background(), "project", "binary.dat", "text"); err == nil || !strings.Contains(err.Error(), "binary") {
		t.Fatalf("binary file was editable: %v", err)
	}
	binary, err := os.ReadFile(binaryPath)
	if err != nil || !bytes.Equal(binary, []byte{0, 1, 2}) {
		t.Fatalf("binary file changed: %v, err = %v", binary, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".opsnerva-") {
			t.Fatalf("text editor left temporary file %q", entry.Name())
		}
	}
}

func TestWorkspaceAdminTextEditorRejectsReadOnlyWorkspace(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_only")
	if err := os.WriteFile(filepath.Join(root, "config.txt"), []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SaveAdminWorkspaceTextFile(context.Background(), "project", "config.txt", "after\n"); err == nil || !strings.Contains(err.Error(), "read_only") {
		t.Fatalf("read-only Workspace was editable: %v", err)
	}
}

func TestWorkspaceFileWatchReportsExternalDirectoryChanges(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	ctx, cancel := context.WithCancel(context.Background())
	watch, err := svc.WatchAdminWorkspaceFiles(ctx, "project", ".")
	if err != nil {
		cancel()
		t.Fatal(err)
	}

	path := filepath.Join(root, "external.txt")
	for _, change := range []func() error{
		func() error { return os.WriteFile(path, []byte("first"), 0o600) },
		func() error { return os.WriteFile(path, []byte("second version"), 0o600) },
		func() error { return os.Remove(path) },
	} {
		if err := change(); err != nil {
			cancel()
			t.Fatal(err)
		}
		select {
		case event := <-watch.Changes:
			if event.WorkspaceID != "project" || event.Path != "." {
				cancel()
				t.Fatalf("unexpected workspace change: %#v", event)
			}
		case watchErr := <-watch.Errors:
			cancel()
			t.Fatalf("workspace watcher failed: %v", watchErr)
		case <-time.After(3 * time.Second):
			cancel()
			t.Fatal("timed out waiting for workspace file change")
		}
	}

	cancel()
	select {
	case _, open := <-watch.Changes:
		if open {
			t.Fatal("workspace change channel remained open after cancellation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("workspace watcher did not stop after cancellation")
	}
}
