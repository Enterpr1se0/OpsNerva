package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestWorkspaceConfigurationSeedsOnlyOnceAndPersists(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workspaces.db")
	st, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InitializeWorkspaces(ctx); err != nil {
		t.Fatal(err)
	}
	items, err := st.ListWorkspaces(ctx)
	if err != nil || len(items) != 1 || items[0].ID != "default" {
		t.Fatalf("configured workspace was not seeded: %#v err=%v", items, err)
	}
	if err := st.DeleteWorkspace(ctx, "default"); err != nil {
		t.Fatal(err)
	}
	if err := st.InitializeWorkspaces(ctx); err != nil {
		t.Fatal(err)
	}
	items, err = st.ListWorkspaces(ctx)
	if err != nil || len(items) != 0 {
		t.Fatalf("removed configured workspace was seeded again: %#v err=%v", items, err)
	}
	now := time.Now().UTC()
	dynamic := domain.Workspace{ID: "dynamic", Access: "read_only", CreatedAt: now, UpdatedAt: now}
	if err := st.CreateWorkspace(ctx, dynamic); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	items, err = reopened.ListWorkspaces(ctx)
	if err != nil || len(items) != 1 || items[0].ID != "dynamic" || items[0].Access != "read_only" {
		t.Fatalf("dynamic workspace did not persist: %#v err=%v", items, err)
	}
}
