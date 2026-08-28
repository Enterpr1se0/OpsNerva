package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func (s *Store) CreateApproval(ctx context.Context, approval domain.Approval) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO approvals(id,run_id,host_id,request_json,request_cipher,request_digest,
status,reason,continuation_kind,checkpoint_id,interrupt_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, approval.ID, approval.RunID,
		approval.HostID, approval.RequestJSON, approval.RequestCipher, approval.RequestDigest, approval.Status,
		approval.Reason, approval.ContinuationKind, approval.CheckpointID, approval.InterruptID, formatTime(approval.CreatedAt))
	if err == nil {
		s.publishChange(Change{Topic: ChangeApprovals, SessionID: approval.SessionID})
	}
	return err
}

func (s *Store) GetApproval(ctx context.Context, id string) (domain.Approval, error) {
	row := s.db.QueryRowContext(ctx, `SELECT approvals.id,approvals.run_id,runs.session_id,approvals.host_id,approvals.request_json,
approvals.request_cipher,approvals.request_digest,approvals.status,approvals.reason,
approvals.continuation_kind,approvals.checkpoint_id,approvals.interrupt_id,
approvals.created_at,approvals.decided_at FROM approvals
JOIN runs ON runs.id=approvals.run_id WHERE approvals.id=?`, id)
	return scanApproval(row)
}

func (s *Store) ListApprovals(ctx context.Context, status string, limit int) ([]domain.Approval, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	statement := `SELECT approvals.id,approvals.run_id,runs.session_id,approvals.host_id,
approvals.request_json,approvals.request_cipher,approvals.request_digest,approvals.status,
approvals.reason,approvals.continuation_kind,approvals.checkpoint_id,approvals.interrupt_id,
approvals.created_at,approvals.decided_at,runs.ai_review_json FROM approvals
JOIN runs ON runs.id=approvals.run_id`
	arguments := make([]any, 0, 2)
	if status != "" {
		statement += " WHERE approvals.status=?"
		arguments = append(arguments, status)
	}
	statement += " ORDER BY approvals.created_at DESC LIMIT ?"
	arguments = append(arguments, limit)
	rows, err := s.db.QueryContext(ctx, statement, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.Approval, 0)
	for rows.Next() {
		approval, err := scanApprovalWithReview(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, approval)
	}
	return result, rows.Err()
}

func (s *Store) ListPendingApprovalsForSession(ctx context.Context, sessionID string) ([]domain.Approval, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT approvals.id,approvals.run_id,runs.session_id,approvals.host_id,
approvals.request_json,approvals.request_cipher,approvals.request_digest,approvals.status,
approvals.reason,approvals.continuation_kind,approvals.checkpoint_id,approvals.interrupt_id,
approvals.created_at,approvals.decided_at FROM approvals
JOIN runs ON runs.id=approvals.run_id WHERE runs.session_id=? AND approvals.status='pending'
ORDER BY approvals.created_at`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.Approval, 0)
	for rows.Next() {
		approval, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, approval)
	}
	return result, rows.Err()
}

func (s *Store) ListAgentApprovalsByCheckpoint(ctx context.Context, checkpointID string) ([]domain.Approval, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT approvals.id,approvals.run_id,runs.session_id,approvals.host_id,
approvals.request_json,approvals.request_cipher,approvals.request_digest,approvals.status,
approvals.reason,approvals.continuation_kind,approvals.checkpoint_id,approvals.interrupt_id,
approvals.created_at,approvals.decided_at FROM approvals
JOIN runs ON runs.id=approvals.run_id
WHERE approvals.continuation_kind=? AND approvals.checkpoint_id=?
ORDER BY approvals.created_at,approvals.id`, domain.ApprovalContinuationAgent, checkpointID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []domain.Approval
	for rows.Next() {
		approval, scanErr := scanApproval(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, approval)
	}
	return result, rows.Err()
}

