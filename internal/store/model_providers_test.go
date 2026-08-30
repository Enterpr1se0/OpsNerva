package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestListModelProvidersKeepsInsertionOrderWhenCreatedAtMatches(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "model-providers.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	createdAt := time.Date(2026, time.August, 30, 6, 53, 9, 0, time.UTC)
	for _, provider := range []domain.ModelProvider{
		{ID: "model_z_first", Name: "first", Kind: "openai", Model: "first", CreatedAt: createdAt},
		{ID: "model_a_second", Name: "second", Kind: "openai", Model: "second", CreatedAt: createdAt},
	} {
		if _, err := st.UpsertModelProvider(ctx, provider); err != nil {
			t.Fatal(err)
		}
	}

	providers, err := st.ListModelProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 2 || providers[0].ID != "model_z_first" || providers[1].ID != "model_a_second" {
		t.Fatalf("providers are not in insertion order: %#v", providers)
	}
}
