package workspacefs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolvePreservesWorkspacePathBoundary(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	fs := New(root)
	for _, relative := range []string{"", ".", " src "} {
		resolved, err := fs.Resolve(relative, false)
		want, wantErr := filepath.EvalSymlinks(filepath.Join(root, NormalizeRelativePath(relative)))
		if err != nil || wantErr != nil || resolved != want {
			t.Fatalf("resolve %q: got=%q want=%q err=%v", relative, resolved, want, err)
		}
	}
	for _, relative := range []string{"/workspace", filepath.Join(root, "src"), "../escape", "src/../escape", "src//file", "src\\file", "a\x00b", "a\nb"} {
		_, err := fs.Resolve(relative, true)
		var invalid *InvalidPathError
		if !errors.As(err, &invalid) {
			t.Errorf("malformed path %q did not retain input-error classification: %v", relative, err)
		}
	}
	for _, relative := range []string{".env", "src/.SSH/key", "data/file", "master.key", "src/deploy-credentials.json", ".opsnerva-file.tmp"} {
		if _, err := fs.Resolve(relative, true); err == nil {
			t.Errorf("sensitive path accepted: %q", relative)
		}
	}
	if _, err := fs.Resolve("src/new.txt", false); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing target: %v", err)
	}
	if _, err := fs.Resolve("src/new.txt", true); err != nil {
		t.Fatalf("missing leaf: %v", err)
	}
	if _, err := fs.Resolve("missing/new.txt", true); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing parent: %v", err)
	}
}

func TestResolveRejectsSymlinkTargetsAndEscapingParents(t *testing.T) {
	parent := t.TempDir()
	root, outside := filepath.Join(parent, "project"), filepath.Join(parent, "project-outside")
	for _, directory := range []string{root, outside} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "private.txt"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	fs := New(root)
	for _, relative := range []string{"escape", "escape/private.txt", "escape/new.txt"} {
		if _, err := fs.Resolve(relative, true); err == nil {
			t.Errorf("escaped root through %q", relative)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "inside.txt"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "inside.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Resolve("link.txt", false); err == nil {
		t.Fatal("final symlink was accepted")
	}
}