// ListDecidedAgentApprovals returns complete decision groups that still need
// to be fed back into Eino. Deleting the checkpoint is the completion marker.
func (s *Store) ListDecidedAgentApprovals(ctx context.Context) ([]domain.Approval, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT approvals.id,approvals.run_id,runs.session_id,approvals.host_id,
approvals.request_json,approvals.request_cipher,approvals.request_digest,approvals.status,
approvals.reason,approvals.continuation_kind,approvals.checkpoint_id,approvals.interrupt_id,
approvals.created_at,approvals.decided_at FROM approvals
JOIN runs ON runs.id=approvals.run_id
JOIN checkpoints ON checkpoints.id=approvals.checkpoint_id
WHERE approvals.continuation_kind=? AND approvals.status IN (?,?)
AND approvals.checkpoint_id<>'' AND approvals.interrupt_id<>''
AND NOT EXISTS (
  SELECT 1 FROM approvals AS waiting
  WHERE waiting.continuation_kind=approvals.continuation_kind
    AND waiting.checkpoint_id=approvals.checkpoint_id
    AND waiting.interrupt_id<>'' AND waiting.status=?
)
ORDER BY approvals.decided_at,approvals.created_at`, domain.ApprovalContinuationAgent,
		domain.ApprovalStatusApproved, domain.ApprovalStatusRejected, domain.ApprovalStatusPending)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []domain.Approval
	for rows.Next() {
		approval, scanErr := scanApproval(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, approval)
	}
	return result, rows.Err()
}

// AbortUnactivatedAgentApprovals reconciles the only unsafe crash window:
// the operation requested approval but Eino never exposed an interrupt target
// that can be resumed.
func (s *Store) AbortUnactivatedAgentApprovals(ctx context.Context, reason string) error {
	now := formatTime(time.Now().UTC())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM checkpoints WHERE id IN (
SELECT checkpoint_id FROM approvals WHERE continuation_kind=? AND status=? AND checkpoint_id<>''
)`, domain.ApprovalContinuationAgent, domain.ApprovalStatusPreparing); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET status='interrupted',error=?,completed_at=? WHERE id IN (
SELECT run_id FROM approvals WHERE continuation_kind=? AND status=?
) AND status='approval_required'`, reason, now, domain.ApprovalContinuationAgent, domain.ApprovalStatusPreparing); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE approvals SET status=?,reason=?,decided_at=?
WHERE continuation_kind=? AND status=?`, domain.ApprovalStatusRejected, reason, now,
		domain.ApprovalContinuationAgent, domain.ApprovalStatusPreparing); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.publishChange(Change{Topic: ChangeApprovals})
	return nil
}

