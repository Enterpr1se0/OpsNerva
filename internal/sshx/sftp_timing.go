package sshx

import (
	"context"
	"log/slog"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/observability"
)

const slowSFTPOperationThreshold = 2 * time.Second

// Timings are request diagnostics, not audit events. A single record describes
// the operation; recursive deletes never log once per file.
type sftpTiming struct {
	ctx       context.Context
	operation string
	hostID    string
	started   time.Time
	last      time.Time
	stages    map[string]time.Duration
}

func newSFTPTiming(ctx context.Context, operation, hostID string) *sftpTiming {
	now := time.Now()
	return &sftpTiming{ctx: ctx, operation: operation, hostID: hostID, started: now, last: now, stages: make(map[string]time.Duration)}
}

func (timing *sftpTiming) mark(stage string) {
	now := time.Now()
	timing.stages[stage] += now.Sub(timing.last)
	timing.last = now
}

func (timing *sftpTiming) finish(err error) {
	duration := time.Since(timing.started)
	level := slog.LevelDebug
	status := "completed"
	if timing.operation == "delete" {
		level = slog.LevelInfo
	}
	if duration >= slowSFTPOperationThreshold {
		level = slog.LevelWarn
	}
	if err != nil {
		status, level = "failed", slog.LevelWarn
		if timing.ctx.Err() != nil {
			status, level = "canceled", slog.LevelDebug
		}
	}
	attrs := []slog.Attr{
		slog.String("component", "sftp"), slog.String("operation", timing.operation),
		slog.String("host_id", timing.hostID), slog.String("status", status),
		slog.Int64("duration_ms", duration.Milliseconds()),
	}
	for stage, duration := range timing.stages {
		attrs = append(attrs, slog.Int64(stage+"_ms", duration.Milliseconds()))
	}
	if err != nil {
		attrs = append(attrs, slog.Any("error", err))
	}
	observability.FromContext(timing.ctx).LogAttrs(timing.ctx, level, "SFTP operation completed", attrs...)
}
