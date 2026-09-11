package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func (s *Store) InitializeWorkspaces(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var initialized int
	err = tx.QueryRowContext(ctx, `SELECT initialized FROM workspace_state WHERE id=1`).Scan(&initialized)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) || initialized == 0 {
		now := formatTime(time.Now().UTC())
		if _, err := tx.ExecContext(ctx, `INSERT INTO workspaces(id,access,created_at,updated_at) VALUES('default','read_write',?,?)
ON CONFLICT(id) DO NOTHING`, now, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO workspace_state(id,initialized) VALUES(1,1)
ON CONFLICT(id) DO UPDATE SET initialized=1`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListWorkspaces(ctx context.Context) ([]domain.Workspace, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,access,created_at,updated_at FROM workspaces ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.Workspace, 0)
	for rows.Next() {
		workspace, err := scanWorkspace(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, workspace)
	}
	return result, rows.Err()
}

func (s *Store) CreateWorkspace(ctx context.Context, workspace domain.Workspace) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO workspaces(id,access,created_at,updated_at) VALUES(?,?,?,?)`,
		workspace.ID, workspace.Access, formatTime(workspace.CreatedAt), formatTime(workspace.UpdatedAt))
	return err
}

func (s *Store) UpdateWorkspace(ctx context.Context, workspace domain.Workspace) error {
	result, err := s.db.ExecContext(ctx, `UPDATE workspaces SET access=?,updated_at=? WHERE id=?`,
		workspace.Access, formatTime(workspace.UpdatedAt), workspace.ID)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteWorkspace(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE id=?`, id)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chat_sessions SET workspace_id='' WHERE workspace_id=?`, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.publishChange(Change{Topic: ChangeSessions})
	s.publishChange(Change{Topic: ChangeChatState})
	return nil
}

func scanWorkspace(row scanner) (domain.Workspace, error) {
	var workspace domain.Workspace
	var created, updated string
	err := row.Scan(&workspace.ID, &workspace.Access, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Workspace{}, ErrNotFound
	}
	if err != nil {
		return domain.Workspace{}, err
	}
	workspace.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	workspace.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return workspace, nil
}