// ActivateAgentApprovals atomically exposes a checkpoint's complete interrupt
// group only after Eino has durably saved it.
func (s *Store) ActivateAgentApprovals(ctx context.Context, checkpointID string, interrupts map[string]string) error {
	if len(interrupts) == 0 {
		return fmt.Errorf("Agent approval interrupt group is empty")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE approvals SET interrupt_id=''
WHERE continuation_kind=? AND checkpoint_id=?`, domain.ApprovalContinuationAgent, checkpointID); err != nil {
		return err
	}
	for id, interruptID := range interrupts {
		result, execErr := tx.ExecContext(ctx, `UPDATE approvals SET
status=CASE WHEN status=? THEN ? ELSE status END,interrupt_id=?
WHERE id=? AND continuation_kind=? AND checkpoint_id=? AND status IN (?,?,?,?)`,
			domain.ApprovalStatusPreparing, domain.ApprovalStatusPending, interruptID,
			id, domain.ApprovalContinuationAgent, checkpointID,
			domain.ApprovalStatusPreparing, domain.ApprovalStatusPending,
			domain.ApprovalStatusApproved, domain.ApprovalStatusRejected)
		if execErr != nil {
			return execErr
		}
		if count, _ := result.RowsAffected(); count == 0 {
			return fmt.Errorf("Agent approval %q changed or is no longer resumable", id)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.publishChange(Change{Topic: ChangeApprovals})
	return nil
}

// DecideAgentApproval records the operator decision without starting an
// approved run. Eino's resumed tool is the sole execution owner.
func (s *Store) DecideAgentApproval(ctx context.Context, id, status, reason string) error {
	if status != domain.ApprovalStatusApproved && status != domain.ApprovalStatusRejected {
		return fmt.Errorf("invalid Agent approval decision %q", status)
	}
	now := formatTime(time.Now().UTC())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE approvals SET status=?,reason=?,decided_at=?
WHERE id=? AND continuation_kind=? AND status=? AND checkpoint_id<>'' AND interrupt_id<>''`,
		status, reason, now, id, domain.ApprovalContinuationAgent, domain.ApprovalStatusPending)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return fmt.Errorf("Agent approval changed or is no longer pending")
	}
	if status == domain.ApprovalStatusRejected {
		result, err = tx.ExecContext(ctx, `UPDATE runs SET status='rejected',error=?,completed_at=?
WHERE id=(SELECT run_id FROM approvals WHERE id=?) AND status='approval_required'`, reason, now, id)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count == 0 {
			return fmt.Errorf("Agent approval run changed or is no longer awaiting approval")
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.publishChange(Change{Topic: ChangeApprovals})
	return nil
}

// ClaimAgentApprovalRun atomically transfers execution ownership to the
// resumed Eino tool. A duplicate resume cannot execute the operation twice.
func (s *Store) ClaimAgentApprovalRun(ctx context.Context, id, runID string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE runs SET status='running'
WHERE id=? AND status='approval_required' AND EXISTS (
  SELECT 1 FROM approvals WHERE approvals.id=? AND approvals.run_id=runs.id
    AND approvals.continuation_kind=? AND approvals.status=?
)`, runID, id, domain.ApprovalContinuationAgent, domain.ApprovalStatusApproved)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return fmt.Errorf("Agent approval run was already claimed or is not approved")
	}
	return nil
}

func (s *Store) RejectPendingApprovalAndRun(ctx context.Context, id, runID, reason string) error {
	now := formatTime(time.Now().UTC())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE approvals SET status=?,reason=?,decided_at=?
WHERE id=? AND run_id=? AND status=?`, domain.ApprovalStatusRejected, reason, now, id, runID, domain.ApprovalStatusPending)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return fmt.Errorf("approval changed or is no longer pending")
	}
	result, err = tx.ExecContext(ctx, `UPDATE runs SET status='rejected',error=?,completed_at=? WHERE id=? AND status='approval_required'`, reason, now, runID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return fmt.Errorf("approval run changed or is no longer awaiting approval")
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.publishChange(Change{Topic: ChangeApprovals})
	return nil
}

