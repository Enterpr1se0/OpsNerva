package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

type faultShellHistory struct {
	shellHistory
	mu         sync.Mutex
	attempts   int
	failures   int
	appendHook func(context.Context, []domain.SSHShellEvent) error
	updateHook func(domain.SSHShell) error
}

func (h *faultShellHistory) Append(ctx context.Context, events []domain.SSHShellEvent, recent string) error {
	h.mu.Lock()
	h.attempts++
	attempt := h.attempts
	h.mu.Unlock()
	if h.appendHook != nil {
		if err := h.appendHook(ctx, events); err != nil {
			return err
		}
	}
	if attempt <= h.failures {
		return errors.New("fixture history unavailable")
	}
	return h.shellHistory.Append(ctx, events, recent)
}

func (h *faultShellHistory) Update(ctx context.Context, shell domain.SSHShell) error {
	if h.updateHook != nil {
		if err := h.updateHook(shell); err != nil {
			return err
		}
	}
	return h.shellHistory.Update(ctx, shell)
}

func writerEvent(sequence uint64) []domain.SSHShellEvent {
	return []domain.SSHShellEvent{{ShellID: "writer", Sequence: sequence, Stream: "stdout", Content: "output"}}
}

func TestShellEventWriterBatchesAndStopsWhenIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		history := &faultShellHistory{shellHistory: newMemoryShellHistory(domain.SSHShell{ID: "writer"})}
		commits := 0
		writer := newShellEventWriter(history, func() { commits++ }, func(err error) { t.Errorf("unexpected failure: %v", err) })
		if err := writer.append(writerEvent(1), "output", false); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if history.attempts != 0 {
			t.Fatal("small batch was persisted synchronously")
		}
		time.Sleep(sshShellPersistDelay)
		synctest.Wait()
		if history.attempts != 1 || commits != 1 {
			t.Fatal("delayed batch did not commit")
		}
		time.Sleep(time.Hour)
		synctest.Wait()
		if history.attempts != 1 {
			t.Fatal("idle writer kept flushing")
		}
		if err := writer.append(writerEvent(2), "outputoutput", true); err != nil {
			t.Fatal(err)
		}
		if err := writer.close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if commits != 2 || history.attempts != 2 {
			t.Fatal("explicit flush was duplicated by close")
		}
		if err := writer.append(writerEvent(3), "", true); err == nil {
			t.Fatal("closed writer accepted data")
		}
	})
}

func TestShellEventWriterRetriesWithoutLosingOrDuplicatingEvents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		history := &faultShellHistory{shellHistory: newMemoryShellHistory(domain.SSHShell{ID: "writer"}), failures: 2}
		commits, failed := 0, 0
		writer := newShellEventWriter(history, func() { commits++ }, func(error) { failed++ })
		if err := writer.append(writerEvent(1), "output", false); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if history.attempts != 3 || commits != 1 || failed != 0 {
			t.Fatalf("retry accounting: attempts=%d commits=%d failed=%d", history.attempts, commits, failed)
		}
		events, _, err := history.ListPage(context.Background(), 0, 0)
		if err != nil || len(events) != 1 || events[0].Sequence != 1 {
			t.Fatalf("events=%#v err=%v", events, err)
		}
		if err := writer.close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestShellEventWriterPermanentFailureIsBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		history := &faultShellHistory{shellHistory: newMemoryShellHistory(domain.SSHShell{ID: "writer"}), failures: 100}
		failed := 0
		writer := newShellEventWriter(history, func() {}, func(error) { failed++ })
		_ = writer.append(writerEvent(1), "", false)
		time.Sleep(time.Hour)
		synctest.Wait()
		if history.attempts != maxShellPersistAttempts || failed != 1 {
			t.Fatalf("attempts=%d failed=%d", history.attempts, failed)
		}
		if err := writer.close(context.Background()); err == nil {
			t.Fatal("close hid permanent persistence failure")
		}
		attempts := history.attempts
		time.Sleep(time.Hour)
		synctest.Wait()
		if history.attempts != attempts || failed != 1 {
			t.Fatal("retry or failure notification survived close")
		}
	})
}

