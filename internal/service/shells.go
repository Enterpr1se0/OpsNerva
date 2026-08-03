package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/ids"
	"eino-ops-agent/internal/observability"
	"eino-ops-agent/internal/sshx"
	"eino-ops-agent/internal/store"
	"eino-ops-agent/internal/terminaltext"
)

const (
	maxActiveSSHShells          = 8
	maxActiveSSHShellsPerHost   = 2
	maxSSHShellInputBytes       = 64 << 10
	maxSSHShellReasonBytes      = 500
	maxSSHShellRecentBytes      = 16 << 10
	maxSSHShellOutputEventBytes = 4 << 10
	defaultShellQueryDelay      = time.Duration(domain.DefaultShellQueryDelaySeconds) * time.Second
	maxShellQueryDelay          = time.Duration(domain.MaxShellQueryDelaySeconds) * time.Second
)

type sshShellState struct {
	mu             sync.Mutex
	eventMu        sync.Mutex
	shell          domain.SSHShell
	session        sshx.ShellSession
	cancel         context.CancelFunc
	closing        bool
	reason         string
	secrets        []string
	pending        map[string]string
	notify         chan struct{}
	recentOutput   string
	ansiStripper   terminaltext.Stripper
	secretPrompt   bool
	outputOwner    executionOwner
	responseCursor uint64
}

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

func (s *Service) ListSSHShells(ctx context.Context, sessionID string, activeOnly bool, reason, actor string) (domain.SSHShellList, error) {
	shells, err := s.store.ListSSHShells(ctx, strings.TrimSpace(sessionID), activeOnly)
	if err != nil {
		return domain.SSHShellList{}, err
	}
	if reason = strings.TrimSpace(reason); reason != "" {
		if len(reason) > maxSSHShellReasonBytes {
			return domain.SSHShellList{}, fmt.Errorf("reason must not exceed %d bytes", maxSSHShellReasonBytes)
		}
		s.audit(context.WithoutCancel(ctx), "", "ssh_shell_list", actor, map[string]any{
			"session_id": strings.TrimSpace(sessionID), "active_only": activeOnly, "reason": s.redactor.Redact(reason),
		})
	}
	return domain.SSHShellList{Shells: shells, Count: len(shells)}, nil
}

func (s *Service) GetSSHShellSnapshot(ctx context.Context, id, expectedSessionID string, after uint64, wait time.Duration, coalesce bool, reason, actor string) (domain.SSHShellSnapshot, error) {
	snapshot, _, err := s.getSSHShellSnapshotPage(ctx, id, expectedSessionID, after, wait, 0, coalesce, reason, actor)
	return snapshot, err
}

func (s *Service) getSSHShellSnapshotPage(ctx context.Context, id, expectedSessionID string, after uint64, wait time.Duration, maxOutputBytes int, coalesce bool, reason, actor string) (domain.SSHShellSnapshot, bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return domain.SSHShellSnapshot{}, false, fmt.Errorf("shell_id is required")
	}
	shell, err := s.store.GetSSHShell(ctx, id)
	if err != nil {
		return domain.SSHShellSnapshot{}, false, err
	}
	if expectedSessionID != "" && shell.SessionID != expectedSessionID {
		return domain.SSHShellSnapshot{}, false, store.ErrNotFound
	}
	if after > shell.LastSequence {
		return domain.SSHShellSnapshot{}, false, fmt.Errorf("invalid after_sequence: must not exceed the shell's last sequence")
	}
	events, hasMore, err := s.store.ListSSHShellEventsPage(ctx, id, after, maxOutputBytes)
	if err != nil {
		return domain.SSHShellSnapshot{}, false, err
	}
	if len(events) == 0 && wait > 0 && shellStatusActive(shell.Status) {
		if wait > 30*time.Second {
			wait = 30 * time.Second
		}
		var notify <-chan struct{}
		s.shellMu.RLock()
		state := s.shells[id]
		s.shellMu.RUnlock()
		if state != nil {
			state.mu.Lock()
			notify = state.notify
			state.mu.Unlock()
		}
		if notify != nil {
			timer := time.NewTimer(wait)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return domain.SSHShellSnapshot{}, false, ctx.Err()
			case <-timer.C:
			case <-notify:
			}
			shell, err = s.store.GetSSHShell(ctx, id)
			if err != nil {
				return domain.SSHShellSnapshot{}, false, err
			}
			events, hasMore, err = s.store.ListSSHShellEventsPage(ctx, id, after, maxOutputBytes)
			if err != nil {
				return domain.SSHShellSnapshot{}, false, err
			}
		}
	}
	recent, err := s.store.GetSSHShellRecentOutput(ctx, id)
	if err != nil {
		return domain.SSHShellSnapshot{}, false, err
	}
	if coalesce {
		events = coalesceSSHShellEvents(events)
	}
	if reason = strings.TrimSpace(reason); reason != "" {
		if len(reason) > maxSSHShellReasonBytes {
			return domain.SSHShellSnapshot{}, false, fmt.Errorf("reason must not exceed %d bytes", maxSSHShellReasonBytes)
		}
		s.audit(context.WithoutCancel(ctx), shell.RunID, interactiveShellComponent(shell.Kind)+"_output", actor, map[string]any{
			"shell_id": shell.ID, "after_sequence": after, "coalesce": coalesce, "reason": s.redactor.Redact(reason),
		})
	}
	nextSequence := shell.LastSequence
	if maxOutputBytes > 0 {
		nextSequence = after
		if len(events) > 0 {
			nextSequence = events[len(events)-1].Sequence
		}
	} else if len(events) > 0 && events[len(events)-1].Sequence > nextSequence {
		nextSequence = events[len(events)-1].Sequence
	}
	return domain.SSHShellSnapshot{
		Shell: shell, Events: events, RecentOutput: recent, NextSequence: nextSequence,
	}, hasMore, nil
}

