package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestRecoveryMarksUnconfirmedToolResultUnknown(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.store.CreateChatSession(ctx, "session-recovery", ""); err != nil {
		t.Fatal(err)
	}
	userMessageID, err := svc.store.AppendPendingChatMessage(ctx, "session-recovery", "user", "continue")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.StartChatToolCall(ctx, domain.ChatToolCall{
		SessionID: "session-recovery", UserMessageID: userMessageID,
		ToolCallID: "call-unknown", ToolName: "mcp__external__mutate", ArgumentsJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecoverInterruptedTasks(ctx); err != nil {
		t.Fatal(err)
	}
	call, err := svc.store.GetChatToolCall(ctx, "session-recovery", "call-unknown")
	if err != nil {
		t.Fatal(err)
	}
	if call.Status != domain.ChatToolCallUnknown || !strings.Contains(call.ResultJSON, `"status":"unknown"`) {
		t.Fatalf("recovered tool call = %#v", call)
	}
	messages, err := svc.store.ListChatContextMessages(ctx, "session-recovery")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Status != "failed" || messages[1].ToolStatus != domain.ChatToolCallUnknown {
		t.Fatalf("recovered context = %#v", messages)
	}
}

func TestRecoveryInterruptsRunsWithoutAnExecutionOwner(t *testing.T) {
	svc, _, host := newTestService(t)
	ctx := context.Background()
	run := domain.Run{
		ID: "run-recovery", HostID: host.ID, RequestJSON: `{}`, RequestDigest: "digest",
		Status: "running", StartedAt: time.Now().UTC(),
	}
	if err := svc.store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecoverInterruptedTasks(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, err := svc.store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != "interrupted" || recovered.CompletedAt.IsZero() || !strings.Contains(recovered.Error, "control plane restarted") {
		t.Fatalf("orphaned run was not interrupted: %#v", recovered)
	}
}
