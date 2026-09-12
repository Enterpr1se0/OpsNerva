package workspacefs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeleteFileReturnsAuditMetadata(t *testing.T) {
	root := t.TempDir()
	content := []byte("binary\x00\r\n")
	if err := os.WriteFile(filepath.Join(root, "file.bin"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := New(root).Delete(context.Background(), " file.bin ", false)
	digest := sha256.Sum256(content)
	if err != nil || result.Path != "file.bin" || result.Type != "file" || result.Size != int64(len(content)) || result.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("deleted file metadata = %+v err=%v", result, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("delete left files or recovery directories: %v err=%v", entries, err)
	}
}

func TestDeleteRechecksRecursiveIntentAndProtectsRoot(t *testing.T) {
	root := t.TempDir()
	fs := New(root)
	if err := fs.CreateDirectory(context.Background(), "generated"); err != nil {
		t.Fatal(err)
	}
	if err := fs.ValidateDeleteTarget("generated", false); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(root, "generated", "new.txt")
	if err := os.WriteFile(child, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Delete(context.Background(), "generated", false); err == nil || !strings.Contains(err.Error(), "recursive=true") {
		t.Fatalf("stale preflight authorized recursion: %v", err)
	}
	assertEditContent(t, child, "keep")
	for _, path := range []string{"", ".", "..", "../outside", "/", ".ssh"} {
		if _, err := fs.Delete(context.Background(), path, true); err == nil {
			t.Fatalf("unsafe deletion target accepted: %q", path)
		}
	}
	result, err := fs.Delete(context.Background(), "generated", true)
	if err != nil || result.Type != "directory" || result.Size != 0 || result.SHA256 != "" {
		t.Fatalf("recursive delete: %+v err=%v", result, err)
	}
	if _, err := os.Stat(filepath.Join(root, "generated")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("directory remains: %v", err)
	}
	if err := fs.CreateDirectory(context.Background(), "empty"); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Delete(context.Background(), "empty", false); err != nil {
		t.Fatalf("empty directory deletion failed: %v", err)
	}
}

func TestDeleteCancellationKeepsFilesAndDirectories(t *testing.T) {
	root := t.TempDir()
	fs := New(root)
	if err := fs.CreateDirectory(context.Background(), "keep"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "keep", "file.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, path := range []string{"keep/file.txt", "keep"} {
		if _, err := fs.Delete(ctx, path, true); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled delete %q = %v", path, err)
		}
	}
	assertEditContent(t, filepath.Join(root, "keep", "file.txt"), "keep")
}

func TestMutationsRejectSymlinkTargetsAndEscapingParents(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	fs := New(root)
	if err := fs.CreateDirectory(context.Background(), "link/new"); err == nil {
		t.Fatal("directory creation escaped root")
	}
	if _, err := fs.Upload(context.Background(), "link/new.txt", "", strings.NewReader("new"), UploadOptions{}); err == nil {
		t.Fatal("upload escaped root")
	}
	for _, path := range []string{"link", "link/keep.txt"} {
		if _, err := fs.Delete(context.Background(), path, true); err == nil {
			t.Fatalf("delete followed symlink %q", path)
		}
	}
	assertEditContent(t, filepath.Join(outside, "keep.txt"), "keep")
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 1 {
		t.Fatalf("outside directory was modified: %v err=%v", entries, err)
	}
}
