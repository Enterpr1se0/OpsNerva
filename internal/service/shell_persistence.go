package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

const (
	maxSSHShellPersistBatch   = 32
	maxSSHShellPersistBytes   = 256 << 10
	maxSSHShellPersistBacklog = 1 << 20
	sshShellPersistDelay      = 12 * time.Millisecond
	maxShellPersistAttempts   = 3
	shellPersistenceTimeout   = 5 * time.Second
)

// shellEventWriter owns buffered events and every delayed flush. Callbacks
// only notify readers or cancel the owning generation; they must not call
// back into the writer. Close rejects new events, joins scheduled work, and
// makes a bounded final flush. No retry survives Close.
type shellEventWriter struct {
	mu        sync.Mutex
	workers   sync.WaitGroup
	history   shellHistory
	committed func()
	failed    func(error)
	events    []domain.SSHShellEvent
	recent    string
	bytes     int
	timer     *time.Timer
	closed    bool
	attempts  int
	fatal     error
}

func newShellEventWriter(history shellHistory, committed func(), failed func(error)) *shellEventWriter {
	return &shellEventWriter{history: history, committed: committed, failed: failed}
}

func (w *shellEventWriter) append(events []domain.SSHShellEvent, recent string, immediate bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errors.New("shell event writer is closed")
	}
	size := 0
	for _, event := range events {
		size += shellEventMemoryBytes(event)
	}
	if w.bytes+size > maxSSHShellPersistBacklog {
		err := fmt.Errorf("SSH shell event persistence backlog exceeded %d bytes", maxSSHShellPersistBacklog)
		w.failLocked(err)
		return err
	}
	w.events = append(w.events, events...)
	w.bytes += size
	w.recent = recent
	var err error
	if w.fatal == nil && (immediate || len(w.events) >= maxSSHShellPersistBatch || w.bytes >= maxSSHShellPersistBytes) {
		err = w.flushLocked(context.Background())
	}
	w.scheduleLocked()
	return err
}

func (w *shellEventWriter) flush(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return w.fatal
	}
	err := w.flushLocked(ctx)
	w.scheduleLocked()
	return err
}

func (w *shellEventWriter) stopTimerLocked() {
	if w.timer != nil {
		if w.timer.Stop() {
			w.workers.Done()
		}
		w.timer = nil
	}
}

func (w *shellEventWriter) scheduleLocked() {
	if w.closed || w.fatal != nil || w.timer != nil || len(w.events) == 0 {
		return
	}
	delay := sshShellPersistDelay
	if w.attempts > 0 {
		delay = time.Duration(w.attempts) * 100 * time.Millisecond
	}
	w.workers.Add(1)
	var timer *time.Timer
	timer = time.AfterFunc(delay, func() {
		defer w.workers.Done()
		w.mu.Lock()
		defer w.mu.Unlock()
		// A stopped callback can already be waiting on mu. It must not
		// clear or execute a newer scheduled flush.
		if w.closed || w.timer != timer {
			return
		}
		w.timer = nil
		_ = w.flushLocked(context.Background())
		w.scheduleLocked()
	})
	w.timer = timer
}

func (w *shellEventWriter) flushLocked(ctx context.Context) error {
	w.stopTimerLocked()
	if len(w.events) == 0 {
		return nil
	}
	writeCtx, cancel := context.WithTimeout(ctx, shellPersistenceTimeout)
	defer cancel()
	if err := w.history.Append(writeCtx, w.events, w.recent); err != nil {
		// Canceling a snapshot request is not a persistence outage and must
		// not consume the generation's background retry budget.
		if ctx.Err() != nil {
			return err
		}
		w.attempts++
		if w.attempts >= maxShellPersistAttempts {
			w.failLocked(err)
		}
		return err
	}
	clear(w.events)
	w.events = w.events[:0]
	w.bytes, w.attempts = 0, 0
	w.committed()
	return nil
}

func (w *shellEventWriter) failLocked(err error) {
	if w.fatal != nil {
		return
	}
	w.fatal = err
	w.stopTimerLocked()
	w.failed(err)
}

func (w *shellEventWriter) close(ctx context.Context) error {
	w.mu.Lock()
	w.closed = true
	w.stopTimerLocked()
	w.mu.Unlock()
	w.workers.Wait()

	w.mu.Lock()
	defer w.mu.Unlock()
	// The final flush shares the same attempt budget as background retries.
	// Allow one last attempt after a fatal error to retain the terminal event.
	for {
		err := w.flushLocked(ctx)
		if err == nil {
			return w.fatal
		}
		if w.attempts >= maxShellPersistAttempts || ctx.Err() != nil {
			w.failLocked(err)
			return errors.Join(w.fatal, err)
		}
		timer := time.NewTimer(time.Duration(w.attempts) * 100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			w.failLocked(ctx.Err())
			return errors.Join(err, ctx.Err())
		case <-timer.C:
		}
	}
}
