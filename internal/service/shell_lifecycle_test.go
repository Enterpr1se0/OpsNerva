package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/security"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"
	"github.com/Enterpr1se0/opsnerva/internal/store"
)

func TestShellLifecycleRunningHistoryFailureCleansOpenedConnection(t *testing.T) {
	svc, _, _ := newTestService(t)
	state := registryShell("start-history-failure", "host", "starting")
	state.history = &faultShellHistory{shellHistory: state.history, updateHook: func(shell domain.SSHShell) error {
		if shell.Status == "running" {
			return errors.New("fixture running-state write failed")
		}
		return nil
	}}
	shellCtx, cancel, err := svc.shells.begin()
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	state.cancel = cancel
	state.writer = state.newEventWriter()
	if err := svc.shells.add(state); err != nil {
		t.Fatal(err)
	}
	connection := &fakeShellSession{done: make(chan struct{})}
	_, err = svc.startInteractiveShellGeneration(context.Background(), shellCtx, state,
		func(_ context.Context, output func(string, []byte)) (sshx.ShellSession, error) {
			connection.callback = output
			output("stdout", []byte("output before failed state write"))
			return connection, nil
		}, true)
	svc.shells.workers.Done()
	if err == nil || !strings.Contains(err.Error(), "running-state write failed") {
		t.Fatalf("start error=%v", err)
	}
	if svc.shells.get(state.shell.ID) != nil {
		t.Fatal("failed start retained a running reservation")
	}
	select {
	case <-connection.done:
	default:
		t.Fatal("failed start leaked its opened transport")
	}
	shell, err := state.history.Get(context.Background())
	if err != nil || shell.Status != "failed" || shell.TerminationReason != "start_failed" {
		t.Fatalf("failed start history=%#v err=%v", shell, err)
	}
	if !state.writer.closed || len(state.writer.events) != 0 {
		t.Fatal("failed start did not drain its writer")
	}
}

func TestShellLifecycleShutdownWaitsForOpeningGeneration(t *testing.T) {
	svc, _, host := newTestService(t)
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	opened := make(chan error, 1)
	connection := &fakeShellSession{done: make(chan struct{})}
	go func() {
		_, err := svc.openInteractiveShell(context.Background(), host, domain.ExecRequest{}, domain.Run{}, "",
			interactiveShellOptions{kind: domain.SSHShellKindSSH, transient: true},
			func(ctx context.Context, output func(string, []byte)) (sshx.ShellSession, error) {
				connection.callback = output
				output("stdout", []byte("opening output"))
				close(entered)
				<-ctx.Done()
				close(canceled)
				<-release
				// An opener that returns a connection after cancellation must
				// still be drained and closed before shutdown can finish.
				return connection, nil
			})
		opened <- err
	}()
	<-entered
	shutdown := make(chan error, 1)
	go func() { shutdown <- svc.Shutdown(context.Background()) }()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel the opener")
	}
	select {
	case err := <-shutdown:
		t.Fatalf("shutdown skipped opening work: %v", err)
	default:
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-shutdown:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not drain opening work")
	}
	if err := <-opened; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled start error=%v", err)
	}
	select {
	case <-connection.done:
	default:
		t.Fatal("late connection survived shutdown")
	}
	if len(svc.shells.transientShells("", false)) != 0 {
		t.Fatal("shutdown retained canceled startup")
	}
	if _, _, err := svc.shells.begin(); err == nil {
		t.Fatal("shutdown admitted another generation")
	}
}

func TestShellLifecycleShutdownReportsUndrainedHistory(t *testing.T) {
	// Use an in-memory failing history, not a closed shared Store, so the
	// failure is deterministic and does not invalidate other cleanup paths.
	svc := &Service{shells: newShellRegistry(), redactor: security.NewRedactor(), executionCancel: func() {}}
	state := registryShell("undrained", "host", "starting")
	history := &faultShellHistory{shellHistory: state.history, failures: 100}
	state.history = history
	shellCtx, cancel, err := svc.shells.begin()
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	state.cancel, state.writer = cancel, state.newEventWriter()
	if err := svc.shells.add(state); err != nil {
		t.Fatal(err)
	}
	_, err = svc.startInteractiveShellGeneration(context.Background(), shellCtx, state,
		func(_ context.Context, output func(string, []byte)) (sshx.ShellSession, error) {
			return &fakeShellSession{done: make(chan struct{}), callback: output}, nil
		}, true)
	svc.shells.workers.Done()
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	if err := svc.Shutdown(ctx); err == nil || !strings.Contains(err.Error(), "history unavailable") {
		t.Fatalf("shutdown hid final persistence failure: %v", err)
	}
	if state.shell.Status != "failed" || state.shell.TerminationReason != "persistence_failed" {
		t.Fatalf("history failure reported success: %#v", state.shell)
	}
	history.mu.Lock()
	attempts := history.attempts
	history.mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	history.mu.Lock()
	defer history.mu.Unlock()
	if history.attempts != attempts || !state.writer.closed || state.writer.timer != nil {
		t.Fatal("persistence retry survived shutdown")
	}
}

