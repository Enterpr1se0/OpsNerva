package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/store"
)

func TestChatSessionsCanBeListedLoadedAndDeleted(t *testing.T) {
	svc, _, host := newTestService(t)
	ctx := context.Background()
	if err := svc.store.AppendChatMessage(ctx, "session-one", "user", "Investigate disk usage"); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.AppendChatMessage(ctx, "session-one", "assistant", "Disk usage is healthy"); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.AppendChatMessage(ctx, "session-one", "reasoning", "I should inspect the filesystem first"); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.AppendChatMessage(ctx, "session-one", "tool", `{"status":"completed","run_id":"run_test"}`, "ssh_exec"); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.AppendChatMessage(ctx, "session-two", "user", "Deploy the API"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	run := domain.Run{
		ID: "run-session-one", SessionID: "session-one", HostID: host.ID, ToolName: "ssh_exec",
		RequestJSON: `{}`, RequestDigest: "digest-session-one", Status: "completed", StartedAt: now, CompletedAt: now,
	}
	if err := svc.store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	approval := domain.Approval{
		ID: "approval-session-one", RunID: run.ID, HostID: host.ID, RequestJSON: `{}`, RequestDigest: run.RequestDigest,
		Status: "rejected", CreatedAt: now,
	}
	if err := svc.store.CreateApproval(ctx, approval); err != nil {
		t.Fatal(err)
	}
	task := domain.Task{ID: "task-session-one", RunID: run.ID, HostID: host.ID, Status: "completed", StartedAt: now, EndedAt: now}
	if err := svc.store.UpsertTask(ctx, task, domain.ExecResult{RunID: run.ID, Status: "completed"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.ReplaceAgentPlan(ctx, domain.AgentPlan{SessionID: "session-one", Goal: "Inspect", Status: "active", Steps: []domain.AgentPlanStep{{Number: 1, Title: "Inspect", Status: "in_progress"}}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.AppendAudit(ctx, domain.AuditEvent{RunID: run.ID, Type: "command_completed", Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.AppendAudit(ctx, domain.AuditEvent{Type: "agent_task_created", Actor: "test", Data: map[string]any{"session_id": "session-one"}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.AppendAudit(ctx, domain.AuditEvent{Type: "agent_task_created", Actor: "test", Data: map[string]any{"session_id": "session-two"}}); err != nil {
		t.Fatal(err)
	}
	sessions, err := svc.ListChatSessions(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 || sessions[0].ID != "session-two" || sessions[1].Title != "Investigate disk usage" || sessions[1].MessageCount != 4 {
		t.Fatalf("unexpected sessions %#v", sessions)
	}
	renamed, err := svc.RenameChatSession(ctx, "session-one", "Disk health", "test")
	if err != nil || renamed.Title != "Disk health" {
		t.Fatalf("renamed session = %#v, err=%v", renamed, err)
	}
	if _, err := svc.RenameChatSession(ctx, "session-one", "", "test"); err == nil {
		t.Fatal("empty conversation title was accepted")
	}
	messages, err := svc.ListChatMessages(ctx, "session-one", 10)
	if err != nil || len(messages) != 4 || messages[1].Role != "assistant" || messages[2].Role != "reasoning" || messages[3].Role != "tool" || messages[3].ToolName != "ssh_exec" {
		t.Fatalf("unexpected messages %#v err=%v", messages, err)
	}
	modelMessages, err := svc.store.ListChatModelMessages(ctx, "session-one", 10)
	if err != nil || len(modelMessages) != 2 || modelMessages[0].Role != "user" || modelMessages[1].Role != "assistant" {
		t.Fatalf("reasoning and tool history leaked into model messages: %#v err=%v", modelMessages, err)
	}
	if err := svc.DeleteChatSession(ctx, "session-one"); err != nil {
		t.Fatal(err)
	}
	messages, err = svc.ListChatMessages(ctx, "session-one", 10)
	if err != nil || len(messages) != 0 {
		t.Fatalf("deleted messages still exist: %#v err=%v", messages, err)
	}
	if retained, err := svc.store.GetRun(ctx, run.ID); err != nil || retained.SessionID != "session-one" {
		t.Fatalf("conversation audit run was not retained: run=%#v err=%v", retained, err)
	}
	if _, err := svc.store.GetApproval(ctx, approval.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("conversation approval survived deletion: %v", err)
	}
	if _, _, _, err := svc.store.GetTask(ctx, task.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("conversation task survived deletion: %v", err)
	}
	if _, err := svc.store.GetAgentPlan(ctx, "session-one"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("conversation plan survived deletion: %v", err)
	}
	audit, err := svc.store.ListAudit(ctx, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	keptDeletedSession := false
	keptOtherSession := false
	for _, event := range audit {
		if event.RunID == run.ID || event.Data["session_id"] == "session-one" {
			keptDeletedSession = true
		}
		if event.Data["session_id"] == "session-two" {
			keptOtherSession = true
		}
	}
	if !keptOtherSession {
		t.Fatalf("another conversation's audit was deleted: %#v", audit)
	}
	if !keptDeletedSession {
		t.Fatalf("deleted conversation's audit was not retained: %#v", audit)
	}
	if err := svc.DeleteChatSession(ctx, "session-one"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected not found on second delete, got %v", err)
	}
}