// AbortAgentApprovalsForSession atomically rejects every unclaimed approval in
// the session's active Agent continuation, including decisions that were
// approved while the rest of their checkpoint group was still waiting.
func (s *Store) AbortAgentApprovalsForSession(ctx context.Context, sessionID, reason string) ([]domain.Approval, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT approvals.id,approvals.run_id,runs.session_id,approvals.host_id,
approvals.request_json,approvals.request_cipher,approvals.request_digest,approvals.status,
approvals.reason,approvals.continuation_kind,approvals.checkpoint_id,approvals.interrupt_id,
approvals.created_at,approvals.decided_at FROM approvals
JOIN runs ON runs.id=approvals.run_id
WHERE runs.session_id=? AND runs.status='approval_required'
AND approvals.continuation_kind=? AND approvals.status IN (?,?)
ORDER BY approvals.created_at,approvals.id`, sessionID, domain.ApprovalContinuationAgent,
		domain.ApprovalStatusPending, domain.ApprovalStatusApproved)
	if err != nil {
		return nil, err
	}
	var approvals []domain.Approval
	for rows.Next() {
		approval, scanErr := scanApproval(rows)
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		approvals = append(approvals, approval)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	decidedAt := time.Now().UTC()
	now := formatTime(decidedAt)
	for index := range approvals {
		approval := &approvals[index]
		result, err := tx.ExecContext(ctx, `UPDATE approvals SET status=?,reason=?,decided_at=?
WHERE id=? AND status IN (?,?)`, domain.ApprovalStatusRejected, reason, now, approval.ID,
			domain.ApprovalStatusPending, domain.ApprovalStatusApproved)
		if err != nil {
			return nil, err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return nil, fmt.Errorf("Agent approval %q changed while it was being stopped", approval.ID)
		}
		result, err = tx.ExecContext(ctx, `UPDATE runs SET status='rejected',error=?,completed_at=?
WHERE id=? AND status='approval_required'`, reason, now, approval.RunID)
		if err != nil {
			return nil, err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return nil, fmt.Errorf("Agent approval run %q changed while it was being stopped", approval.RunID)
		}
		approval.Status = domain.ApprovalStatusRejected
		approval.Reason = reason
		approval.DecidedAt = decidedAt
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if len(approvals) > 0 {
		s.publishChange(Change{Topic: ChangeApprovals, SessionID: sessionID})
	}
	return approvals, nil
}

func (s *Store) ApprovePendingAndStartRun(ctx context.Context, id, runID, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE approvals SET status='approved',reason=?,decided_at=?
WHERE id=? AND run_id=? AND status='pending'`, reason, formatTime(time.Now().UTC()), id, runID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return fmt.Errorf("approval changed or is no longer pending; refresh and review it again")
	}
	result, err = tx.ExecContext(ctx, `UPDATE runs SET status='running' WHERE id=? AND status='approval_required'`, runID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return fmt.Errorf("approval run changed or is no longer awaiting approval")
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.publishChange(Change{Topic: ChangeApprovals})
	return nil
}

func (s *Store) UpdatePendingApprovalExplanation(ctx context.Context, approvalID, runID, reviewJSON string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE runs SET ai_review_json=?
WHERE id=? AND status='approval_required' AND EXISTS (
  SELECT 1 FROM approvals WHERE approvals.id=? AND approvals.run_id=runs.id AND approvals.status IN ('preparing','pending')
)`, reviewJSON, runID, approvalID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return fmt.Errorf("approval is no longer pending")
	}
	s.publishChange(Change{Topic: ChangeApprovals})
	return nil
}

func (s *Store) UpdateRunAIReview(ctx context.Context, runID, reviewJSON string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE runs SET ai_review_json=? WHERE id=?`, reviewJSON, runID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return ErrNotFound
	}
	s.publishChange(Change{Topic: ChangeApprovals})
	return nil
}

func scanApprovalWithReview(row scanner) (domain.Approval, error) {
	var approval domain.Approval
	var created string
	var decided sql.NullString
	var reviewJSON string
	err := row.Scan(&approval.ID, &approval.RunID, &approval.SessionID, &approval.HostID, &approval.RequestJSON,
		&approval.RequestCipher, &approval.RequestDigest, &approval.Status, &approval.Reason,
		&approval.ContinuationKind, &approval.CheckpointID, &approval.InterruptID, &created, &decided, &reviewJSON)
	if err != nil {
		return domain.Approval{}, err
	}
	approval.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	if decided.Valid {
		approval.DecidedAt, _ = time.Parse(time.RFC3339Nano, decided.String)
	}
	if reviewJSON != "" {
		var review domain.CommandReview
		if json.Unmarshal([]byte(reviewJSON), &review) == nil {
			approval.AIReview = &review
		}
	}
	return approval, nil
}

func scanApproval(row scanner) (domain.Approval, error) {
	var approval domain.Approval
	var created string
	var decided sql.NullString
	err := row.Scan(&approval.ID, &approval.RunID, &approval.SessionID, &approval.HostID, &approval.RequestJSON, &approval.RequestCipher,
		&approval.RequestDigest, &approval.Status, &approval.Reason, &approval.ContinuationKind, &approval.CheckpointID, &approval.InterruptID,
		&created, &decided)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Approval{}, ErrNotFound
	}
	if err != nil {
		return domain.Approval{}, err
	}
	approval.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	if decided.Valid {
		approval.DecidedAt, _ = time.Parse(time.RFC3339Nano, decided.String)
	}
	return approval, nil
}