// ReadableSSHShellSnapshot removes terminal control sequences for a model-facing
// snapshot. It replays earlier raw output into the parser so an incremental
// request remains correct when its first event begins in the middle of an ANSI
// sequence. Web terminal snapshots continue to use the untouched raw events.
func (s *Service) ReadableSSHShellSnapshot(ctx context.Context, snapshot domain.SSHShellSnapshot, after uint64) (domain.SSHShellSnapshot, error) {
	if snapshot.Shell.ID == "" {
		return snapshot, nil
	}
	needsReplay := false
	for index := range snapshot.Events {
		event := &snapshot.Events[index]
		if event.Stream != "stdout" && event.Stream != "stderr" {
			continue
		}
		if event.ReadableContent == nil {
			needsReplay = true
			break
		}
	}
	if !needsReplay {
		for index := range snapshot.Events {
			event := &snapshot.Events[index]
			if (event.Stream == "stdout" || event.Stream == "stderr") && event.ReadableContent != nil {
				event.Content = *event.ReadableContent
			}
		}
		return snapshot, nil
	}
	previous, err := s.store.ListSSHShellEvents(ctx, snapshot.Shell.ID, 0)
	if err != nil {
		return snapshot, err
	}
	var stripper terminaltext.Stripper
	for _, event := range previous {
		if event.Sequence > after {
			break
		}
		if event.Stream == "stdout" || event.Stream == "stderr" {
			_ = stripper.WriteString(event.Content)
		}
	}
	for index := range snapshot.Events {
		if snapshot.Events[index].Stream == "stdout" || snapshot.Events[index].Stream == "stderr" {
			snapshot.Events[index].Content = stripper.WriteString(snapshot.Events[index].Content)
		}
	}
	return snapshot, nil
}

func (s *Service) WriteSSHShell(ctx context.Context, id, expectedSessionID, input, reason, actor string) (domain.SSHShellSnapshot, error) {
	page, err := s.WriteSSHShellPage(ctx, id, expectedSessionID, input, defaultShellQueryDelay, 0, reason, actor)
	return page.Snapshot, err
}

func (s *Service) WriteSSHShellPage(ctx context.Context, id, expectedSessionID, input string, queryDelay time.Duration, maxOutputBytes int, reason, actor string) (domain.SSHShellOutputPage, error) {
	if err := validateShellQueryDelay(queryDelay); err != nil {
		return domain.SSHShellOutputPage{}, err
	}
	_, before, err := s.liveSSHShell(id, expectedSessionID)
	if err != nil {
		return domain.SSHShellOutputPage{}, err
	}
	if err := s.SendSSHShellInput(ctx, id, expectedSessionID, input, reason, actor); err != nil {
		return domain.SSHShellOutputPage{}, err
	}
	if err := waitShellQueryDelay(ctx, queryDelay); err != nil {
		return domain.SSHShellOutputPage{}, err
	}
	snapshot, hasMore, err := s.getSSHShellSnapshotPage(ctx, id, expectedSessionID, before, 0, maxOutputBytes, false, "", "")
	page := domain.SSHShellOutputPage{Snapshot: snapshot, HasMore: hasMore}
	if err == nil && (actor == "eino-agent" || actor == "mcp-client") {
		s.markSSHShellResponseRead(id, expectedSessionID, page.Snapshot.NextSequence)
	}
	return page, err
}

func (s *Service) WaitSSHShellOutput(ctx context.Context, id, expectedSessionID, reason, actor string) (domain.SSHShellSnapshot, error) {
	page, err := s.QuerySSHShellOutput(ctx, id, expectedSessionID, nil, defaultShellQueryDelay, 0, reason, actor)
	return page.Snapshot, err
}