func TestShellEventWriterCloseJoinsInFlightFlush(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		history := &faultShellHistory{shellHistory: newMemoryShellHistory(domain.SSHShell{ID: "writer"}),
			appendHook: func(ctx context.Context, _ []domain.SSHShellEvent) error {
				close(entered)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}}
		writer := newShellEventWriter(history, func() {}, func(err error) { t.Errorf("unexpected failure: %v", err) })
		_ = writer.append(writerEvent(1), "", false)
		time.Sleep(sshShellPersistDelay)
		<-entered
		closed := make(chan error, 1)
		go func() { closed <- writer.close(context.Background()) }()
		select {
		case <-closed:
			t.Fatal("close did not join the in-flight flush")
		default:
		}
		close(release)
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Hour)
		synctest.Wait()
		if history.attempts != 1 {
			t.Fatalf("flush count after close=%d", history.attempts)
		}
	})
}

func TestShellEventWriterBacklogCountsReadableBytes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		history := newMemoryShellHistory(domain.SSHShell{ID: "writer"})
		failed := 0
		writer := newShellEventWriter(history, func() {}, func(error) { failed++ })
		readable := strings.Repeat("x", maxSSHShellPersistBacklog/2)
		events := writerEvent(1)
		events[0].Content, events[0].ReadableContent = readable, &readable
		if err := writer.append(events, "", false); err == nil || failed != 1 {
			t.Fatalf("oversize raw/readable batch accepted: err=%v failures=%d", err, failed)
		}
		if len(writer.events) != 0 {
			t.Fatal("oversize batch grew the backlog")
		}
		if err := writer.close(context.Background()); err == nil {
			t.Fatal("close hid a rejected output batch")
		}
	})
}

func TestShellEventWriterRequestCancellationDoesNotStopGeneration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		history := &faultShellHistory{shellHistory: newMemoryShellHistory(domain.SSHShell{ID: "writer"}),
			appendHook: func(ctx context.Context, _ []domain.SSHShellEvent) error { return ctx.Err() }}
		failed := 0
		writer := newShellEventWriter(history, func() {}, func(error) { failed++ })
		_ = writer.append(writerEvent(1), "", false)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		for range maxShellPersistAttempts {
			if err := writer.flush(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("flush cancellation=%v", err)
			}
		}
		if failed != 0 {
			t.Fatal("canceled read requests stopped the generation")
		}
		time.Sleep(sshShellPersistDelay)
		synctest.Wait()
		if err := writer.close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestShellEventWriterFinalFlushHonorsDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		history := &faultShellHistory{shellHistory: newMemoryShellHistory(domain.SSHShell{ID: "writer"}),
			appendHook: func(ctx context.Context, _ []domain.SSHShellEvent) error { <-ctx.Done(); return ctx.Err() }}
		failed := 0
		writer := newShellEventWriter(history, func() {}, func(error) { failed++ })
		_ = writer.append(writerEvent(1), "", false)
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		defer cancel()
		if err := writer.close(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("close deadline=%v", err)
		}
		time.Sleep(time.Hour)
		synctest.Wait()
		if history.attempts != 1 || failed != 1 {
			t.Fatalf("deadline retried: attempts=%d failed=%d", history.attempts, failed)
		}
	})
}

func TestShellStatusDeliveryDoesNotDependOnRetrySuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		state := registryShell("writer", "host", "running")
		state.history = &faultShellHistory{shellHistory: state.history, failures: 1}
		state.writer = state.newEventWriter()
		subscriber := &sshShellSubscriber{events: make(chan domain.SSHShellEvent, 10), done: make(chan struct{}), overflow: make(chan struct{})}
		state.subscribers = map[uint64]*sshShellSubscriber{1: subscriber}
		svc := &Service{}
		svc.appendSSHShellEvent(state, "status", "", "running")
		if len(subscriber.events) != 1 {
			t.Fatal("failed first write hid the live status")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if len(subscriber.events) != 1 {
			t.Fatal("retry duplicated the live status")
		}
		if err := state.writer.close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}
