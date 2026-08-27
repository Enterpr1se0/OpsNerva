package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/observability"
)

const (
	maxConcurrentApprovalExplanations = 2
	maxQueuedApprovalExplanations     = 4
)

type approvalExplanationTask struct {
	cancel context.CancelFunc
}

type ApprovalReviewer interface {
	Review(context.Context, domain.CommandReviewInput) (domain.CommandReview, error)
}

type FreshApprovalReviewer interface {
	ReviewFresh(context.Context, domain.CommandReviewInput) (domain.CommandReview, error)
}

type AutomaticApprovalReviewer interface {
	Review(context.Context, domain.AutomaticApprovalInput) (domain.CommandReview, error)
}

func (s *Service) SetApprovalReviewer(reviewer ApprovalReviewer) {
	s.reviewerMu.Lock()
	s.reviewer = reviewer
	s.reviewerMu.Unlock()
}

func (s *Service) approvalReviewer() ApprovalReviewer {
	s.reviewerMu.RLock()
	defer s.reviewerMu.RUnlock()
	return s.reviewer
}

func (s *Service) SetAutomaticApprovalReviewer(reviewer AutomaticApprovalReviewer) {
	s.reviewerMu.Lock()
	s.automaticReviewer = reviewer
	s.reviewerMu.Unlock()
}

func (s *Service) automaticApprovalReviewer() AutomaticApprovalReviewer {
	s.reviewerMu.RLock()
	defer s.reviewerMu.RUnlock()
	return s.automaticReviewer
}

func (s *Service) registerApprovalExplanation(approvalID string, task *approvalExplanationTask) {
	s.explanationMu.Lock()
	previous := s.explanationActive[approvalID]
	s.explanationActive[approvalID] = task
	s.explanationMu.Unlock()
	if previous != nil {
		previous.cancel()
	}
}

func (s *Service) clearApprovalExplanation(approvalID string, task *approvalExplanationTask) {
	s.explanationMu.Lock()
	if s.explanationActive[approvalID] == task {
		delete(s.explanationActive, approvalID)
	}
	s.explanationMu.Unlock()
}

func (s *Service) cancelApprovalExplanation(ctx context.Context, approvalID, runID string) bool {
	s.explanationMu.Lock()
	task := s.explanationActive[approvalID]
	if task != nil {
		delete(s.explanationActive, approvalID)
	}
	s.explanationMu.Unlock()
	if task == nil {
		return false
	}
	task.cancel()
	if runID != "" {
		_ = s.store.UpdateRunAIReview(ctx, runID, "")
	}
	return true
}

func (s *Service) GetApproval(ctx context.Context, approvalID string) (domain.Approval, error) {
	return s.store.GetApproval(ctx, approvalID)
}

func (s *Service) ListDecidedAgentApprovals(ctx context.Context) ([]domain.Approval, error) {
	return s.store.ListDecidedAgentApprovals(ctx)
}

func (s *Service) ListAgentApprovalsByCheckpoint(ctx context.Context, checkpointID string) ([]domain.Approval, error) {
	return s.store.ListAgentApprovalsByCheckpoint(ctx, checkpointID)
}

// ActivateAgentApprovals validates and exposes one complete Eino interrupt
// group. A partial group never becomes visible to the operator.
func (s *Service) ActivateAgentApprovals(ctx context.Context, checkpointID string, interrupts map[string]string) ([]domain.Approval, error) {
	if checkpointID == "" || len(interrupts) == 0 {
		return nil, fmt.Errorf("Agent approval checkpoint and interrupt IDs are required")
	}
	for approvalID, interruptID := range interrupts {
		if approvalID == "" || interruptID == "" {
			return nil, fmt.Errorf("Agent approval checkpoint and interrupt IDs are required")
		}
		approval, err := s.store.GetApproval(ctx, approvalID)
		if err != nil {
			return nil, err
		}
		if approval.ContinuationKind != domain.ApprovalContinuationAgent || approval.CheckpointID != checkpointID {
			return nil, fmt.Errorf("approval is not bound to this Agent continuation")
		}
	}
	if checkpoint, present, err := s.store.Get(ctx, checkpointID); err != nil {
		return nil, err
	} else if !present || len(checkpoint) == 0 {
		return nil, fmt.Errorf("Agent approval checkpoint was not persisted")
	}
	if err := s.store.ActivateAgentApprovals(ctx, checkpointID, interrupts); err != nil {
		return nil, err
	}
	return s.store.ListAgentApprovalsByCheckpoint(ctx, checkpointID)
}

