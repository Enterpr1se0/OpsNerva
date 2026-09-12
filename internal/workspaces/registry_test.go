package workspaces

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

// The fake implements only registration persistence; failure and notification
// hooks exercise the registry without a database, Service, or shell runtime.
type memoryPersistence struct {
	mu          sync.Mutex
	entries     map[string]domain.Workspace
	initialized bool
	failOn      string
	failure     error
	afterWrite  func(string)
}

func (p *memoryPersistence) InitializeWorkspaces(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failOn == "initialize" {
		return p.failure
	}
	if !p.initialized {
		p.entries = map[string]domain.Workspace{"default": {ID: "default", Access: "read_write"}}
		p.initialized = true
	}
	return nil
}

func (p *memoryPersistence) ListWorkspaces(context.Context) ([]domain.Workspace, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failOn == "list" {
		return nil, p.failure
	}
	result := make([]domain.Workspace, 0, len(p.entries))
	for _, workspace := range p.entries {
		result = append(result, workspace)
	}
	return result, nil
}

func (p *memoryPersistence) CreateWorkspace(_ context.Context, workspace domain.Workspace) error {
	return p.mutate("create", workspace)
}

func (p *memoryPersistence) UpdateWorkspace(_ context.Context, workspace domain.Workspace) error {
	return p.mutate("update", workspace)
}

func (p *memoryPersistence) DeleteWorkspace(_ context.Context, id string) error {
	return p.mutate("delete", domain.Workspace{ID: id})
}

func (p *memoryPersistence) mutate(operation string, workspace domain.Workspace) error {
	p.mu.Lock()
	if p.failOn == operation {
		p.mu.Unlock()
		return p.failure
	}
	_, exists := p.entries[workspace.ID]
	if operation == "create" && exists || operation != "create" && !exists {
		p.mu.Unlock()
		return fmt.Errorf("unexpected %s for %q", operation, workspace.ID)
	}
	if operation == "delete" {
		delete(p.entries, workspace.ID)
	} else {
		p.entries[workspace.ID] = workspace
	}
	p.mu.Unlock()
	if p.afterWrite != nil {
		p.afterWrite(operation)
	}
	return nil
}

func newTestRegistry(t *testing.T) (*Registry, *memoryPersistence, string) {
	t.Helper()
	root := t.TempDir()
	persistence := &memoryPersistence{}
	registry := New(persistence)
	if err := registry.Initialize(context.Background(), root, ""); err != nil {
		t.Fatal(err)
	}
	return registry, persistence, root
}