func TestShellLifecycleFailedOpenReleasesReservation(t *testing.T) {
	svc, transport, host := newTestService(t)
	transport.shellOpenErrs = []error{errors.New("fixture open failure")}
	if _, err := svc.StartOperatorSSHShell(context.Background(), host.ID, domain.SSHShellSurfaceQuick, "admin-web"); err == nil {
		t.Fatal("failed transport open was accepted")
	}
	if shells := svc.shells.transientShells("", false); len(shells) != 0 {
		t.Fatalf("failed start retained its reservation: %#v", shells)
	}
	for range maxActiveSSHShellsPerHost {
		if _, err := svc.StartOperatorSSHShell(context.Background(), host.ID, domain.SSHShellSurfaceQuick, "admin-web"); err != nil {
			t.Fatalf("failed start consumed host capacity: %v", err)
		}
	}
	if _, err := svc.StartOperatorSSHShell(context.Background(), host.ID, domain.SSHShellSurfaceQuick, "admin-web"); err == nil {
		t.Fatal("operator start bypassed the registry limit")
	}
}

func TestShellLifecycleShutdownClosesConnectionsAndClearsRegistry(t *testing.T) {
	svc, _, host := newTestService(t)
	var connections []*fakeShellSession
	var states []*sshShellState
	// Exercise the shared lifecycle without requiring a local Bash/PTY.
	for _, kind := range []string{domain.SSHShellKindSSH, domain.SSHShellKindWorkspace} {
		opener := func(_ context.Context, output func(string, []byte)) (sshx.ShellSession, error) {
			connection := &fakeShellSession{done: make(chan struct{}), callback: output}
			connections = append(connections, connection)
			output("stdout", []byte("ready\n"))
			return connection, nil
		}
		shell, err := svc.openInteractiveShell(context.Background(), host, domain.ExecRequest{
			ShellCols: 80, ShellRows: 24,
		}, domain.Run{}, "admin-web", interactiveShellOptions{kind: kind, transient: true}, opener)
		if err != nil {
			t.Fatal(err)
		}
		states = append(states, svc.shells.get(shell.ID))
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := svc.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	for index, state := range states {
		if svc.shells.get(state.shell.ID) != nil {
			t.Fatal("shutdown returned before clearing active registry entries")
		}
		select {
		case <-connections[index].done:
		default:
			t.Fatal("shutdown returned before closing the transport")
		}
		snapshot, err := state.history.Get(context.Background())
		if err != nil || snapshot.Status != "interrupted" || snapshot.TerminationReason != "service_stopped" {
			t.Fatalf("shutdown terminal state = %#v, err=%v", snapshot, err)
		}
	}
}

func TestShellLifecycleReconnectIgnoresOldConnectionOutput(t *testing.T) {
	svc, transport, host := newTestService(t)
	ctx := context.Background()
	shell, err := svc.StartOperatorSSHShell(ctx, host.ID, domain.SSHShellSurfaceQuick, "admin-web")
	if err != nil {
		t.Fatal(err)
	}
	first := runningFakeShell(t, svc, shell.ID)
	first.finish(sshx.ShellExit{Err: errors.New("connection lost")})
	waitForOperatorShellStatus(t, svc, shell.ID, "failed")
	if _, err := svc.ReconnectOperatorSSHShell(ctx, shell.ID, "admin-web"); err != nil {
		t.Fatal(err)
	}
	first.callback("stdout", []byte("stale-generation-output"))
	transport.mu.Lock()
	second := transport.shellSessions[1]
	transport.mu.Unlock()
	second.callback("stdout", []byte("current-generation-output"))
	snapshot, err := svc.GetSSHShellSnapshot(ctx, shell.ID, "", 0, 0, false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(snapshot.RecentOutput, "stale-generation-output") ||
		!strings.Contains(snapshot.RecentOutput, "current-generation-output") {
		t.Fatalf("reconnect output isolation failed: %q", snapshot.RecentOutput)
	}
	if _, err := svc.store.GetSSHShell(ctx, shell.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("operator lifecycle wrote persistent shell history: %v", err)
	}
}