// DecideAgentApproval persists the operator decision but deliberately leaves
// an approved run unclaimed. ResumeAgentApproval is the only execution path.
func (s *Service) DecideAgentApproval(ctx context.Context, approvalID, decision, reason, actor string) (domain.ExecResult, error) {
	approval, err := s.store.GetApproval(ctx, approvalID)
	if err != nil {
		return domain.ExecResult{}, err
	}
	if approval.ContinuationKind != domain.ApprovalContinuationAgent {
		return domain.ExecResult{}, fmt.Errorf("approval does not belong to an Agent continuation")
	}
	if approval.Status != domain.ApprovalStatusPending {
		return domain.ExecResult{}, fmt.Errorf("approval is %s", approval.Status)
	}
	if decision != domain.ApprovalStatusApproved && decision != domain.ApprovalStatusRejected {
		return domain.ExecResult{}, fmt.Errorf("invalid Agent approval decision %q", decision)
	}
	if _, err := s.loadApprovalExecution(ctx, approval); err != nil {
		return domain.ExecResult{}, err
	}
	if err := s.store.DecideAgentApproval(ctx, approval.ID, decision, reason); err != nil {
		return domain.ExecResult{}, err
	}
	s.cancelApprovalExplanation(ctx, approval.ID, approval.RunID)
	run, err := s.store.GetRun(ctx, approval.RunID)
	if err != nil {
		return domain.ExecResult{}, err
	}
	approval.Status, approval.Reason = decision, reason
	eventType := "approval_granted"
	if decision == domain.ApprovalStatusRejected {
		eventType = "approval_rejected"
		s.publishExecutionEvent(ExecutionEvent{SessionID: run.SessionID, RunID: run.ID, Status: run.Status})
	}
	s.audit(ctx, run.ID, eventType, actor, map[string]any{
		"approval_id": approval.ID, "reason": reason, "session_id": approval.SessionID,
	})
	observability.FromContext(ctx).With("component", "approval", "approval_id", approval.ID, "actor", actor).
		InfoContext(ctx, "Agent approval decided", "decision", decision, "run_id", run.ID, "session_id", approval.SessionID)
	result := execResultFromRun(run, approval.ID, "")
	result.Status = decision
	if decision == domain.ApprovalStatusRejected {
		result.OperatorInstruction = reason
	}
	return result, nil
}

// ResumeAgentApproval restores the exact operation selected by the checkpoint.
// The atomic claim prevents duplicate resumes from executing it twice.
func (s *Service) ResumeAgentApproval(ctx context.Context, approvalID string) (domain.ExecResult, error) {
	approval, err := s.store.GetApproval(ctx, approvalID)
	if err != nil {
		return domain.ExecResult{}, err
	}
	if approval.ContinuationKind != domain.ApprovalContinuationAgent {
		return domain.ExecResult{}, fmt.Errorf("approval does not belong to an Agent continuation")
	}
	if approval.Status == domain.ApprovalStatusRejected {
		run, err := s.store.GetRun(ctx, approval.RunID)
		if err != nil {
			return domain.ExecResult{}, err
		}
		return execResultFromRun(run, approval.ID, approval.Reason), nil
	}
	if approval.Status != domain.ApprovalStatusApproved {
		return domain.ExecResult{}, fmt.Errorf("approval is %s", approval.Status)
	}
	approved, err := s.loadApprovalExecution(ctx, approval)
	if err != nil {
		return domain.ExecResult{}, err
	}
	if terminalExecutionStatus(approved.run.Status) {
		return execResultFromRun(approved.run, approval.ID, ""), nil
	}
	approved.actor = "eino-agent"
	if err := s.authorizeApprovedAgentSSHExecution(ctx, approved.actor, approved.host, approved.request); err != nil {
		s.finishApprovedExecutionError(approved, err)
		if run, loadErr := s.store.GetRun(context.WithoutCancel(ctx), approved.run.ID); loadErr == nil {
			return execResultFromRun(run, approval.ID, ""), err
		}
		return domain.ExecResult{}, err
	}
	if err := s.store.ClaimAgentApprovalRun(ctx, approval.ID, approved.run.ID); err != nil {
		return domain.ExecResult{}, err
	}
	approved.run.Status = "running"
	return s.executeApproved(ctx, approved)
}

