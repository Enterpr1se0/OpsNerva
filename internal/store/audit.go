package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/ids"
)

func (s *Store) AppendAudit(ctx context.Context, event domain.AuditEvent) error {
	data, err := json.Marshal(event.Data)
	if err != nil {
		return err
	}
	if event.ID == "" {
		event.ID = ids.New("evt")
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO audit_events(id,run_id,event_type,actor,data_json,created_at)
VALUES(?,?,?,?,?,?)`, event.ID, event.RunID, event.Type, event.Actor, string(data), formatTime(event.CreatedAt))
	if err == nil {
		s.publishChange(Change{Topic: ChangeAudit, Audit: &event})
	}
	return err
}

func (s *Store) ListAudit(ctx context.Context, runID string, limit int) ([]domain.AuditEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	page, err := s.ListAuditPage(ctx, runID, limit, time.Time{}, "")
	return page.Events, err
}

func (s *Store) ListAuditPage(ctx context.Context, runID string, limit int, cursorCreated time.Time, cursorID string) (domain.AuditEventPage, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	if cursorCreated.IsZero() != (strings.TrimSpace(cursorID) == "") {
		return domain.AuditEventPage{}, fmt.Errorf("invalid audit cursor boundary")
	}
	statement, arguments := auditEventsPageQuery(runID, limit, cursorCreated, cursorID)
	rows, err := s.db.QueryContext(ctx, statement, arguments...)
	if err != nil {
		return domain.AuditEventPage{}, err
	}
	defer rows.Close()
	page := domain.AuditEventPage{Events: make([]domain.AuditEvent, 0, limit+1)}
	for rows.Next() {
		var event domain.AuditEvent
		var data, created string
		if err := rows.Scan(&event.ID, &event.RunID, &event.Type, &event.Actor, &data, &created); err != nil {
			return domain.AuditEventPage{}, err
		}
		_ = json.Unmarshal([]byte(data), &event.Data)
		event.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		page.Events = append(page.Events, event)
	}
	if err := rows.Err(); err != nil {
		return domain.AuditEventPage{}, err
	}
	if len(page.Events) > limit {
		page.HasMore = true
		page.Events = page.Events[:limit]
	}
	if page.HasMore && len(page.Events) > 0 {
		last := page.Events[len(page.Events)-1]
		page.NextCreatedAt, page.NextID = last.CreatedAt, last.ID
	}
	return page, nil
}

func auditEventsPageQuery(runID string, limit int, cursorCreated time.Time, cursorID string) (string, []any) {
	statement := `SELECT id,run_id,event_type,actor,data_json,created_at FROM audit_events`
	arguments := make([]any, 0, 6)
	conditions := make([]string, 0, 2)
	if runID != "" {
		conditions = append(conditions, "run_id=?")
		arguments = append(arguments, runID)
	}
	if !cursorCreated.IsZero() {
		conditions = append(conditions, "(created_at,id)<(?,?)")
		created := formatTime(cursorCreated.UTC())
		arguments = append(arguments, created, strings.TrimSpace(cursorID))
	}
	if len(conditions) > 0 {
		statement += " WHERE " + strings.Join(conditions, " AND ")
	}
	statement += " ORDER BY created_at DESC,id DESC LIMIT ?"
	arguments = append(arguments, limit+1)
	return statement, arguments
}

const deletableAuditRunSQL = `runs.status IN ('completed','failed','partial','interrupted','rejected','denied','expired','stopped','closed','skipped','cancelled','canceled','unavailable')
AND NOT EXISTS (SELECT 1 FROM approvals WHERE approvals.run_id=runs.id AND approvals.status='pending')
AND NOT EXISTS (SELECT 1 FROM tasks WHERE tasks.run_id=runs.id AND tasks.status IN ('created','pending','active','running','retrying','stopping','waiting_for_approval','approval_required'))
AND NOT EXISTS (SELECT 1 FROM ssh_shell_sessions WHERE ssh_shell_sessions.run_id=runs.id AND ssh_shell_sessions.status IN ('starting','running','stopping'))`

// DeleteAuditRuns removes completed audit runs in one conversation, direct
// operations (an empty session ID), or all scopes when sessionID is nil.
func (s *Store) DeleteAuditRuns(ctx context.Context, sessionID *string, actor string) (domain.AuditRunDeleteResult, error) {
	where := "1=1"
	var arguments []any
	scope := "all"
	resultSessionID := ""
	if sessionID != nil {
		resultSessionID = strings.TrimSpace(*sessionID)
		where = "runs.session_id=?"
		arguments = []any{resultSessionID}
		scope = "session"
		if resultSessionID == "" {
			scope = "direct"
		}
	}
	result, _, err := s.deleteAuditRuns(ctx, where, arguments, actor, scope, resultSessionID)
	return result, err
}

func (s *Store) deleteAuditRuns(ctx context.Context, where string, arguments []any, actor, scope, sessionID string) (domain.AuditRunDeleteResult, int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.AuditRunDeleteResult{}, 0, err
	}
	defer tx.Rollback()
	var total, deletable int
	countStatement := `SELECT COUNT(*),COALESCE(SUM(CASE WHEN ` + deletableAuditRunSQL + ` THEN 1 ELSE 0 END),0) FROM runs WHERE ` + where
	if err := tx.QueryRowContext(ctx, countStatement, arguments...).Scan(&total, &deletable); err != nil {
		return domain.AuditRunDeleteResult{}, 0, err
	}
	result := domain.AuditRunDeleteResult{Deleted: deletable, Retained: total - deletable, Scope: scope, SessionID: sessionID}
	if deletable == 0 {
		return result, total, tx.Commit()
	}
	selection := `SELECT runs.id FROM runs WHERE (` + where + `) AND (` + deletableAuditRunSQL + `)`
	statements := []string{
		`UPDATE chat_tool_calls SET run_id='' WHERE run_id IN (` + selection + `)`,
		`DELETE FROM ssh_shell_sessions WHERE run_id IN (` + selection + `)`,
		`DELETE FROM approvals WHERE run_id IN (` + selection + `)`,
		`DELETE FROM tasks WHERE run_id IN (` + selection + `)`,
		`DELETE FROM audit_events WHERE run_id IN (` + selection + `)`,
		`DELETE FROM runs WHERE id IN (` + selection + `)`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement, arguments...); err != nil {
			return domain.AuditRunDeleteResult{}, total, err
		}
	}
	if result.Retained > 0 {
		rows, err := tx.QueryContext(ctx, `SELECT runs.id FROM runs WHERE `+where+` ORDER BY started_at DESC,id DESC`, arguments...)
		if err != nil {
			return domain.AuditRunDeleteResult{}, total, err
		}
		for rows.Next() {
			var runID string
			if err := rows.Scan(&runID); err != nil {
				rows.Close()
				return domain.AuditRunDeleteResult{}, total, err
			}
			result.RetainedRunIDs = append(result.RetainedRunIDs, runID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return domain.AuditRunDeleteResult{}, total, err
		}
		if err := rows.Close(); err != nil {
			return domain.AuditRunDeleteResult{}, total, err
		}
		if len(result.RetainedRunIDs) != result.Retained {
			return domain.AuditRunDeleteResult{}, total, fmt.Errorf("collect retained audit runs: expected %d, got %d", result.Retained, len(result.RetainedRunIDs))
		}
	}
	if actor == "" {
		actor = "local-user"
	}
	eventData := map[string]any{
		"deleted":  result.Deleted,
		"retained": result.Retained,
		"scope":    result.Scope,
	}
	if result.SessionID != "" {
		eventData["session_id"] = result.SessionID
	}
	if len(result.RetainedRunIDs) > 0 {
		eventData["retained_run_ids"] = result.RetainedRunIDs
	}
	auditEvent := domain.AuditEvent{
		ID:        ids.New("evt"),
		Type:      "audit_records_deleted",
		Actor:     actor,
		Data:      eventData,
		CreatedAt: time.Now().UTC(),
	}
	data, err := json.Marshal(auditEvent.Data)
	if err != nil {
		return domain.AuditRunDeleteResult{}, total, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_events(id,run_id,event_type,actor,data_json,created_at) VALUES(?,?,?,?,?,?)`,
		auditEvent.ID, auditEvent.RunID, auditEvent.Type, auditEvent.Actor, string(data), formatTime(auditEvent.CreatedAt)); err != nil {
		return domain.AuditRunDeleteResult{}, total, err
	}
	if err := tx.Commit(); err != nil {
		return domain.AuditRunDeleteResult{}, total, err
	}
	result.AuditEventID = auditEvent.ID
	s.publishChange(Change{Topic: ChangeAudit, Audit: &auditEvent})
	return result, total, nil
}
