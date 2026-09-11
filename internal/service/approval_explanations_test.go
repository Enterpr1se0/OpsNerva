package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

type fakeCommandExplainer struct {
	mu     sync.Mutex
	review domain.CommandReview
	err    error
	inputs []domain.CommandReviewInput
}

func (f *fakeCommandExplainer) Review(_ context.Context, input domain.CommandReviewInput) (domain.CommandReview, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inputs = append(f.inputs, input)
	return f.review, f.err
}

func (f *fakeCommandExplainer) Inputs() []domain.CommandReviewInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.CommandReviewInput(nil), f.inputs...)
}

type blockingCommandExplainer struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	review  domain.CommandReview
}

func (r *blockingCommandExplainer) Review(ctx context.Context, _ domain.CommandReviewInput) (domain.CommandReview, error) {
	r.once.Do(func() { close(r.started) })
	select {
	case <-r.release:
		return r.review, nil
	case <-ctx.Done():
		return domain.CommandReview{}, ctx.Err()
	}
}

type trackingCommandExplainer struct {
	started chan struct{}
	mu      sync.Mutex
	active  int
	maximum int
}

func (r *trackingCommandExplainer) Review(ctx context.Context, _ domain.CommandReviewInput) (domain.CommandReview, error) {
	r.mu.Lock()
	r.active++
	if r.active > r.maximum {
		r.maximum = r.active
	}
	r.mu.Unlock()
	r.started <- struct{}{}
	defer func() {
		r.mu.Lock()
		r.active--
		r.mu.Unlock()
	}()
	<-ctx.Done()
	return domain.CommandReview{}, ctx.Err()
}

func (r *trackingCommandExplainer) maxActive() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.maximum
}

func waitForApproval(t *testing.T, svc *Service, approvalID string, ready func(domain.Approval) bool) domain.Approval {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		approvals, err := svc.ListApprovals(context.Background(), "", 200)
		if err != nil {
			t.Fatal(err)
		}
		for _, approval := range approvals {
			if approval.ID == approvalID && ready(approval) {
				return approval
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for approval %s", approvalID)
	return domain.Approval{}
}

func TestCommandReviewDeadlineUsesReadableConfiguredTimeout(t *testing.T) {
	svc, _, _ := newTestService(t)
	review := svc.normalizeCommandReview(
		domain.CommandReview{Model: "small-model"},
		fmt.Errorf("[NodeRunError] failed to create chat completion: %w", context.DeadlineExceeded),
		45,
	)
	if review.Status != "unavailable" || review.Model != "small-model" || len(review.Errors) != 1 || review.Errors[0] != "approval Agent did not respond within 45 seconds" {
		t.Fatalf("unexpected normalized deadline review: %#v", review)
	}
}

func TestCommandReviewRedactsModelOutput(t *testing.T) {
	svc, _, _ := newTestService(t)
	review := svc.normalizeCommandReview(domain.CommandReview{
		Status: "completed", Decision: domain.ApprovalAgentReject, Reason: "password=review-secret",
		Explanation: &domain.CommandExplanation{
			Summary: "password=summary-secret", Mechanism: "api_key=mechanism-secret", Risks: []string{"password=risk-secret"},
		},
	}, nil, 30)
	encoded, err := json.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"review-secret", "summary-secret", "mechanism-secret", "risk-secret"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("approval Agent output retained secret %q: %s", secret, encoded)
		}
	}
}