func (s *Service) loadApprovalExecution(ctx context.Context, approval domain.Approval) (approvedExecution, error) {
	requestData, err := s.encryptor.Decrypt(approval.RequestCipher)
	if err != nil {
		return approvedExecution{}, err
	}
	if len(requestData) == 0 {
		requestData = []byte(approval.RequestJSON)
	}
	var req domain.ExecRequest
	if err := json.Unmarshal(requestData, &req); err != nil {
		return approvedExecution{}, err
	}
	_, digest, err := canonicalRequest(req)
	if err != nil || digest != approval.RequestDigest {
		return approvedExecution{}, fmt.Errorf("approved request digest no longer matches")
	}
	run, err := s.store.GetRun(ctx, approval.RunID)
	if err != nil {
		return approvedExecution{}, err
	}
	host, err := s.store.GetHost(ctx, approval.HostID)
	if err != nil {
		return approvedExecution{}, err
	}
	return approvedExecution{approval: approval, request: req, run: run, host: host}, nil
}

func (s *Service) Approve(ctx context.Context, approvalID, reason, actor string) (domain.ExecResult, error) {
	approved, err := s.approveForExecution(ctx, approvalID, reason, actor)
	if err != nil {
		return domain.ExecResult{}, err
	}
	return s.executeApproved(ctx, approved)
}

func (s *Service) ApproveAsync(ctx context.Context, approvalID, reason, actor string) (domain.ExecResult, error) {
	approved, err := s.approveForExecution(ctx, approvalID, reason, actor)
	if err != nil {
		return domain.ExecResult{}, err
	}
	if err := s.startApprovedExecution(ctx, approved); err != nil {
		s.finishApprovedExecutionError(approved, err)
		return domain.ExecResult{}, err
	}
	return execResultFromRun(approved.run, approved.approval.ID, ""), nil
}

type approvedExecution struct {
	approval domain.Approval
	request  domain.ExecRequest
	run      domain.Run
	host     domain.Host
	actor    string
}

func (s *Service) approveForExecution(ctx context.Context, approvalID, reason, actor string) (approvedExecution, error) {
	logger := observability.FromContext(ctx).With("component", "approval", "approval_id", approvalID, "actor", actor)
	approval, err := s.store.GetApproval(ctx, approvalID)
	if err != nil {
		return approvedExecution{}, err
	}
	if approval.Status != "pending" {
		logger.WarnContext(ctx, "approval decision ignored", "status", approval.Status)
		return approvedExecution{}, fmt.Errorf("approval is %s", approval.Status)
	}
	if approval.ContinuationKind == domain.ApprovalContinuationAgent {
		return approvedExecution{}, fmt.Errorf("Agent approval must resume its Eino checkpoint")
	}
	approved, err := s.loadApprovalExecution(ctx, approval)
	if err != nil {
		return approvedExecution{}, err
	}
	if err := s.store.ApprovePendingAndStartRun(ctx, approval.ID, approved.run.ID, reason); err != nil {
		return approvedExecution{}, err
	}
	s.cancelApprovalExplanation(ctx, approval.ID, approval.RunID)
	approved.run.Status = "running"
	approved.actor = actor
	s.publishExecutionEvent(ExecutionEvent{SessionID: approved.run.SessionID, RunID: approved.run.ID, Status: approved.run.Status})
	s.audit(ctx, approved.run.ID, "approval_granted", actor, map[string]any{"approval_id": approval.ID, "reason": reason, "session_id": approval.SessionID})
	logger.InfoContext(ctx, "approval granted", "run_id", approved.run.ID, "session_id", approval.SessionID)
	return approved, nil
}

