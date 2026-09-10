package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/observability"
	"github.com/Enterpr1se0/opsnerva/internal/store"
	"github.com/Enterpr1se0/opsnerva/internal/terminaltext"
)

// ReconnectOperatorSSHShell starts a new transport generation inside an
// existing operator-owned logical shell. The shell identity, bounded output
// history and event sequence stay unchanged.
func (s *Service) ReconnectOperatorSSHShell(ctx context.Context, id, actor string) (domain.SSHShell, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return domain.SSHShell{}, fmt.Errorf("shell_id is required")
	}
	shellCtx, cancel := context.WithCancel(s.executionCtx)
	s.shellMu.Lock()
	state := s.shells[id]
	if state == nil || s.historyForState(state).Persistent() {
		s.shellMu.Unlock()
		cancel()
		return domain.SSHShell{}, store.ErrNotFound
	}
	state.mu.Lock()
	if !operatorShellReconnectable(state.shell) {
		status := state.shell.Status
		state.mu.Unlock()
		s.shellMu.Unlock()
		cancel()
		return domain.SSHShell{}, fmt.Errorf("%w: interactive shell %q is %s", ErrSSHShellReconnectConflict, id, status)
	}
	activeTotal, activeHost := 0, 0
	for currentID, current := range s.shells {
		if currentID == id {
			continue
		}
		current.mu.Lock()
		active := shellStatusActive(current.shell.Status)
		hostMatch := current.shell.HostID == state.shell.HostID
		current.mu.Unlock()
		if active {
			activeTotal++
			if hostMatch {
				activeHost++
			}
		}
	}
	if activeTotal >= maxActiveSSHShells || activeHost >= maxActiveSSHShellsPerHost {
		state.mu.Unlock()
		s.shellMu.Unlock()
		cancel()
		return domain.SSHShell{}, fmt.Errorf("interactive shell limit reached")
	}
	state.generation++
	generation := state.generation
	state.cancel = cancel
	state.session = nil
	state.closing = false
	state.reason = ""
	state.secretPrompt = false
	state.ansiStripper = terminaltext.Stripper{}
	state.shell.Status = "starting"
	state.shell.ExitCode = nil
	state.shell.TerminationReason = ""
	state.shell.Error = ""
	state.shell.EndedAt = time.Time{}
	shell := state.shell
	state.mu.Unlock()
	s.shellMu.Unlock()
	if err := state.history.Update(context.WithoutCancel(ctx), shell); err != nil {
		s.failSSHShellStart(state, generation, err, false)
		return domain.SSHShell{}, err
	}
	s.appendSSHShellEvent(state, "status", "", "starting")
	state.mu.Lock()
	shell = state.shell
	state.mu.Unlock()
	s.publishShellState(shell, false)

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

func operatorShellReconnectable(shell domain.SSHShell) bool {
	if shell.Surface != domain.SSHShellSurfaceQuick && shell.Surface != domain.SSHShellSurfaceWorkspace && shell.Surface != domain.WorkspaceShellSurfaceOperator {
		return false
	}
	if shell.Status != "failed" {
		return false
	}
	return shell.TerminationReason == "connection_lost" || shell.TerminationReason == "process_lost"
}
