package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestChatMessagePageUsesStableCursorAndProjectsLargeToolResults(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/chat-page.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for index := 0; index < 7; index++ {
		if err := st.AppendChatMessage(ctx, "page-session", "user", fmt.Sprintf("message-%d", index)); err != nil {
			t.Fatal(err)
		}
	}
	first, err := st.ListChatMessagesPage(ctx, "page-session", 3, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Messages) != 3 || !first.HasMore || first.Messages[0].Content != "message-4" || first.Messages[2].Content != "message-6" {
		t.Fatalf("first page = %#v", first)
	}
	second, err := st.ListChatMessagesPage(ctx, "page-session", 3, first.NextCreatedAt, first.NextID)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Messages) != 3 || !second.HasMore || second.Messages[0].Content != "message-1" || second.Messages[2].Content != "message-3" {
		t.Fatalf("second page = %#v", second)
	}
	third, err := st.ListChatMessagesPage(ctx, "page-session", 3, second.NextCreatedAt, second.NextID)
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Messages) != 1 || third.HasMore || third.Messages[0].Content != "message-0" {
		t.Fatalf("third page = %#v", third)
	}
	if _, err := st.ListChatMessagesPage(ctx, "page-session", 3, first.NextCreatedAt, ""); err == nil {
		t.Fatal("partial chat history cursor was accepted")
	}
	if _, err := st.ListChatMessagesPage(ctx, "page-session", 3, first.NextCreatedAt, "missing-message"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown chat history cursor error = %v", err)
	}

	large := `{"status":"completed","stdout":"` + strings.Repeat("x", maxChatToolMessagePreviewChars+1024) + `"}`
	if err := st.AppendChatMessage(ctx, "large-tool-session", "tool", large, "ssh_exec"); err != nil {
		t.Fatal(err)
	}
	projected, err := st.ListChatMessagesPage(ctx, "large-tool-session", 10, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(projected.Messages) != 1 || !projected.Messages[0].ContentTruncated || len(projected.Messages[0].Content) >= len(large) {
		t.Fatalf("projected tool message = %#v", projected.Messages)
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(projected.Messages[0].Content), &envelope); err != nil || envelope["output_limited"] != true {
		t.Fatalf("projected content = %q, err = %v", projected.Messages[0].Content, err)
	}
	full, err := st.GetChatMessage(ctx, "large-tool-session", projected.Messages[0].ID)
	if err != nil || full.Content != large || full.ContentTruncated {
		t.Fatalf("full tool message chars=%d truncated=%v err=%v", len(full.Content), full.ContentTruncated, err)
	}
	if _, err := st.GetChatMessage(ctx, "another-session", projected.Messages[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-session message lookup error = %v", err)
	}
}

func TestProjectedSSHTaskStatusKeepsLiveSubscriptionIdentity(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/task-projection.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.CreateChatSession(ctx, "task-projection", ""); err != nil {
		t.Fatal(err)
	}
	userMessageID, err := st.AppendPendingChatMessage(ctx, "task-projection", "user", "wait for the task")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartChatToolCall(ctx, domain.ChatToolCall{
		SessionID: "task-projection", UserMessageID: userMessageID, ToolCallID: "call-task-status", ToolName: "ssh_task",
		ArgumentsJSON: `{"action":"status","task_id":"task-live"}`,
		ResultJSON:    `{"status":"in_progress","task_id":"task-live","stdout":"` + strings.Repeat("x", maxChatToolMessagePreviewChars+1024) + `"}`,
	}); err != nil {
		t.Fatal(err)
	}
	page, err := st.ListChatMessagesPage(ctx, "task-projection", 10, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 2 || !page.Messages[1].ContentTruncated {
		t.Fatalf("projected messages = %#v", page.Messages)
	}
	var content struct {
		TaskID  string `json:"task_id"`
		Display struct {
			Arguments struct {
				Action string `json:"action"`
				TaskID string `json:"task_id"`
			} `json:"arguments"`
		} `json:"_display"`
	}
	if err := json.Unmarshal([]byte(page.Messages[1].Content), &content); err != nil {
		t.Fatal(err)
	}
	if content.TaskID != "task-live" || content.Display.Arguments.Action != "status" || content.Display.Arguments.TaskID != "task-live" {
		t.Fatalf("projected task identity = %#v", content)
	}
}

func TestChatHistoryHasNoImplicitLimit(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/chat-history.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for i := 0; i < 550; i++ {
		if err := st.AppendChatMessage(ctx, "long-session", "reasoning", fmt.Sprintf("segment-%03d", i)); err != nil {
			t.Fatal(err)
		}
	}
	history, err := st.ListChatMessages(ctx, "long-session", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 550 {
		t.Fatalf("complete history count = %d, want 550", len(history))
	}
	if history[0].Content != "segment-000" || history[549].Content != "segment-549" {
		t.Fatalf("complete history order is wrong: first=%q last=%q", history[0].Content, history[549].Content)
	}

	recent, err := st.ListChatMessages(ctx, "long-session", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 7 || recent[0].Content != "segment-543" || recent[6].Content != "segment-549" {
		t.Fatalf("explicit history limit returned unexpected records: %#v", recent)
	}
}

func TestChatAttachmentsPersistForHistoryAndModelContext(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/chat-images.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	imageData := []byte("valid-image-fixture")
	messageID, err := st.AppendPendingChatMessageWithAttachments(ctx, "session-images", "user", "inspect this", []domain.ChatAttachment{{
		Name: "screen.png", MIMEType: "image/png", Data: imageData,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetChatMessageStatus(ctx, messageID, "completed"); err != nil {
		t.Fatal(err)
	}

	history, err := st.ListChatMessages(ctx, "session-images", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || len(history[0].Attachments) != 1 {
		t.Fatalf("history attachments = %#v", history)
	}
	metadata := history[0].Attachments[0]
	if metadata.Name != "screen.png" || metadata.MIMEType != "image/png" || metadata.SizeBytes != int64(len(imageData)) || len(metadata.Data) != 0 {
		t.Fatalf("public attachment metadata = %#v", metadata)
	}

	modelHistory, err := st.ListChatContextMessages(ctx, "session-images")
	if err != nil {
		t.Fatal(err)
	}
	if len(modelHistory) != 1 || len(modelHistory[0].Attachments) != 1 || !bytes.Equal(modelHistory[0].Attachments[0].Data, imageData) {
		t.Fatalf("model attachment data = %#v", modelHistory)
	}
	loaded, err := st.GetChatAttachment(ctx, "session-images", metadata.ID)
	if err != nil || !bytes.Equal(loaded.Data, imageData) {
		t.Fatalf("loaded attachment = %#v, err = %v", loaded, err)
	}
	if _, err := st.GetChatAttachment(ctx, "another-session", metadata.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-session attachment lookup error = %v", err)
	}

	if err := st.DeleteChatSession(ctx, "session-images"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetChatAttachment(ctx, "session-images", metadata.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("attachment survived session deletion: %v", err)
	}
}
