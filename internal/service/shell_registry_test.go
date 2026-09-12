package service

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/store"
)

func registryShell(id, hostID, status string) *sshShellState {
	shell := domain.SSHShell{
		ID: id, HostID: hostID, Kind: domain.SSHShellKindSSH,
		Surface: domain.SSHShellSurfaceQuick, Status: status,
		StartedAt: time.Now().UTC(),
	}
	return &sshShellState{
		shell: shell, generation: 1, notify: make(chan struct{}),
		pending: make(map[string]string), history: newMemoryShellHistory(shell),
	}
}

func TestShellRegistryConcurrentAdmission(t *testing.T) {
	for _, perHost := range []bool{false, true} {
		t.Run(fmt.Sprintf("per_host=%t", perHost), func(t *testing.T) {
			registry := newShellRegistry()
			start := make(chan struct{})
			results := make(chan error, 32)
			var workers sync.WaitGroup
			for index := range 32 {
				workers.Go(func() {
					hostID := fmt.Sprintf("host-%d", index)
					if perHost {
						hostID = "shared-host"
					}
					state := registryShell(fmt.Sprintf("shell-%d", index), hostID, "starting")
					<-start
					results <- registry.add(state)
				})
			}
			close(start)
			workers.Wait()
			close(results)
			accepted := 0
			for err := range results {
				if err == nil {
					accepted++
				}
			}
			want := maxActiveSSHShells
			if perHost {
				want = maxActiveSSHShellsPerHost
			}
			if accepted != want || len(registry.transientShells("", true)) != want {
				t.Fatalf("admitted %d shells, want %d", accepted, want)
			}
		})
	}
}

func TestShellRegistryStartAndReconnectShareCapacity(t *testing.T) {
	for _, perHost := range []bool{false, true} {
		t.Run(fmt.Sprintf("per_host=%t", perHost), func(t *testing.T) {
			registry := newShellRegistry()
			target := registryShell("reconnect-target", "target-host", "failed")
			target.shell.TerminationReason = "connection_lost"
			if err := registry.add(target); err != nil {
				t.Fatal(err)
			}
			occupied := maxActiveSSHShells - 1
			if perHost {
				occupied = maxActiveSSHShellsPerHost - 1
			}
			for index := range occupied {
				hostID := fmt.Sprintf("occupied-host-%d", index)
				if perHost {
					hostID = "target-host"
				}
				if err := registry.add(registryShell(fmt.Sprintf("occupied-%d", index), hostID, "stopping")); err != nil {
					t.Fatal(err)
				}
			}
			_, cancel := context.WithCancel(context.Background())
			defer cancel()
			start := make(chan struct{})
			results := make(chan error, 24)
			var workers sync.WaitGroup
			for index := range 24 {
				workers.Go(func() {
					<-start
					if index%2 == 0 {
						_, _, _, err := registry.beginReconnect(target.shell.ID, cancel)
						results <- err
					} else {
						results <- registry.add(registryShell(fmt.Sprintf("new-%d", index), "target-host", "starting"))
					}
				})
			}
			close(start)
			workers.Wait()
			close(results)
			accepted := 0
			for err := range results {
				if err == nil {
					accepted++
				}
			}
			if accepted != 1 {
				t.Fatalf("start/reconnect admitted %d operations for one remaining slot", accepted)
			}
			if registry.get(target.shell.ID) != target || target.generation > 2 {
				t.Fatal("competing reconnect changed logical identity or reserved multiple generations")
			}
		})
	}
}

