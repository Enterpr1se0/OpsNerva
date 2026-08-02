package store

import (
	"context"
	"testing"

	"eino-ops-agent/internal/domain"
)

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
