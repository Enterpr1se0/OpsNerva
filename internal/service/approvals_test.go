package service

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

type fakeAutomaticApprovalReviewer struct {
	mu     sync.Mutex
	review domain.CommandReview
	err    error
	inputs []domain.AutomaticApprovalInput
}

func (f *fakeAutomaticApprovalReviewer) Review(_ context.Context, input domain.AutomaticApprovalInput) (domain.CommandReview, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inputs = append(f.inputs, input)
	return f.review, f.err
}

func (f *fakeAutomaticApprovalReviewer) Inputs() []domain.AutomaticApprovalInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.AutomaticApprovalInput(nil), f.inputs...)
}

func assertNoPendingApprovals(t *testing.T, svc *Service) {
	t.Helper()
	approvals, err := svc.ListApprovals(context.Background(), "pending", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(approvals) != 0 {
		t.Fatalf("operator connection unexpectedly created approvals: %#v", approvals)
	}
}

func saveApprovalMode(t *testing.T, svc *Service, mode string) {
	t.Helper()
	explanationsEnabled := false
	if _, err := svc.SaveSystemSettings(context.Background(), domain.SystemSettingsInput{
		AgentMaxIterations:          domain.DefaultAgentMaxIterations,
		ApprovalMode:                &mode,
		ApprovalExplanationsEnabled: &explanationsEnabled,
	}, "test"); err != nil {
		t.Fatal(err)
	}
}

func TestElevatedExecutionUsesManagedSecretAfterApproval(t *testing.T) {
	svc, transport, _ := newTestService(t)
	agentRootEnabled := true
	host, err := svc.SaveHost(context.Background(), domain.HostInput{
		Name: "sudo-host", Address: "192.0.2.11", Port: 22, User: "ops", AuthType: "password",
		Password: "ssh-secret", SudoMode: "password", SudoPassword: "sudo-secret",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	host, err = svc.SetHostAgentRootEnabled(context.Background(), host.ID, agentRootEnabled, "test")
	if err != nil {
		t.Fatal(err)
	}
	callArguments := `{"host_id":"sudo-host","program":"id","elevated":true,"reason":"verify managed root access"}`
	ctx := WithExecutionOwner(context.Background(), "call-elevated", "ssh_exec", callArguments)
	result, err := svc.Submit(ctx, domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "id", Elevated: true, Reason: "verify managed root access",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "approval_required" {
		t.Fatalf("elevated request bypassed approval: %#v", result)
	}
	stored, err := svc.store.GetRun(context.Background(), result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ToolName != "ssh_exec" || stored.ToolArgumentsJSON != callArguments || !strings.Contains(stored.RequestJSON, `"elevated":true`) {
		t.Fatalf("elevated Tool Call was not preserved in history: %#v", stored)
	}
	if _, err := svc.Approve(context.Background(), result.ApprovalID, "root access reviewed", "operator"); err != nil {
		t.Fatal(err)
	}
	if len(transport.hosts) != 1 || transport.hosts[0].Password != "ssh-secret" || transport.hosts[0].SudoPassword != "sudo-secret" {
		t.Fatalf("transport did not receive transient managed credentials: %#v", transport.hosts)
	}
}

func TestManualApprovalModeKeepsHumanApproval(t *testing.T) {
	svc, transport, host := newTestService(t)
	automatic := &fakeAutomaticApprovalReviewer{review: domain.CommandReview{
		Status: "completed", Decision: domain.ApprovalAgentAllow, Reason: "范围明确",
		Explanation: &domain.CommandExplanation{Summary: "重启 demo", Mechanism: "systemd 重启单元"}, ReviewedAt: time.Now().UTC(),
	}}
	svc.SetAutomaticApprovalReviewer(automatic)
	svc.SetApprovalReviewer(&fakeCommandExplainer{review: domain.CommandReview{
		Status: "completed", Decision: domain.ApprovalAgentAllow, Reason: "范围明确", ReviewedAt: time.Now().UTC(),
	}})
	result, err := svc.Submit(context.Background(), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"}, Reason: "recover demo",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "approval_required" || result.AutoApproved || result.ApprovalID == "" || len(transport.calls) != 0 || len(automatic.Inputs()) != 0 {
		t.Fatalf("manual mode bypassed human approval: result=%#v calls=%d", result, len(transport.calls))
	}
}

func TestAutoApprovalModeExecutesAllowedOperation(t *testing.T) {
	svc, transport, host := newTestService(t)
	saveApprovalMode(t, svc, domain.ApprovalModeAuto)
	reviewer := &fakeAutomaticApprovalReviewer{review: domain.CommandReview{
		Status: "completed", Decision: domain.ApprovalAgentAllow, Reason: "目标与范围明确",
		Explanation: &domain.CommandExplanation{Summary: "重启 demo", Mechanism: "systemd 重启单元"}, ReviewedAt: time.Now().UTC(),
	}}
	svc.SetAutomaticApprovalReviewer(reviewer)
	ctx := WithApprovalUserRequest(context.Background(), "重启 demo 服务以恢复运行")
	result, err := svc.Submit(ctx, domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"}, Reason: "recover demo",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" || !result.AutoApproved || len(transport.calls) != 1 {
		t.Fatalf("approval Agent did not allow execution: result=%#v calls=%d", result, len(transport.calls))
	}
	run, err := svc.store.GetRun(context.Background(), result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.AIReview == nil || run.AIReview.Kind != domain.CommandReviewKindAutomaticApproval || run.AIReview.Decision != domain.ApprovalAgentAllow {
		t.Fatalf("approval Agent decision was not persisted: %#v", run.AIReview)
	}
	inputs := reviewer.Inputs()
	if len(inputs) != 1 || inputs[0].UserRequest != "重启 demo 服务以恢复运行" {
		t.Fatalf("approval Agent did not receive the current user request: %#v", inputs)
	}
}

func TestAutoApprovalModeRejectsOperation(t *testing.T) {
	svc, transport, host := newTestService(t)
	saveApprovalMode(t, svc, domain.ApprovalModeAuto)
	svc.SetAutomaticApprovalReviewer(&fakeAutomaticApprovalReviewer{review: domain.CommandReview{
		Status: "completed", Decision: domain.ApprovalAgentReject, Reason: "请求范围过大",
		Explanation: &domain.CommandExplanation{Summary: "重启 demo", Mechanism: "systemd 重启单元"}, ReviewedAt: time.Now().UTC(),
	}})
	result, err := svc.Submit(WithApprovalUserRequest(context.Background(), "只查看 demo 状态"), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"}, Reason: "recover demo",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "rejected" || !strings.Contains(result.Stderr, "请求范围过大") || len(transport.calls) != 0 {
		t.Fatalf("approval Agent rejection was not enforced: result=%#v calls=%d", result, len(transport.calls))
	}
	approvals, err := svc.ListApprovals(context.Background(), "pending", 10)
	if err != nil || len(approvals) != 0 {
		t.Fatalf("automatic rejection created a human approval: %#v err=%v", approvals, err)
	}
}

func TestAutoApprovalModeFallsBackToHuman(t *testing.T) {
	svc, transport, host := newTestService(t)
	saveApprovalMode(t, svc, domain.ApprovalModeAuto)
	result, err := svc.Submit(WithApprovalUserRequest(context.Background(), "重启 demo 服务"), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"}, Reason: "recover demo",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "approval_required" || result.ApprovalID == "" || len(transport.calls) != 0 {
		t.Fatalf("unavailable approval Agent did not fall back to a human: result=%#v calls=%d", result, len(transport.calls))
	}
	approval := waitForApproval(t, svc, result.ApprovalID, func(value domain.Approval) bool {
		return value.AIReview != nil
	})
	if approval.AIReview.Status != "unavailable" {
		t.Fatalf("fallback reason was not persisted: %#v", approval.AIReview)
	}
}

func TestAutoApprovalModeDoesNotReuseExplanationAgent(t *testing.T) {
	svc, transport, host := newTestService(t)
	saveApprovalMode(t, svc, domain.ApprovalModeAuto)
	explainer := &fakeCommandExplainer{review: domain.CommandReview{
		Status: "completed", Decision: domain.ApprovalAgentAllow, Reason: "范围明确", ReviewedAt: time.Now().UTC(),
	}}
	svc.SetApprovalReviewer(explainer)
	result, err := svc.Submit(WithApprovalUserRequest(context.Background(), "重启 demo 服务"), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"}, Reason: "recover demo",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "approval_required" || result.ApprovalID == "" || len(transport.calls) != 0 || len(explainer.Inputs()) != 0 {
		t.Fatalf("Auto mode reused the explanation Agent: result=%#v calls=%d explanation_reviews=%d", result, len(transport.calls), len(explainer.Inputs()))
	}
}

func TestAutoApprovalModeFallsBackOnInvalidReview(t *testing.T) {
	svc, transport, host := newTestService(t)
	saveApprovalMode(t, svc, domain.ApprovalModeAuto)
	svc.SetAutomaticApprovalReviewer(&fakeAutomaticApprovalReviewer{review: domain.CommandReview{
		Status: "completed", Decision: domain.ApprovalAgentAllow, ReviewedAt: time.Now().UTC(),
	}})
	result, err := svc.Submit(WithApprovalUserRequest(context.Background(), "重启 demo 服务"), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"}, Reason: "recover demo",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "approval_required" || len(transport.calls) != 0 {
		t.Fatalf("invalid approval Agent response bypassed the human fallback: result=%#v calls=%d", result, len(transport.calls))
	}
}

func TestAutoApprovalModeUsesManualDecisionWithoutExecuting(t *testing.T) {
	svc, transport, host := newTestService(t)
	saveApprovalMode(t, svc, domain.ApprovalModeAuto)
	svc.SetAutomaticApprovalReviewer(&fakeAutomaticApprovalReviewer{review: domain.CommandReview{
		Status: "completed", Decision: domain.ApprovalAgentManual, Reason: "目标范围需要用户确认",
		Explanation: &domain.CommandExplanation{Summary: "重启 demo", Mechanism: "systemd 重启单元"}, ReviewedAt: time.Now().UTC(),
	}})
	result, err := svc.Submit(WithApprovalUserRequest(context.Background(), "检查并修复 demo"), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"}, Reason: "recover demo",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "approval_required" || result.ApprovalID == "" || len(transport.calls) != 0 {
		t.Fatalf("manual approval Agent decision did not fall back to the operator: result=%#v calls=%d", result, len(transport.calls))
	}
	run, err := svc.store.GetRun(context.Background(), result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.AIReview == nil || run.AIReview.Decision != domain.ApprovalAgentManual {
		t.Fatalf("manual approval Agent decision was not persisted: %#v", run.AIReview)
	}
}

func TestAutoApprovalModeRequiresCurrentUserRequest(t *testing.T) {
	svc, transport, host := newTestService(t)
	saveApprovalMode(t, svc, domain.ApprovalModeAuto)
	reviewer := &fakeAutomaticApprovalReviewer{review: domain.CommandReview{
		Status: "completed", Decision: domain.ApprovalAgentAllow, Reason: "范围明确",
		Explanation: &domain.CommandExplanation{Summary: "重启 demo", Mechanism: "systemd 重启单元"}, ReviewedAt: time.Now().UTC(),
	}}
	svc.SetAutomaticApprovalReviewer(reviewer)
	result, err := svc.Submit(context.Background(), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"}, Reason: "recover demo",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "approval_required" || result.ApprovalID == "" || len(transport.calls) != 0 || len(reviewer.Inputs()) != 0 {
		t.Fatalf("missing user request did not fail closed to manual approval: result=%#v calls=%d reviews=%d", result, len(transport.calls), len(reviewer.Inputs()))
	}
}

func TestFullAccessModeBypassesPolicyOnlyForAgent(t *testing.T) {
	svc, transport, host := newTestService(t)
	saveApprovalMode(t, svc, domain.ApprovalModeFullAccess)
	request := domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecScript, Script: "cat ~/.ssh/id_ed25519", Reason: "inspect configured credential",
	}
	result, err := svc.Submit(context.Background(), request, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" || result.AutoApproved || len(transport.calls) != 1 {
		t.Fatalf("full access did not execute the Agent request directly: result=%#v calls=%d", result, len(transport.calls))
	}
	mcpResult, err := svc.Submit(context.Background(), request, "mcp-client")
	if err != nil {
		t.Fatal(err)
	}
	if mcpResult.Status != "completed" || mcpResult.AutoApproved || len(transport.calls) != 2 {
		t.Fatalf("full access did not apply to the LLM-facing MCP server: result=%#v calls=%d", mcpResult, len(transport.calls))
	}
	operatorResult, err := svc.Submit(context.Background(), request, "admin-web")
	if err != nil {
		t.Fatal(err)
	}
	if operatorResult.Status != "completed" || operatorResult.AutoApproved || len(transport.calls) != 3 {
		t.Fatalf("authenticated operator request did not execute directly: result=%#v calls=%d", operatorResult, len(transport.calls))
	}
}

func TestChangeRequiresApprovalThenExecutes(t *testing.T) {
	svc, transport, host := newTestService(t)
	result, err := svc.Submit(context.Background(), domain.ExecRequest{HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"}, Reason: "recover service"}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "approval_required" || result.ApprovalID == "" || len(transport.calls) != 0 {
		t.Fatalf("unexpected pending result %#v calls=%d", result, len(transport.calls))
	}
	approved, err := svc.Approve(context.Background(), result.ApprovalID, "reviewed", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != "completed" || len(transport.calls) != 1 {
		t.Fatalf("unexpected approved result %#v calls=%d", approved, len(transport.calls))
	}
}

func TestAgentApprovalDecisionDoesNotExecuteBeforeCheckpointResume(t *testing.T) {
	svc, transport, host := newTestService(t)
	ctx := WithAgentApprovalContinuation(WithSessionID(context.Background(), "session_agent_resume"), "checkpoint_agent_resume")
	pending, err := svc.Submit(ctx, domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"},
		Reason: "restart demo after review",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != "approval_required" || pending.ApprovalID == "" || len(transport.calls) != 0 {
		t.Fatalf("unexpected pending result %#v calls=%d", pending, len(transport.calls))
	}
	preparing, err := svc.GetApproval(context.Background(), pending.ApprovalID)
	if err != nil || preparing.Status != domain.ApprovalStatusPreparing || preparing.CheckpointID != "checkpoint_agent_resume" {
		t.Fatalf("Agent continuation was not prepared: approval=%#v err=%v", preparing, err)
	}
	if listed, err := svc.ListApprovals(context.Background(), domain.ApprovalStatusPending, 10); err != nil || len(listed) != 0 {
		t.Fatalf("approval was visible before its checkpoint: approvals=%#v err=%v", listed, err)
	}
	if err := svc.store.Set(context.Background(), preparing.CheckpointID, []byte("checkpoint")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ActivateAgentApprovals(context.Background(), preparing.CheckpointID, map[string]string{preparing.ID: "interrupt_agent_resume"}); err != nil {
		t.Fatal(err)
	}
	decision, err := svc.DecideAgentApproval(context.Background(), preparing.ID, domain.ApprovalStatusApproved, "reviewed", "operator")
	if err != nil || decision.Status != domain.ApprovalStatusApproved || len(transport.calls) != 0 {
		t.Fatalf("approval decision executed the operation: result=%#v calls=%d err=%v", decision, len(transport.calls), err)
	}
	run, err := svc.store.GetRun(context.Background(), pending.RunID)
	if err != nil || run.Status != "approval_required" {
		t.Fatalf("approved run was claimed before resume: run=%#v err=%v", run, err)
	}
	completed, err := svc.ResumeAgentApproval(context.Background(), preparing.ID)
	if err != nil || completed.Status != "completed" || len(transport.calls) != 1 {
		t.Fatalf("resumed operation = %#v calls=%d err=%v", completed, len(transport.calls), err)
	}
	if replayed, err := svc.ResumeAgentApproval(context.Background(), preparing.ID); err != nil || replayed.Status != "completed" || len(transport.calls) != 1 {
		t.Fatalf("completed approval was not replay-safe: result=%#v calls=%d err=%v", replayed, len(transport.calls), err)
	}
}

func TestRejectedAgentApprovalResumesAsToolRejectionWithoutExecution(t *testing.T) {
	svc, transport, host := newTestService(t)
	ctx := WithAgentApprovalContinuation(WithSessionID(context.Background(), "session_agent_reject"), "checkpoint_agent_reject")
	pending, err := svc.Submit(ctx, domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"},
		Reason: "restart demo after review",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.store.Set(context.Background(), "checkpoint_agent_reject", []byte("checkpoint")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ActivateAgentApprovals(context.Background(), "checkpoint_agent_reject", map[string]string{pending.ApprovalID: "interrupt_agent_reject"}); err != nil {
		t.Fatal(err)
	}
	const instruction = "inspect logs first"
	if _, err := svc.DecideAgentApproval(context.Background(), pending.ApprovalID, domain.ApprovalStatusRejected, instruction, "operator"); err != nil {
		t.Fatal(err)
	}
	result, err := svc.ResumeAgentApproval(context.Background(), pending.ApprovalID)
	if err != nil || result.Status != domain.ApprovalStatusRejected || result.OperatorInstruction != instruction {
		t.Fatalf("rejected resume = %#v err=%v", result, err)
	}
	if len(transport.calls) != 0 {
		t.Fatalf("rejected Agent approval executed %d operations", len(transport.calls))
	}
}

func TestManualApprovalReasonIsOptional(t *testing.T) {
	svc, transport, host := newTestService(t)
	result, err := svc.Submit(context.Background(), domain.ExecRequest{HostID: host.ID, Mode: domain.ExecProgram, Program: "rm", Args: []string{"-rf", "/tmp/demo"}, Reason: "clean fixture"}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "approval_required" {
		t.Fatalf("unexpected manual approval result %#v", result)
	}
	approved, err := svc.Approve(context.Background(), result.ApprovalID, "", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != "completed" || len(transport.calls) != 1 {
		t.Fatalf("unexpected approved result %#v", approved)
	}
}

func TestCredentialReadUsesManualApproval(t *testing.T) {
	svc, transport, host := newTestService(t)
	result, err := svc.Submit(context.Background(), domain.ExecRequest{HostID: host.ID, Mode: domain.ExecScript, Script: "cat ~/.ssh/id_ed25519", Reason: "inspect configured credential"}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "approval_required" || result.ApprovalID == "" || len(transport.calls) != 0 {
		t.Fatalf("credential read bypassed manual approval: %#v", result)
	}
	if err := svc.Reject(context.Background(), result.ApprovalID, "not approved", "operator"); err != nil {
		t.Fatal(err)
	}
}

func TestAbortApprovalsForSession(t *testing.T) {
	svc, transport, host := newTestService(t)
	request := domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"},
		Reason: "recover demo service",
	}
	target, err := svc.Submit(WithSessionID(context.Background(), "session_stop"), request, "eino-agent")
	if err != nil || target.ApprovalID == "" {
		t.Fatalf("target approval = %#v err=%v", target, err)
	}
	request.Args = []string{"restart", "other"}
	other, err := svc.Submit(WithSessionID(context.Background(), "session_other"), request, "eino-agent")
	if err != nil || other.ApprovalID == "" {
		t.Fatalf("other approval = %#v err=%v", other, err)
	}

	rejected, err := svc.AbortApprovalsForSession(context.Background(), "session_stop", "Agent run stopped by the operator", "operator")
	if err != nil || rejected != 1 {
		t.Fatalf("rejected approvals = %d err=%v", rejected, err)
	}
	targetApproval, err := svc.store.GetApproval(context.Background(), target.ApprovalID)
	if err != nil || targetApproval.Status != "rejected" {
		t.Fatalf("target approval = %#v err=%v", targetApproval, err)
	}
	otherApproval, err := svc.store.GetApproval(context.Background(), other.ApprovalID)
	if err != nil || otherApproval.Status != "pending" {
		t.Fatalf("unrelated approval changed = %#v err=%v", otherApproval, err)
	}
	if len(transport.calls) != 0 {
		t.Fatalf("rejected approval executed %d commands", len(transport.calls))
	}
}

func TestAbortApprovalsForSessionRejectsPartiallyDecidedAgentGroup(t *testing.T) {
	svc, transport, host := newTestService(t)
	const (
		sessionID    = "session_stop_agent_group"
		checkpointID = "checkpoint_stop_agent_group"
	)
	ctx := WithAgentApprovalContinuation(WithSessionID(context.Background(), sessionID), checkpointID)
	approvals := make([]domain.ExecResult, 0, 2)
	for _, unit := range []string{"first", "second"} {
		result, err := svc.Submit(ctx, domain.ExecRequest{
			HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", unit},
			Reason: "test stopping a partially decided approval group",
		}, "eino-agent")
		if err != nil || result.ApprovalID == "" {
			t.Fatalf("Agent approval = %#v err=%v", result, err)
		}
		approvals = append(approvals, result)
	}
	if err := svc.store.Set(ctx, checkpointID, []byte("checkpoint")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ActivateAgentApprovals(ctx, checkpointID, map[string]string{
		approvals[0].ApprovalID: "interrupt-first", approvals[1].ApprovalID: "interrupt-second",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DecideAgentApproval(ctx, approvals[0].ApprovalID, domain.ApprovalStatusApproved, "approved first", "operator"); err != nil {
		t.Fatal(err)
	}
	aborted, err := svc.AbortApprovalsForSession(ctx, sessionID, "Agent run stopped by the operator", "operator")
	if err != nil || aborted != 2 {
		t.Fatalf("aborted approvals = %d err=%v", aborted, err)
	}
	for _, result := range approvals {
		approval, approvalErr := svc.store.GetApproval(ctx, result.ApprovalID)
		run, runErr := svc.store.GetRun(ctx, result.RunID)
		if approvalErr != nil || runErr != nil || approval.Status != domain.ApprovalStatusRejected || run.Status != domain.ApprovalStatusRejected {
			t.Fatalf("aborted Agent approval=%#v run=%#v errors=%v/%v", approval, run, approvalErr, runErr)
		}
	}
	if len(transport.calls) != 0 {
		t.Fatalf("aborted Agent group executed %d operations", len(transport.calls))
	}
}