func (s *Service) startApprovedExecution(parent context.Context, approved approvedExecution) error {
	executionCtx, cancel := context.WithCancel(context.WithoutCancel(parent))
	s.executionMu.Lock()
	if s.executionClosed {
		s.executionMu.Unlock()
		cancel()
		return fmt.Errorf("service is shutting down")
	}
	if _, cancelled := s.cancelledExecutions[approved.run.ID]; cancelled {
		delete(s.cancelledExecutions, approved.run.ID)
		s.executionMu.Unlock()
		cancel()
		return context.Canceled
	}
	s.executionCancels[approved.run.ID] = cancel
	s.executionWG.Add(1)
	s.executionMu.Unlock()

	stopServiceCancellation := context.AfterFunc(s.executionCtx, cancel)
	go func() {
		defer s.executionWG.Done()
		defer stopServiceCancellation()
		defer cancel()
		defer func() {
			s.executionMu.Lock()
			delete(s.executionCancels, approved.run.ID)
			delete(s.cancelledExecutions, approved.run.ID)
			s.executionMu.Unlock()
		}()
		defer func() {
			if recovered := recover(); recovered != nil {
				err := fmt.Errorf("approved execution stopped unexpectedly")
				observability.FromContext(executionCtx).ErrorContext(executionCtx, "approved execution panicked", "run_id", approved.run.ID, "panic", s.redactor.Redact(fmt.Sprint(recovered)))
				s.finishApprovedExecutionError(approved, err)
			}
		}()
		_, _ = s.executeApproved(executionCtx, approved)
	}()
	return nil
}

func (s *Service) cancelApprovedExecution(runID string) bool {
	if runID == "" {
		return false
	}
	s.executionMu.Lock()
	cancel := s.executionCancels[runID]
	if cancel == nil {
		s.cancelledExecutions[runID] = struct{}{}
	}
	s.executionMu.Unlock()
	if cancel != nil {
		cancel()
	}
	return true
}

func (s *Service) executeApproved(ctx context.Context, approved approvedExecution) (domain.ExecResult, error) {
	result, err := s.execute(ctx, approved.host, approved.request, approved.run, approved.actor, nil)
	if err == nil {
		return result, nil
	}
	s.finishApprovedExecutionError(approved, err)
	if result.RunID != "" {
		return result, err
	}
	run, loadErr := s.store.GetRun(context.WithoutCancel(ctx), approved.run.ID)
	if loadErr == nil {
		result = execResultFromRun(run, approved.approval.ID, "")
	}
	return result, err
}

func (s *Service) finishApprovedExecutionError(approved approvedExecution, cause error) {
	defer s.clearExecutionOwner(approved.run.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	run, err := s.store.GetRun(ctx, approved.run.ID)
	if err != nil || (run.Status != "created" && run.Status != "approval_required" && run.Status != "running") {
		return
	}
	run.Status = "failed"
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		run.Status = "interrupted"
	}
	run.Error = s.redactor.Redact(cause.Error())
	run.CompletedAt = time.Now().UTC()
	if err := s.store.UpdateRun(ctx, run); err != nil {
		observability.FromContext(ctx).ErrorContext(ctx, "persist approved execution failure failed", "run_id", run.ID, "error", err)
		return
	}
	s.publishExecutionEvent(ExecutionEvent{SessionID: run.SessionID, RunID: run.ID, Status: run.Status})
	s.audit(ctx, run.ID, "command_completed", approved.actor, map[string]any{"status": run.Status, "error": run.Error})
	observability.FromContext(ctx).ErrorContext(ctx, "approved execution stopped before completion", "run_id", run.ID, "status", run.Status, "error", run.Error)
}

func (s *Service) Reject(ctx context.Context, approvalID, reason, actor string) error {
	logger := observability.FromContext(ctx).With("component", "approval", "approval_id", approvalID, "actor", actor)
	approval, err := s.store.GetApproval(ctx, approvalID)
	if err != nil {
		return err
	}
	if approval.ContinuationKind == domain.ApprovalContinuationAgent {
		_, err := s.DecideAgentApproval(ctx, approval.ID, domain.ApprovalStatusRejected, reason, actor)
		return err
	}
	if err := s.store.RejectPendingApprovalAndRun(ctx, approval.ID, approval.RunID, reason); err != nil {
		return err
	}
	s.cancelApprovalExplanation(ctx, approval.ID, approval.RunID)
	run, err := s.store.GetRun(ctx, approval.RunID)
	if err != nil {
		return err
	}
	s.publishExecutionEvent(ExecutionEvent{SessionID: run.SessionID, RunID: run.ID, Status: run.Status})
	s.audit(ctx, run.ID, "approval_rejected", actor, map[string]any{"approval_id": approval.ID, "reason": reason})
	logger.InfoContext(ctx, "approval rejected", "run_id", run.ID, "session_id", approval.SessionID)
	return nil
}