func TestCommandExplainerPersistsAdviceForManualApproval(t *testing.T) {
	svc, _, host := newTestService(t)
	explainer := &fakeCommandExplainer{review: domain.CommandReview{
		Status:      "completed",
		Explanation: &domain.CommandExplanation{Summary: "重启服务", Mechanism: "由 systemd 停止并重新启动单元"},
		ReviewedAt:  time.Now().UTC(),
	}}
	svc.SetApprovalReviewer(explainer)
	result, err := svc.Submit(context.Background(), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"},
		Reason: "recover demo",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "approval_required" {
		t.Fatalf("manual approval was bypassed: %#v", result)
	}
	waitForApproval(t, svc, result.ApprovalID, func(approval domain.Approval) bool {
		return approval.AIReview != nil && approval.AIReview.Status != "pending"
	})
	approvals, err := svc.ListApprovals(context.Background(), "pending", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(approvals) != 1 || approvals[0].AIReview == nil || approvals[0].AIReview.Explanation == nil {
		t.Fatalf("structured explanation was not normalized and persisted: %#v", approvals)
	}
	inputs := explainer.Inputs()
	if len(inputs) != 1 || inputs[0].RequestDigest == "" {
		t.Fatalf("explanation Agent did not receive bounded context: %#v", inputs)
	}
}

func TestApprovalIsCreatedWithoutWaitingForCommandExplanation(t *testing.T) {
	svc, _, host := newTestService(t)
	explainer := &blockingCommandExplainer{
		started: make(chan struct{}), release: make(chan struct{}),
		review: domain.CommandReview{
			Status: "completed", Explanation: &domain.CommandExplanation{Summary: "重启服务", Mechanism: "systemd 重启单元"}, ReviewedAt: time.Now().UTC(),
		},
	}
	svc.SetApprovalReviewer(explainer)
	released := false
	defer func() {
		if !released {
			close(explainer.release)
		}
	}()

	type outcome struct {
		result domain.ExecResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := svc.Submit(context.Background(), domain.ExecRequest{
			HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"},
			Reason: "recover demo",
		}, "eino-agent")
		done <- outcome{result: result, err: err}
	}()

	var submitted outcome
	select {
	case submitted = <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("approval creation blocked on command explanation")
	}
	if submitted.err != nil || submitted.result.Status != "approval_required" {
		t.Fatalf("unexpected immediate approval result: %#v err=%v", submitted.result, submitted.err)
	}
	select {
	case <-explainer.started:
	case <-time.After(time.Second):
		t.Fatal("background explanation did not start")
	}
	waitForApproval(t, svc, submitted.result.ApprovalID, func(approval domain.Approval) bool {
		return approval.AIReview != nil && approval.AIReview.Status == "pending"
	})
	close(explainer.release)
	released = true
	waitForApproval(t, svc, submitted.result.ApprovalID, func(approval domain.Approval) bool {
		return approval.AIReview != nil && approval.AIReview.Status == "completed"
	})
}

func TestApprovalDecisionCancelsCommandExplanation(t *testing.T) {
	svc, transport, host := newTestService(t)
	explainer := &blockingCommandExplainer{
		started: make(chan struct{}), release: make(chan struct{}),
		review: domain.CommandReview{
			Status: "completed", Explanation: &domain.CommandExplanation{Summary: "重启服务", Mechanism: "systemd 重启单元"}, ReviewedAt: time.Now().UTC(),
		},
	}
	svc.SetApprovalReviewer(explainer)
	pending, err := svc.Submit(context.Background(), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"},
		Reason: "recover demo",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-explainer.started:
	case <-time.After(time.Second):
		t.Fatal("background explanation did not start")
	}
	if _, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed operation", "operator"); err != nil {
		t.Fatal(err)
	}
	svc.explainWG.Wait()

	approval, err := svc.store.GetApproval(context.Background(), pending.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	if approval.Status != "approved" {
		t.Fatalf("late explanation overwrote the approval decision: %#v", approval)
	}
	run, err := svc.store.GetRun(context.Background(), pending.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.AIReview != nil || run.AIReviewJSON != "" {
		t.Fatalf("canceled explanation remained attached to the decided run: %#v", run.AIReview)
	}
	if len(transport.calls) != 1 {
		t.Fatalf("operation was not executed exactly once: %#v", transport.calls)
	}
}

func TestCommandExplanationConcurrencyIsBounded(t *testing.T) {
	svc, _, host := newTestService(t)
	explainer := &trackingCommandExplainer{started: make(chan struct{}, 4)}
	svc.SetApprovalReviewer(explainer)
	results := make([]domain.ExecResult, 0, 3)
	for index := 0; index < 3; index++ {
		result, err := svc.Submit(context.Background(), domain.ExecRequest{
			HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", fmt.Sprintf("demo-%d", index)},
			Reason: "recover demo",
		}, "eino-agent")
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, result)
	}
	for index := 0; index < maxConcurrentApprovalExplanations; index++ {
		select {
		case <-explainer.started:
		case <-time.After(time.Second):
			t.Fatal("expected explanation did not start")
		}
	}
	select {
	case <-explainer.started:
		t.Fatal("explanation concurrency limit was exceeded")
	case <-time.After(100 * time.Millisecond):
	}
	if err := svc.Reject(context.Background(), results[0].ApprovalID, "not approved", "operator"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-explainer.started:
	case <-time.After(time.Second):
		t.Fatal("queued explanation did not start after a slot was released")
	}
	for _, result := range results[1:] {
		if err := svc.Reject(context.Background(), result.ApprovalID, "not approved", "operator"); err != nil {
			t.Fatal(err)
		}
	}
	svc.explainWG.Wait()
	if maximum := explainer.maxActive(); maximum != maxConcurrentApprovalExplanations {
		t.Fatalf("maximum concurrent explanations = %d", maximum)
	}
}

func TestCommandExplanationQueueIsBounded(t *testing.T) {
	svc, _, host := newTestService(t)
	svc.explanationSem = make(chan struct{}, 1)
	svc.explanationSlots = make(chan struct{}, 1)
	explainer := &trackingCommandExplainer{started: make(chan struct{}, 2)}
	svc.SetApprovalReviewer(explainer)
	first, err := svc.Submit(context.Background(), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo-one"},
		Reason: "recover demo",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-explainer.started:
	case <-time.After(time.Second):
		t.Fatal("first explanation did not start")
	}
	second, err := svc.Submit(context.Background(), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo-two"},
		Reason: "recover demo",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	skipped := waitForApproval(t, svc, second.ApprovalID, func(approval domain.Approval) bool {
		return approval.AIReview != nil && approval.AIReview.Status == "unavailable"
	})
	if len(skipped.AIReview.Errors) != 1 || !strings.Contains(skipped.AIReview.Errors[0], "queue is full") {
		t.Fatalf("queue overflow was not reported clearly: %#v", skipped.AIReview)
	}
	if err := svc.Reject(context.Background(), first.ApprovalID, "not approved", "operator"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Reject(context.Background(), second.ApprovalID, "not approved", "operator"); err != nil {
		t.Fatal(err)
	}
	svc.explainWG.Wait()
}

func TestRetryApprovalExplanationDoesNotExecute(t *testing.T) {
	svc, transport, host := newTestService(t)
	ctx := context.Background()
	pending, err := svc.Submit(ctx, domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"},
		Reason: "recover demo",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	explainer := &fakeCommandExplainer{review: domain.CommandReview{
		Status: "completed", Explanation: &domain.CommandExplanation{
			Summary: "重启服务", Mechanism: "systemd 会停止并重新启动服务",
			Risks: []string{"可能短暂中断请求"},
		}, ReviewedAt: time.Now().UTC(),
	}}
	svc.SetApprovalReviewer(explainer)

	updated, err := svc.RetryApprovalExplanation(ctx, pending.ApprovalID, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "pending" || updated.AIReview == nil || updated.AIReview.Explanation == nil {
		t.Fatalf("explanation retry changed the pending approval: %#v", updated)
	}
	if len(transport.calls) != 0 {
		t.Fatalf("explanation retry executed the operation: %#v", transport.calls)
	}
	inputs := explainer.Inputs()
	if len(inputs) != 1 || inputs[0].RequestDigest != updated.RequestDigest {
		t.Fatalf("explanation retry did not receive the exact pending request: %#v", inputs)
	}
	if _, err := svc.Approve(ctx, updated.ID, "reviewed operation", "operator"); err != nil {
		t.Fatal(err)
	}
	if len(transport.calls) != 1 {
		t.Fatalf("approved operation was not executed exactly once: %#v", transport.calls)
	}
}

func TestRetryApprovalExplanationPersistsDegradedResultAndKeepsPending(t *testing.T) {
	svc, transport, host := newTestService(t)
	ctx := context.Background()
	pending, err := svc.Submit(ctx, domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"},
		Reason: "recover demo",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	svc.SetApprovalReviewer(&fakeCommandExplainer{err: errors.New("model timed out")})
	updated, err := svc.RetryApprovalExplanation(ctx, pending.ApprovalID, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "pending" || updated.AIReview == nil || updated.AIReview.Status != "unavailable" {
		t.Fatalf("degraded retry changed the approval boundary: %#v", updated)
	}
	if len(updated.AIReview.Errors) != 1 || !strings.Contains(updated.AIReview.Errors[0], "model timed out") {
		t.Fatalf("degraded retry error was not preserved: %#v", updated.AIReview)
	}
	if len(transport.calls) != 0 {
		t.Fatalf("degraded explanation retry executed the operation: %#v", transport.calls)
	}
	listed, err := svc.ListApprovals(ctx, "pending", 10)
	if err != nil || len(listed) != 1 || listed[0].AIReview == nil || listed[0].AIReview.Status != "unavailable" {
		t.Fatalf("degraded retry was not persisted: approvals=%#v err=%v", listed, err)
	}
}

func TestApprovalDecisionCancelsRetriedCommandExplanation(t *testing.T) {
	svc, _, host := newTestService(t)
	explainer := &trackingCommandExplainer{started: make(chan struct{}, 2)}
	svc.SetApprovalReviewer(explainer)
	pending, err := svc.Submit(context.Background(), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"},
		Reason: "recover demo",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-explainer.started:
	case <-time.After(time.Second):
		t.Fatal("automatic explanation did not start")
	}

	retryDone := make(chan error, 1)
	go func() {
		_, retryErr := svc.RetryApprovalExplanation(context.Background(), pending.ApprovalID, "operator")
		retryDone <- retryErr
	}()
	select {
	case <-explainer.started:
	case <-time.After(time.Second):
		t.Fatal("retried explanation did not start")
	}
	if err := svc.Reject(context.Background(), pending.ApprovalID, "not approved", "operator"); err != nil {
		t.Fatal(err)
	}
	select {
	case retryErr := <-retryDone:
		if !errors.Is(retryErr, context.Canceled) {
			t.Fatalf("retry error = %v", retryErr)
		}
	case <-time.After(time.Second):
		t.Fatal("retried explanation continued after approval rejection")
	}
}
