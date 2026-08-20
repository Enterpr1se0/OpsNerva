package service

import (
	"context"
	"fmt"
	"sync"

	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/store"
)

const (
	maxOperatorTerminalHistoryBytes  = 2 << 20
	maxOperatorTerminalHistoryEvents = 4096
	maxShellModelPageEvents          = 512
)

// shellHistory separates the live PTY runtime from its retention policy.
// Agent and MCP shells use SQLite; terminals opened directly by the Web app
// use a bounded in-memory implementation and disappear with the process.
type shellHistory interface {
	Persistent() bool
	Create(context.Context, domain.SSHShell) error
	Update(context.Context, domain.SSHShell) error
	Get(context.Context) (domain.SSHShell, error)
	Append(context.Context, []domain.SSHShellEvent, string) error
	ListPage(context.Context, uint64, int) ([]domain.SSHShellEvent, bool, error)
	RecentOutput(context.Context) (string, error)
	LastAgentInputSequence(context.Context) (uint64, error)
	AdvanceResponseSequence(context.Context, string, uint64) error
}

type persistentShellHistory struct {
	store   *store.Store
	shellID string
}

func (h *persistentShellHistory) Persistent() bool { return true }

func (h *persistentShellHistory) Create(ctx context.Context, shell domain.SSHShell) error {
	return h.store.CreateSSHShell(ctx, shell)
}

func (h *persistentShellHistory) Update(ctx context.Context, shell domain.SSHShell) error {
	return h.store.UpdateSSHShell(ctx, shell)
}

func (h *persistentShellHistory) Get(ctx context.Context) (domain.SSHShell, error) {
	return h.store.GetSSHShell(ctx, h.shellID)
}

func (h *persistentShellHistory) Append(ctx context.Context, events []domain.SSHShellEvent, recent string) error {
	return h.store.AppendSSHShellEvents(ctx, events, recent)
}

func (h *persistentShellHistory) ListPage(ctx context.Context, after uint64, maxOutputBytes int) ([]domain.SSHShellEvent, bool, error) {
	return h.store.ListSSHShellEventsPage(ctx, h.shellID, after, maxOutputBytes)
}

func (h *persistentShellHistory) RecentOutput(ctx context.Context) (string, error) {
	return h.store.GetSSHShellRecentOutput(ctx, h.shellID)
}

func (h *persistentShellHistory) LastAgentInputSequence(ctx context.Context) (uint64, error) {
	return h.store.LastSSHShellAgentInputSequence(ctx, h.shellID)
}

func (h *persistentShellHistory) AdvanceResponseSequence(ctx context.Context, expectedSessionID string, sequence uint64) error {
	return h.store.AdvanceSSHShellResponseSequence(ctx, h.shellID, expectedSessionID, sequence)
}

type memoryShellHistory struct {
	mu               sync.RWMutex
	shell            domain.SSHShell
	events           []domain.SSHShellEvent
	eventBytes       int
	recentOutput     string
	responseSequence uint64
}

func newMemoryShellHistory(shell domain.SSHShell) *memoryShellHistory {
	return &memoryShellHistory{shell: shell}
}

func (h *memoryShellHistory) Persistent() bool { return false }

func (h *memoryShellHistory) Create(_ context.Context, shell domain.SSHShell) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.shell.ID != "" && h.shell.ID != shell.ID {
		return fmt.Errorf("terminal history is already bound")
	}
	h.shell = shell
	return nil
}

func (h *memoryShellHistory) Update(_ context.Context, shell domain.SSHShell) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.shell.ID == "" || h.shell.ID != shell.ID {
		return store.ErrNotFound
	}
	h.shell = shell
	return nil
}

func (h *memoryShellHistory) Get(context.Context) (domain.SSHShell, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.shell.ID == "" {
		return domain.SSHShell{}, store.ErrNotFound
	}
	return h.shell, nil
}

func (h *memoryShellHistory) Append(_ context.Context, events []domain.SSHShellEvent, recent string) error {
	if len(events) == 0 {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.shell.ID == "" || events[len(events)-1].ShellID != h.shell.ID {
		return store.ErrNotFound
	}
	for _, event := range events {
		h.events = append(h.events, event)
		h.eventBytes += shellEventMemoryBytes(event)
	}
	h.shell.LastSequence = events[len(events)-1].Sequence
	h.recentOutput = recent
	for len(h.events) > 1 && (len(h.events) > maxOperatorTerminalHistoryEvents || h.eventBytes > maxOperatorTerminalHistoryBytes) {
		h.eventBytes -= shellEventMemoryBytes(h.events[0])
		h.events = h.events[1:]
	}
	return nil
}

func (h *memoryShellHistory) ListPage(_ context.Context, after uint64, maxOutputBytes int) ([]domain.SSHShellEvent, bool, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	result := make([]domain.SSHShellEvent, 0)
	outputBytes := 0
	for _, event := range h.events {
		if event.Sequence <= after {
			continue
		}
		if maxOutputBytes > 0 && len(result) >= maxShellModelPageEvents {
			return result, true, nil
		}
		eventBytes := shellEventOutputBytes(event)
		if maxOutputBytes > 0 && outputBytes > 0 && outputBytes+eventBytes > maxOutputBytes {
			return result, true, nil
		}
		result = append(result, event)
		outputBytes += eventBytes
	}
	return result, false, nil
}

func (h *memoryShellHistory) RecentOutput(context.Context) (string, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.recentOutput, nil
}

func (h *memoryShellHistory) LastAgentInputSequence(context.Context) (uint64, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var sequence uint64
	for _, event := range h.events {
		if event.Stream == "input" && event.Source == "agent" && event.Sequence > sequence {
			sequence = event.Sequence
		}
	}
	return sequence, nil
}

func (h *memoryShellHistory) AdvanceResponseSequence(_ context.Context, expectedSessionID string, sequence uint64) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if expectedSessionID != "" && h.shell.SessionID != expectedSessionID {
		return store.ErrNotFound
	}
	if sequence > h.responseSequence {
		h.responseSequence = sequence
	}
	return nil
}

func shellEventOutputBytes(event domain.SSHShellEvent) int {
	if event.Stream != "stdout" && event.Stream != "stderr" {
		return 0
	}
	if event.ReadableContent != nil {
		return len(*event.ReadableContent)
	}
	return len(event.Content)
}

func shellEventMemoryBytes(event domain.SSHShellEvent) int {
	result := len(event.Content) + len(event.Source) + len(event.Status) + 96
	if event.ReadableContent != nil {
		result += len(*event.ReadableContent)
	}
	return result
}
