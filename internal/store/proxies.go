package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/ids"
)

func (s *Store) UpsertProxy(ctx context.Context, proxy domain.Proxy) (domain.Proxy, error) {
	now := time.Now().UTC()
	if proxy.ID == "" {
		proxy.ID = ids.New("proxy")
		proxy.CreatedAt = now
	}
	proxy.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `INSERT INTO proxies(id,name,url,username,password_cipher,created_at,updated_at)
VALUES(?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name,url=excluded.url,username=excluded.username,
password_cipher=excluded.password_cipher,updated_at=excluded.updated_at`,
		proxy.ID, proxy.Name, proxy.URL, proxy.Username, proxy.PasswordCipher,
		formatTime(proxy.CreatedAt), formatTime(proxy.UpdatedAt))
	if err != nil {
		return domain.Proxy{}, err
	}
	return s.GetProxy(ctx, proxy.ID)
}

func (s *Store) GetProxy(ctx context.Context, id string) (domain.Proxy, error) {
	return scanProxy(s.db.QueryRowContext(ctx, `SELECT id,name,url,username,password_cipher,created_at,updated_at FROM proxies WHERE id=?`, id))
}

func (s *Store) ListProxies(ctx context.Context) ([]domain.Proxy, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,url,username,password_cipher,created_at,updated_at FROM proxies ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.Proxy, 0)
	for rows.Next() {
		proxy, err := scanProxy(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, proxy)
	}
	return result, rows.Err()
}

func (s *Store) DeleteProxy(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM proxies WHERE id=?
AND NOT EXISTS(SELECT 1 FROM model_providers WHERE proxy_id=?)
AND NOT EXISTS(SELECT 1 FROM hosts WHERE proxy_id=?)
AND NOT EXISTS(SELECT 1 FROM web_search_settings WHERE id=1 AND proxy_id=?)`, id, id, id, id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		var exists int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM proxies WHERE id=?`, id).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return ErrNotFound
		}
		return ErrInUse
	}
	return nil
}

func (s *Store) ProxyReferences(ctx context.Context, id string) ([]string, error) {
	result := make([]string, 0)
	rows, err := s.db.QueryContext(ctx, `SELECT 'model provider: '||name FROM model_providers WHERE proxy_id=?
UNION ALL SELECT 'SSH host: '||name FROM hosts WHERE proxy_id=?
UNION ALL SELECT 'Tavily Web' FROM web_search_settings WHERE id=1 AND proxy_id=?`, id, id, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var reference string
		if err := rows.Scan(&reference); err != nil {
			return nil, err
		}
		result = append(result, reference)
	}
	return result, rows.Err()
}

func scanProxy(row scanner) (domain.Proxy, error) {
	var proxy domain.Proxy
	var created, updated string
	err := row.Scan(&proxy.ID, &proxy.Name, &proxy.URL, &proxy.Username, &proxy.PasswordCipher, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Proxy{}, ErrNotFound
	}
	if err != nil {
		return domain.Proxy{}, err
	}
	proxy.HasPassword = proxy.PasswordCipher != ""
	proxy.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	proxy.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return proxy, nil
}
