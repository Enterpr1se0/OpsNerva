package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"eino-ops-agent/internal/domain"
)

type ConfigurationSnapshot struct {
	Proxies        []domain.Proxy
	Hosts          []domain.Host
	ModelProviders []domain.ModelProvider
}

func (s *Store) ConfigurationSnapshot(ctx context.Context) (ConfigurationSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ConfigurationSnapshot{}, err
	}
	defer tx.Rollback()
	result := ConfigurationSnapshot{Proxies: []domain.Proxy{}, Hosts: []domain.Host{}, ModelProviders: []domain.ModelProvider{}}
	proxyRows, err := tx.QueryContext(ctx, `SELECT id,name,url,username,password_cipher,created_at,updated_at FROM proxies ORDER BY name`)
	if err != nil {
		return ConfigurationSnapshot{}, err
	}
	for proxyRows.Next() {
		proxy, scanErr := scanProxy(proxyRows)
		if scanErr != nil {
			proxyRows.Close()
			return ConfigurationSnapshot{}, scanErr
		}
		result.Proxies = append(result.Proxies, proxy)
	}
	if err := proxyRows.Err(); err != nil {
		proxyRows.Close()
		return ConfigurationSnapshot{}, err
	}
	if err := proxyRows.Close(); err != nil {
		return ConfigurationSnapshot{}, err
	}
	hostRows, err := tx.QueryContext(ctx, `SELECT id,name,address,port,username,agent_enabled,auth_type,private_key_cipher,
known_hosts_file,proxy_jump_host_id,proxy_id,password_cipher,sudo_mode,sudo_password_cipher,created_at,updated_at
FROM hosts WHERE auth_type<>'workspace' ORDER BY name`)
	if err != nil {
		return ConfigurationSnapshot{}, err
	}
	for hostRows.Next() {
		host, scanErr := scanHost(hostRows)
		if scanErr != nil {
			hostRows.Close()
			return ConfigurationSnapshot{}, scanErr
		}
		result.Hosts = append(result.Hosts, host)
	}
	if err := hostRows.Err(); err != nil {
		hostRows.Close()
		return ConfigurationSnapshot{}, err
	}
	if err := hostRows.Close(); err != nil {
		return ConfigurationSnapshot{}, err
	}
	providerRows, err := tx.QueryContext(ctx, `SELECT id,name,kind,base_url,model,context_window,api_key_cipher,proxy_id,user_agent,reasoning_effort,active,created_at,updated_at
FROM model_providers ORDER BY created_at,id`)
	if err != nil {
		return ConfigurationSnapshot{}, err
	}
	for providerRows.Next() {
		provider, scanErr := scanModelProvider(providerRows)
		if scanErr != nil {
			providerRows.Close()
			return ConfigurationSnapshot{}, scanErr
		}
		result.ModelProviders = append(result.ModelProviders, provider)
	}
	if err := providerRows.Err(); err != nil {
		providerRows.Close()
		return ConfigurationSnapshot{}, err
	}
	if err := providerRows.Close(); err != nil {
		return ConfigurationSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return ConfigurationSnapshot{}, err
	}
	return result, nil
}

func (s *Store) ApplyConfiguration(ctx context.Context, snapshot ConfigurationSnapshot) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	for _, proxy := range snapshot.Proxies {
		_, err = tx.ExecContext(ctx, `INSERT INTO proxies(id,name,url,username,password_cipher,created_at,updated_at)
VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,url=excluded.url,username=excluded.username,password_cipher=excluded.password_cipher,updated_at=excluded.updated_at`,
			proxy.ID, proxy.Name, proxy.URL, proxy.Username, proxy.PasswordCipher, formatTime(now), formatTime(now))
		if err != nil {
			return fmt.Errorf("import proxy %q: %w", proxy.Name, err)
		}
	}
	for _, host := range snapshot.Hosts {
		var existingType string
		err = tx.QueryRowContext(ctx, `SELECT auth_type FROM hosts WHERE id=?`, host.ID).Scan(&existingType)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil && existingType == "workspace" {
			return fmt.Errorf("host id %q belongs to an internal workspace", host.ID)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO hosts(id,name,address,port,username,agent_enabled,auth_type,private_key_cipher,known_hosts_file,proxy_jump_host_id,proxy_id,password_cipher,sudo_mode,sudo_password_cipher,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,address=excluded.address,port=excluded.port,username=excluded.username,agent_enabled=excluded.agent_enabled,auth_type=excluded.auth_type,private_key_cipher=excluded.private_key_cipher,known_hosts_file=excluded.known_hosts_file,proxy_jump_host_id=excluded.proxy_jump_host_id,proxy_id=excluded.proxy_id,password_cipher=excluded.password_cipher,sudo_mode=excluded.sudo_mode,sudo_password_cipher=excluded.sudo_password_cipher,updated_at=excluded.updated_at`,
			host.ID, host.Name, host.Address, host.Port, host.User, boolInt(host.AgentEnabled), host.AuthType, host.PrivateKeyCipher, host.KnownHostsFile, host.ProxyJumpHostID, host.ProxyID, host.PasswordCipher, host.SudoMode, host.SudoCipher, formatTime(now), formatTime(now))
		if err != nil {
			return fmt.Errorf("import host %q: %w", host.Name, err)
		}
	}
	activeID := ""
	for _, provider := range snapshot.ModelProviders {
		if provider.Active {
			activeID = provider.ID
			break
		}
	}
	if activeID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE model_providers SET active=0 WHERE active=1`); err != nil {
			return err
		}
	}
	for _, provider := range snapshot.ModelProviders {
		active := 0
		if activeID != "" && provider.ID == activeID {
			active = 1
		}
		query := `INSERT INTO model_providers(id,name,kind,base_url,model,context_window,api_key_cipher,proxy_id,user_agent,reasoning_effort,active,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,kind=excluded.kind,base_url=excluded.base_url,model=excluded.model,context_window=excluded.context_window,api_key_cipher=excluded.api_key_cipher,proxy_id=excluded.proxy_id,user_agent=excluded.user_agent,reasoning_effort=excluded.reasoning_effort,updated_at=excluded.updated_at`
		if activeID != "" {
			query += `,active=excluded.active`
		}
		_, err = tx.ExecContext(ctx, query, provider.ID, provider.Name, provider.Kind, provider.BaseURL, provider.Model, provider.ContextWindow, provider.APIKeyCipher, provider.ProxyID, provider.UserAgent, provider.ReasoningEffort, active, formatTime(now), formatTime(now))
		if err != nil {
			return fmt.Errorf("import model provider %q: %w", provider.Name, err)
		}
	}
	for _, host := range snapshot.Hosts {
		if host.ProxyID != "" {
			if err := requireConfigurationReference(ctx, tx, "proxies", host.ProxyID, "host", host.Name); err != nil {
				return err
			}
		}
		if host.ProxyJumpHostID != "" {
			if err := requireConfigurationReference(ctx, tx, "hosts", host.ProxyJumpHostID, "host", host.Name); err != nil {
				return err
			}
		}
	}
	for _, provider := range snapshot.ModelProviders {
		if provider.ProxyID != "" {
			if err := requireConfigurationReference(ctx, tx, "proxies", provider.ProxyID, "model provider", provider.Name); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func requireConfigurationReference(ctx context.Context, tx *sql.Tx, table, id, ownerKind, ownerName string) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE id=?`, id).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("%s %q references missing %s id %q", ownerKind, ownerName, table, id)
	}
	return nil
}