func TestShellRegistryReconnectPreservesHistoryAndRejectsWithoutMutation(t *testing.T) {
	registry := newShellRegistry()
	state := registryShell("retained", "host", "failed")
	state.shell.TerminationReason = "connection_lost"
	state.shell.EndedAt = time.Now().UTC()
	state.shell.Error = "old connection error"
	state.shell.LastSequence = 41
	state.shell.Cols, state.shell.Rows = 120, 40
	state.recentOutput, state.responseCursor = "previous output", 38
	history := state.history
	if err := registry.add(state); err != nil {
		t.Fatal(err)
	}
	for index := range maxActiveSSHShellsPerHost {
		if err := registry.add(registryShell(fmt.Sprintf("busy-%d", index), "host", "running")); err != nil {
			t.Fatal(err)
		}
	}
	before := state.shell
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, _, _, err := registry.beginReconnect("retained", cancel); err == nil {
		t.Fatal("reconnect bypassed host capacity")
	}
	if !reflect.DeepEqual(state.shell, before) || state.generation != 1 || state.cancel != nil {
		t.Fatal("rejected reconnect changed the retained shell")
	}
	registry.remove(registry.get("busy-0"))
	current, shell, generation, err := registry.beginReconnect("retained", cancel)
	if err != nil {
		t.Fatal(err)
	}
	if current != state || generation != 2 || shell.ID != before.ID || shell.StartedAt != before.StartedAt ||
		shell.LastSequence != 41 || shell.Cols != 120 || shell.Rows != 40 || state.responseCursor != 38 ||
		state.recentOutput != "previous output" || state.history != history {
		t.Fatalf("reconnect replaced retained data: %#v", shell)
	}
	if shell.Status != "starting" || shell.Error != "" || shell.TerminationReason != "" || !shell.EndedAt.IsZero() {
		t.Fatalf("reconnect retained previous terminal status: %#v", shell)
	}
	if _, _, _, err := registry.beginReconnect("retained", cancel); !errors.Is(err, ErrSSHShellReconnectConflict) {
		t.Fatalf("duplicate reconnect error = %v", err)
	}
	state.cancel()
	if ctx.Err() != context.Canceled {
		t.Fatal("generation cancellation was not bound")
	}
}

func TestShellRegistryRunningCapturesConnectionAndSessionBoundary(t *testing.T) {
	registry := newShellRegistry()
	state := registryShell("active", "host", "running")
	state.shell.SessionID, state.shell.LastSequence = "owner", 12
	first := &fakeShellSession{done: make(chan struct{}), callback: func(string, []byte) {}}
	state.session = first
	if err := registry.add(state); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := registry.running("active", "other"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("session isolation error = %v", err)
	}
	_, connection, sequence, err := registry.running(" active ", "owner")
	if err != nil || connection != first || sequence != 12 {
		t.Fatalf("running connection = %v, sequence=%d err=%v", connection, sequence, err)
	}
	state.mu.Lock()
	state.session = nil
	state.shell.Status = "failed"
	state.mu.Unlock()
	if _, _, _, err := registry.running("active", "owner"); err == nil {
		t.Fatal("exited shell accepted a new I/O operation")
	}
	second := &fakeShellSession{done: make(chan struct{}), callback: func(string, []byte) {}}
	state.mu.Lock()
	state.session = second
	state.shell.Status = "running"
	state.generation++
	state.mu.Unlock()
	if _, err := connection.Write([]byte("old generation")); err != nil {
		t.Fatal(err)
	}
	if len(first.inputs) != 1 || len(second.inputs) != 0 {
		t.Fatal("captured I/O was redirected to the replacement connection")
	}
}

func TestShellRegistryRetentionAndActiveQueries(t *testing.T) {
	registry := newShellRegistry()
	retained := registryShell("retained", "host", "failed")
	retained.shell.TerminationReason = "connection_lost"
	persistent := registryShell("agent", "host", "running")
	persistent.history = &persistentShellHistory{shellID: "agent"}
	workspace := registryShell("workspace", "workspace-host", "starting")
	workspace.shell.Kind, workspace.shell.WorkspaceID = domain.SSHShellKindWorkspace, "project"
	workspace.shell.SessionID = "conversation"
	for _, state := range []*sshShellState{retained, persistent, workspace} {
		if err := registry.add(state); err != nil {
			t.Fatal(err)
		}
	}
	if len(registry.transientShells("", true)) != 2 || len(registry.transientShells("conversation", true)) != 1 {
		t.Fatal("transient list lost reconnectable shell or included persisted history")
	}
	list := registry.transientShells("conversation", true)
	list[0].Status = "changed snapshot"
	if workspace.shell.Status != "starting" {
		t.Fatal("list mutation changed the registry")
	}
	if !registry.hasActive(func(shell domain.SSHShell) bool { return shell.WorkspaceID == "project" }) ||
		registry.hasActive(func(shell domain.SSHShell) bool { return shell.ID == "retained" }) {
		t.Fatal("active guard treated retained failure as running or missed starting workspace")
	}
	if _, _, _, err := registry.beginReconnect("agent", func() {}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("operator reconnect accepted persisted Agent shell: %v", err)
	}
	registry.remove(retained)
	replacement := registryShell("retained", "host", "starting")
	if err := registry.add(replacement); err != nil {
		t.Fatal(err)
	}
	registry.remove(retained)
	if registry.get("retained") != replacement {
		t.Fatal("stale removal deleted another shell instance")
	}
}
