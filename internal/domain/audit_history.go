package domain

import "time"

// AuditHistoryCursor identifies a group by session ID or a run by run ID.
// An empty group ID is valid: it denotes direct/legacy operations.
type AuditHistoryCursor struct {
	StartedAt time.Time `json:"started_at"`
	ID        string    `json:"id"`
}

type AuditHistoryFilter struct {
	Query         string
	HostID        string
	SessionID     *string // nil: all groups; pointer to "": only direct operations
	StartedAfter  time.Time
	StartedBefore time.Time
	SnapshotAt    time.Time
	Before        *AuditHistoryCursor
	Limit         int
}

type AuditHistoryGroup struct {
	SessionID       string    `json:"session_id"`
	Kind            string    `json:"kind"` // chat, mcp, direct
	Title           string    `json:"title"`
	SessionExists   bool      `json:"session_exists"`
	LatestStartedAt time.Time `json:"latest_started_at"`
	RunCount        int       `json:"run_count"`
	PendingCount    int       `json:"pending_count"`
}

// SnapshotAt is a historical time ceiling, not a frozen database snapshot:
// status changes and deletions remain visible when a page is revalidated.
type AuditHistoryGroupPage struct {
	Groups     []AuditHistoryGroup `json:"groups"`
	SnapshotAt time.Time           `json:"snapshot_at"`
	HasMore    bool                `json:"has_more"`
	NextCursor *AuditHistoryCursor `json:"next_cursor,omitempty"`
}

type AuditHistoryRunPage struct {
	Runs       []Run               `json:"runs"`
	SnapshotAt time.Time           `json:"snapshot_at"`
	HasMore    bool                `json:"has_more"`
	NextCursor *AuditHistoryCursor `json:"next_cursor,omitempty"`
}
