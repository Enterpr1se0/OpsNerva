package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestPendingApprovalDoesNotExpire(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "approvals.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	created := time.Now().UTC().Add(-24 * time.Hour)
	host, err := st.UpsertHost(ctx, domain.Host{
		ID: "host-approval", Name: "approval-host", Address: "192.0.2.20", Port: 22,
		User: "ops", AuthType: "agent", SudoMode: "none", CreatedAt: created,
	})
	if err != nil {
		t.Fatal(err)
	}
	run := domain.Run{
		ID: "run-approval", HostID: host.ID, RequestJSON: `{}`, RequestDigest: "digest",
		Status: "approval_required", AIReviewJSON: `{"status":"completed","decision":"manual_review"}`, StartedAt: created,
	}
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	approval := domain.Approval{
		ID: "approval-pending", RunID: run.ID, HostID: host.ID, RequestJSON: `{}`,
		RequestDigest: run.RequestDigest, Status: "pending", CreatedAt: created,
	}
	if err := st.CreateApproval(ctx, approval); err != nil {
		t.Fatal(err)
	}
	pending, err := st.ListApprovals(ctx, "pending", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != approval.ID || pending[0].Status != "pending" || pending[0].AIReview == nil || pending[0].AIReview.Decision != "manual_review" {
		t.Fatalf("old pending approval changed unexpectedly: %#v", pending)
	}
}

func TestAgentApprovalRequiresCheckpointBeforeDecisionAndResumeClaim(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "agent-approval.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().UTC()
	if _, err := st.CreateChatSession(ctx, "session-agent-approval", ""); err != nil {
		t.Fatal(err)
	}
	userMessageID, err := st.AppendPendingChatMessage(ctx, "session-agent-approval", "user", "restart demo")
	if err != nil {
		t.Fatal(err)
	}
	host, err := st.UpsertHost(ctx, domain.Host{
		ID: "host-agent-approval", Name: "agent-approval", Address: "192.0.2.21", Port: 22,
		User: "ops", AuthType: "agent", SudoMode: "none", CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	run := domain.Run{
		ID: "run-agent-approval", SessionID: "session-agent-approval", HostID: host.ID,
		RequestJSON: `{}`, RequestDigest: "digest", Status: "approval_required", StartedAt: now,
	}
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	approval := domain.Approval{
		ID: "approval-agent", RunID: run.ID, HostID: host.ID, RequestJSON: `{}`,
		RequestDigest: run.RequestDigest, Status: domain.ApprovalStatusPreparing,
		ContinuationKind: domain.ApprovalContinuationAgent, CheckpointID: "checkpoint-agent", CreatedAt: now,
	}
	if err := st.CreateApproval(ctx, approval); err != nil {
		t.Fatal(err)
	}
	if err := st.Set(ctx, approval.CheckpointID, []byte("checkpoint")); err != nil {
		t.Fatal(err)
	}
	if err := st.ActivateAgentApprovals(ctx, approval.CheckpointID, map[string]string{
		approval.ID: "interrupt-agent", "missing-approval": "missing-interrupt",
	}); err == nil {
		t.Fatal("partial approval group activation unexpectedly succeeded")
	}
	if unchanged, err := st.GetApproval(ctx, approval.ID); err != nil || unchanged.Status != domain.ApprovalStatusPreparing || unchanged.InterruptID != "" {
		t.Fatalf("failed group activation committed partially: approval=%#v err=%v", unchanged, err)
	}
	if pending, err := st.ListApprovals(ctx, domain.ApprovalStatusPending, 10); err != nil || len(pending) != 0 {
		t.Fatalf("preparing approval was visible: approvals=%#v err=%v", pending, err)
	}
	if err := st.ActivateAgentApprovals(ctx, approval.CheckpointID, map[string]string{approval.ID: "interrupt-agent"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartChatToolCall(ctx, domain.ChatToolCall{
		SessionID: "session-agent-approval", UserMessageID: userMessageID, ToolCallID: "approval-call",
		ToolName: "approval_fixture", ArgumentsJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.BindChatToolCallRun(ctx, "session-agent-approval", "approval-call", run.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.FailPendingChatMessages(ctx); err != nil {
		t.Fatal(err)
	}
	if message, err := st.GetChatMessage(ctx, "session-agent-approval", userMessageID); err != nil || message.Status != "pending" {
		t.Fatalf("resumable pending message was failed: message=%#v err=%v", message, err)
	}
	if err := st.DecideAgentApproval(ctx, approval.ID, domain.ApprovalStatusApproved, "reviewed"); err != nil {
		t.Fatal(err)
	}
	if resumable, err := st.ListDecidedAgentApprovals(ctx); err != nil || len(resumable) != 1 || resumable[0].ID != approval.ID {
		t.Fatalf("decided Agent approval was not resumable: approvals=%#v err=%v", resumable, err)
	}
	beforeClaim, err := st.GetRun(ctx, run.ID)
	if err != nil || beforeClaim.Status != "approval_required" {
		t.Fatalf("approval decision started execution: run=%#v err=%v", beforeClaim, err)
	}
	if err := st.ClaimAgentApprovalRun(ctx, approval.ID, run.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err := st.GetRun(ctx, run.ID)
	if err != nil || claimed.Status != "running" {
		t.Fatalf("resumed approval was not claimed: run=%#v err=%v", claimed, err)
	}
	if err := st.ClaimAgentApprovalRun(ctx, approval.ID, run.ID); err == nil {
		t.Fatal("duplicate resume claimed the run twice")
	}
	if resumable, err := st.ListDecidedAgentApprovals(ctx); err != nil || len(resumable) != 1 {
		t.Fatalf("claimed Agent approval lost its checkpoint before turn completion: approvals=%#v err=%v", resumable, err)
	}
	if err := st.Delete(ctx, approval.CheckpointID); err != nil {
		t.Fatal(err)
	}
	if err := st.FailPendingChatMessages(ctx); err != nil {
		t.Fatal(err)
	}
	if message, err := st.GetChatMessage(ctx, "session-agent-approval", userMessageID); err != nil || message.Status != "failed" {
		t.Fatalf("orphaned pending message survived recovery: message=%#v err=%v", message, err)
	}
	if resumable, err := st.ListDecidedAgentApprovals(ctx); err != nil || len(resumable) != 0 {
		t.Fatalf("completed checkpoint remained resumable: approvals=%#v err=%v", resumable, err)
	}
}

func TestAgentApprovalGroupBecomesResumableOnlyWhenEveryItemIsDecided(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "agent-approval-group.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	host, err := st.UpsertHost(ctx, domain.Host{
		ID: "host-agent-group", Name: "agent-group", Address: "192.0.2.24", Port: 22,
		User: "ops", AuthType: "agent", SudoMode: "none", CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	const checkpointID = "checkpoint-agent-group"
	interrupts := make(map[string]string, 2)
	approvals := make([]domain.Approval, 0, 2)
	for index := 1; index <= 2; index++ {
		run := domain.Run{
			ID: fmt.Sprintf("run-agent-group-%d", index), SessionID: "session-agent-group", HostID: host.ID,
			RequestJSON: `{}`, RequestDigest: fmt.Sprintf("digest-%d", index), Status: "approval_required", StartedAt: now,
		}
		if err := st.CreateRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		approval := domain.Approval{
			ID: fmt.Sprintf("approval-agent-group-%d", index), RunID: run.ID, HostID: host.ID,
			RequestJSON: `{}`, RequestDigest: run.RequestDigest, Status: domain.ApprovalStatusPreparing,
			ContinuationKind: domain.ApprovalContinuationAgent, CheckpointID: checkpointID, CreatedAt: now.Add(time.Duration(index) * time.Millisecond),
		}
		if err := st.CreateApproval(ctx, approval); err != nil {
			t.Fatal(err)
		}
		approvals = append(approvals, approval)
		interrupts[approval.ID] = fmt.Sprintf("interrupt-%d", index)
	}
	if err := st.Set(ctx, checkpointID, []byte("checkpoint")); err != nil {
		t.Fatal(err)
	}
	if err := st.ActivateAgentApprovals(ctx, checkpointID, interrupts); err != nil {
		t.Fatal(err)
	}
	if err := st.DecideAgentApproval(ctx, approvals[0].ID, domain.ApprovalStatusApproved, "first"); err != nil {
		t.Fatal(err)
	}
	if ready, err := st.ListDecidedAgentApprovals(ctx); err != nil || len(ready) != 0 {
		t.Fatalf("partial approval group became resumable: approvals=%#v err=%v", ready, err)
	}
	if err := st.DecideAgentApproval(ctx, approvals[1].ID, domain.ApprovalStatusApproved, "second"); err != nil {
		t.Fatal(err)
	}
	if ready, err := st.ListDecidedAgentApprovals(ctx); err != nil || len(ready) != 2 {
		t.Fatalf("complete approval group was not resumable: approvals=%#v err=%v", ready, err)
	}
}

func TestUnactivatedAgentApprovalIsAbortedDuringRecovery(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "preparing-agent-approval.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	host, err := st.UpsertHost(ctx, domain.Host{
		ID: "host-preparing", Name: "preparing", Address: "192.0.2.23", Port: 22,
		User: "ops", AuthType: "agent", SudoMode: "none", CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	run := domain.Run{ID: "run-preparing", SessionID: "session-preparing", HostID: host.ID,
		RequestJSON: `{}`, RequestDigest: "digest", Status: "approval_required", StartedAt: now}
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	approval := domain.Approval{ID: "approval-preparing", RunID: run.ID, HostID: host.ID,
		RequestJSON: `{}`, RequestDigest: run.RequestDigest, Status: domain.ApprovalStatusPreparing,
		ContinuationKind: domain.ApprovalContinuationAgent, CheckpointID: "checkpoint-preparing", CreatedAt: now}
	if err := st.CreateApproval(ctx, approval); err != nil {
		t.Fatal(err)
	}
	if err := st.Set(ctx, approval.CheckpointID, []byte("incomplete")); err != nil {
		t.Fatal(err)
	}
	const reason = "control plane restarted before activation"
	if err := st.AbortUnactivatedAgentApprovals(ctx, reason); err != nil {
		t.Fatal(err)
	}
	recoveredApproval, err := st.GetApproval(ctx, approval.ID)
	if err != nil || recoveredApproval.Status != domain.ApprovalStatusRejected || recoveredApproval.Reason != reason {
		t.Fatalf("preparing approval was not rejected: approval=%#v err=%v", recoveredApproval, err)
	}
	recoveredRun, err := st.GetRun(ctx, run.ID)
	if err != nil || recoveredRun.Status != "interrupted" || recoveredRun.Error != reason {
		t.Fatalf("preparing run was not interrupted: run=%#v err=%v", recoveredRun, err)
	}
	if _, present, err := st.Get(ctx, approval.CheckpointID); err != nil || present {
		t.Fatalf("incomplete checkpoint was retained: present=%v err=%v", present, err)
	}
}

func TestAgentApprovalRejectionAtomicallyRejectsRun(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "agent-rejection.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().UTC()
	host, err := st.UpsertHost(ctx, domain.Host{
		ID: "host-agent-rejection", Name: "agent-rejection", Address: "192.0.2.22", Port: 22,
		User: "ops", AuthType: "agent", SudoMode: "none", CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	run := domain.Run{
		ID: "run-agent-rejection", SessionID: "session-agent-rejection", HostID: host.ID,
		RequestJSON: `{}`, RequestDigest: "digest", Status: "approval_required", StartedAt: now,
	}
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	approval := domain.Approval{
		ID: "approval-agent-rejection", RunID: run.ID, HostID: host.ID, RequestJSON: `{}`,
		RequestDigest: run.RequestDigest, Status: domain.ApprovalStatusPreparing,
		ContinuationKind: domain.ApprovalContinuationAgent, CheckpointID: "checkpoint-rejection", CreatedAt: now,
	}
	if err := st.CreateApproval(ctx, approval); err != nil {
		t.Fatal(err)
	}
	if err := st.ActivateAgentApprovals(ctx, approval.CheckpointID, map[string]string{approval.ID: "interrupt-rejection"}); err != nil {
		t.Fatal(err)
	}
	if err := st.DecideAgentApproval(ctx, approval.ID, domain.ApprovalStatusRejected, "inspect logs first"); err != nil {
		t.Fatal(err)
	}
	rejectedApproval, err := st.GetApproval(ctx, approval.ID)
	if err != nil || rejectedApproval.Status != domain.ApprovalStatusRejected {
		t.Fatalf("approval rejection was not persisted: approval=%#v err=%v", rejectedApproval, err)
	}
	rejectedRun, err := st.GetRun(ctx, run.ID)
	if err != nil || rejectedRun.Status != "rejected" || rejectedRun.Error != "inspect logs first" || rejectedRun.CompletedAt.IsZero() {
		t.Fatalf("run rejection was not atomic: run=%#v err=%v", rejectedRun, err)
	}
}
