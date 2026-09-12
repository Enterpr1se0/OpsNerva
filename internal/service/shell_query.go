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

const (
	defaultShellQueryDelay = time.Duration(domain.DefaultShellQueryDelaySeconds) * time.Second
	maxShellQueryDelay     = time.Duration(domain.MaxShellQueryDelaySeconds) * time.Second
)

func (s *Service) GetSSHShellSnapshot(ctx context.Context, id, expectedSessionID string, after uint64, wait time.Duration, coalesce bool, reason, actor string) (domain.SSHShellSnapshot, error) {
	snapshot, _, err := s.getSSHShellSnapshotPage(ctx, id, expectedSessionID, after, wait, 0, coalesce, reason, actor)
	return snapshot, err
}

func (s *Service) getSSHShellSnapshotPage(ctx context.Context, id, expectedSessionID string, after uint64, wait time.Duration, maxOutputBytes int, coalesce bool, reason, actor string) (domain.SSHShellSnapshot, bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return domain.SSHShellSnapshot{}, false, fmt.Errorf("shell_id is required")
	}
	if err := s.flushLiveSSHShellEvents(ctx, id); err != nil {
		return domain.SSHShellSnapshot{}, false, err
	}
	history, state := s.historyForShell(id)
	shell, err := history.Get(ctx)
	if err != nil {
		return domain.SSHShellSnapshot{}, false, err
	}
	if expectedSessionID != "" && shell.SessionID != expectedSessionID {
		return domain.SSHShellSnapshot{}, false, store.ErrNotFound
	}
	if after > shell.LastSequence {
		return domain.SSHShellSnapshot{}, false, fmt.Errorf("invalid after_sequence: must not exceed the shell's last sequence")
	}
	events, hasMore, err := history.ListPage(ctx, after, maxOutputBytes)
	if err != nil {
		return domain.SSHShellSnapshot{}, false, err
	}
	if len(events) == 0 && wait > 0 && shellStatusActive(shell.Status) {
		if wait > 30*time.Second {
			wait = 30 * time.Second
		}
		var notify <-chan struct{}
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
			shell, err = history.Get(ctx)
			if err != nil {
				return domain.SSHShellSnapshot{}, false, err
			}
			events, hasMore, err = history.ListPage(ctx, after, maxOutputBytes)
			if err != nil {
				return domain.SSHShellSnapshot{}, false, err
			}
		}
	}
	recent, err := history.RecentOutput(ctx)
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
	_, _, before, err := s.shells.running(id, expectedSessionID)
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
	history, state := s.historyForShell(id)
	shell, err := history.Get(ctx)
	if err != nil {
		return 0, err
	}
	if expectedSessionID != "" && shell.SessionID != expectedSessionID {
		return 0, store.ErrNotFound
	}
	cursor, err := history.LastAgentInputSequence(ctx)
	if err != nil {
		return 0, err
	}
	if shell.ResponseSequence > cursor {
		cursor = shell.ResponseSequence
	}
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
	history, state := s.historyForShell(id)
	if err := history.AdvanceResponseSequence(context.Background(), expectedSessionID, sequence); err != nil {
		observability.FromContext(context.Background()).ErrorContext(context.Background(), "persist SSH shell response cursor failed",
			"component", "ssh_shell", "shell_id", id, "sequence", sequence, "error", err)
	}
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
	state := s.shells.get(strings.TrimSpace(id))
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
