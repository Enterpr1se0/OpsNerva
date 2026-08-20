package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"eino-ops-agent/internal/domain"
)

func TestMemoryShellHistoryIsBoundedAndKeepsSequence(t *testing.T) {
	shell := domain.SSHShell{ID: "operator-terminal", Status: "running", StartedAt: time.Now().UTC()}
	history := newMemoryShellHistory(shell)
	content := strings.Repeat("x", 1024)
	for sequence := uint64(1); sequence <= 5000; sequence++ {
		readable := content
		if err := history.Append(context.Background(), []domain.SSHShellEvent{{
			ShellID: shell.ID, Sequence: sequence, Stream: "stdout", Content: content, ReadableContent: &readable,
		}}, content); err != nil {
			t.Fatal(err)
		}
	}
	if len(history.events) >= 5000 || len(history.events) > maxOperatorTerminalHistoryEvents || history.eventBytes > maxOperatorTerminalHistoryBytes+shellEventMemoryBytes(history.events[0]) {
		t.Fatalf("terminal history was not bounded: events=%d bytes=%d", len(history.events), history.eventBytes)
	}
	events, hasMore, err := history.ListPage(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if hasMore || len(events) == 0 || events[0].Sequence <= 1 || events[len(events)-1].Sequence != 5000 {
		t.Fatalf("bounded terminal history sequence is invalid: first=%d last=%d more=%v", events[0].Sequence, events[len(events)-1].Sequence, hasMore)
	}
}
