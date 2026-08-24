package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"
)

const maxProxyJumpDepth = 4

var ErrAgentHostAccessDenied = errors.New("host is not available to Agent")

type sshConnectionBinding struct {
	Target sshHostBinding   `json:"target"`
	Jumps  []sshHostBinding `json:"jumps,omitempty"`
}

type sshHostBinding struct {
	ID              string `json:"id"`
	Address         string `json:"address"`
	Port            int    `json:"port"`
	User            string `json:"user"`
	AuthType        string `json:"auth_type"`
	KnownHostsFile  string `json:"known_hosts_file,omitempty"`
	ProxyJumpHostID string `json:"proxy_jump_host_id,omitempty"`
	ProxyID         string `json:"proxy_id,omitempty"`
	ProxyURL        string `json:"proxy_url,omitempty"`
	ProxyUsername   string `json:"proxy_username,omitempty"`
	ProxyUpdatedAt  string `json:"proxy_updated_at,omitempty"`
	UpdatedAt       string `json:"updated_at"`
}

func (s *Service) resolveSSHConnection(ctx context.Context, target domain.Host) (sshx.ConnectionSpec, string, error) {
	var err error
	target, err = s.resolveSSHHostProxy(ctx, target)
	if err != nil {
		return sshx.ConnectionSpec{}, "", err
	}
	connection := sshx.ConnectionSpec{Target: target}
	seen := map[string]struct{}{target.ID: {}}
	current := target
	nearestFirst := make([]domain.Host, 0, maxProxyJumpDepth)
	for current.ProxyJumpHostID != "" {
		if len(nearestFirst) >= maxProxyJumpDepth {
			return sshx.ConnectionSpec{}, "", fmt.Errorf("SSH ProxyJump chain exceeds %d hosts", maxProxyJumpDepth)
		}
		jump, err := s.store.GetHost(ctx, current.ProxyJumpHostID)
		if err != nil {
			return sshx.ConnectionSpec{}, "", fmt.Errorf("load ProxyJump host %q: %w", current.ProxyJumpHostID, err)
		}
		if _, duplicate := seen[jump.ID]; duplicate {
			return sshx.ConnectionSpec{}, "", fmt.Errorf("SSH ProxyJump chain contains a cycle at %q", jump.Name)
		}
		jump, err = s.resolveSSHHostProxy(ctx, jump)
		if err != nil {
			return sshx.ConnectionSpec{}, "", err
		}
		seen[jump.ID] = struct{}{}
		nearestFirst = append(nearestFirst, jump)
		current = jump
	}
	for index := len(nearestFirst) - 1; index >= 0; index-- {
		connection.Jumps = append(connection.Jumps, nearestFirst[index])
	}

	binding := sshConnectionBinding{Target: bindSSHHost(connection.Target)}
	for _, jump := range connection.Jumps {
		binding.Jumps = append(binding.Jumps, bindSSHHost(jump))
	}
	data, err := json.Marshal(binding)
	if err != nil {
		return sshx.ConnectionSpec{}, "", err
	}
	digest := sha256.Sum256(data)
	return connection, hex.EncodeToString(digest[:]), nil
}

func requireAgentHostAccess(actor string, host domain.Host) error {
	if actor == "eino-agent" && !host.AgentEnabled {
		return ErrAgentHostAccessDenied
	}
	return nil
}

func requireAgentSSHAccess(actor string, connection sshx.ConnectionSpec) error {
	if err := requireAgentHostAccess(actor, connection.Target); err != nil {
		return err
	}
	for _, jump := range connection.Jumps {
		if err := requireAgentHostAccess(actor, jump); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) resolveSSHHostProxy(ctx context.Context, host domain.Host) (domain.Host, error) {
	if host.ProxyID == "" {
		return host, nil
	}
	proxy, err := s.store.GetProxy(ctx, host.ProxyID)
	if err != nil {
		return domain.Host{}, fmt.Errorf("load proxy %q for SSH host %q: %w", host.ProxyID, host.Name, err)
	}
	if _, err := sshx.NormalizeProxyURL(proxy.URL); err != nil {
		return domain.Host{}, fmt.Errorf("proxy %q is not compatible with SSH: %w", proxy.Name, err)
	}
	host.ProxyURL = proxy.URL
	host.ProxyUsername = proxy.Username
	host.ProxyPasswordCipher = proxy.PasswordCipher
	host.ProxyUpdatedAt = proxy.UpdatedAt
	return host, nil
}

func bindSSHHost(host domain.Host) sshHostBinding {
	updated := ""
	if !host.UpdatedAt.IsZero() {
		updated = host.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	proxyUpdated := ""
	if !host.ProxyUpdatedAt.IsZero() {
		proxyUpdated = host.ProxyUpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	return sshHostBinding{
		ID: host.ID, Address: host.Address, Port: host.Port, User: host.User,
		AuthType: host.AuthType, KnownHostsFile: host.KnownHostsFile, ProxyURL: host.ProxyURL, ProxyUsername: host.ProxyUsername,
		ProxyID: host.ProxyID, ProxyUpdatedAt: proxyUpdated, ProxyJumpHostID: host.ProxyJumpHostID, UpdatedAt: updated,
	}
}

func (s *Service) hydrateSSHConnection(connection sshx.ConnectionSpec, includeSudo bool) (sshx.ConnectionSpec, error) {
	target, err := s.hydrateHostSecrets(connection.Target, includeSudo)
	if err != nil {
		return sshx.ConnectionSpec{}, err
	}
	connection.Target = target
	for index := range connection.Jumps {
		jump, err := s.hydrateHostSecrets(connection.Jumps[index], false)
		if err != nil {
			return sshx.ConnectionSpec{}, fmt.Errorf("prepare ProxyJump credentials for %q: %w", connection.Jumps[index].Name, err)
		}
		connection.Jumps[index] = jump
	}
	return connection, nil
}

func bindSSHRequest(req *domain.ExecRequest, digest string) {
	req.SSHConnectionDigest = digest
}

func bindSSHTransferSource(req *domain.ExecRequest, digest string) {
	req.SourceConnectionDigest = digest
}

func verifySSHRequestBinding(req domain.ExecRequest, digest string) error {
	if req.SSHConnectionDigest != digest {
		return fmt.Errorf("approved SSH connection changed after submission")
	}
	return nil
}

func verifySSHTransferSourceBinding(req domain.ExecRequest, digest string) error {
	if req.SourceConnectionDigest != digest {
		return fmt.Errorf("approved source SSH connection changed after submission")
	}
	return nil
}
