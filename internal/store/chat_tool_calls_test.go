package store

import (
	"context"
	"strings"
	"testing"

	"eino-ops-agent/internal/domain"
)

func TestChatToolCallPersistsStableLifecycleAndContextResult(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/tool-calls.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.CreateChatSession(ctx, "session-tool", ""); err != nil {
		t.Fatal(err)
	}
	userMessageID, err := st.AppendPendingChatMessage(ctx, "session-tool", "user", "inspect")
	if err != nil {
		t.Fatal(err)
	}
	started, err := st.StartChatToolCall(ctx, domain.ChatToolCall{
		SessionID: "session-tool", UserMessageID: userMessageID, ToolCallID: "call-tool",
		ToolName: "ssh_exec", ArgumentsJSON: `{"host_id":"host-one","program":"uptime"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if started.Status != domain.ChatToolCallRunning || started.MessageID == "" || !strings.Contains(started.ResultJSON, `"status":"running"`) {
		t.Fatalf("started tool call = %#v", started)
	}
	if err := st.BindChatToolCallRun(ctx, "session-tool", "call-tool", "run-tool"); err != nil {
		t.Fatal(err)
	}
	finished, err := st.FinishChatToolCall(ctx, "session-tool", "call-tool", "run-tool", domain.ChatToolCallCompleted, `{"status":"completed","stdout":"up"}`, "")
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != domain.ChatToolCallCompleted || finished.RunID != "run-tool" || finished.MessageID != started.MessageID {
		t.Fatalf("finished tool call = %#v", finished)
	}
	if err := st.SetChatMessageStatus(ctx, userMessageID, "failed"); err != nil {
		t.Fatal(err)
	}
	messages, err := st.ListChatContextMessages(ctx, "session-tool")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[1].Role != "tool" || messages[1].ToolCallID != "call-tool" || messages[1].RunID != "run-tool" || !strings.Contains(messages[1].Content, `"stdout":"up"`) {
		t.Fatalf("context messages = %#v", messages)
	}
}

func TestChatToolCallRejectsMismatchedRunAndPreservesConfirmedTerminal(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/tool-calls.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.CreateChatSession(ctx, "session-tool", ""); err != nil {
		t.Fatal(err)
	}
	userMessageID, err := st.AppendPendingChatMessage(ctx, "session-tool", "user", "inspect")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartChatToolCall(ctx, domain.ChatToolCall{
		SessionID: "session-tool", UserMessageID: userMessageID, ToolCallID: "call-tool", ToolName: "ssh_exec",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.BindChatToolCallRun(ctx, "session-tool", "call-tool", "run-one"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FinishChatToolCall(ctx, "session-tool", "call-tool", "run-two", domain.ChatToolCallFailed, `{}`, "wrong run"); err == nil {
		t.Fatal("mismatched run_id updated the tool call")
	}
	if _, err := st.FinishChatToolCall(ctx, "session-tool", "call-tool", "run-one", domain.ChatToolCallCompleted, `{"status":"completed"}`, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MarkChatToolCallUnknown(ctx, "session-tool", "call-tool"); err != nil {
		t.Fatal(err)
	}
	call, err := st.GetChatToolCall(ctx, "session-tool", "call-tool")
	if err != nil {
		t.Fatal(err)
	}
	if call.Status != domain.ChatToolCallCompleted {
		t.Fatalf("confirmed terminal status was overwritten: %#v", call)
	}
}
