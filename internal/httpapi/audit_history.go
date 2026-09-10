package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func auditHistoryFilter(r *http.Request, defaultLimit int) (domain.AuditHistoryFilter, error) {
	query := r.URL.Query()
	filter := domain.AuditHistoryFilter{Query: query.Get("q"), HostID: query.Get("host_id"), Limit: defaultLimit}
	if query.Has("limit") {
		limit, err := strconv.Atoi(query.Get("limit"))
		if err != nil || limit < 1 || limit > 200 {
			return filter, fmt.Errorf("invalid audit page limit: use 1 through 200")
		}
		filter.Limit = limit
	}
	if query.Has("snapshot_at") {
		value, err := time.Parse(time.RFC3339Nano, query.Get("snapshot_at"))
		if err != nil || value.IsZero() || value.After(time.Now()) {
			return filter, fmt.Errorf("invalid audit snapshot_at")
		}
		filter.SnapshotAt = value.UTC()
	}
	for key, destination := range map[string]*time.Time{
		"started_after":  &filter.StartedAfter,
		"started_before": &filter.StartedBefore,
	} {
		if !query.Has(key) {
			continue
		}
		value, err := time.Parse(time.RFC3339Nano, query.Get(key))
		if err != nil || value.IsZero() {
			return filter, fmt.Errorf("invalid audit %s", key)
		}
		*destination = value.UTC()
	}
	if !filter.StartedAfter.IsZero() && !filter.StartedBefore.IsZero() && filter.StartedAfter.After(filter.StartedBefore) {
		return filter, fmt.Errorf("invalid audit time range: started_after must not be later than started_before")
	}
	if query.Has("cursor_started_at") || query.Has("cursor_id") {
		if filter.SnapshotAt.IsZero() || !query.Has("cursor_started_at") || !query.Has("cursor_id") {
			return filter, fmt.Errorf("audit cursor_started_at, cursor_id and snapshot_at are required together")
		}
		started, err := time.Parse(time.RFC3339Nano, query.Get("cursor_started_at"))
		if err != nil || started.IsZero() || started.After(filter.SnapshotAt) {
			return filter, fmt.Errorf("invalid audit cursor_started_at")
		}
		filter.Before = &domain.AuditHistoryCursor{StartedAt: started.UTC(), ID: query.Get("cursor_id")}
	}
	return filter, nil
}

func (s *Server) listAuditHistoryGroups(w http.ResponseWriter, r *http.Request) {
	filter, err := auditHistoryFilter(r, 20)
	if err != nil {
		writeErrorStatus(w, err, http.StatusBadRequest)
		return
	}
	page, err := s.service.ListAuditHistoryGroups(r.Context(), filter)
	respond(w, page, err)
}

func (s *Server) listAuditHistoryRuns(w http.ResponseWriter, r *http.Request) {
	filter, err := auditHistoryFilter(r, 50)
	if err != nil {
		writeErrorStatus(w, err, http.StatusBadRequest)
		return
	}
	if !r.URL.Query().Has("session_id") {
		writeErrorStatus(w, fmt.Errorf("audit session_id is required; use an empty value for direct operations"), http.StatusBadRequest)
		return
	}
	sessionID := r.URL.Query().Get("session_id")
	filter.SessionID = &sessionID
	page, err := s.service.ListAuditHistoryRuns(r.Context(), filter)
	respond(w, page, err)
}
