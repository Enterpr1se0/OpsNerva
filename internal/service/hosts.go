package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"
)

func (s *Service) AddHost(ctx context.Context, host domain.Host, actor string) (domain.Host, error) {
	agentEnabled := host.AgentEnabled
	return s.SaveHost(ctx, domain.HostInput{
		ID: host.ID, Name: host.Name, Address: host.Address, Port: host.Port, User: host.User,
		AgentEnabled: &agentEnabled,
		AuthType:     host.AuthType, KnownHostsFile: host.KnownHostsFile, ProxyJumpHostID: host.ProxyJumpHostID,
		ProxyID: host.ProxyID, SudoMode: host.SudoMode,
	}, actor)
}

func (s *Service) SaveHost(ctx context.Context, input domain.HostInput, actor string) (domain.Host, error) {
	input.ID = strings.TrimSpace(input.ID)
	input.Name = strings.TrimSpace(input.Name)
	input.Address = strings.TrimSpace(input.Address)
	input.User = strings.TrimSpace(input.User)
	input.AuthType = strings.TrimSpace(input.AuthType)
	input.KnownHostsFile = strings.TrimSpace(input.KnownHostsFile)
	input.ProxyJumpHostID = strings.TrimSpace(input.ProxyJumpHostID)
	input.ProxyID = strings.TrimSpace(input.ProxyID)
	input.SudoMode = strings.TrimSpace(input.SudoMode)
	var existing domain.Host
	hasExisting := false
	if input.ID != "" {
		var err error
		existing, err = s.store.GetHost(ctx, input.ID)
		if err != nil {
			return domain.Host{}, err
		}
		hasExisting = true
	}
	if input.Name == "" {
		return domain.Host{}, fmt.Errorf("host name is required")
	}
	if input.Port == 0 {
		input.Port = 22
	}
	if input.Port < 1 || input.Port > 65535 {
		return domain.Host{}, fmt.Errorf("invalid SSH port")
	}
	if input.AuthType == "" {
		input.AuthType = "agent"
	}
	if input.SudoMode == "" {
		input.SudoMode = "none"
	}
	if input.Address == "" || input.User == "" {
		return domain.Host{}, fmt.Errorf("address and user are required")
	}
	if input.ProxyID != "" {
		proxy, err := s.store.GetProxy(ctx, input.ProxyID)
		if err != nil {
			return domain.Host{}, fmt.Errorf("load proxy %q: %w", input.ProxyID, err)
		}
		if _, err := sshx.NormalizeProxyURL(proxy.URL); err != nil {
			return domain.Host{}, fmt.Errorf("proxy %q is not compatible with SSH: %w", proxy.Name, err)
		}
	}
	if input.AuthType == "key" && input.PrivateKey == "" && (!hasExisting || existing.PrivateKeyCipher == "") {
		return domain.Host{}, fmt.Errorf("private_key upload is required for key authentication")
	}
	switch input.AuthType {
	case "agent", "key", "password":
	default:
		return domain.Host{}, fmt.Errorf("invalid SSH authentication type %q", input.AuthType)
	}
	switch input.SudoMode {
	case "none", "nopasswd", "password":
	default:
		return domain.Host{}, fmt.Errorf("invalid sudo mode %q", input.SudoMode)
	}
	if containsCredentialControl(input.Password) || containsCredentialControl(input.SudoPassword) {
		return domain.Host{}, fmt.Errorf("credentials cannot contain NUL, carriage return, or newline characters")
	}
	if len(input.Password) > 1024 || len(input.SudoPassword) > 1024 {
		return domain.Host{}, fmt.Errorf("password is too long")
	}
	if input.AuthType != "key" {
		input.PrivateKey = ""
	}
	agentEnabled := true
	if hasExisting {
		agentEnabled = existing.AgentEnabled
	}
	if input.AgentEnabled != nil {
		agentEnabled = *input.AgentEnabled
	}
	agentRootEnabled := false
	if hasExisting {
		agentRootEnabled = existing.AgentRootEnabled
	}

	host := domain.Host{
		ID: input.ID, Name: input.Name, Address: input.Address, Port: input.Port, User: input.User,
		AgentEnabled: agentEnabled, AgentRootEnabled: agentRootEnabled,
		AuthType: input.AuthType, KnownHostsFile: input.KnownHostsFile, ProxyJumpHostID: input.ProxyJumpHostID,
		ProxyID: input.ProxyID, SudoMode: input.SudoMode,
	}
	if hasExisting {
		host.CreatedAt = existing.CreatedAt
		host.PasswordCipher = existing.PasswordCipher
		host.SudoCipher = existing.SudoCipher
		host.PrivateKeyCipher = existing.PrivateKeyCipher
	}
	if input.AuthType != "key" {
		host.PrivateKeyCipher = ""
	} else if input.PrivateKey != "" {
		privateKey := []byte(input.PrivateKey)
		if err := sshx.ValidatePrivateKey(privateKey); err != nil {
			return domain.Host{}, fmt.Errorf("invalid SSH private key upload: %w", err)
		}
		cipher, err := s.encryptor.Encrypt(privateKey)
		if err != nil {
			return domain.Host{}, fmt.Errorf("encrypt SSH private key: %w", err)
		}
		host.PrivateKeyCipher = cipher
	}
	if input.AuthType != "password" {
		host.PasswordCipher = ""
	} else if input.Password != "" {
		cipher, err := s.encryptor.Encrypt([]byte(input.Password))
		if err != nil {
			return domain.Host{}, fmt.Errorf("encrypt SSH password: %w", err)
		}
		host.PasswordCipher = cipher
	}
	if input.SudoMode != "password" {
		host.SudoCipher = ""
	} else if input.SudoPassword != "" {
		cipher, err := s.encryptor.Encrypt([]byte(input.SudoPassword))
		if err != nil {
			return domain.Host{}, fmt.Errorf("encrypt sudo password: %w", err)
		}
		host.SudoCipher = cipher
	}
	if input.AuthType == "password" && host.PasswordCipher == "" {
		return domain.Host{}, fmt.Errorf("password is required for password authentication")
	}
	if input.SudoMode == "password" && host.SudoCipher == "" {
		return domain.Host{}, fmt.Errorf("sudo_password is required for password sudo mode")
	}
	if input.ProxyJumpHostID != "" {
		if input.ProxyJumpHostID == input.ID && input.ID != "" {
			return domain.Host{}, fmt.Errorf("a host cannot use itself as ProxyJump")
		}
		_, err := s.store.GetHost(ctx, input.ProxyJumpHostID)
		if err != nil {
			return domain.Host{}, fmt.Errorf("load ProxyJump host %q: %w", input.ProxyJumpHostID, err)
		}
	}
	if !hostSupportsRoot(host) {
		host.AgentRootEnabled = false
	}

	created, err := s.store.UpsertHost(ctx, host)
	if err != nil {
		return domain.Host{}, err
	}
	s.audit(ctx, "", "host_saved", actor, map[string]any{
		"host_id": created.ID, "name": created.Name, "agent_enabled": created.AgentEnabled, "agent_root_enabled": created.AgentRootEnabled, "auth_type": created.AuthType, "has_private_key": created.HasPrivateKey, "sudo_mode": created.SudoMode,
	})
	return created, nil
}

