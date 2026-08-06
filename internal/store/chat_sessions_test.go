package store

import (
	"context"
	"path/filepath"
	"testing"
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
	if err := st.AppendChatMessage(ctx, created.ID, "user", "inspect the project"); err != nil {
		t.Fatal(err)
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
	if len(sessions) != 1 || sessions[0].WorkspaceID != "project-b" || sessions[0].ContextTokens != 24576 || sessions[0].ContextWindow != 128000 || sessions[0].MessageCount != 1 {
		t.Fatalf("sessions = %#v", sessions)
	}
}
