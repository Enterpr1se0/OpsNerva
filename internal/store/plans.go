package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func readAgentPlan(ctx context.Context, reader *sql.Tx, sessionID string) (domain.AgentPlan, error) {
	var plan domain.AgentPlan
	var created, updated string
	err := reader.QueryRowContext(ctx, `SELECT session_id,goal,status,created_at,updated_at FROM agent_plans WHERE session_id=?`, sessionID).
		Scan(&plan.SessionID, &plan.Goal, &plan.Status, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return plan, ErrNotFound
	}
	if err != nil {
		return plan, err
	}
	plan.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	plan.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	rows, err := reader.QueryContext(ctx, `SELECT step_number,title,description,status,updated_at FROM agent_plan_steps WHERE session_id=? ORDER BY step_number`, sessionID)
	if err != nil {
		return plan, err
	}
	defer rows.Close()
	plan.Steps = make([]domain.AgentPlanStep, 0)
	for rows.Next() {
		var step domain.AgentPlanStep
		var stamp string
		if err := rows.Scan(&step.Number, &step.Title, &step.Description, &step.Status, &stamp); err != nil {
			return plan, err
		}
		step.UpdatedAt, _ = time.Parse(time.RFC3339Nano, stamp)
		plan.Steps = append(plan.Steps, step)
	}
	return plan, rows.Err()
}

func writeAgentPlan(ctx context.Context, tx *sql.Tx, plan domain.AgentPlan) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_plans(session_id,goal,status,created_at,updated_at) VALUES(?,?,?,?,?)
ON CONFLICT(session_id) DO UPDATE SET goal=excluded.goal,status=excluded.status,created_at=excluded.created_at,updated_at=excluded.updated_at`,
		plan.SessionID, plan.Goal, plan.Status, formatTime(plan.CreatedAt), formatTime(plan.UpdatedAt)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_plan_steps WHERE session_id=?`, plan.SessionID); err != nil {
		return err
	}
	for _, step := range plan.Steps {
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_plan_steps(session_id,step_number,title,description,status,updated_at) VALUES(?,?,?,?,?,?)`,
			plan.SessionID, step.Number, step.Title, step.Description, step.Status, formatTime(step.UpdatedAt)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) GetAgentPlan(ctx context.Context, sessionID string) (domain.AgentPlan, error) {
	// The header and steps must come from the same committed revision.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.AgentPlan{}, err
	}
	defer tx.Rollback()
	return readAgentPlan(ctx, tx, sessionID)
}

func (s *Store) ReplaceAgentPlan(ctx context.Context, plan domain.AgentPlan) (domain.AgentPlan, error) {
	return s.changeAgentPlan(ctx, plan.SessionID, false, func(_ domain.AgentPlan, now time.Time) (domain.AgentPlan, error) {
		plan.CreatedAt, plan.UpdatedAt = now, now
		plan.Steps = slices.Clone(plan.Steps)
		for i := range plan.Steps {
			plan.Steps[i].UpdatedAt = now
		}
		return plan, nil
	})
}

func (s *Store) TransitionAgentPlanStep(ctx context.Context, sessionID string, number int, status string) (domain.AgentPlan, error) {
	return s.changeAgentPlan(ctx, sessionID, true, func(plan domain.AgentPlan, now time.Time) (domain.AgentPlan, error) {
		return plan.TransitionStep(number, status, now)
	})
}

func (s *Store) ReviseAgentPlanRemaining(ctx context.Context, sessionID string, titles []string) (domain.AgentPlan, error) {
	return s.changeAgentPlan(ctx, sessionID, true, func(plan domain.AgentPlan, now time.Time) (domain.AgentPlan, error) {
		return plan.ReviseRemaining(titles, now)
	})
}

func (s *Store) changeAgentPlan(ctx context.Context, sessionID string, existing bool, change func(domain.AgentPlan, time.Time) (domain.AgentPlan, error)) (domain.AgentPlan, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.AgentPlan{}, err
	}
	defer tx.Rollback()
	// Obtain the SQLite writer before reading the plan; concurrent updates must
	// validate against the last committed state, not a stale read transaction.
	if _, err := tx.ExecContext(ctx, `UPDATE agent_plans SET status=status WHERE session_id=?`, sessionID); err != nil {
		return domain.AgentPlan{}, err
	}
	var plan domain.AgentPlan
	if existing {
		plan, err = readAgentPlan(ctx, tx, sessionID)
		if err != nil {
			return plan, err
		}
	}
	now := time.Now().UTC()
	plan, err = change(plan, now)
	if err != nil {
		return plan, err
	}
	if err := writeAgentPlan(ctx, tx, plan); err != nil {
		return plan, err
	}
	if err := tx.Commit(); err != nil {
		return plan, err
	}
	s.publishChange(Change{Topic: ChangeChatState, SessionID: sessionID})
	return plan, nil
}
