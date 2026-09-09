package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

// Existing timestamps use RFC3339Nano with variable fractional precision.
// Pad only the query key: lexical order of .1Z and .12Z is not time order.
const auditRunTimeSQL = `(substr(started_at,1,19)||'.'||substr(substr(started_at,21,max(length(started_at)-21,0))||'000000000',1,9)||'Z')`
const auditTimeLayout = "2006-01-02T15:04:05.000000000Z"

func auditHistoryWhere(filter domain.AuditHistoryFilter, snapshotBound bool) (string, []any, error) {
	if filter.Limit <= 0 || filter.Limit > 200 {
		return "", nil, fmt.Errorf("invalid audit page limit: use 1 through 200")
	}
	if filter.SnapshotAt.IsZero() {
		return "", nil, fmt.Errorf("audit snapshot_at is required")
	}
	if filter.Before != nil && (filter.Before.StartedAt.IsZero() || filter.Before.StartedAt.After(filter.SnapshotAt)) {
		return "", nil, fmt.Errorf("invalid audit cursor boundary")
	}
	where := " WHERE 1=1"
	var args []any
	if snapshotBound {
		where += " AND " + auditRunTimeSQL + "<=?"
		args = append(args, filter.SnapshotAt.UTC().Format(auditTimeLayout))
	}
	if filter.SessionID != nil {
		where += " AND session_id=?"
		args = append(args, *filter.SessionID)
	}
	if filter.Query != "" {
		where += ` AND (search_text LIKE ? ESCAPE '\' OR request_json LIKE ? ESCAPE '\' OR tool_arguments_json LIKE ? ESCAPE '\')`
		pattern := likePattern(filter.Query)
		args = append(args, pattern, pattern, pattern)
	}
	return where, args, nil
}

func auditHistoryGroupsQuery(filter domain.AuditHistoryFilter) (string, []any, error) {
	where, args, err := auditHistoryWhere(filter, true)
	if err != nil {
		return "", nil, err
	}
	selection := `SELECT session_id,MAX(` + auditRunTimeSQL + `) AS latest_started_at,
COUNT(*) AS run_count,SUM(status='approval_required') AS pending_count
FROM runs` + where + " GROUP BY session_id"
	if filter.Before != nil {
		selection += " HAVING (MAX(" + auditRunTimeSQL + "),session_id)<(?,?)"
		args = append(args, filter.Before.StartedAt.UTC().Format(auditTimeLayout), filter.Before.ID)
	}
	selection += " ORDER BY latest_started_at DESC,session_id DESC LIMIT ?"
	args = append(args, filter.Limit+1)
	// Resolve titles only for the selected page, including sessions older than
	// the chat sidebar's limit. Missing sessions do not remove their audit runs.
	statement := `WITH selected AS (` + selection + `)
SELECT selected.session_id,selected.latest_started_at,selected.run_count,selected.pending_count,
sessions.session_id IS NOT NULL,mcp.id IS NOT NULL,
CASE WHEN sessions.session_id IS NOT NULL THEN ` + chatSessionDisplayTitleSQL + `
ELSE COALESCE(mcp.client_name,'') END
FROM selected
LEFT JOIN chat_sessions AS sessions ON sessions.session_id=selected.session_id
LEFT JOIN mcp_client_sessions AS mcp ON mcp.id=selected.session_id
ORDER BY selected.latest_started_at DESC,selected.session_id DESC`
	return statement, args, nil
}

func (s *Store) ListAuditHistoryGroups(ctx context.Context, filter domain.AuditHistoryFilter) (domain.AuditHistoryGroupPage, error) {
	statement, args, err := auditHistoryGroupsQuery(filter)
	if err != nil {
		return domain.AuditHistoryGroupPage{}, err
	}
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return domain.AuditHistoryGroupPage{}, err
	}
	defer rows.Close()
	page := domain.AuditHistoryGroupPage{Groups: make([]domain.AuditHistoryGroup, 0, filter.Limit), SnapshotAt: filter.SnapshotAt}
	for rows.Next() {
		var group domain.AuditHistoryGroup
		var started string
		var chatExists, mcpExists bool
		if err := rows.Scan(&group.SessionID, &started, &group.RunCount, &group.PendingCount, &chatExists, &mcpExists, &group.Title); err != nil {
			return domain.AuditHistoryGroupPage{}, err
		}
		group.LatestStartedAt, err = time.Parse(time.RFC3339Nano, started)
		if err != nil {
			return domain.AuditHistoryGroupPage{}, err
		}
		group.Kind = "chat"
		group.SessionExists = chatExists
		if group.SessionID == "" {
			group.Kind = "direct"
		} else if mcpExists || strings.HasPrefix(group.SessionID, "mcp_sess_") {
			group.Kind = "mcp"
			group.SessionExists = mcpExists
		}
		page.Groups = append(page.Groups, group)
	}
	if err := rows.Err(); err != nil {
		return domain.AuditHistoryGroupPage{}, err
	}
	if len(page.Groups) > filter.Limit {
		page.HasMore = true
		page.Groups = page.Groups[:filter.Limit]
		last := page.Groups[len(page.Groups)-1]
		page.NextCursor = &domain.AuditHistoryCursor{StartedAt: last.LatestStartedAt, ID: last.SessionID}
	}
	return page, nil
}

