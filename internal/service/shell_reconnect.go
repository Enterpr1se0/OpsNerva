package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/observability"
)

// ReconnectOperatorSSHShell starts a new transport generation inside an
// existing operator-owned logical shell. The shell identity, bounded output
// history and event sequence stay unchanged.
func (s *Service) ReconnectOperatorSSHShell(ctx context.Context, id, actor string) (domain.SSHShell, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return domain.SSHShell{}, fmt.Errorf("shell_id is required")
	}
	shellCtx, cancel, err := s.shells.begin()
	if err != nil {
		return domain.SSHShell{}, err
	}
	running := false
	defer func() {
		if !running {
			cancel()
		}
		s.shells.workers.Done()
	}()
	state, shell, generation, err := s.shells.beginReconnect(id, cancel)
	if err != nil {
		cancel()
		return domain.SSHShell{}, err
	}
	state.lifecycleMu.Lock()
	state.mu.Lock()
	valid := state.generation == generation && state.shell.Status == "starting" && !state.outputClosed
	state.mu.Unlock()
	if !valid {
		state.lifecycleMu.Unlock()
		return domain.SSHShell{}, context.Canceled
	}
	if err := state.history.Update(context.WithoutCancel(ctx), shell); err != nil {
		state.lifecycleMu.Unlock()
		s.failSSHShellStart(state, generation, err, false)
		return domain.SSHShell{}, err
	}
	s.appendSSHShellEvent(state, "status", "", "starting")
	state.mu.Lock()
	shell = state.shell
	state.mu.Unlock()
	s.publishShellState(shell, false)
	state.lifecycleMu.Unlock()

	host, opener, err := s.prepareOperatorShellReconnect(shellCtx, shell)
	if err != nil {
		s.failSSHShellStart(state, generation, err, false)
		return domain.SSHShell{}, err
	}
	state.mu.Lock()
	if state.generation == generation && state.shell.Status == "starting" {
		state.shell.HostName = host.Name
		if state.shell.Kind == domain.SSHShellKindSSH {
			state.shell.User = host.User
		}
	}
	state.mu.Unlock()
	release, err := s.acquire(shellCtx, host.ID)
	if err != nil {
		s.failSSHShellStart(state, generation, err, false)
		return domain.SSHShell{}, err
	}
	defer release()
	reconnected, err := s.startInteractiveShellGeneration(context.WithoutCancel(ctx), shellCtx, state, opener, false)
	if err != nil {
		return domain.SSHShell{}, err
	}
	running = true
	observability.FromContext(ctx).InfoContext(ctx, "operator shell reconnected",
		"component", interactiveShellComponent(shell.Kind), "shell_id", shell.ID,
		"host_id", shell.HostID, "generation", generation, "actor", actor)
	return reconnected, nil
}

func (s *Service) prepareOperatorShellReconnect(ctx context.Context, shell domain.SSHShell) (domain.Host, interactiveShellOpener, error) {
	switch shell.Kind {
	case domain.SSHShellKindSSH:
		host, connection, req, err := s.prepareOperatorSSHShell(ctx, shell.HostID, shell.Surface, shell.Cwd, shell.Cols, shell.Rows)
		if err != nil {
			return domain.Host{}, nil, err
		}
		_, opener, err := s.sshInteractiveShell(connection, req, host.User, true)
		return host, opener, err
	case domain.SSHShellKindWorkspace:
		req, err := s.workspaceShellStartRequest(ctx, shell.WorkspaceID, shell.Cwd, nil, shell.Cols, shell.Rows, webOperatorReason)
		if err != nil {
			return domain.Host{}, nil, err
		}
		if req.WorkspaceShellBackend != shell.Backend {
			return domain.Host{}, nil, fmt.Errorf("workspace shell backend changed from %q to %q", shell.Backend, req.WorkspaceShellBackend)
		}
		host, err := s.workspaceHost(ctx, shell.WorkspaceID)
		if err != nil {
			return domain.Host{}, nil, err
		}
		_, opener, err := s.workspaceInteractiveShell(ctx, host, req, true)
		return host, opener, err
	default:
		return domain.Host{}, nil, fmt.Errorf("unsupported operator shell kind %q", shell.Kind)
	}
}
