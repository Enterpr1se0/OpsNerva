package store

import (
	"context"
	"path/filepath"
	"testing"

	"eino-ops-agent/internal/domain"
)

func TestChatSessionPersistsWorkspaceBinding(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "chat-sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	created, err := st.CreateChatSession(ctx, "session-bound", "project-a")
	if err != nil {
		t.Fatal(err)
	}
	if created.WorkspaceID != "project-a" {
		t.Fatalf("created Workspace = %q", created.WorkspaceID)
	}
	if created.Title != "New conversation" || created.TitleSet {
		t.Fatalf("new session title = %q, set=%v", created.Title, created.TitleSet)
	}
	if err := st.AppendChatMessage(ctx, created.ID, "user", "inspect the project"); err != nil {
		t.Fatal(err)
	}
	generated, changed, err := st.SetChatSessionTitleIfEmpty(ctx, created.ID, "Inspect project")
	if err != nil || !changed || generated.Title != "Inspect project" || !generated.TitleSet {
		t.Fatalf("generated title = %#v, changed=%v, err=%v", generated, changed, err)
	}
	if _, changed, err := st.SetChatSessionTitleIfEmpty(ctx, created.ID, "Overwrite title"); err != nil || changed {
		t.Fatalf("existing title overwritten: changed=%v err=%v", changed, err)
	}
	renamed, err := st.SetChatSessionTitle(ctx, created.ID, "Project status")
	if err != nil || renamed.Title != "Project status" {
		t.Fatalf("renamed title = %#v, err=%v", renamed, err)
	}
	updated, err := st.SetChatSessionWorkspace(ctx, created.ID, "project-b")
	if err != nil {
		t.Fatal(err)
	}
	if updated.WorkspaceID != "project-b" {
		t.Fatalf("updated Workspace = %q", updated.WorkspaceID)
	}
	if err := st.SetChatSessionContextUsage(ctx, created.ID, 24576, 128000); err != nil {
		t.Fatal(err)
	}
	updated, err = st.GetChatSession(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ContextTokens != 24576 || updated.ContextWindow != 128000 {
		t.Fatalf("context usage = %d/%d", updated.ContextTokens, updated.ContextWindow)
	}
	sessions, err := st.ListChatSessions(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Title != "Project status" || sessions[0].WorkspaceID != "project-b" || sessions[0].ContextTokens != 24576 || sessions[0].ContextWindow != 128000 || sessions[0].MessageCount != 1 {
		t.Fatalf("sessions = %#v", sessions)
	}
}

func TestChatMessageTokenUsagePersistsCurrentContext(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "chat-token-usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.CreateChatSession(ctx, "session-usage", ""); err != nil {
		t.Fatal(err)
	}
	first := domain.ChatTokenUsage{
		InputTokens: 100, OutputTokens: 20, TotalTokens: 120, CachedTokens: 40, ReasoningTokens: 5,
	}
	if err := st.AppendChatAssistantMessageWithUsage(ctx, "message-one", "session-usage", "First", first); err != nil {
		t.Fatal(err)
	}
	second := domain.ChatTokenUsage{
		InputTokens: 200, OutputTokens: 30, TotalTokens: 230,
	}
	if err := st.AppendChatAssistantMessageWithUsage(ctx, "message-two", "session-usage", "Second", second); err != nil {
		t.Fatal(err)
	}
	messages, err := st.ListChatMessages(ctx, "session-usage", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].TokenUsage == nil || messages[1].TokenUsage == nil {
		t.Fatalf("stored token usage = %#v", messages)
	}
	if *messages[0].TokenUsage != first || *messages[1].TokenUsage != second {
		t.Fatalf("loaded token usage = %#v, %#v", messages[0].TokenUsage, messages[1].TokenUsage)
	}
}
