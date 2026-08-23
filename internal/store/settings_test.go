package store

import (
	"context"
	"sync"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestMemoryStoreKeepsOneDatabaseAcrossConcurrentCalls(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var workers sync.WaitGroup
	errors := make(chan error, 8)
	for index := 0; index < 8; index++ {
		workers.Add(1)
		go func(enabled bool) {
			defer workers.Done()
			errors <- st.SetAgentToolEnabled(ctx, "memory-test-tool", enabled)
		}(index%2 == 0)
	}
	workers.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	states, err := st.AgentToolStates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := states["memory-test-tool"]; !ok {
		t.Fatal("concurrent in-memory writes did not use the initialized database")
	}
}

func TestSystemSettingsPersistExplicitEmptySystemPrompt(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/settings.db"
	st, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := st.GetSystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settings.SystemPrompt != domain.DefaultSystemPrompt || settings.DefaultSystemPrompt != domain.DefaultSystemPrompt {
		t.Fatalf("unexpected initial prompt settings: %#v", settings)
	}
	settings.SystemPrompt = ""
	if _, err := st.SaveSystemSettings(ctx, settings); err != nil {
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
	settings, err = reopened.GetSystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settings.SystemPrompt != "" {
		t.Fatalf("explicit empty system prompt was replaced: %q", settings.SystemPrompt)
	}
	if settings.DefaultSystemPrompt != domain.DefaultSystemPrompt {
		t.Fatalf("default system prompt was not returned separately: %q", settings.DefaultSystemPrompt)
	}
}
