package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"
)

const maxSSHShellReasonBytes = 500

var (
	ErrSSHShellHostStatusUnavailable = errors.New("SSH shell host monitoring is unavailable")
	ErrSSHShellReconnectConflict     = errors.New("SSH shell cannot be reconnected in its current state")
)

func (s *Service) StartSSHShell(ctx context.Context, hostID, cwd string, elevated bool, cols, rows int, reason, actor string) (domain.ExecResult, error) {
	if SessionIDFromContext(ctx) == "" {
		return domain.ExecResult{}, fmt.Errorf("interactive SSH shells require an Agent or MCP session")
	}
	return s.Submit(ctx, domain.ExecRequest{
		HostID: strings.TrimSpace(hostID), Mode: domain.ExecSSHShellStart,
		Cwd: strings.TrimSpace(cwd), Elevated: elevated,
		ShellCols: cols, ShellRows: rows,
		Reason: strings.TrimSpace(reason),
	}, actor)
}

// StartOperatorSSHShell opens a normal-user PTY directly from the authenticated
// Web console. It is intentionally independent from an Agent conversation.
func (s *Service) StartOperatorSSHShell(ctx context.Context, hostID, surface, actor string) (domain.SSHShell, error) {
	host, connection, req, err := s.prepareOperatorSSHShell(ctx, hostID, surface, "", 0, 0)
	if err != nil {
		return domain.SSHShell{}, err
	}
	release, err := s.acquire(ctx, host.ID)
	if err != nil {
		return domain.SSHShell{}, err
	}
	defer release()
	return s.openOperatorSSHTerminal(ctx, host, connection, req, actor)
}

func (s *Service) prepareOperatorSSHShell(ctx context.Context, hostID, surface, cwd string, cols, rows int) (domain.Host, sshx.ConnectionSpec, domain.ExecRequest, error) {
	surface = strings.TrimSpace(surface)
	if surface == "" {
		surface = domain.SSHShellSurfaceQuick
	}
	if surface != domain.SSHShellSurfaceQuick && surface != domain.SSHShellSurfaceWorkspace {
		return domain.Host{}, sshx.ConnectionSpec{}, domain.ExecRequest{}, fmt.Errorf("invalid SSH shell surface %q", surface)
	}
	req := domain.ExecRequest{
		HostID:       strings.TrimSpace(hostID),
		Mode:         domain.ExecSSHShellStart,
		Reason:       webOperatorReason,
		ShellSurface: surface,
		Cwd:          strings.TrimSpace(cwd),
		ShellCols:    cols,
		ShellRows:    rows,
	}
	normalizeRequest(&req, s.limits)
	if err := validateRequestLimits(req, s.limits, s.redactor); err != nil {
		return domain.Host{}, sshx.ConnectionSpec{}, domain.ExecRequest{}, err
	}
	host, err := s.store.GetHost(ctx, req.HostID)
	if err != nil {
		return domain.Host{}, sshx.ConnectionSpec{}, domain.ExecRequest{}, err
	}
	connection, digest, err := s.resolveSSHConnection(ctx, host)
	if err != nil {
		return domain.Host{}, sshx.ConnectionSpec{}, domain.ExecRequest{}, err
	}
	bindSSHRequest(&req, digest)
	if err := validateExecutionRequest(host, req); err != nil {
		return domain.Host{}, sshx.ConnectionSpec{}, domain.ExecRequest{}, err
	}
	connection, err = s.prepareSSHExecutionConnection(ctx, connection, digest, false, true)
	if err != nil {
		return domain.Host{}, sshx.ConnectionSpec{}, domain.ExecRequest{}, err
	}
	return host, connection, req, nil
}

func (s *Service) ListSSHShells(ctx context.Context, sessionID string, activeOnly bool, reason, actor string) (domain.SSHShellList, error) {
	sessionID = strings.TrimSpace(sessionID)
	shells, err := s.store.ListSSHShells(ctx, sessionID, activeOnly)
	if err != nil {
		return domain.SSHShellList{}, err
	}
	seen := make(map[string]struct{}, len(shells))
	for _, shell := range shells {
		seen[shell.ID] = struct{}{}
	}
	for _, shell := range s.shells.transientShells(sessionID, activeOnly) {
		if _, exists := seen[shell.ID]; !exists {
			shells = append(shells, shell)
			seen[shell.ID] = struct{}{}
		}
	}
	sort.Slice(shells, func(i, j int) bool { return shells[i].StartedAt.Before(shells[j].StartedAt) })
	if reason = strings.TrimSpace(reason); reason != "" && (actor == "eino-agent" || actor == "mcp-client") {
		if len(reason) > maxSSHShellReasonBytes {
			return domain.SSHShellList{}, fmt.Errorf("reason must not exceed %d bytes", maxSSHShellReasonBytes)
		}
		s.audit(context.WithoutCancel(ctx), "", "ssh_shell_list", actor, map[string]any{
			"session_id": sessionID, "active_only": activeOnly, "reason": s.redactor.Redact(reason),
		})
	}
	return domain.SSHShellList{Shells: shells, Count: len(shells)}, nil
}

