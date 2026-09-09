package service

import (
	"context"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
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
