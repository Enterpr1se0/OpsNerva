package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/ids"
)

const operatorConnectionReason = "started directly by the operator from the Web console"

// StartOperatorSSHTunnel starts a tunnel after an authenticated Web operator has
// explicitly submitted the form. Agent-initiated tunnels continue through
// StartSSHTunnel and the normal approval policy.
func (s *Service) StartOperatorSSHTunnel(ctx context.Context, hostID, remoteHost string, remotePort, localPort int, actor string) (domain.SSHTunnel, error) {
	remoteHost = strings.Trim(strings.TrimSpace(remoteHost), "[]")
	if remoteHost == "" {
		remoteHost = "127.0.0.1"
	}
	result, err := s.executeOperatorConnection(ctx, domain.ExecRequest{
		HostID:           strings.TrimSpace(hostID),
		Mode:             domain.ExecSSHTunnelStart,
		Reason:           operatorConnectionReason,
		TunnelRemoteHost: remoteHost,
		TunnelRemotePort: remotePort,
		TunnelLocalPort:  localPort,
	}, actor)
	if err != nil {
		return domain.SSHTunnel{}, err
	}
	if result.Tunnel == nil {
		return domain.SSHTunnel{}, fmt.Errorf("SSH tunnel start completed without tunnel state")
	}
	return *result.Tunnel, nil
}

// StartOperatorSSHShell opens a normal-user PTY directly from the authenticated
// Web console. It is intentionally independent from an Agent conversation.
func (s *Service) StartOperatorSSHShell(ctx context.Context, hostID, surface, actor string) (domain.SSHShell, error) {
	surface = strings.TrimSpace(surface)
	if surface == "" {
		surface = domain.SSHShellSurfaceQuick
	}
	if surface != domain.SSHShellSurfaceQuick && surface != domain.SSHShellSurfaceWorkspace {
		return domain.SSHShell{}, fmt.Errorf("invalid SSH shell surface %q", surface)
	}
	result, err := s.executeOperatorConnection(ctx, domain.ExecRequest{
		HostID:       strings.TrimSpace(hostID),
		Mode:         domain.ExecSSHShellStart,
		Reason:       operatorConnectionReason,
		ShellSurface: surface,
	}, actor)
	if err != nil {
		return domain.SSHShell{}, err
	}
	if result.Shell == nil {
		return domain.SSHShell{}, fmt.Errorf("SSH shell start completed without shell state")
	}
	return *result.Shell, nil
}

func (s *Service) executeOperatorConnection(ctx context.Context, req domain.ExecRequest, actor string) (domain.ExecResult, error) {
	normalizeRequest(&req, s.limits)
	if req.Mode != domain.ExecSSHTunnelStart && req.Mode != domain.ExecSSHShellStart {
		return domain.ExecResult{}, fmt.Errorf("invalid operator connection mode")
	}
	if err := validateRequestLimits(req, s.limits, s.redactor); err != nil {
		return domain.ExecResult{}, err
	}
	host, err := s.store.GetHost(ctx, req.HostID)
	if err != nil {
		return domain.ExecResult{}, err
	}
	_, connectionDigest, err := s.resolveSSHConnection(ctx, host)
	if err != nil {
		return domain.ExecResult{}, err
	}
	bindSSHRequest(&req, connectionDigest)
	if err := validateExecutionRequest(host, req); err != nil {
		return domain.ExecResult{}, err
	}
	requestJSON, requestDigest, err := canonicalRequest(req)
	if err != nil {
		return domain.ExecResult{}, err
	}
	requestCipher, err := s.encryptor.Encrypt([]byte(requestJSON))
	if err != nil {
		return domain.ExecResult{}, err
	}
	run := domain.Run{
		ID: ids.New("run"), HostID: host.ID,
		RequestJSON: s.redactor.Redact(requestJSON), RequestCipher: requestCipher,
		SearchText: s.redactor.Redact(req.SearchText()), RequestDigest: requestDigest,
		Risk: domain.RiskChange, Status: "running", StartedAt: time.Now().UTC(),
	}
	if err := s.store.CreateRun(ctx, run); err != nil {
		return domain.ExecResult{}, err
	}
	s.audit(context.WithoutCancel(ctx), run.ID, "operator_connection_requested", actor, map[string]any{
		"host_id": host.ID, "mode": req.Mode,
	})
	return s.execute(ctx, host, req, run, actor, []string{"authenticated_web_operator"}, nil)
}
