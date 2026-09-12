package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/ids"
	"github.com/Enterpr1se0/opsnerva/internal/observability"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"
	"github.com/Enterpr1se0/opsnerva/internal/store"
)

type interactiveShellOpener func(context.Context, func(string, []byte)) (sshx.ShellSession, error)

type interactiveShellOptions struct {
	kind        string
	workspaceID string
	backend     string
	user        string
	secrets     []string
	transient   bool
}

func (s *Service) openInteractiveShell(
	ctx context.Context,
	host domain.Host,
	req domain.ExecRequest,
	run domain.Run,
	actor string,
	options interactiveShellOptions,
	opener interactiveShellOpener,
) (domain.SSHShell, error) {
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
	started := time.Now().UTC()
	surface := req.ShellSurface
	if surface == "" {
		if options.kind == domain.SSHShellKindWorkspace && run.SessionID == "" {
			surface = domain.WorkspaceShellSurfaceOperator
		} else if options.kind == domain.SSHShellKindWorkspace {
			surface = domain.WorkspaceShellSurfaceAgent
		} else if owner, ok := executionOwnerFromContext(ctx); ok && owner.Source == "mcp" {
			surface = domain.SSHShellSurfaceMCP
		} else if run.SessionID == "" {
			surface = domain.SSHShellSurfaceQuick
		} else {
			surface = domain.SSHShellSurfaceAgent
		}
	}
	state := &sshShellState{
		shell: domain.SSHShell{
			ID: ids.New("shell"), RunID: run.ID, SessionID: run.SessionID, Kind: options.kind, Surface: surface,
			HostID: host.ID, HostName: host.Name, User: host.User, Elevated: req.Elevated,
			WorkspaceID: options.workspaceID, Backend: options.backend,
			Cwd: req.Cwd, Status: "starting", Cols: req.ShellCols, Rows: req.ShellRows,
			StartedAt: started,
		},
		cancel: cancel, generation: 1, pending: make(map[string]string), notify: make(chan struct{}),
	}
	if options.transient {
		state.history = newMemoryShellHistory(state.shell)
	} else {
		state.history = &persistentShellHistory{store: s.store, shellID: state.shell.ID}
	}
	if owner, ok := executionOwnerFromContext(ctx); ok {
		state.outputOwner = owner
	}
	if options.user != "" {
		state.shell.User = options.user
	}
	for _, secret := range options.secrets {
		state.secrets = appendUniqueSecret(state.secrets, secret)
	}
	state.writer = state.newEventWriter()
	state.lifecycleMu.Lock()
	if err := s.shells.add(state); err != nil {
		state.lifecycleMu.Unlock()
		cancel()
		return domain.SSHShell{}, err
	}
	if err := state.history.Create(ctx, state.shell); err != nil {
		cancel()
		s.shells.remove(state)
		state.lifecycleMu.Unlock()
		return domain.SSHShell{}, err
	}
	s.publishShellState(state.shell, false)
	state.lifecycleMu.Unlock()
	shell, err := s.startInteractiveShellGeneration(ctx, shellCtx, state, opener, true)
	if err != nil {
		return domain.SSHShell{}, err
	}
	running = true
	component := interactiveShellComponent(options.kind)
	if state.history.Persistent() {
		s.audit(context.WithoutCancel(ctx), run.ID, component+"_started", actor, map[string]any{
			"shell_id": shell.ID, "host_id": shell.HostID, "elevated": shell.Elevated,
			"workspace_id": shell.WorkspaceID, "cwd": shell.Cwd,
		})
	}
	observability.FromContext(ctx).InfoContext(ctx, "interactive shell started",
		"component", component, "shell_id", shell.ID, "run_id", run.ID,
		"session_id", run.SessionID, "host_id", host.ID, "elevated", shell.Elevated)
	return shell, nil
}

func (s *Service) startInteractiveShellGeneration(ctx, shellCtx context.Context, state *sshShellState, opener interactiveShellOpener, removeOnFailure bool) (domain.SSHShell, error) {
	state.mu.Lock()
	generation := state.generation
	state.mu.Unlock()
	interactive, err := opener(shellCtx, func(stream string, data []byte) {
		s.appendSSHShellGenerationOutput(state, generation, stream, data)
	})
	if err != nil {
		s.failSSHShellStart(state, generation, err, removeOnFailure)
		return domain.SSHShell{}, err
	}
	state.lifecycleMu.Lock()
	state.mu.Lock()
	valid := state.generation == generation && state.shell.Status == "starting" && shellCtx.Err() == nil
	shell := state.shell
	state.mu.Unlock()
	if !valid {
		err = context.Canceled
	} else {
		// Persist the running projection before making the transport writable.
		// On error the logical state is still starting and follows one cleanup path.
		shell.Status = "running"
		err = state.history.Update(ctx, shell)
	}
	if err != nil {
		_ = interactive.Close()
		_ = interactive.Wait()
		state.lifecycleMu.Unlock()
		s.failSSHShellStart(state, generation, err, removeOnFailure)
		return domain.SSHShell{}, err
	}
	state.mu.Lock()
	state.session = interactive
	state.shell.Status = "running"
	state.mu.Unlock()
	s.appendSSHShellEvent(state, "status", "", "running")
	state.mu.Lock()
	shell = state.shell
	state.mu.Unlock()
	s.publishShellState(shell, false)
	state.lifecycleMu.Unlock()
	// The caller's preparation work is still counted, so shutdown cannot
	// finish while ownership is transferred to this child worker.
	s.shells.workers.Add(1)
	go s.runSSHShell(shellCtx, state, interactive, generation)
	return shell, nil
}