func auditHistoryRunsQuery(filter domain.AuditHistoryFilter) (string, []any, error) {
	if filter.SessionID == nil {
		return "", nil, fmt.Errorf("audit session_id is required")
	}
	if filter.Before != nil && filter.Before.ID == "" {
		return "", nil, fmt.Errorf("invalid audit run cursor ID")
	}
	// Both cursor ranges are already below snapshot_at. A redundant upper
	// bound lets SQLite choose that weaker range and scan timestamp ties.
	where, args, err := auditHistoryWhere(filter, filter.Before == nil)
	if err != nil {
		return "", nil, err
	}
	const columns = "id,session_id,host_id,tool_name,request_json,status,exit_code,ai_review_json,started_at,completed_at"
	if filter.Before != nil {
		// SQLite does not seek the ID component of a row-value comparison
		// against this expression index. Separate the equal-time ID range
		// from older timestamps so deep pages don't scan all timestamp ties.
		source := "SELECT " + columns + "," + auditRunTimeSQL + " AS audit_time FROM runs" + where
		// Limit each disjoint range before merging. The final sort sees at
		// most 2*(limit+1) rows, never the whole equal-timestamp bucket.
		statement := "WITH same_time AS (" + source + " AND " + auditRunTimeSQL + "=? AND id<? ORDER BY id DESC LIMIT ?)," +
			"earlier AS (" + source + " AND " + auditRunTimeSQL + "<? ORDER BY " + auditRunTimeSQL + " DESC,id DESC LIMIT ?) " +
			"SELECT " + columns + " FROM (SELECT * FROM same_time UNION ALL SELECT * FROM earlier) ORDER BY audit_time DESC,id DESC LIMIT ?"
		boundary := filter.Before.StartedAt.UTC().Format(auditTimeLayout)
		cursorArgs := append([]any{}, args...)
		cursorArgs = append(cursorArgs, boundary, filter.Before.ID, filter.Limit+1)
		cursorArgs = append(cursorArgs, args...)
		cursorArgs = append(cursorArgs, boundary, filter.Limit+1, filter.Limit+1)
		return statement, cursorArgs, nil
	}
	statement := "SELECT " + columns + " FROM runs" + where + " ORDER BY " + auditRunTimeSQL + " DESC,id DESC LIMIT ?"
	return statement, append(args, filter.Limit+1), nil
}

func (s *Store) ListAuditHistoryRuns(ctx context.Context, filter domain.AuditHistoryFilter) (domain.AuditHistoryRunPage, error) {
	statement, args, err := auditHistoryRunsQuery(filter)
	if err != nil {
		return domain.AuditHistoryRunPage{}, err
	}
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return domain.AuditHistoryRunPage{}, err
	}
	defer rows.Close()
	page := domain.AuditHistoryRunPage{Runs: make([]domain.Run, 0, filter.Limit), SnapshotAt: filter.SnapshotAt}
	for rows.Next() {
		run, err := scanRunSummary(rows)
		if err != nil {
			return domain.AuditHistoryRunPage{}, err
		}
		page.Runs = append(page.Runs, run)
	}
	if err := rows.Err(); err != nil {
		return domain.AuditHistoryRunPage{}, err
	}
	if len(page.Runs) > filter.Limit {
		page.HasMore = true
		page.Runs = page.Runs[:filter.Limit]
		last := page.Runs[len(page.Runs)-1]
		page.NextCursor = &domain.AuditHistoryCursor{StartedAt: last.StartedAt, ID: last.ID}
	}
	return page, nil
}
