package store

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

// Convert persisted plan/task data to the ordered plan model. Runtime code
// only reads plans; the retired task-file storage is removed after conversion.
func (s *Store) migrateAgentPlans(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The original plan tables survived the switch to plantask. Remove their
	// obsolete blocked state as well; pause now follows the actual Agent run.
	if _, err := tx.ExecContext(ctx, `UPDATE agent_plan_steps SET status='in_progress' WHERE status='blocked';
UPDATE agent_plans SET status='active' WHERE status='blocked'`); err != nil {
		return err
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='agent_task_files'`).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return tx.Commit()
	}
	rows, err := tx.QueryContext(ctx, `SELECT f.session_id,f.file_path,f.content,f.created_at,f.updated_at,COALESCE(c.title,'')
FROM agent_task_files f LEFT JOIN chat_sessions c ON c.session_id=f.session_id ORDER BY f.session_id,f.file_path`)
	if err != nil {
		return err
	}
	plans := make(map[string]*domain.AgentPlan)
	for rows.Next() {
		var sessionID, path, content, created, updated, goal string
		if err := rows.Scan(&sessionID, &path, &content, &created, &updated, &goal); err != nil {
			rows.Close()
			return err
		}
		// Stored paths may have Windows separators when moving a database.
		name := filepath.Base(strings.ReplaceAll(path, "\\", "/"))
		if name == ".highwatermark" {
			continue
		}
		number, err := strconv.Atoi(strings.TrimSuffix(name, ".json"))
		if err != nil || !strings.HasSuffix(name, ".json") || number < 1 {
			rows.Close()
			return fmt.Errorf("migrate agent task: invalid file %q", path)
		}
		var task struct {
			Subject     string `json:"subject"`
			Description string `json:"description"`
			Status      string `json:"status"`
		}
		if err := json.Unmarshal([]byte(content), &task); err != nil {
			rows.Close()
			return fmt.Errorf("migrate agent task %s: %w", path, err)
		}
		createdAt, _ := time.Parse(time.RFC3339Nano, created)
		updatedAt, _ := time.Parse(time.RFC3339Nano, updated)
		plan := plans[sessionID]
		if plan == nil {
			plan = &domain.AgentPlan{SessionID: sessionID, Goal: goal, CreatedAt: createdAt, UpdatedAt: updatedAt}
			plans[sessionID] = plan
		}
		if createdAt.Before(plan.CreatedAt) {
			plan.CreatedAt = createdAt
		}
		if updatedAt.After(plan.UpdatedAt) {
			plan.UpdatedAt = updatedAt
		}
		plan.Steps = append(plan.Steps, domain.AgentPlanStep{Number: number, Title: task.Subject, Description: task.Description, Status: task.Status, UpdatedAt: updatedAt})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, plan := range plans {
		// Keep finished work, followed by the unfinished sequence. Dependencies
		// and owners do not participate in the restored sequential mechanism.
		sort.SliceStable(plan.Steps, func(i, j int) bool {
			left, right := plan.Steps[i], plan.Steps[j]
			if (left.Status == "completed") != (right.Status == "completed") {
				return left.Status == "completed"
			}
			return left.Number < right.Number
		})
		plan.Status = "completed"
		for i := range plan.Steps {
			plan.Steps[i].Number = i + 1
			if plan.Steps[i].Status != "completed" {
				plan.Steps[i].Status = "pending"
				if plan.Status == "completed" {
					plan.Steps[i].Status = "in_progress"
					plan.Status = "active"
				}
			}
		}
		if plan.Goal == "" {
			plan.Goal = plan.Steps[0].Title
		}
		if err := writeAgentPlan(ctx, tx, *plan); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE agent_task_files;
DELETE FROM agent_tool_settings WHERE name IN ('TaskCreate','TaskGet','TaskUpdate','TaskList')`); err != nil {
		return err
	}
	return tx.Commit()
}