func (s *Service) CloseSSHShell(ctx context.Context, id, expectedSessionID, reason, actor string) (domain.SSHShell, error) {
	id = strings.TrimSpace(id)
	state := s.shells.get(id)
	if state == nil {
		return domain.SSHShell{}, store.ErrNotFound
	}
	state.lifecycleMu.Lock()
	defer state.lifecycleMu.Unlock()
	if reason = strings.TrimSpace(reason); len(reason) > maxSSHShellReasonBytes {
		return domain.SSHShell{}, fmt.Errorf("reason must not exceed %d bytes", maxSSHShellReasonBytes)
	}
	state.mu.Lock()
	if expectedSessionID != "" && state.shell.SessionID != expectedSessionID {
		state.mu.Unlock()
		return domain.SSHShell{}, store.ErrNotFound
	}
	status := state.shell.Status
	if status != "running" && status != "starting" && !operatorShellReconnectable(state.shell) {
		state.mu.Unlock()
		return domain.SSHShell{}, fmt.Errorf("interactive shell %q is %s", id, status)
	}
	state.closing = true
	state.reason = "requested_close"
	state.shell.Status = "stopping"
	shell := state.shell
	cancel := state.cancel
	session := state.session
	state.mu.Unlock()
	s.publishShellState(shell, false)
	history := state.history
	_ = history.Update(context.WithoutCancel(ctx), shell)
	if cancel != nil {
		cancel()
	}
	if session != nil {
		_ = session.Close()
	}
	if status != "running" {
		shell.Status = "closed"
		shell.TerminationReason = "requested_close"
		shell.ExitCode = nil
		shell.EndedAt = time.Now().UTC()
		shell = s.finishSSHShellLocked(state, state.generation, shell, true)
	}
	if history.Persistent() {
		s.audit(context.WithoutCancel(ctx), shell.RunID, interactiveShellComponent(shell.Kind)+"_close_requested", actor, map[string]any{
			"shell_id": shell.ID, "host_id": shell.HostID, "reason": s.redactor.Redact(reason),
		})
	}
	return shell, nil
}

func (s *Service) runSSHShell(ctx context.Context, state *sshShellState, session sshx.ShellSession, generation uint64) {
	defer s.shells.workers.Done()
	done := make(chan sshx.ShellExit, 1)
	go func() {
		done <- session.Wait()
	}()

	var result sshx.ShellExit
	select {
	case result = <-done:
	case <-ctx.Done():
		_ = session.Close()
		result = <-done
	}
	state.lifecycleMu.Lock()
	defer state.lifecycleMu.Unlock()
	state.mu.Lock()
	if state.generation != generation || state.session != session {
		state.mu.Unlock()
		return
	}
	status := "completed"
	termination := "remote_exit"
	if state.shell.Kind == domain.SSHShellKindWorkspace {
		termination = "process_exit"
	}
	exitCode := result.ExitCode
	if state.closing && state.reason == "requested_close" {
		status = "closed"
		termination = "requested_close"
		exitCode = nil
	} else if state.closing && state.reason == "persistence_failed" {
		status = "failed"
		termination = "persistence_failed"
		exitCode = nil
		result.Err = errors.New("interactive shell stopped because event persistence could not keep up")
	} else if ctx.Err() != nil {
		status = "interrupted"
		termination = "service_stopped"
		exitCode = nil
	} else if result.Err != nil {
		status = "failed"
		if result.Signal != "" {
			termination = "remote_signal"
			if state.shell.Kind == domain.SSHShellKindWorkspace {
				termination = "process_signal"
			}
		} else if result.ExitCode == nil {
			termination = "connection_lost"
			if state.shell.Kind == domain.SSHShellKindWorkspace {
				termination = "process_lost"
			}
		}
	} else if result.ExitCode == nil {
		status = "failed"
		termination = "connection_lost"
		if state.shell.Kind == domain.SSHShellKindWorkspace {
			termination = "process_lost"
		}
		result.Err = fmt.Errorf("interactive shell ended without an exit status")
	}
	shell := state.shell
	state.mu.Unlock()
	shell.Status, shell.TerminationReason, shell.ExitCode = status, termination, exitCode
	shell.EndedAt = time.Now().UTC()
	if status == "failed" {
		shell.Error = s.redactor.Redact(result.Err.Error())
	}
	_ = session.Close()
	shell = s.finishSSHShellLocked(state, generation, shell, false)
	history := state.history
	if history.Persistent() {
		s.audit(context.Background(), shell.RunID, interactiveShellComponent(shell.Kind)+"_stopped", "control-plane", map[string]any{
			"shell_id": shell.ID, "host_id": shell.HostID, "status": shell.Status,
			"termination_reason": shell.TerminationReason, "exit_code": shell.ExitCode,
		})
	}
}