// AbortApprovalsForSession stops every approval that still belongs to a live
// execution path for the session.
func (s *Service) AbortApprovalsForSession(ctx context.Context, sessionID, reason, actor string) (int, error) {
	abortedAgent, err := s.store.AbortAgentApprovalsForSession(ctx, sessionID, reason)
	if err != nil {
		return 0, err
	}
	for _, approval := range abortedAgent {
		s.cancelApprovalExplanation(ctx, approval.ID, approval.RunID)
		s.publishExecutionEvent(ExecutionEvent{SessionID: approval.SessionID, RunID: approval.RunID, Status: domain.ApprovalStatusRejected})
		s.audit(ctx, approval.RunID, "approval_rejected", actor, map[string]any{"approval_id": approval.ID, "reason": reason})
	}
	approvals, err := s.store.ListPendingApprovalsForSession(ctx, sessionID)
	if err != nil {
		return len(abortedAgent), err
	}
	rejected := len(abortedAgent)
	for _, approval := range approvals {
		if err := s.Reject(ctx, approval.ID, reason, actor); err != nil {
			current, getErr := s.store.GetApproval(ctx, approval.ID)
			if getErr == nil && current.Status != "pending" {
				continue
			}
			return rejected, err
		}
		rejected++
	}
	return rejected, nil
}

func (s *Service) commandReviewInput(ctx context.Context, req domain.ExecRequest, host domain.Host, digest, sessionID string) domain.CommandReviewInput {
	currentTask := ""
	if sessionID != "" {
		if tasks, err := s.store.ListAgentTasks(ctx, sessionID); err == nil {
			currentTask = currentAgentTask(tasks)
		}
	}
	return domain.CommandReviewInput{
		Request:       req,
		Host:          hostCapability(host),
		CurrentTask:   currentTask,
		RequestDigest: digest,
	}
}

func (s *Service) automaticApprovalInput(ctx context.Context, req domain.ExecRequest, host domain.Host, digest, sessionID string) domain.AutomaticApprovalInput {
	input := domain.AutomaticApprovalInput{
		Request: req, Host: hostCapability(host),
		UserRequest: s.redactor.Redact(approvalUserRequestFromContext(ctx)), RequestDigest: digest,
	}
	if sessionID == "" {
		return input
	}
	if tasks, err := s.store.ListAgentTasks(ctx, sessionID); err == nil {
		input.CurrentTask = s.redactor.Redact(currentAgentTask(tasks))
	}
	return input
}

func (s *Service) reviewForAutomaticApproval(ctx context.Context, reviewer AutomaticApprovalReviewer, input domain.AutomaticApprovalInput, timeoutSeconds int) domain.CommandReview {
	if strings.TrimSpace(input.UserRequest) == "" {
		return markAutomaticApprovalReview(s.normalizeCommandReview(domain.CommandReview{}, fmt.Errorf("current user request is unavailable for Auto approval"), timeoutSeconds))
	}
	if reviewer == nil {
		return markAutomaticApprovalReview(s.normalizeCommandReview(domain.CommandReview{}, fmt.Errorf("Auto approval Agent is unavailable for the active model"), timeoutSeconds))
	}
	timeoutSeconds = effectiveSubagentTimeoutSeconds(timeoutSeconds)
	reviewCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()
	select {
	case s.automaticApprovalSem <- struct{}{}:
		defer func() { <-s.automaticApprovalSem }()
	case <-reviewCtx.Done():
		return markAutomaticApprovalReview(s.normalizeCommandReview(domain.CommandReview{}, reviewCtx.Err(), timeoutSeconds))
	}
	review, err := reviewer.Review(reviewCtx, input)
	review = s.normalizeCommandReview(review, err, timeoutSeconds)
	if review.Status == "completed" && review.Decision != domain.ApprovalAgentAllow && review.Decision != domain.ApprovalAgentReject && review.Decision != domain.ApprovalAgentManual {
		review.Status = "degraded"
		review.Errors = append(review.Errors, "Auto approval Agent returned an invalid decision")
	}
	if review.Status == "completed" && strings.TrimSpace(review.Reason) == "" {
		review.Status = "degraded"
		review.Errors = append(review.Errors, "Auto approval Agent returned no reason")
	}
	if review.Status == "completed" && (review.Explanation == nil || strings.TrimSpace(review.Explanation.Summary) == "" || strings.TrimSpace(review.Explanation.Mechanism) == "") {
		review.Status = "degraded"
		review.Errors = append(review.Errors, "Auto approval Agent returned no operation explanation")
	}
	return markAutomaticApprovalReview(review)
}

