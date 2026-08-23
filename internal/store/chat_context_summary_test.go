package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestChatContextSummaryPersistsAndAdvancesRevision(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "context-summary.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.CreateChatSession(ctx, "summary-session", ""); err != nil {
		t.Fatal(err)
	}
	boundary, err := st.AppendPendingChatMessage(ctx, "summary-session", "assistant", "first result")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetChatMessageStatus(ctx, boundary, "completed"); err != nil {
		t.Fatal(err)
	}

	first, err := st.SaveChatContextSummary(ctx, domain.ChatContextSummary{
		SessionID: "summary-session", Summary: "first summary", ThroughMessageID: boundary,
		Trigger: "auto", SourceTokens: 100, SummaryTokens: 20, Model: "test-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision != 1 || first.Trigger != "auto" || first.Summary != "first summary" {
		t.Fatalf("first summary = %#v", first)
	}
	second, err := st.SaveChatContextSummary(ctx, domain.ChatContextSummary{
		SessionID: "summary-session", Summary: "second summary", ThroughMessageID: boundary,
		Trigger: "manual", SourceTokens: 120, SummaryTokens: 15, Model: "test-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Revision != 2 || second.Trigger != "manual" || second.Summary != "second summary" {
		t.Fatalf("second summary = %#v", second)
	}
	if err := st.DeleteChatSession(ctx, "summary-session"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetChatContextSummary(ctx, "summary-session"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("summary survived session deletion: %v", err)
	}
}