func (s *Service) GetSSHShellHostStatus(ctx context.Context, id string) (sshx.HostStatus, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return sshx.HostStatus{}, fmt.Errorf("shell_id is required")
	}
	history, state := s.historyForShell(id)
	shell, err := history.Get(ctx)
	if err != nil {
		return sshx.HostStatus{}, err
	}
	if shell.Kind != domain.SSHShellKindSSH {
		return sshx.HostStatus{}, fmt.Errorf("%w: shell %q is not an SSH host shell", ErrSSHShellHostStatusUnavailable, id)
	}
	if state == nil {
		return sshx.HostStatus{}, fmt.Errorf("%w: shell %q is not active", ErrSSHShellHostStatusUnavailable, id)
	}
	state.mu.Lock()
	status := state.shell.Status
	session := state.session
	state.mu.Unlock()
	if !shellStatusActive(status) || session == nil {
		return sshx.HostStatus{}, fmt.Errorf("%w: shell %q is not running", ErrSSHShellHostStatusUnavailable, id)
	}
	monitor, ok := session.(sshx.HostStatusSession)
	if !ok {
		return sshx.HostStatus{}, fmt.Errorf("%w: SSH transport does not support monitoring", ErrSSHShellHostStatusUnavailable)
	}
	monitorCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return monitor.HostStatus(monitorCtx)
}

func (s *Service) openSSHShell(ctx context.Context, host domain.Host, connection sshx.ConnectionSpec, req domain.ExecRequest, run domain.Run, actor string) (domain.SSHShell, error) {
	return s.openSSHShellRuntime(ctx, host, connection, req, run, actor, false)
}

func (s *Service) openOperatorSSHTerminal(ctx context.Context, host domain.Host, connection sshx.ConnectionSpec, req domain.ExecRequest, actor string) (domain.SSHShell, error) {
	return s.openSSHShellRuntime(ctx, host, connection, req, domain.Run{}, actor, true)
}

func (s *Service) openSSHShellRuntime(ctx context.Context, host domain.Host, connection sshx.ConnectionSpec, req domain.ExecRequest, run domain.Run, actor string, transient bool) (domain.SSHShell, error) {
	options, opener, err := s.sshInteractiveShell(connection, req, host.User, transient)
	if err != nil {
		return domain.SSHShell{}, err
	}
	return s.openInteractiveShell(ctx, host, req, run, actor, options, opener)
}

func (s *Service) sshInteractiveShell(connection sshx.ConnectionSpec, req domain.ExecRequest, user string, transient bool) (interactiveShellOptions, interactiveShellOpener, error) {
	transport, ok := s.transport.(sshx.InteractiveTransport)
	if !ok {
		return interactiveShellOptions{}, nil, fmt.Errorf("configured SSH transport does not support interactive PTY sessions")
	}
	if err := validateSSHShellRequest(req); err != nil {
		return interactiveShellOptions{}, nil, err
	}
	secrets := []string(nil)
	if !transient && req.Elevated && connection.Target.SudoPassword != "" {
		secrets = append(secrets, connection.Target.SudoPassword)
	}
	return interactiveShellOptions{
		kind: domain.SSHShellKindSSH, user: user, secrets: secrets, transient: transient,
	}, func(shellCtx context.Context, output func(string, []byte)) (sshx.ShellSession, error) {
		return transport.OpenShell(shellCtx, connection, req, req.ShellCols, req.ShellRows, output)
	}, nil
}

func interactiveShellComponent(kind string) string {
	if kind == domain.SSHShellKindWorkspace {
		return "workspace_shell"
	}
	return "ssh_shell"
}

func validateSSHShellRequest(req domain.ExecRequest) error {
	if req.Mode != domain.ExecSSHShellStart {
		return fmt.Errorf("invalid SSH shell request mode")
	}
	return validateInteractiveShellSize(req)
}

func validateInteractiveShellSize(req domain.ExecRequest) error {
	if req.ShellCols < 20 || req.ShellCols > 500 || req.ShellRows < 5 || req.ShellRows > 200 {
		return fmt.Errorf("interactive terminal size is out of range")
	}
	return nil
}

func marshalSSHShell(shell domain.SSHShell) ([]byte, error) {
	data, err := json.Marshal(shell)
	if err != nil {
		return nil, fmt.Errorf("encode SSH shell state: %w", err)
	}
	return data, nil
}

func sshShellUsage() *domain.SSHShellUsage {
	return &domain.SSHShellUsage{
		Input:  "action=input sends raw bytes, delays wait_seconds, then reads one bounded output page; submit=true appends a carriage return",
		Output: "action=output delays wait_seconds, then reads one bounded output page; pass next_sequence as after_sequence to continue",
		Close:  "call action=close when finished; the shell remains active until it is closed or disconnected",
	}
}