func (s *Service) GetHost(ctx context.Context, id string) (domain.Host, error) {
	host, err := s.store.GetHost(ctx, id)
	if err != nil {
		return domain.Host{}, err
	}
	return s.withStoredHostKey(host), nil
}

func (s *Service) SetHostAgentRootEnabled(ctx context.Context, id string, enabled bool, actor string) (domain.Host, error) {
	host, err := s.store.GetHost(ctx, strings.TrimSpace(id))
	if err != nil {
		return domain.Host{}, err
	}
	if enabled && !hostSupportsRoot(host) {
		return domain.Host{}, fmt.Errorf("%w: %q", ErrHostAgentRootUnavailable, host.Name)
	}
	host, err = s.store.SetHostAgentRootEnabled(ctx, host.ID, enabled)
	if err != nil {
		return domain.Host{}, err
	}
	s.audit(ctx, "", "host_agent_root_changed", actor, map[string]any{"host_id": host.ID, "enabled": enabled})
	return s.withStoredHostKey(host), nil
}

func (s *Service) ListHosts(ctx context.Context) ([]domain.Host, error) {
	hosts, err := s.store.ListHosts(ctx)
	if err != nil {
		return nil, err
	}
	for index := range hosts {
		hosts[index] = s.withStoredHostKey(hosts[index])
	}
	return hosts, nil
}

