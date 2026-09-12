package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"
	"github.com/Enterpr1se0/opsnerva/internal/store"
	"github.com/Enterpr1se0/opsnerva/internal/terminaltext"
)

const (
	maxActiveSSHShells        = 8
	maxActiveSSHShellsPerHost = 2
)

// shellRegistry owns logical shell membership and admission. Starting a shell
// and reserving a reconnect generation share one capacity check and lock.
// It also owns generation cancellation and shutdown accounting, but performs
// no transport I/O, persistence or event publication. Lifecycle changes take
// state.lifecycleMu before registry.mu; output takes state.eventMu before
// state.mu. Never enter the registry while holding state.mu.
type shellRegistry struct {
	mu      sync.RWMutex
	states  map[string]*sshShellState
	ctx     context.Context
	cancel  context.CancelFunc
	closed  bool
	workers sync.WaitGroup
	done    chan struct{}
	err     error
}

type sshShellState struct {
	lifecycleMu    sync.Mutex
	mu             sync.Mutex
	eventMu        sync.Mutex
	shell          domain.SSHShell
	session        sshx.ShellSession
	cancel         context.CancelFunc
	generation     uint64
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
	subscribers    map[uint64]*sshShellSubscriber
	subscriberID   uint64
	writer         *shellEventWriter
	outputClosed   bool
	history        shellHistory
}

func newShellRegistry() *shellRegistry {
	ctx, cancel := context.WithCancel(context.Background())
	return &shellRegistry{states: make(map[string]*sshShellState), ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

// begin tracks preparation. A successful start adds its child worker before
// completing this work item, so shutdown cannot race an uncounted generation.
func (r *shellRegistry) begin() (context.Context, context.CancelFunc, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, nil, fmt.Errorf("service is shutting down")
	}
	r.workers.Add(1)
	ctx, cancel := context.WithCancel(r.ctx)
	return ctx, cancel, nil
}

func (r *shellRegistry) shutdown() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		r.cancel()
		go func() {
			r.workers.Wait()
			close(r.done)
		}()
	}
	return r.done
}

func (r *shellRegistry) recordError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err == nil {
		r.err = err
	}
}

func (r *shellRegistry) shutdownError() error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.err
}

func (r *shellRegistry) get(id string) *sshShellState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.states[strings.TrimSpace(id)]
}

// add reserves capacity before history creation or opening the transport.
// Failed starts must remove their reservation.
func (r *shellRegistry) add(state *sshShellState) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return fmt.Errorf("service is shutting down")
	}
	if err := r.checkCapacityLocked(state.shell.HostID, nil); err != nil {
		return err
	}
	r.states[state.shell.ID] = state
	return nil
}

func (r *shellRegistry) remove(state *sshShellState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.states[state.shell.ID] == state {
		delete(r.states, state.shell.ID)
	}
}

func (r *shellRegistry) checkCapacityLocked(hostID string, exclude *sshShellState) error {
	activeTotal, activeHost := 0, 0
	for _, state := range r.states {
		if state == exclude {
			continue
		}
		state.mu.Lock()
		active := shellStatusActive(state.shell.Status)
		hostMatch := state.shell.HostID == hostID
		state.mu.Unlock()
		if active {
			activeTotal++
			if hostMatch {
				activeHost++
			}
		}
	}
	if activeTotal >= maxActiveSSHShells || activeHost >= maxActiveSSHShellsPerHost {
		return fmt.Errorf("interactive shell limit reached")
	}
	return nil
}

func (r *shellRegistry) beginReconnect(id string, cancel context.CancelFunc) (*sshShellState, domain.SSHShell, uint64, error) {
	state := r.get(id)
	if state == nil {
		return nil, domain.SSHShell{}, 0, store.ErrNotFound
	}
	state.lifecycleMu.Lock()
	defer state.lifecycleMu.Unlock()
	state.eventMu.Lock()
	defer state.eventMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, domain.SSHShell{}, 0, fmt.Errorf("service is shutting down")
	}
	if r.states[id] != state || state.history.Persistent() {
		return nil, domain.SSHShell{}, 0, store.ErrNotFound
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if !operatorShellReconnectable(state.shell) {
		return nil, domain.SSHShell{}, 0, fmt.Errorf("%w: interactive shell %q is %s", ErrSSHShellReconnectConflict, id, state.shell.Status)
	}
	if err := r.checkCapacityLocked(state.shell.HostID, state); err != nil {
		return nil, domain.SSHShell{}, 0, err
	}
	state.generation++
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
	state.outputClosed = false
	state.writer = state.newEventWriter()
	return state, state.shell, state.generation, nil
}

// running captures the transport under the same lock as the status check.
// I/O must use this reference, not reread state.session after releasing the lock:
// the current generation may exit or be replaced by a reconnect meanwhile.
func (r *shellRegistry) running(id, expectedSessionID string) (*sshShellState, sshx.ShellSession, uint64, error) {
	id = strings.TrimSpace(id)
	state := r.get(id)
	if state == nil {
		return nil, nil, 0, store.ErrNotFound
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if expectedSessionID != "" && state.shell.SessionID != expectedSessionID {
		return nil, nil, 0, store.ErrNotFound
	}
	if state.shell.Status != "running" || state.session == nil {
		return nil, nil, 0, fmt.Errorf("interactive shell %q is %s", id, state.shell.Status)
	}
	return state, state.session, state.shell.LastSequence, nil
}

func (r *shellRegistry) transientShells(sessionID string, activeOnly bool) []domain.SSHShell {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var shells []domain.SSHShell
	for _, state := range r.states {
		if state.history.Persistent() {
			continue
		}
		state.mu.Lock()
		shell := state.shell
		state.mu.Unlock()
		if (sessionID != "" && shell.SessionID != sessionID) || (activeOnly && !shellStatusActive(shell.Status) && !operatorShellReconnectable(shell)) {
			continue
		}
		shells = append(shells, shell)
	}
	return shells
}

func (r *shellRegistry) hasActive(match func(domain.SSHShell) bool) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, state := range r.states {
		state.mu.Lock()
		shell := state.shell
		state.mu.Unlock()
		if shellStatusActive(shell.Status) && match(shell) {
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

func operatorShellReconnectable(shell domain.SSHShell) bool {
	if shell.Surface != domain.SSHShellSurfaceQuick && shell.Surface != domain.SSHShellSurfaceWorkspace && shell.Surface != domain.WorkspaceShellSurfaceOperator {
		return false
	}
	if shell.Status != "failed" {
		return false
	}
	return shell.TerminationReason == "connection_lost" || shell.TerminationReason == "process_lost"
}
