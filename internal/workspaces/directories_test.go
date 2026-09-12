package workspaces

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestRegistryValidatesManagedRootAndDataSeparation(t *testing.T) {
	base := t.TempDir()
	for _, test := range []struct{ name, root, dataDir string }{
		{"relative", "relative", ""},
		{"filesystem root", filepath.VolumeName(base) + string(filepath.Separator), ""},
		{"same as data", base, base},
		{"inside data", filepath.Join(base, "managed"), base},
		{"contains data", base, filepath.Join(base, "data")},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.dataDir != "" {
				if err := os.MkdirAll(test.dataDir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			persistence := &memoryPersistence{}
			r := New(persistence)
			if err := r.Initialize(context.Background(), test.root, test.dataDir); err == nil {
				t.Fatal("invalid root was accepted")
			}
			if persistence.initialized || len(r.Snapshot()) != 0 {
				t.Fatal("invalid root initialized persistence or published entries")
			}
		})
	}
	r := New(&memoryPersistence{})
	if err := r.Initialize(context.Background(), filepath.Join(base, "workspace"), filepath.Join(base, "data")); err != nil {
		t.Fatalf("separate sibling roots rejected: %v", err)
	}
}

func TestRegistryRejectsUnsafeIdentityAndPersistedRecords(t *testing.T) {
	r, _, _ := newTestRegistry(t)
	for _, id := range []string{"", ".", "..", "../escape", "CON", "com1.txt", "LPT9", "trailing.", "a/b", "space here", strings.Repeat("a", 65)} {
		if _, err := r.Create(context.Background(), id, "read_only"); err == nil {
			t.Errorf("unsafe identity accepted: %q", id)
		}
	}
	if _, err := r.Create(context.Background(), "project", "full_access"); err == nil {
		t.Fatal("invalid access was accepted")
	}
	if _, err := r.Update(context.Background(), "default", ""); err == nil {
		t.Fatal("empty access update was accepted")
	}
	root := t.TempDir()
	persistence := &memoryPersistence{initialized: true, entries: map[string]domain.Workspace{
		"invalid": {ID: "../escape", Access: "read_write"},
	}}
	bad := New(persistence)
	if err := bad.Initialize(context.Background(), root, ""); err == nil || !strings.Contains(err.Error(), "stored workspace") {
		t.Fatalf("invalid persisted identity was accepted: %v", err)
	}
	if len(bad.Snapshot()) != 0 {
		t.Fatal("invalid initialization published partial entries")
	}
}

func TestRegistryDirectoryRemovalNeverTargetsRootOrOutside(t *testing.T) {
	for _, scope := range []string{"root", "outside"} {
		t.Run(scope, func(t *testing.T) {
			r, _, root := newTestRegistry(t)
			protected := root
			if scope == "outside" {
				protected = t.TempDir()
			}
			marker := filepath.Join(protected, "keep.txt")
			if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			// Even a corrupt in-memory path must not broaden recursive deletion.
			r.mu.Lock()
			workspace := r.entries["default"]
			workspace.Root = protected
			r.entries["default"] = workspace
			r.mu.Unlock()
			removed, err := r.Delete(context.Background(), "default")
			if err != nil || removed == nil || removed.FilesRemoved {
				t.Fatalf("unsafe removal result: %+v err=%v", removed, err)
			}
			if _, ok := r.Get("default"); ok {
				t.Fatal("unregistered entry remained available")
			}
			content, err := os.ReadFile(marker)
			if err != nil || string(content) != "keep" {
				t.Fatalf("protected path was removed: %q err=%v", content, err)
			}
		})
	}
}