func (s *Service) withStoredHostKey(host domain.Host) domain.Host {
	key, ok := s.transport.StoredHostKey(host)
	if ok {
		host.HostKey = &domain.HostKey{
			Fingerprint: key.Fingerprint,
			Algorithm:   key.Algorithm,
			Trusted:     key.Trusted,
		}
	}
	return host
}

func (s *Service) DeleteHost(ctx context.Context, id, actor string) error {
	if s.hasSSHTunnelForHost(id) {
		return fmt.Errorf("%w: stop the tunnel before deleting host %q", ErrHostHasActiveTunnel, id)
	}
	if s.shells.hasActive(func(shell domain.SSHShell) bool { return shell.HostID == id }) {
		return fmt.Errorf("host %q has an active SSH shell; close it before deleting the host", id)
	}
	hosts, err := s.store.ListHosts(ctx)
	if err != nil {
		return err
	}
	for _, host := range hosts {
		if host.ProxyJumpHostID == id {
			return fmt.Errorf("host %q is still used as ProxyJump by %q", id, host.Name)
		}
	}
	if err := s.store.DeleteHost(ctx, id); err != nil {
		return err
	}
	s.audit(ctx, "", "host_deleted", actor, map[string]any{"host_id": id})
	return nil
}

func (s *Service) ProbeHost(ctx context.Context, id, actor string) (sshx.HostInfo, error) {
	host, err := s.store.GetHost(ctx, id)
	if err != nil {
		return sshx.HostInfo{}, err
	}
	connection, binding, err := s.resolveSSHConnection(ctx, host)
	if err != nil {
		return sshx.HostInfo{}, err
	}
	if err := requireAgentSSHAccess(actor, connection); err != nil {
		return sshx.HostInfo{}, err
	}
	connection, err = s.hydrateSSHConnection(connection, false)
	if err != nil {
		return sshx.HostInfo{}, err
	}
	info, err := s.transport.Probe(ctx, connection)
	if err == nil {
		shellPath, detectErr := detectedShellPathFromHostInfo(info)
		if detectErr != nil {
			return sshx.HostInfo{}, detectErr
		}
		if storeErr := s.storeDetectedHostShell(ctx, host.ID, binding, shellPath); storeErr != nil {
			return sshx.HostInfo{}, storeErr
		}
	}
	return info, err
}

func (s *Service) ScanHostKey(ctx context.Context, id string) (sshx.HostKey, error) {
	host, err := s.store.GetHost(ctx, id)
	if err != nil {
		return sshx.HostKey{}, err
	}
	connection, _, err := s.resolveSSHConnection(ctx, host)
	if err != nil {
		return sshx.HostKey{}, err
	}
	connection, err = s.hydrateSSHConnection(connection, false)
	if err != nil {
		return sshx.HostKey{}, err
	}
	return s.transport.ScanHostKey(ctx, connection)
}

func (s *Service) TrustHostKey(ctx context.Context, id, fingerprint, actor string) (sshx.HostKey, error) {
	host, err := s.store.GetHost(ctx, id)
	if err != nil {
		return sshx.HostKey{}, err
	}
	connection, _, err := s.resolveSSHConnection(ctx, host)
	if err != nil {
		return sshx.HostKey{}, err
	}
	connection, err = s.hydrateSSHConnection(connection, false)
	if err != nil {
		return sshx.HostKey{}, err
	}
	key, err := s.transport.TrustHostKey(ctx, connection, fingerprint)
	if err == nil {
		s.audit(ctx, "", "host_key_trusted", actor, map[string]any{"host_id": id, "fingerprint": key.Fingerprint})
	}
	return key, err
}
