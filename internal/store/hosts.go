package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/ids"
)

func (s *Store) UpsertHost(ctx context.Context, host domain.Host) (domain.Host, error) {
	now := time.Now().UTC()
	if host.ID == "" {
		host.ID = ids.New("host")
		host.CreatedAt = now
	}
	if host.Port == 0 {
		host.Port = 22
	}
	host.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `
INSERT INTO hosts(id,name,address,port,username,agent_enabled,agent_root_enabled,auth_type,private_key_cipher,known_hosts_file,proxy_jump_host_id,proxy_id,password_cipher,sudo_mode,sudo_password_cipher,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name,address=excluded.address,port=excluded.port,
username=excluded.username,agent_enabled=excluded.agent_enabled,agent_root_enabled=excluded.agent_root_enabled,detected_shell='',detected_shell_binding='',auth_type=excluded.auth_type,private_key_cipher=excluded.private_key_cipher,
known_hosts_file=excluded.known_hosts_file,proxy_jump_host_id=excluded.proxy_jump_host_id,
proxy_id=excluded.proxy_id,password_cipher=excluded.password_cipher,
sudo_mode=excluded.sudo_mode,sudo_password_cipher=excluded.sudo_password_cipher,updated_at=excluded.updated_at`,
		host.ID, host.Name, host.Address, host.Port, host.User, boolInt(host.AgentEnabled), boolInt(host.AgentRootEnabled), host.AuthType, host.PrivateKeyCipher,
		host.KnownHostsFile, host.ProxyJumpHostID, host.ProxyID,
		host.PasswordCipher, host.SudoMode, host.SudoCipher,
		formatTime(host.CreatedAt), formatTime(host.UpdatedAt))
	if err != nil {
		return domain.Host{}, err
	}
	return s.GetHost(ctx, host.ID)
}

func (s *Store) GetHost(ctx context.Context, id string) (domain.Host, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,name,address,port,username,agent_enabled,agent_root_enabled,detected_shell,detected_shell_binding,auth_type,private_key_cipher,
known_hosts_file,proxy_jump_host_id,proxy_id,password_cipher,
sudo_mode,sudo_password_cipher,created_at,updated_at FROM hosts WHERE id=? OR name=?`, id, id)
	return scanHost(row)
}

func (s *Store) SetHostAgentRootEnabled(ctx context.Context, id string, enabled bool) (domain.Host, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE hosts SET agent_root_enabled=? WHERE id=? AND auth_type<>'workspace'`, boolInt(enabled), id)
	if err != nil {
		return domain.Host{}, err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return domain.Host{}, ErrNotFound
	}
	return s.GetHost(ctx, id)
}

func (s *Store) SetHostDetectedShell(ctx context.Context, id, binding, shell string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE hosts SET detected_shell=?,detected_shell_binding=? WHERE id=? AND auth_type<>'workspace'`, shell, binding, id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListHosts(ctx context.Context) ([]domain.Host, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,address,port,username,agent_enabled,agent_root_enabled,detected_shell,detected_shell_binding,auth_type,private_key_cipher,
known_hosts_file,proxy_jump_host_id,proxy_id,password_cipher,
sudo_mode,sudo_password_cipher,created_at,updated_at FROM hosts WHERE auth_type<>'workspace' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.Host, 0)
	for rows.Next() {
		host, err := scanHost(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, host)
	}
	return result, rows.Err()
}

func (s *Store) DeleteHost(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	deletions := []struct {
		query string
		args  []any
	}{
		{`DELETE FROM ssh_shell_sessions WHERE host_id=? OR run_id IN (SELECT id FROM runs WHERE host_id=?)`, []any{id, id}},
		{`DELETE FROM approvals WHERE host_id=? OR run_id IN (SELECT id FROM runs WHERE host_id=?)`, []any{id, id}},
		{`DELETE FROM tasks WHERE host_id=? OR run_id IN (SELECT id FROM runs WHERE host_id=?)`, []any{id, id}},
		{`DELETE FROM runs WHERE host_id=?`, []any{id}},
	}
	for _, deletion := range deletions {
		if _, err := tx.ExecContext(ctx, deletion.query, deletion.args...); err != nil {
			tx.Rollback()
			return fmt.Errorf("delete host records: %w", err)
		}
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM hosts WHERE id=?`, id)
	if err != nil {
		tx.Rollback()
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		tx.Rollback()
		return ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.publishChange(Change{Topic: ChangeApprovals})
	s.publishChange(Change{Topic: ChangeAudit})
	return nil
}

func scanHost(row scanner) (domain.Host, error) {
	var host domain.Host
	var agentEnabled, agentRootEnabled int
	var created, updated string
	err := row.Scan(&host.ID, &host.Name, &host.Address, &host.Port, &host.User, &agentEnabled, &agentRootEnabled, &host.DetectedShell, &host.DetectedShellBinding, &host.AuthType,
		&host.PrivateKeyCipher, &host.KnownHostsFile, &host.ProxyJumpHostID, &host.ProxyID,
		&host.PasswordCipher, &host.SudoMode, &host.SudoCipher, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Host{}, ErrNotFound
	}
	host.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	host.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	host.AgentEnabled = agentEnabled != 0
	host.AgentRootEnabled = agentRootEnabled != 0
	host.HasPassword = host.PasswordCipher != ""
	host.HasSudoPassword = host.SudoCipher != ""
	host.HasPrivateKey = host.PrivateKeyCipher != ""
	return host, err
}