func markAutomaticApprovalReview(review domain.CommandReview) domain.CommandReview {
	review.Kind = domain.CommandReviewKindAutomaticApproval
	return review
}

// startPendingApprovalExplanation keeps model latency outside the human
// approval response path. Explanation work is bounded globally and canceled as
// soon as its approval is no longer pending.
func (s *Service) startPendingApprovalExplanation(parent context.Context, approval domain.Approval, input domain.CommandReviewInput, reviewer ApprovalReviewer, timeoutSeconds int) {
	baseCtx := context.WithoutCancel(parent)
	timeoutSeconds = effectiveSubagentTimeoutSeconds(timeoutSeconds)
	logger := observability.FromContext(baseCtx).With(
		"component", "approval", "approval_id", approval.ID, "run_id", approval.RunID,
	)
	select {
	case s.explanationSlots <- struct{}{}:
	default:
		review := domain.CommandReview{
			Status: "unavailable",
			Errors: []string{"approval Agent review skipped because the local queue is full"}, ReviewedAt: time.Now().UTC(),
		}
		persistCtx, cancelPersist := context.WithTimeout(baseCtx, 3*time.Second)
		err := s.persistPendingApprovalExplanation(persistCtx, approval, review, 0)
		cancelPersist()
		if err != nil {
			logger.ErrorContext(baseCtx, "persist skipped approval explanation failed", "error", err)
		} else {
			logger.WarnContext(baseCtx, "approval explanation skipped", "reason", "queue_full")
		}
		return
	}

	queuedAt := time.Now()
	explanationCtx, cancelExplanation := context.WithTimeout(baseCtx, time.Duration(timeoutSeconds)*time.Second)
	task := &approvalExplanationTask{cancel: cancelExplanation}
	s.registerApprovalExplanation(approval.ID, task)
	s.explainWG.Add(1)
	go func() {
		defer s.explainWG.Done()
		defer func() { <-s.explanationSlots }()
		defer cancelExplanation()
		defer s.clearApprovalExplanation(approval.ID, task)
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.ErrorContext(baseCtx, "approval Agent panicked", "panic", fmt.Sprint(recovered))
			}
		}()

		select {
		case s.explanationSem <- struct{}{}:
			defer func() { <-s.explanationSem }()
		case <-explanationCtx.Done():
			if errors.Is(explanationCtx.Err(), context.Canceled) {
				logger.InfoContext(baseCtx, "approval explanation canceled while queued", "queue_ms", time.Since(queuedAt).Milliseconds())
				return
			}
			review := s.normalizeCommandReview(domain.CommandReview{}, explanationCtx.Err(), timeoutSeconds)
			persistCtx, cancelPersist := context.WithTimeout(baseCtx, 3*time.Second)
			err := s.persistPendingApprovalExplanation(persistCtx, approval, review, time.Since(queuedAt))
			cancelPersist()
			if err != nil {
				logger.ErrorContext(baseCtx, "persist queued approval explanation timeout failed", "error", err)
			}
			return
		}

		started := time.Now()
		logger.InfoContext(baseCtx, "approval explanation started", "queue_ms", started.Sub(queuedAt).Milliseconds())
		review, reviewErr := reviewer.Review(explanationCtx, input)
		if errors.Is(explanationCtx.Err(), context.Canceled) {
			logger.InfoContext(baseCtx, "approval explanation canceled", "duration_ms", time.Since(started).Milliseconds())
			return
		}
		review = s.normalizeCommandReview(review, reviewErr, timeoutSeconds)
		persistCtx, cancelPersist := context.WithTimeout(baseCtx, 3*time.Second)
		err := s.persistPendingApprovalExplanation(persistCtx, approval, review, time.Since(started))
		cancelPersist()
		if err != nil {
			current, getErr := s.store.GetApproval(baseCtx, approval.ID)
			if getErr == nil && current.Status != "pending" {
				logger.InfoContext(baseCtx, "approval explanation discarded after decision", "status", current.Status, "duration_ms", time.Since(started).Milliseconds())
				return
			}
			logger.ErrorContext(baseCtx, "persist approval explanation failed", "error", err, "duration_ms", time.Since(started).Milliseconds())
			return
		}
		logger.InfoContext(baseCtx, "approval explanation completed", "status", review.Status, "duration_ms", time.Since(started).Milliseconds())
	}()
}