func (s *Service) QuerySSHShellOutput(ctx context.Context, id, expectedSessionID string, afterSequence *uint64, queryDelay time.Duration, maxOutputBytes int, reason, actor string) (domain.SSHShellOutputPage, error) {
	if err := validateShellQueryDelay(queryDelay); err != nil {
		return domain.SSHShellOutputPage{}, err
	}
	var after uint64
	var err error
	if afterSequence == nil {
		after, err = s.sshShellResponseCursor(ctx, id, expectedSessionID)
	} else {
		after = *afterSequence
	}
	if err != nil {
		return domain.SSHShellOutputPage{}, err
	}
	if _, _, err := s.getSSHShellSnapshotPage(ctx, id, expectedSessionID, after, 0, maxOutputBytes, false, "", ""); err != nil {
		return domain.SSHShellOutputPage{}, err
	}
	s.setSSHShellOutputOwner(ctx, id, expectedSessionID, actor)
	if err := waitShellQueryDelay(ctx, queryDelay); err != nil {
		return domain.SSHShellOutputPage{}, err
	}
	snapshot, hasMore, err := s.getSSHShellSnapshotPage(ctx, id, expectedSessionID, after, 0, maxOutputBytes, false, reason, actor)
	page := domain.SSHShellOutputPage{Snapshot: snapshot, HasMore: hasMore}
	if err == nil && (actor == "eino-agent" || actor == "mcp-client") {
		s.markSSHShellResponseRead(id, expectedSessionID, page.Snapshot.NextSequence)
	}
	return page, err
}

func (s *Service) sshShellResponseCursor(ctx context.Context, id, expectedSessionID string) (uint64, error) {
	shell, err := s.store.GetSSHShell(ctx, strings.TrimSpace(id))
	if err != nil {
		return 0, err
	}
	if expectedSessionID != "" && shell.SessionID != expectedSessionID {
		return 0, store.ErrNotFound
	}
	cursor, err := s.store.LastSSHShellAgentInputSequence(ctx, shell.ID)
	if err != nil {
		return 0, err
	}
	if shell.ResponseSequence > cursor {
		cursor = shell.ResponseSequence
	}
	s.shellMu.RLock()
	state := s.shells[shell.ID]
	s.shellMu.RUnlock()
	if state != nil {
		state.mu.Lock()
		if state.responseCursor > cursor {
			cursor = state.responseCursor
		}
		state.mu.Unlock()
	}
	return cursor, nil
}

func (s *Service) markSSHShellResponseRead(id, expectedSessionID string, sequence uint64) {
	id = strings.TrimSpace(id)
	if err := s.store.AdvanceSSHShellResponseSequence(context.Background(), id, expectedSessionID, sequence); err != nil {
		observability.FromContext(context.Background()).ErrorContext(context.Background(), "persist SSH shell response cursor failed",
			"component", "ssh_shell", "shell_id", id, "sequence", sequence, "error", err)
	}
	s.shellMu.RLock()
	state := s.shells[id]
	s.shellMu.RUnlock()
	if state == nil {
		return
	}
	state.mu.Lock()
	if (expectedSessionID == "" || state.shell.SessionID == expectedSessionID) && sequence > state.responseCursor {
		state.responseCursor = sequence
	}
	state.mu.Unlock()
}

func (s *Service) setSSHShellOutputOwner(ctx context.Context, id, expectedSessionID, actor string) {
	s.shellMu.RLock()
	state := s.shells[strings.TrimSpace(id)]
	s.shellMu.RUnlock()
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if expectedSessionID != "" && state.shell.SessionID != expectedSessionID {
		return
	}
	if owner, ok := executionOwnerFromContext(ctx); ok && (actor == "eino-agent" || actor == "mcp-client") {
		state.outputOwner = owner
	} else if actor != "eino-agent" && actor != "mcp-client" {
		state.outputOwner = executionOwner{}
	}
}

