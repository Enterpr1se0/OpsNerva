package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/ids"
)

func (s *Store) UpsertModelProvider(ctx context.Context, provider domain.ModelProvider) (domain.ModelProvider, error) {
	now := time.Now().UTC()
	if provider.ID == "" {
		provider.ID = ids.New("model")
		provider.CreatedAt = now
	}
	provider.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `
INSERT INTO model_providers(id,name,kind,base_url,model,context_window,api_key_cipher,proxy_id,user_agent,reasoning_effort,active,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name,kind=excluded.kind,base_url=excluded.base_url,
model=excluded.model,context_window=excluded.context_window,api_key_cipher=excluded.api_key_cipher,proxy_id=excluded.proxy_id,
user_agent=excluded.user_agent,reasoning_effort=excluded.reasoning_effort,updated_at=excluded.updated_at`,
		provider.ID, provider.Name, provider.Kind, provider.BaseURL, provider.Model, provider.ContextWindow, provider.APIKeyCipher,
		provider.ProxyID, provider.UserAgent, provider.ReasoningEffort,
		boolInt(provider.Active), formatTime(provider.CreatedAt), formatTime(provider.UpdatedAt))
	if err != nil {
		return domain.ModelProvider{}, err
	}
	return s.GetModelProvider(ctx, provider.ID)
}

func (s *Store) GetModelProvider(ctx context.Context, id string) (domain.ModelProvider, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,name,kind,base_url,model,context_window,api_key_cipher,proxy_id,user_agent,reasoning_effort,active,created_at,updated_at
FROM model_providers WHERE id=?`, id)
	return scanModelProvider(row)
}

func (s *Store) ActiveModelProvider(ctx context.Context) (domain.ModelProvider, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,name,kind,base_url,model,context_window,api_key_cipher,proxy_id,user_agent,reasoning_effort,active,created_at,updated_at
FROM model_providers WHERE active=1 LIMIT 1`)
	return scanModelProvider(row)
}

func (s *Store) ListModelProviders(ctx context.Context) ([]domain.ModelProvider, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,kind,base_url,model,context_window,api_key_cipher,proxy_id,user_agent,reasoning_effort,active,created_at,updated_at
FROM model_providers ORDER BY created_at,rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.ModelProvider, 0)
	for rows.Next() {
		provider, err := scanModelProvider(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, provider)
	}
	return result, rows.Err()
}

func (s *Store) ActivateModelProvider(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE model_providers SET active=0 WHERE active=1`); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE model_providers SET active=1,updated_at=? WHERE id=?`, formatTime(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) DeleteModelProvider(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM model_providers WHERE id=?`, id)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func scanModelProvider(row scanner) (domain.ModelProvider, error) {
	var provider domain.ModelProvider
	var active int
	var created, updated string
	err := row.Scan(&provider.ID, &provider.Name, &provider.Kind, &provider.BaseURL, &provider.Model, &provider.ContextWindow,
		&provider.APIKeyCipher, &provider.ProxyID, &provider.UserAgent, &provider.ReasoningEffort, &active, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ModelProvider{}, ErrNotFound
	}
	if err != nil {
		return domain.ModelProvider{}, err
	}
	provider.HasAPIKey = provider.APIKeyCipher != ""
	provider.Active = active != 0
	provider.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	provider.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return provider, nil
}