func (s *Service) persistPendingApprovalExplanation(ctx context.Context, approval domain.Approval, review domain.CommandReview, duration time.Duration) error {
	reviewJSON, err := json.Marshal(review)
	if err != nil {
		return fmt.Errorf("encode approval explanation: %w", err)
	}
	if err := s.store.UpdatePendingApprovalExplanation(ctx, approval.ID, approval.RunID, string(reviewJSON)); err != nil {
		return err
	}
	s.audit(ctx, approval.RunID, "approval_agent_reviewed", "approval-agent", map[string]any{
		"approval_id": approval.ID, "status": review.Status,
		"model": review.Model, "duration_ms": duration.Milliseconds(),
	})
	return nil
}

func (s *Service) normalizeCommandReview(review domain.CommandReview, reviewErr error, timeoutSeconds int) domain.CommandReview {
	if reviewErr != nil {
		model := review.Model
		message := reviewErr.Error()
		if errors.Is(reviewErr, context.DeadlineExceeded) || strings.Contains(strings.ToLower(message), "context deadline exceeded") {
			message = fmt.Sprintf("approval Agent did not respond within %d seconds", effectiveSubagentTimeoutSeconds(timeoutSeconds))
		}
		review = domain.CommandReview{
			Status: "unavailable", Model: model,
			Errors: []string{message}, ReviewedAt: time.Now().UTC(),
		}
	}
	if review.Status != "completed" && review.Status != "degraded" && review.Status != "unavailable" {
		review.Status = "degraded"
	}
	if review.ReviewedAt.IsZero() {
		review.ReviewedAt = time.Now().UTC()
	}
	review.Decision = strings.ToLower(strings.TrimSpace(review.Decision))
	review.Reason = s.redactor.Redact(strings.TrimSpace(review.Reason))
	if len(review.Reason) > 1000 {
		review.Reason = review.Reason[:1000]
	}
	if review.Explanation != nil {
		review.Explanation.Summary = s.redactor.Redact(review.Explanation.Summary)
		review.Explanation.Mechanism = s.redactor.Redact(review.Explanation.Mechanism)
		for index := range review.Explanation.Risks {
			review.Explanation.Risks[index] = s.redactor.Redact(review.Explanation.Risks[index])
		}
	}
	if len(review.Errors) > 5 {
		review.Errors = review.Errors[:5]
	}
	for index := range review.Errors {
		review.Errors[index] = s.redactor.Redact(review.Errors[index])
		if len(review.Errors[index]) > 800 {
			review.Errors[index] = review.Errors[index][:800]
		}
	}
	return review
}

func effectiveSubagentTimeoutSeconds(timeoutSeconds int) int {
	if timeoutSeconds < domain.MinSubagentTimeoutSeconds || timeoutSeconds > domain.MaxSubagentTimeoutSeconds {
		return domain.DefaultSubagentTimeoutSeconds
	}
	return timeoutSeconds
}

