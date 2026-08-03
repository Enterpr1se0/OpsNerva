package store

import (
	"context"
	"errors"
	"testing"

	"eino-ops-agent/internal/domain"
)

func TestPruneChatTurnsExcludedFromContext(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/chat.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	const sessionID = "session-prune"
	droppedID, err := st.AppendPendingChatMessageWithAttachments(ctx, sessionID, "user", "drop failed prompt", []domain.ChatAttachment{{
		ID: "image-dropped", Name: "secret.png", MIMEType: "image/png", Data: []byte("image"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetChatMessageStatus(ctx, droppedID, "failed"); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendChatMessage(ctx, sessionID, "reasoning", "transient reasoning"); err != nil {
		t.Fatal(err)
	}

	interruptedID, err := st.AppendPendingChatMessage(ctx, sessionID, "user", "drop interrupted prompt")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetChatMessageStatus(ctx, interruptedID, "failed"); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendChatMessage(ctx, sessionID, "assistant", domain.AgentInterruptedMessage); err != nil {
		t.Fatal(err)
	}

	toolID, err := st.AppendPendingChatMessage(ctx, sessionID, "user", "keep tool results")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetChatMessageStatus(ctx, toolID, "failed"); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendChatMessage(ctx, sessionID, "tool", `{"status":"completed"}`, "ssh_exec"); err != nil {
		t.Fatal(err)
	}

	progressID, err := st.AppendPendingChatMessage(ctx, sessionID, "user", "keep visible progress")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetChatMessageStatus(ctx, progressID, "failed"); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendChatMessage(ctx, sessionID, domain.ChatMessageRoleAssistantProgress, "visible tool preamble"); err != nil {
		t.Fatal(err)
	}

	answerID, err := st.AppendPendingChatMessage(ctx, sessionID, "user", "keep assistant response")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetChatMessageStatus(ctx, answerID, "failed"); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendChatMessage(ctx, sessionID, "assistant", "useful partial answer"); err != nil {
		t.Fatal(err)
	}

	pruned, err := st.PruneChatTurnsExcludedFromContext(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 2 {
		t.Fatalf("pruned turns = %d, want 2", pruned)
	}
	messages, err := st.ListChatMessages(ctx, sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 6 ||
		messages[0].Content != "keep tool results" || messages[1].Role != "tool" ||
		messages[2].Content != "keep visible progress" || messages[3].Role != domain.ChatMessageRoleAssistantProgress ||
		messages[4].Content != "keep assistant response" || messages[5].Content != "useful partial answer" {
		t.Fatalf("remaining messages = %#v", messages)
	}
	if _, err := st.GetChatAttachment(ctx, sessionID, "image-dropped"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pruned attachment remained: %v", err)
	}
}
