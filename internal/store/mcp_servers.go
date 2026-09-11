package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/ids"
)

func (s *Store) UpsertMCPServer(ctx context.Context, server domain.MCPServer) (domain.MCPServer, error) {
	now := time.Now().UTC()
	if server.ID == "" {
		server.ID = ids.New("mcp")
		server.CreatedAt = now
	}
	server.UpdatedAt = now
	argsJSON, err := json.Marshal(server.Args)
	if err != nil {
		return domain.MCPServer{}, err
	}
	envKeysJSON, err := json.Marshal(server.EnvKeys)
	if err != nil {
		return domain.MCPServer{}, err
	}
	headerKeysJSON, err := json.Marshal(server.HeaderKeys)
	if err != nil {
		return domain.MCPServer{}, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO mcp_servers(id,name,transport,command,args_json,cwd,url,env_keys_json,header_keys_json,secrets_cipher,enabled,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name,transport=excluded.transport,command=excluded.command,args_json=excluded.args_json,
cwd=excluded.cwd,url=excluded.url,env_keys_json=excluded.env_keys_json,header_keys_json=excluded.header_keys_json,
secrets_cipher=excluded.secrets_cipher,enabled=excluded.enabled,updated_at=excluded.updated_at`,
		server.ID, server.Name, server.Transport, server.Command, string(argsJSON), server.Cwd, server.URL, string(envKeysJSON),
		string(headerKeysJSON), server.SecretsCipher, boolInt(server.Enabled), formatTime(server.CreatedAt), formatTime(server.UpdatedAt))
	if err != nil {
		return domain.MCPServer{}, err
	}
	return s.GetMCPServer(ctx, server.ID)
}

func (s *Store) GetMCPServer(ctx context.Context, id string) (domain.MCPServer, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,name,transport,command,args_json,cwd,url,env_keys_json,header_keys_json,secrets_cipher,enabled,created_at,updated_at FROM mcp_servers WHERE id=?`, id)
	return scanMCPServer(row)
}

func (s *Store) ListMCPServers(ctx context.Context) ([]domain.MCPServer, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,transport,command,args_json,cwd,url,env_keys_json,header_keys_json,secrets_cipher,enabled,created_at,updated_at FROM mcp_servers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.MCPServer, 0)
	for rows.Next() {
		server, err := scanMCPServer(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, server)
	}
	return result, rows.Err()
}

func (s *Store) SetMCPServerEnabled(ctx context.Context, id string, enabled bool) error {
	result, err := s.db.ExecContext(ctx, `UPDATE mcp_servers SET enabled=?,updated_at=? WHERE id=?`, boolInt(enabled), formatTime(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) UpdateMCPServerSecrets(ctx context.Context, id, secretsCipher string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE mcp_servers SET secrets_cipher=?,updated_at=? WHERE id=?`,
		secretsCipher, formatTime(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteMCPServer(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM mcp_servers WHERE id=?`, id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return ErrNotFound
	}
	return nil
}

func scanMCPServer(row scanner) (domain.MCPServer, error) {
	var server domain.MCPServer
	var argsJSON, envKeysJSON, headerKeysJSON, created, updated string
	var enabled int
	err := row.Scan(&server.ID, &server.Name, &server.Transport, &server.Command, &argsJSON, &server.Cwd, &server.URL,
		&envKeysJSON, &headerKeysJSON, &server.SecretsCipher, &enabled, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.MCPServer{}, ErrNotFound
	}
	if err != nil {
		return domain.MCPServer{}, err
	}
	if err := json.Unmarshal([]byte(argsJSON), &server.Args); err != nil {
		return domain.MCPServer{}, err
	}
	if err := json.Unmarshal([]byte(envKeysJSON), &server.EnvKeys); err != nil {
		return domain.MCPServer{}, err
	}
	if err := json.Unmarshal([]byte(headerKeysJSON), &server.HeaderKeys); err != nil {
		return domain.MCPServer{}, err
	}
	server.Enabled = enabled != 0
	server.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	server.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return server, nil
}