// RetryApprovalExplanation reruns the tool-free command explainer for an
// existing pending approval. It never decides the approval or
// executes the operation.
func (s *Service) RetryApprovalExplanation(ctx context.Context, approvalID, actor string) (domain.Approval, error) {
	logger := observability.FromContext(ctx).With("component", "approval", "approval_id", approvalID, "actor", actor)
	approval, err := s.store.GetApproval(ctx, approvalID)
	if err != nil {
		return domain.Approval{}, err
	}
	if approval.Status != "pending" {
		return domain.Approval{}, fmt.Errorf("approval is %s", approval.Status)
	}
	settings, err := s.store.GetSystemSettings(ctx)
	if err != nil {
		return domain.Approval{}, err
	}
	if !settings.ApprovalExplanationsEnabled {
		return domain.Approval{}, fmt.Errorf("approval explanations are disabled in system settings")
	}
	reviewer := s.approvalReviewer()
	if reviewer == nil {
		return domain.Approval{}, fmt.Errorf("approval Agent is unavailable for the active model")
	}

	requestData, err := s.encryptor.Decrypt(approval.RequestCipher)
	if err != nil {
		return domain.Approval{}, err
	}
	if len(requestData) == 0 {
		requestData = []byte(approval.RequestJSON)
	}
	var req domain.ExecRequest
	if err := json.Unmarshal(requestData, &req); err != nil {
		return domain.Approval{}, err
	}
	_, digest, err := canonicalRequest(req)
	if err != nil || digest != approval.RequestDigest {
		return domain.Approval{}, fmt.Errorf("approval request digest no longer matches")
	}
	run, err := s.store.GetRun(ctx, approval.RunID)
	if err != nil {
		return domain.Approval{}, err
	}
	host, err := s.store.GetHost(ctx, approval.HostID)
	if err != nil {
		return domain.Approval{}, err
	}

	currentTask := ""
	if approval.SessionID != "" {
		if tasks, taskErr := s.store.ListAgentTasks(ctx, approval.SessionID); taskErr == nil {
			currentTask = currentAgentTask(tasks)
		}
	}
	input := domain.CommandReviewInput{
		Request: req, Host: hostCapability(host),
		CurrentTask: currentTask, RequestDigest: digest,
	}

	retryCtx, cancelRetry := context.WithCancel(ctx)
	task := &approvalExplanationTask{cancel: cancelRetry}
	s.registerApprovalExplanation(approval.ID, task)
	defer cancelRetry()
	defer s.clearApprovalExplanation(approval.ID, task)

	// Close the decision race between the initial read and task registration.
	current, err := s.store.GetApproval(retryCtx, approval.ID)
	if err != nil {
		return domain.Approval{}, err
	}
	if current.Status != "pending" {
		return domain.Approval{}, fmt.Errorf("approval is %s", current.Status)
	}
	if err := s.store.UpdateRunAIReview(retryCtx, run.ID, ""); err != nil {
		return domain.Approval{}, err
	}

	logger.InfoContext(ctx, "approval explanation retry started", "run_id", run.ID)
	started := time.Now()
	timeoutSeconds := effectiveSubagentTimeoutSeconds(settings.SubagentTimeoutSeconds)
	explanationCtx, cancel := context.WithTimeout(retryCtx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()
	select {
	case s.explanationSem <- struct{}{}:
		defer func() { <-s.explanationSem }()
	case <-explanationCtx.Done():
		return domain.Approval{}, explanationCtx.Err()
	}
	var review domain.CommandReview
	var reviewErr error
	if freshReviewer, ok := reviewer.(FreshApprovalReviewer); ok {
		review, reviewErr = freshReviewer.ReviewFresh(explanationCtx, input)
	} else {
		review, reviewErr = reviewer.Review(explanationCtx, input)
	}
	cancel()
	if retryCtx.Err() != nil {
		return domain.Approval{}, retryCtx.Err()
	}
	review = s.normalizeCommandReview(review, reviewErr, timeoutSeconds)
	reviewJSON, err := json.Marshal(review)
	if err != nil {
		return domain.Approval{}, err
	}
	if err := s.store.UpdatePendingApprovalExplanation(retryCtx, approval.ID, run.ID, string(reviewJSON)); err != nil {
		return domain.Approval{}, err
	}
	s.audit(ctx, run.ID, "command_ai_explanation_retried", actor, map[string]any{
		"approval_id": approval.ID, "status": review.Status,
		"model": review.Model, "duration_ms": time.Since(started).Milliseconds(),
	})
	logger.InfoContext(ctx, "approval explanation retry completed", "run_id", run.ID, "status", review.Status,
		"duration_ms", time.Since(started).Milliseconds())

	approval.RequestJSON = string(requestData)
	approval.AIReview = &review
	return approval, nil
}

func (s *Service) ListApprovals(ctx context.Context, status string, limit int) ([]domain.Approval, error) {
	approvals, err := s.store.ListApprovals(ctx, status, limit)
	if err != nil {
		return nil, err
	}
	for index := range approvals {
		plain, decryptErr := s.encryptor.Decrypt(approvals[index].RequestCipher)
		if decryptErr != nil {
			return nil, decryptErr
		}
		if len(plain) > 0 {
			approvals[index].RequestJSON = string(plain)
		}
	}
	return approvals, nil
}
