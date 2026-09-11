package service

import (
	"context"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/observability"
	"github.com/Enterpr1se0/opsnerva/internal/store"
)

func (s *Service) ListAuditHistoryGroups(ctx context.Context, filter domain.AuditHistoryFilter) (domain.AuditHistoryGroupPage, error) {
	if sessionID := SessionIDFromContext(ctx); sessionID != "" {
		filter.SessionID = &sessionID
	}
	if filter.SnapshotAt.IsZero() && filter.Before == nil {
		filter.SnapshotAt = time.Now().UTC()
	}
	return s.store.ListAuditHistoryGroups(ctx, filter)
}

func (s *Service) ListAuditHistoryRuns(ctx context.Context, filter domain.AuditHistoryFilter) (domain.AuditHistoryRunPage, error) {
	if sessionID := SessionIDFromContext(ctx); sessionID != "" && (filter.SessionID == nil || *filter.SessionID != sessionID) {
		return domain.AuditHistoryRunPage{}, store.ErrNotFound
	}
	if filter.SnapshotAt.IsZero() && filter.Before == nil {
		filter.SnapshotAt = time.Now().UTC()
	}
	return s.store.ListAuditHistoryRuns(ctx, filter)
}

func (s *Service) ListAudit(ctx context.Context, runID string, limit int) ([]domain.AuditEvent, error) {
	return s.store.ListAudit(ctx, runID, limit)
}

func (s *Service) ListAuditPage(ctx context.Context, runID string, limit int, cursorCreated time.Time, cursorID string) (domain.AuditEventPage, error) {
	return s.store.ListAuditPage(ctx, runID, limit, cursorCreated, cursorID)
}

func (s *Service) DeleteAuditRuns(ctx context.Context, sessionID *string, actor string) (domain.AuditRunDeleteResult, error) {
	return s.store.DeleteAuditRuns(ctx, sessionID, actor)
}

func (s *Service) audit(ctx context.Context, runID, eventType, actor string, data map[string]any) {
	if actor == "" {
		actor = "local-user"
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.store.AppendAudit(persistCtx, domain.AuditEvent{RunID: runID, Type: eventType, Actor: actor, Data: data}); err != nil {
		observability.FromContext(ctx).ErrorContext(context.WithoutCancel(ctx), "persist audit event failed",
			"component", "audit", "run_id", runID, "event_type", eventType, "actor", actor, "error", err)
	}
}
