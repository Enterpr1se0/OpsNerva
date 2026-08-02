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
	sessions, err := st.ListChatSessions(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].WorkspaceID != "project-b" || sessions[0].MessageCount != 1 {
		t.Fatalf("sessions = %#v", sessions)
	}
}