func TestRegistryLifecycleAndIndependentSnapshots(t *testing.T) {
	r, persistence, root := newTestRegistry(t)
	ctx := context.Background()
	created, err := r.Create(ctx, " project ", "")
	if err != nil || created.ID != "project" || created.Access != "read_only" || filepath.Base(created.Root) != "project" {
		t.Fatalf("create: %+v err=%v", created, err)
	}
	snapshot := r.Snapshot()
	for index := range snapshot {
		snapshot[index].ID = "modified"
		snapshot[index].Access = "corrupted"
		snapshot[index].Root = "outside"
	}
	if got, ok := r.Get(" project "); !ok || got != created {
		t.Fatalf("snapshot changed registry: %+v", got)
	}
	updated, err := r.Update(ctx, "project", "read_write")
	if err != nil || updated.Access != "read_write" || updated.Root != created.Root {
		t.Fatalf("update: %+v err=%v", updated, err)
	}
	if err := os.WriteFile(filepath.Join(created.Root, "old.txt"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	removed, err := r.Delete(ctx, "project")
	if err != nil || removed == nil || !removed.FilesRemoved || removed.Workspace != updated {
		t.Fatalf("delete: %+v err=%v", removed, err)
	}
	if _, ok := r.Get("project"); ok {
		t.Fatal("deleted registry entry remained visible")
	}
	if _, err := os.Stat(created.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("directory removal: %v", err)
	}
	if _, err := r.Delete(ctx, "default"); err != nil {
		t.Fatal(err)
	}
	reloaded := New(persistence)
	if err := reloaded.Initialize(ctx, root, ""); err != nil || len(reloaded.Snapshot()) != 0 {
		t.Fatalf("restart recreated deleted registration: %+v err=%v", reloaded.Snapshot(), err)
	}
}

func TestRegistryPersistenceFailuresKeepPublishedState(t *testing.T) {
	for _, operation := range []string{"create", "update", "delete", "initialize", "list"} {
		t.Run(operation, func(t *testing.T) {
			r, persistence, _ := newTestRegistry(t)
			ctx := context.Background()
			original, ok := r.Get("default")
			if !ok {
				t.Fatal("missing default registration")
			}
			failure := errors.New("fixture persistence failure")
			persistence.failOn, persistence.failure = operation, failure
			var err error
			switch operation {
			case "create":
				_, err = r.Create(ctx, "new", "read_write")
			case "update":
				_, err = r.Update(ctx, "default", "read_only")
			case "delete":
				var removed *Removal
				removed, err = r.Delete(ctx, "default")
				if removed != nil {
					t.Fatalf("failed persistence reported unregistration: %+v", removed)
				}
			default:
				err = r.Initialize(ctx, t.TempDir(), "")
			}
			if !errors.Is(err, failure) {
				t.Fatalf("persistence error was lost: %v", err)
			}
			current, ok := r.Get("default")
			if !ok || current != original || len(r.Snapshot()) != 1 {
				t.Fatalf("failed operation changed published state: %+v", r.Snapshot())
			}
			if _, err := os.Stat(original.Root); err != nil {
				t.Fatalf("failed persistence removed active directory: %v", err)
			}
		})
	}
}

func TestRegistryRejectsUninitializedAndCancelledMutations(t *testing.T) {
	r := New(&memoryPersistence{})
	if _, err := r.Create(context.Background(), "../invalid", "read_only"); err == nil || !strings.Contains(err.Error(), "not initialized") {
		t.Fatalf("uninitialized registry accepted creation: %v", err)
	}
	r, _, root := newTestRegistry(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Create(ctx, "cancelled", "read_only"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled create: %v", err)
	}
	if _, err := r.Update(ctx, "default", "read_only"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled update: %v", err)
	}
	if _, err := r.Delete(ctx, "default"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled delete: %v", err)
	}
	if err := r.Initialize(ctx, filepath.Join(root, "cancelled"), ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled initialization: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "default" {
		t.Fatalf("cancelled mutations changed directories: %v err=%v", entries, err)
	}
}

func TestRegistryConcurrentCreateRejectsCaseInsensitiveDuplicates(t *testing.T) {
	r, persistence, _ := newTestRegistry(t)
	const callers = 24
	start := make(chan struct{})
	results := make(chan error, callers)
	for index := range callers {
		go func() {
			<-start
			id := "project"
			if index%2 == 0 {
				id = "PROJECT"
			}
			_, err := r.Create(context.Background(), id, "read_write")
			results <- err
		}()
	}
	close(start)
	succeeded := 0
	for range callers {
		if err := <-results; err == nil {
			succeeded++
		} else if !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("unexpected duplicate error: %v", err)
		}
	}
	stored, err := persistence.ListWorkspaces(context.Background())
	if err != nil || succeeded != 1 || len(stored) != 2 || len(r.Snapshot()) != 2 {
		t.Fatalf("duplicate create: successes=%d stored=%+v snapshot=%+v err=%v", succeeded, stored, r.Snapshot(), err)
	}
}

func TestRegistrySerializesMutationsWithoutBlockingSnapshotReaders(t *testing.T) {
	for _, first := range []string{"update", "delete"} {
		t.Run(first, func(t *testing.T) {
			r, persistence, _ := newTestRegistry(t)
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			var writes int
			persistence.afterWrite = func(string) {
				// A synchronous persistence notification may read a snapshot.
				_ = r.Snapshot()
				writes++
				if writes == 1 {
					close(entered)
					<-release
				}
			}
			firstDone := make(chan error, 1)
			go func() {
				if first == "delete" {
					_, err := r.Delete(context.Background(), "default")
					firstDone <- err
				} else {
					_, err := r.Update(context.Background(), "default", "read_only")
					firstDone <- err
				}
			}()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("persistence callback could not read registry snapshot")
			}
			if current, ok := r.Get("default"); !ok || current.Access != "read_write" {
				t.Fatalf("unfinished persistence return changed snapshot: %+v", current)
			}
			secondDone := make(chan error, 1)
			go func() {
				if first == "delete" {
					_, err := r.Create(context.Background(), "default", "read_write")
					secondDone <- err
				} else {
					_, err := r.Update(context.Background(), "default", "read_write")
					secondDone <- err
				}
			}()
			select {
			case err := <-secondDone:
				t.Fatalf("second mutation overtook the first: %v", err)
			case <-time.After(30 * time.Millisecond):
			}
			unblock()
			for _, done := range []<-chan error{firstDone, secondDone} {
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("mutation did not finish")
				}
			}
			current, ok := r.Get("default")
			stored, err := persistence.ListWorkspaces(context.Background())
			if !ok || err != nil || len(stored) != 1 || stored[0].Access != current.Access || current.Access != "read_write" {
				t.Fatalf("registry diverged from persistence: %+v stored=%+v err=%v", current, stored, err)
			}
			if _, err := os.Stat(current.Root); err != nil {
				t.Fatalf("new registration lost its directory: %v", err)
			}
		})
	}
}