func (s *Service) failSSHShellStart(state *sshShellState, generation uint64, cause error, remove bool) {
	state.lifecycleMu.Lock()
	defer state.lifecycleMu.Unlock()
	state.mu.Lock()
	if state.generation != generation || state.outputClosed {
		state.mu.Unlock()
		return
	}
	shell := state.shell
	reason := state.reason
	state.mu.Unlock()
	shell.Status = "failed"
	switch {
	case reason == "requested_close":
		shell.Status, shell.TerminationReason = "closed", "requested_close"
	case reason == "persistence_failed":
		shell.TerminationReason = "persistence_failed"
	case s.shells.ctx.Err() != nil:
		shell.Status, shell.TerminationReason = "interrupted", "service_stopped"
	case remove:
		shell.TerminationReason = "start_failed"
	case shell.Kind == domain.SSHShellKindWorkspace:
		shell.TerminationReason = "process_lost"
	default:
		shell.TerminationReason = "connection_lost"
	}
	if shell.Status == "failed" {
		shell.Error = s.redactor.Redact(cause.Error())
	}
	shell.EndedAt = time.Now().UTC()
	s.finishSSHShellLocked(state, generation, shell, remove)
}

// finishSSHShellLocked is the only generation finalizer. lifecycleMu prevents
// reconnect/close from racing terminal history, output drain and state deltas.
func (s *Service) finishSSHShellLocked(state *sshShellState, generation uint64, terminal domain.SSHShell, remove bool) domain.SSHShell {
	state.eventMu.Lock()
	defer state.eventMu.Unlock()
	state.mu.Lock()
	alreadyFinished := state.outputClosed
	if state.generation != generation || (alreadyFinished && terminal.Status != "closed") {
		shell := state.shell
		state.mu.Unlock()
		return shell
	}
	state.shell.Status, state.shell.TerminationReason = terminal.Status, terminal.TerminationReason
	state.shell.ExitCode, state.shell.Error, state.shell.EndedAt = terminal.ExitCode, terminal.Error, terminal.EndedAt
	state.session = nil
	cancel := state.cancel
	state.mu.Unlock()
	persistCtx, persistCancel := context.WithTimeout(context.Background(), shellPersistenceTimeout)
	defer persistCancel()
	var persistErr error
	if alreadyFinished {
		// Closing a retained operator terminal is a logical-history update,
		// not another connection generation or a restarted flush worker.
		state.mu.Lock()
		state.shell.LastSequence++
		event := domain.SSHShellEvent{ShellID: state.shell.ID, Sequence: state.shell.LastSequence,
			Stream: "status", Status: terminal.Status, CreatedAt: time.Now().UTC()}
		recent := state.recentOutput
		state.mu.Unlock()
		persistErr = state.history.Append(persistCtx, []domain.SSHShellEvent{event}, recent)
		s.publishSSHShellEvents(state, []domain.SSHShellEvent{event})
	} else {
		s.flushSSHShellPendingLocked(state)
		s.appendSSHShellEventLocked(state, "status", "", terminal.Status)
		state.mu.Lock()
		state.outputClosed = true
		state.mu.Unlock()
		persistErr = state.writer.close(persistCtx)
	}
	state.mu.Lock()
	if persistErr != nil {
		state.shell.Status, state.shell.TerminationReason = "failed", "persistence_failed"
		state.shell.Error = s.redactor.Redact(persistErr.Error())
	}
	state.cancel = nil
	shell := state.shell
	state.mu.Unlock()
	if err := state.history.Update(persistCtx, shell); err != nil {
		persistErr = errors.Join(persistErr, err)
		state.mu.Lock()
		state.shell.Status, state.shell.TerminationReason = "failed", "persistence_failed"
		state.shell.Error = s.redactor.Redact(persistErr.Error())
		shell = state.shell
		state.mu.Unlock()
	}
	if persistErr != nil {
		err := fmt.Errorf("finalize shell %s: %s", shell.ID, s.redactor.Redact(persistErr.Error()))
		s.shells.recordError(err)
		observability.FromContext(context.Background()).Error("finalize SSH shell history failed", "shell_id", shell.ID, "error", err)
	}
	state.mu.Lock()
	close(state.notify)
	state.notify = make(chan struct{})
	state.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	remove = remove || state.history.Persistent() || !operatorShellReconnectable(shell)
	if remove {
		s.shells.remove(state)
	}
	s.publishShellState(shell, remove)
	return shell
}
