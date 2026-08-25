package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func (s *Store) UpsertTask(ctx context.Context, task domain.Task, result domain.ExecResult, taskError string) error {
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return err
	}
	var ended any
	if !task.EndedAt.IsZero() {
		ended = formatTime(task.EndedAt)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO tasks(id,run_id,session_id,host_id,status,revision,result_json,error,started_at,ended_at) VALUES(?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET run_id=excluded.run_id,session_id=excluded.session_id,status=excluded.status,revision=excluded.revision,result_json=excluded.result_json,error=excluded.error,ended_at=excluded.ended_at
WHERE excluded.revision >= tasks.revision`,
		task.ID, task.RunID, task.SessionID, task.HostID, task.Status, task.Revision, string(resultJSON), taskError, formatTime(task.StartedAt), ended)
	return err
}

func (s *Store) GetTask(ctx context.Context, id string) (domain.Task, domain.ExecResult, string, error) {
	var task domain.Task
	var resultJSON, taskError, started string
	var ended sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id,run_id,session_id,host_id,status,revision,result_json,error,started_at,ended_at FROM tasks WHERE id=?`, id).Scan(
		&task.ID, &task.RunID, &task.SessionID, &task.HostID, &task.Status, &task.Revision, &resultJSON, &taskError, &started, &ended,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Task{}, domain.ExecResult{}, "", ErrNotFound
	}
	if err != nil {
		return domain.Task{}, domain.ExecResult{}, "", err
	}
	task.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
	if ended.Valid {
		task.EndedAt, _ = time.Parse(time.RFC3339Nano, ended.String)
	}
	var result domain.ExecResult
	_ = json.Unmarshal([]byte(resultJSON), &result)
	return task, result, taskError, nil
}

func (s *Store) InterruptActiveTasks(ctx context.Context) error {
	now := formatTime(time.Now().UTC())
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET status='interrupted',revision=revision+1,error=CASE WHEN error='' THEN 'control plane restarted before the task completed' ELSE error END,ended_at=? WHERE status IN ('running','approval_required')`, now)
	return err
}