func waitShellQueryDelay(ctx context.Context, delay time.Duration) error {
	if delay == 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func validateShellQueryDelay(delay time.Duration) error {
	if delay < 0 || delay > maxShellQueryDelay {
		return fmt.Errorf("wait_seconds must be between 0 and %d", int(maxShellQueryDelay/time.Second))
	}
	return nil
}

func (s *Service) SendSSHShellInput(ctx context.Context, id, expectedSessionID, input, reason, actor string) error {
	if input == "" {
		return fmt.Errorf("input is required")
	}
	if len(input) > maxSSHShellInputBytes || strings.ContainsRune(input, '\x00') {
		return fmt.Errorf("shell input must contain 1-%d bytes and no NUL characters", maxSSHShellInputBytes)
	}
	if s.redactor.Redact(input) != input {
		return fmt.Errorf("shell input appears to contain a credential; the operator must use the private Web terminal input")
	}
	state, _, err := s.liveSSHShell(id, expectedSessionID)
	if err != nil {
		return err
	}
	s.setSSHShellOutputOwner(ctx, id, expectedSessionID, actor)
	state.mu.Lock()
	secretPrompt := state.secretPrompt
	shell := state.shell
	state.mu.Unlock()
	if secretPrompt {
		return fmt.Errorf("the remote terminal is requesting a credential; wait for the operator to use the private Web terminal input")
	}
	if reason = strings.TrimSpace(reason); len(reason) > maxSSHShellReasonBytes {
		return fmt.Errorf("reason must not exceed %d bytes", maxSSHShellReasonBytes)
	}
	source := "operator"
	if actor == "eino-agent" || actor == "mcp-client" {
		source = "agent"
	}
	s.appendSSHShellInputEvent(state, source, s.redactor.Redact(input), false, len(input))
	if _, err := state.session.Write([]byte(input)); err != nil {
		return fmt.Errorf("write SSH shell input: %w", err)
	}
	s.audit(context.WithoutCancel(ctx), shell.RunID, interactiveShellComponent(shell.Kind)+"_input", actor, map[string]any{
		"shell_id": shell.ID, "host_id": shell.HostID, "input": s.redactor.Redact(input),
		"source": source, "reason": s.redactor.Redact(reason),
	})
	return nil
}

func (s *Service) WriteSensitiveSSHShellInput(ctx context.Context, id, input, actor string) error {
	if input == "" {
		return fmt.Errorf("sensitive input is required")
	}
	if len(input) > maxSSHShellInputBytes || strings.ContainsRune(input, '\x00') {
		return fmt.Errorf("sensitive shell input must contain 1-%d bytes and no NUL characters", maxSSHShellInputBytes)
	}
	state, _, err := s.liveSSHShell(id, "")
	if err != nil {
		return err
	}
	secret := strings.TrimRight(input, "\r\n")
	if secret != "" {
		state.eventMu.Lock()
		state.secrets = appendUniqueSecret(state.secrets, secret)
		state.eventMu.Unlock()
	}
	state.mu.Lock()
	shell := state.shell
	state.mu.Unlock()
	s.appendSSHShellInputEvent(state, "operator", "", true, len(input))
	if _, err := state.session.Write([]byte(input)); err != nil {
		return fmt.Errorf("write sensitive SSH shell input: %w", err)
	}
	s.audit(context.WithoutCancel(ctx), shell.RunID, interactiveShellComponent(shell.Kind)+"_sensitive_input", actor, map[string]any{
		"shell_id": shell.ID, "host_id": shell.HostID, "bytes": len(input), "source": "operator",
	})
	return nil
}

func (s *Service) ResizeSSHShell(ctx context.Context, id string, cols, rows int, actor string) (domain.SSHShell, error) {
	state, _, err := s.liveSSHShell(id, "")
	if err != nil {
		return domain.SSHShell{}, err
	}
	if err := state.session.Resize(cols, rows); err != nil {
		return domain.SSHShell{}, err
	}
	state.mu.Lock()
	state.shell.Cols, state.shell.Rows = cols, rows
	shell := state.shell
	state.mu.Unlock()
	if err := s.store.UpdateSSHShell(ctx, shell); err != nil {
		return domain.SSHShell{}, err
	}
	s.audit(context.WithoutCancel(ctx), shell.RunID, interactiveShellComponent(shell.Kind)+"_resized", actor, map[string]any{
		"shell_id": shell.ID, "cols": cols, "rows": rows,
	})
	return shell, nil
}

func (s *Service) InterruptSSHShell(ctx context.Context, id, expectedSessionID, reason, actor string) (domain.SSHShell, error) {
	state, _, err := s.liveSSHShell(id, expectedSessionID)
	if err != nil {
		return domain.SSHShell{}, err
	}
	if reason = strings.TrimSpace(reason); len(reason) > maxSSHShellReasonBytes {
		return domain.SSHShell{}, fmt.Errorf("reason must not exceed %d bytes", maxSSHShellReasonBytes)
	}
	source := "operator"
	if actor == "eino-agent" || actor == "mcp-client" {
		source = "agent"
	}
	s.appendSSHShellInputEvent(state, source, "\x03", false, 1)
	if err := state.session.Interrupt(); err != nil {
		return domain.SSHShell{}, err
	}
	state.mu.Lock()
	shell := state.shell
	state.mu.Unlock()
	s.audit(context.WithoutCancel(ctx), shell.RunID, interactiveShellComponent(shell.Kind)+"_interrupted", actor, map[string]any{
		"shell_id": shell.ID, "input": "Ctrl+C", "reason": s.redactor.Redact(reason),
	})
	return shell, nil
}

func (s *Service) CloseSSHShell(ctx context.Context, id, expectedSessionID, reason, actor string) (domain.SSHShell, error) {
	state, _, err := s.liveSSHShell(id, expectedSessionID)
	if err != nil {
		return domain.SSHShell{}, err
	}
	if reason = strings.TrimSpace(reason); len(reason) > maxSSHShellReasonBytes {
		return domain.SSHShell{}, fmt.Errorf("reason must not exceed %d bytes", maxSSHShellReasonBytes)
	}
	state.mu.Lock()
	if !state.closing {
		state.closing = true
		state.reason = "requested_close"
		state.shell.Status = "stopping"
	}
	shell := state.shell
	cancel := state.cancel
	state.mu.Unlock()
	_ = s.store.UpdateSSHShell(context.WithoutCancel(ctx), shell)
	cancel()
	_ = state.session.Close()
	s.audit(context.WithoutCancel(ctx), shell.RunID, interactiveShellComponent(shell.Kind)+"_close_requested", actor, map[string]any{
		"shell_id": shell.ID, "host_id": shell.HostID, "reason": s.redactor.Redact(reason),
	})
	return shell, nil
}

func (s *Service) openSSHShell(ctx context.Context, host domain.Host, connection sshx.ConnectionSpec, req domain.ExecRequest, run domain.Run, actor string) (domain.SSHShell, error) {
	transport, ok := s.transport.(sshx.InteractiveTransport)
	if !ok {
		return domain.SSHShell{}, fmt.Errorf("configured SSH transport does not support interactive PTY sessions")
	}
	if err := validateSSHShellRequest(req); err != nil {
		return domain.SSHShell{}, err
	}
	secrets := []string(nil)
	if req.Elevated && connection.Target.SudoPassword != "" {
		secrets = append(secrets, connection.Target.SudoPassword)
	}
	return s.openInteractiveShell(ctx, host, req, run, actor, interactiveShellOptions{
		kind: domain.SSHShellKindSSH, user: host.User, secrets: secrets,
	}, func(shellCtx context.Context, output func(string, []byte)) (sshx.ShellSession, error) {
		return transport.OpenShell(shellCtx, connection, req, req.ShellCols, req.ShellRows, output)
	})
}

type interactiveShellOptions struct {
	kind        string
	workspaceID string
	backend     string
	user        string
	secrets     []string
}

func (s *Service) openInteractiveShell(
	ctx context.Context,
	host domain.Host,
	req domain.ExecRequest,
	run domain.Run,
	actor string,
	options interactiveShellOptions,
	opener func(context.Context, func(string, []byte)) (sshx.ShellSession, error),
) (domain.SSHShell, error) {
	s.shellMu.Lock()
	activeTotal, activeHost := 0, 0
	for _, current := range s.shells {
		current.mu.Lock()
		active := shellStatusActive(current.shell.Status)
		hostMatch := current.shell.HostID == host.ID
		current.mu.Unlock()
		if active {
			activeTotal++
			if hostMatch {
				activeHost++
			}
		}
	}
	if activeTotal >= maxActiveSSHShells || activeHost >= maxActiveSSHShellsPerHost {
		s.shellMu.Unlock()
		return domain.SSHShell{}, fmt.Errorf("interactive shell limit reached")
	}
	started := time.Now().UTC()
	shellCtx, cancel := context.WithCancel(s.executionCtx)
	surface := req.ShellSurface
	if surface == "" {
		if options.kind == domain.SSHShellKindWorkspace && run.SessionID == "" {
			surface = domain.WorkspaceShellSurfaceOperator
		} else if options.kind == domain.SSHShellKindWorkspace {
			surface = domain.WorkspaceShellSurfaceAgent
		} else if run.SessionID == mcpClientSessionID {
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
		cancel: cancel, pending: make(map[string]string), notify: make(chan struct{}),
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
	s.shells[state.shell.ID] = state
	s.shellMu.Unlock()
	if err := s.store.CreateSSHShell(ctx, state.shell); err != nil {
		cancel()
		s.shellMu.Lock()
		delete(s.shells, state.shell.ID)
		s.shellMu.Unlock()
		return domain.SSHShell{}, err
	}

	s.executionMu.Lock()
	if s.executionClosed {
		s.executionMu.Unlock()
		cancel()
		s.failSSHShellStart(state, fmt.Errorf("service is shutting down"))
		return domain.SSHShell{}, fmt.Errorf("service is shutting down")
	}
	s.executionWG.Add(1)
	s.executionMu.Unlock()
	workerStarted := false
	defer func() {
		if !workerStarted {
			s.executionWG.Done()
		}
	}()

	interactive, err := opener(shellCtx, func(stream string, data []byte) {
		s.appendSSHShellOutput(state, stream, data)
	})
	if err != nil {
		cancel()
		s.failSSHShellStart(state, err)
		return domain.SSHShell{}, err
	}
	state.mu.Lock()
	state.session = interactive
	state.shell.Status = "running"
	shell := state.shell
	state.mu.Unlock()
	if err := s.store.UpdateSSHShell(ctx, shell); err != nil {
		cancel()
		_ = interactive.Close()
		s.failSSHShellStart(state, err)
		return domain.SSHShell{}, err
	}
	s.appendSSHShellEvent(state, "status", "", "running")
	workerStarted = true
	go s.runSSHShell(shellCtx, state)
	component := interactiveShellComponent(options.kind)
	s.audit(context.WithoutCancel(ctx), run.ID, component+"_started", actor, map[string]any{
		"shell_id": shell.ID, "host_id": shell.HostID, "elevated": shell.Elevated,
		"workspace_id": shell.WorkspaceID, "cwd": shell.Cwd,
	})
	observability.FromContext(ctx).InfoContext(ctx, "interactive shell started",
		"component", component, "shell_id", shell.ID, "run_id", run.ID,
		"session_id", run.SessionID, "host_id", host.ID, "elevated", shell.Elevated)
	return shell, nil
}

func interactiveShellComponent(kind string) string {
	if kind == domain.SSHShellKindWorkspace {
		return "workspace_shell"
	}
	return "ssh_shell"
}

func (s *Service) runSSHShell(ctx context.Context, state *sshShellState) {
	defer s.executionWG.Done()
	done := make(chan sshx.ShellExit, 1)
	go func() {
		done <- state.session.Wait()
	}()

	var result sshx.ShellExit
	select {
	case result = <-done:
	case <-ctx.Done():
		_ = state.session.Close()
		result = <-done
	}
	state.eventMu.Lock()
	s.flushSSHShellPendingLocked(state)
	state.eventMu.Unlock()

	state.mu.Lock()
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
	state.shell.Status = status
	state.shell.ExitCode = exitCode
	state.shell.TerminationReason = termination
	state.shell.EndedAt = time.Now().UTC()
	if status == "failed" {
		state.shell.Error = s.redactor.Redact(result.Err.Error())
	}
	shell := state.shell
	state.mu.Unlock()
	s.appendSSHShellEvent(state, "status", "", status)
	state.mu.Lock()
	shell = state.shell
	state.mu.Unlock()
	_ = s.store.UpdateSSHShell(context.Background(), shell)
	state.cancel()
	_ = state.session.Close()
	s.shellMu.Lock()
	if current := s.shells[shell.ID]; current == state {
		delete(s.shells, shell.ID)
	}
	s.shellMu.Unlock()
	s.audit(context.Background(), shell.RunID, interactiveShellComponent(shell.Kind)+"_stopped", "control-plane", map[string]any{
		"shell_id": shell.ID, "host_id": shell.HostID, "status": status,
		"termination_reason": termination, "exit_code": exitCode,
	})
}

func (s *Service) failSSHShellStart(state *sshShellState, cause error) {
	state.mu.Lock()
	state.shell.Status = "failed"
	state.shell.TerminationReason = "start_failed"
	state.shell.Error = s.redactor.Redact(cause.Error())
	state.shell.EndedAt = time.Now().UTC()
	shell := state.shell
	state.mu.Unlock()
	s.appendSSHShellEvent(state, "status", "", "failed")
	state.mu.Lock()
	shell = state.shell
	state.mu.Unlock()
	_ = s.store.UpdateSSHShell(context.Background(), shell)
	s.shellMu.Lock()
	if current := s.shells[shell.ID]; current == state {
		delete(s.shells, shell.ID)
	}
	s.shellMu.Unlock()
}

func (s *Service) appendSSHShellOutput(state *sshShellState, stream string, data []byte) {
	if len(data) == 0 {
		return
	}
	if stream != "stderr" {
		stream = "stdout"
	}
	state.eventMu.Lock()
	combined := state.pending[stream] + string(data)
	safeEnd := len(combined)
	for _, secret := range state.secrets {
		maxPrefix := len(secret) - 1
		if maxPrefix > len(combined) {
			maxPrefix = len(combined)
		}
		for size := maxPrefix; size > 0; size-- {
			if strings.HasSuffix(combined, secret[:size]) && len(combined)-size < safeEnd {
				safeEnd = len(combined) - size
				break
			}
		}
	}
	state.pending[stream] = combined[safeEnd:]
	content := redactKnownSecrets(s.redactor.Redact(combined[:safeEnd]), state.secrets)
	readable := ""
	if content != "" {
		readable = s.appendSSHShellOutputEventsLocked(state, stream, content)
	}
	state.eventMu.Unlock()
	if readable == "" {
		return
	}
	state.mu.Lock()
	shell := state.shell
	owner := state.outputOwner
	state.mu.Unlock()
	if owner.ToolCallID == "" && owner.ToolName == "" {
		return
	}
	s.publishExecutionEvent(ExecutionEvent{
		SessionID: shell.SessionID, RunID: shell.ID,
		ToolCallID: owner.ToolCallID, ToolName: owner.ToolName,
		Stream: stream, Content: readable, Status: "running",
	})
}

func (s *Service) appendSSHShellInputEvent(state *sshShellState, source, content string, sensitive bool, inputBytes int) {
	state.eventMu.Lock()
	defer state.eventMu.Unlock()
	s.appendSSHShellEventValueLocked(state, domain.SSHShellEvent{
		Stream: "input", Source: source, Content: content, Sensitive: sensitive,
		InputBytes: inputBytes,
	})
}

func (s *Service) appendSSHShellEvent(state *sshShellState, stream, content, status string) {
	state.eventMu.Lock()
	defer state.eventMu.Unlock()
	s.appendSSHShellEventLocked(state, stream, content, status)
}

func (s *Service) appendSSHShellEventLocked(state *sshShellState, stream, content, status string) {
	s.appendSSHShellEventValueLocked(state, domain.SSHShellEvent{
		Stream: stream, Content: content, Status: status,
	})
}

func (s *Service) appendSSHShellEventValueLocked(state *sshShellState, event domain.SSHShellEvent) {
	state.mu.Lock()
	state.shell.LastSequence++
	event.ShellID = state.shell.ID
	event.Sequence = state.shell.LastSequence
	event.CreatedAt = time.Now().UTC()
	recent := state.recentOutput
	state.mu.Unlock()
	if err := s.store.AppendSSHShellEvent(context.Background(), event, recent); err != nil {
		observability.FromContext(context.Background()).ErrorContext(context.Background(), "persist SSH shell output failed",
			"component", "ssh_shell", "shell_id", event.ShellID, "sequence", event.Sequence, "error", err)
		return
	}
	state.mu.Lock()
	close(state.notify)
	state.notify = make(chan struct{})
	state.mu.Unlock()
}

func (s *Service) flushSSHShellPendingLocked(state *sshShellState) {
	streams := make([]string, 0, len(state.pending))
	for stream := range state.pending {
		streams = append(streams, stream)
	}
	sort.Strings(streams)
	for _, stream := range streams {
		content := redactKnownSecrets(s.redactor.Redact(state.pending[stream]), state.secrets)
		state.pending[stream] = ""
		if content != "" {
			s.appendSSHShellOutputEventsLocked(state, stream, content)
		}
	}
}

func (s *Service) appendSSHShellOutputEventsLocked(state *sshShellState, stream, content string) string {
	var combined strings.Builder
	for content != "" {
		end := len(content)
		if end > maxSSHShellOutputEventBytes {
			end = maxSSHShellOutputEventBytes
			for end > 0 && !utf8.RuneStart(content[end]) {
				end--
			}
			if end == 0 {
				end = maxSSHShellOutputEventBytes
			}
		}
		part := content[:end]
		content = content[end:]
		readable := updateSSHShellOutputState(state, part)
		combined.WriteString(readable)
		s.appendSSHShellEventValueLocked(state, domain.SSHShellEvent{
			Stream: stream, Content: part, ReadableContent: &readable, Status: "running",
		})
	}
	return combined.String()
}

func (s *Service) liveSSHShell(id, expectedSessionID string) (*sshShellState, uint64, error) {
	id = strings.TrimSpace(id)
	s.shellMu.RLock()
	state := s.shells[id]
	s.shellMu.RUnlock()
	if state == nil {
		return nil, 0, store.ErrNotFound
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if expectedSessionID != "" && state.shell.SessionID != expectedSessionID {
		return nil, 0, store.ErrNotFound
	}
	if state.shell.Status != "running" || state.session == nil {
		return nil, 0, fmt.Errorf("interactive shell %q is %s", id, state.shell.Status)
	}
	return state, state.shell.LastSequence, nil
}

func (s *Service) hasActiveSSHShellForHost(hostID string) bool {
	s.shellMu.RLock()
	defer s.shellMu.RUnlock()
	for _, state := range s.shells {
		state.mu.Lock()
		active := state.shell.HostID == hostID && shellStatusActive(state.shell.Status)
		state.mu.Unlock()
		if active {
			return true
		}
	}
	return false
}

func (s *Service) hasActiveSSHShellForSession(sessionID string) bool {
	s.shellMu.RLock()
	defer s.shellMu.RUnlock()
	for _, state := range s.shells {
		state.mu.Lock()
		active := state.shell.SessionID == sessionID && shellStatusActive(state.shell.Status)
		state.mu.Unlock()
		if active {
			return true
		}
	}
	return false
}

func (s *Service) hasActiveWorkspaceShell(workspaceID string) bool {
	s.shellMu.RLock()
	defer s.shellMu.RUnlock()
	for _, state := range s.shells {
		state.mu.Lock()
		active := state.shell.Kind == domain.SSHShellKindWorkspace && state.shell.WorkspaceID == workspaceID && shellStatusActive(state.shell.Status)
		state.mu.Unlock()
		if active {
			return true
		}
	}
	return false
}

func (s *Service) hasActiveWorkspaceShellForSession(sessionID string) bool {
	s.shellMu.RLock()
	defer s.shellMu.RUnlock()
	for _, state := range s.shells {
		state.mu.Lock()
		active := state.shell.Kind == domain.SSHShellKindWorkspace && state.shell.SessionID == sessionID && shellStatusActive(state.shell.Status)
		state.mu.Unlock()
		if active {
			return true
		}
	}
	return false
}

func (s *Service) hasAnyActiveWorkspaceShell() bool {
	s.shellMu.RLock()
	defer s.shellMu.RUnlock()
	for _, state := range s.shells {
		state.mu.Lock()
		active := state.shell.Kind == domain.SSHShellKindWorkspace && shellStatusActive(state.shell.Status)
		state.mu.Unlock()
		if active {
			return true
		}
	}
	return false
}

func shellStatusActive(status string) bool {
	switch status {
	case "starting", "running", "stopping":
		return true
	default:
		return false
	}
}

func appendUniqueSecret(values []string, secret string) []string {
	if secret == "" {
		return values
	}
	for _, value := range values {
		if value == secret {
			return values
		}
	}
	return append(values, secret)
}

func redactKnownSecrets(content string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			content = strings.ReplaceAll(content, secret, "[REDACTED]")
		}
	}
	return content
}

func updateSSHShellOutputState(state *sshShellState, content string) string {
	state.mu.Lock()
	readable := state.ansiStripper.WriteString(content)
	recent := appendReadableSSHShellText(state.recentOutput, readable)
	state.recentOutput = recent
	line := recent
	if index := strings.LastIndexAny(line, "\r\n"); index >= 0 {
		line = line[index+1:]
	}
	line = strings.ToLower(strings.TrimSpace(line))
	state.secretPrompt = strings.HasSuffix(line, ":") &&
		(strings.Contains(line, "password") || strings.Contains(line, "passphrase") ||
			strings.Contains(line, "token") || strings.Contains(line, "secret"))
	state.mu.Unlock()
	return readable
}

func appendReadableSSHShellOutput(previous, content string) string {
	return appendReadableSSHShellText(previous, terminaltext.Strip(content))
}

func appendReadableSSHShellText(previous, clean string) string {
	runes := []rune(previous)
	cleanRunes := []rune(clean)
	for index := 0; index < len(cleanRunes); index++ {
		switch cleanRunes[index] {
		case '\r':
			if index+1 < len(cleanRunes) && cleanRunes[index+1] == '\n' {
				runes = append(runes, '\n')
				index++
				continue
			}
			runes = append(runes, '\n')
		case '\b':
			if len(runes) > 0 && runes[len(runes)-1] != '\n' {
				runes = runes[:len(runes)-1]
			}
		default:
			runes = append(runes, cleanRunes[index])
		}
	}
	for len(string(runes)) > maxSSHShellRecentBytes && len(runes) > 0 {
		remove := len(runes) / 4
		if remove < 1 {
			remove = 1
		}
		runes = runes[remove:]
	}
	return string(runes)
}

func coalesceSSHShellEvents(events []domain.SSHShellEvent) []domain.SSHShellEvent {
	if len(events) == 0 {
		return events
	}
	result := make([]domain.SSHShellEvent, 0, len(events))
	for index := 0; index < len(events); {
		merged := events[index]
		merged.FirstSequence = merged.Sequence
		var content strings.Builder
		content.WriteString(merged.Content)
		next := index + 1
		for next < len(events) &&
			merged.Stream == events[next].Stream &&
			merged.Source == events[next].Source &&
			merged.Sensitive == events[next].Sensitive &&
			merged.Status == events[next].Status {
			content.WriteString(events[next].Content)
			merged.InputBytes += events[next].InputBytes
			merged.Sequence = events[next].Sequence
			merged.CreatedAt = events[next].CreatedAt
			next++
		}
		merged.Content = content.String()
		result = append(result, merged)
		index = next
	}
	return result
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
