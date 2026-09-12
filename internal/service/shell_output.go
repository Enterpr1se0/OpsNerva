package service

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/observability"
	"github.com/Enterpr1se0/opsnerva/internal/terminaltext"
)

const (
	maxSSHShellRecentBytes      = 16 << 10
	maxSSHShellOutputEventBytes = 16 << 10
)

type sshShellSubscriber struct {
	events       chan domain.SSHShellEvent
	overflow     chan struct{}
	done         chan struct{}
	overflowOnce sync.Once
	cancelOnce   sync.Once
}

func (s *Service) SubscribeSSHShellEvents(id string) (<-chan domain.SSHShellEvent, <-chan struct{}, func()) {
	state := s.shells.get(strings.TrimSpace(id))
	if state == nil {
		return nil, nil, func() {}
	}
	subscriber := &sshShellSubscriber{
		events: make(chan domain.SSHShellEvent, 256), overflow: make(chan struct{}), done: make(chan struct{}),
	}
	state.mu.Lock()
	if state.subscribers == nil {
		state.subscribers = make(map[uint64]*sshShellSubscriber)
	}
	state.subscriberID++
	idValue := state.subscriberID
	state.subscribers[idValue] = subscriber
	state.mu.Unlock()
	cancel := func() {
		subscriber.cancelOnce.Do(func() {
			state.mu.Lock()
			delete(state.subscribers, idValue)
			state.mu.Unlock()
			close(subscriber.done)
		})
	}
	return subscriber.events, subscriber.overflow, cancel
}

func (s *Service) appendSSHShellGenerationOutput(state *sshShellState, generation uint64, stream string, data []byte) {
	if len(data) == 0 {
		return
	}
	state.eventMu.Lock()
	defer state.eventMu.Unlock()
	state.mu.Lock()
	current := state.generation == generation && !state.outputClosed && state.reason != "persistence_failed"
	state.mu.Unlock()
	if !current {
		return
	}
	if stream != "stderr" {
		stream = "stdout"
	}
	if !state.history.Persistent() {
		s.appendSSHShellOutputEventsLocked(state, stream, string(data))
		return
	}
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
	s.appendSSHShellEventValuesLocked(state, []domain.SSHShellEvent{event})
}

func (s *Service) appendSSHShellEventValuesLocked(state *sshShellState, events []domain.SSHShellEvent) {
	s.recordSSHShellEventsLocked(state, events, true)
}

func (s *Service) recordSSHShellEventsLocked(state *sshShellState, events []domain.SSHShellEvent, immediate bool) {
	if len(events) == 0 {
		return
	}
	state.mu.Lock()
	if state.outputClosed {
		state.mu.Unlock()
		return
	}
	createdAt := time.Now().UTC()
	for index := range events {
		state.shell.LastSequence++
		events[index].ShellID = state.shell.ID
		events[index].Sequence = state.shell.LastSequence
		events[index].CreatedAt = createdAt
	}
	recent := state.recentOutput
	state.mu.Unlock()
	// Live delivery is independent from persistence retries, just as stdout
	// already was. Retrying a batch must neither lose nor duplicate a status.
	_ = state.writer.append(events, recent, immediate)
	s.publishSSHShellEvents(state, events)
}

func (state *sshShellState) newEventWriter() *shellEventWriter {
	return newShellEventWriter(state.history, func() {
		state.mu.Lock()
		close(state.notify)
		state.notify = make(chan struct{})
		state.mu.Unlock()
	}, func(err error) {
		state.mu.Lock()
		if !state.closing {
			state.closing = true
			state.reason = "persistence_failed"
		}
		cancel := state.cancel
		shellID := state.shell.ID
		state.mu.Unlock()
		observability.FromContext(context.Background()).Error("persist SSH shell events failed",
			"component", "ssh_shell", "shell_id", shellID, "error", err)
		if cancel != nil {
			cancel()
		}
	})
}

func (s *Service) flushLiveSSHShellEvents(ctx context.Context, id string) error {
	state := s.shells.get(id)
	if state == nil {
		return nil
	}
	state.eventMu.Lock()
	defer state.eventMu.Unlock()
	return state.writer.flush(ctx)
}

func (s *Service) publishSSHShellEvents(state *sshShellState, events []domain.SSHShellEvent) {
	state.mu.Lock()
	subscribers := make([]*sshShellSubscriber, 0, len(state.subscribers))
	for _, subscriber := range state.subscribers {
		subscribers = append(subscribers, subscriber)
	}
	state.mu.Unlock()
	for _, event := range events {
		for _, subscriber := range subscribers {
			select {
			case <-subscriber.done:
				continue
			default:
			}
			select {
			case subscriber.events <- event:
			default:
				subscriber.overflowOnce.Do(func() { close(subscriber.overflow) })
			}
		}
	}
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
	events := make([]domain.SSHShellEvent, 0, (len(content)+maxSSHShellOutputEventBytes-1)/maxSSHShellOutputEventBytes)
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
		events = append(events, domain.SSHShellEvent{
			Stream: stream, Content: part, ReadableContent: &readable, Status: "running",
		})
	}
	s.recordSSHShellEventsLocked(state, events, false)
	return combined.String()
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
