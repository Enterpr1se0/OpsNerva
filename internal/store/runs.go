package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func (s *Store) InterruptActiveRuns(ctx context.Context, reason string) error {
	now := formatTime(time.Now().UTC())
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET status='interrupted',
error=CASE WHEN error='' THEN ? ELSE error END,completed_at=COALESCE(completed_at,?)
WHERE status IN ('created','running')`, reason, now)
	return err
}

func (s *Store) CreateRun(ctx context.Context, run domain.Run) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO runs(id,session_id,host_id,tool_name,tool_arguments_json,request_json,request_cipher,search_text,request_digest,status,ai_review_json,
started_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, run.ID, run.SessionID, run.HostID, run.ToolName, run.ToolArgumentsJSON, run.RequestJSON, run.RequestCipher, run.SearchText, run.RequestDigest,
		run.Status, run.AIReviewJSON, formatTime(run.StartedAt))
	return err
}

func (s *Store) UpdateRun(ctx context.Context, run domain.Run) error {
	var completed any
	if !run.CompletedAt.IsZero() {
		completed = formatTime(run.CompletedAt)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET status=?,exit_code=?,stdout_redacted=?,stderr_redacted=?,
stdout_cipher=?,stderr_cipher=?,error=?,completed_at=? WHERE id=?`, run.Status, run.ExitCode,
		run.StdoutRedacted, run.StderrRedacted, run.StdoutCipher, run.StderrCipher, run.Error, completed, run.ID)
	return err
}

func (s *Store) GetRun(ctx context.Context, id string) (domain.Run, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,session_id,host_id,tool_name,tool_arguments_json,request_json,request_cipher,request_digest,status,
exit_code,stdout_redacted,stderr_redacted,stdout_cipher,stderr_cipher,error,ai_review_json,started_at,completed_at
FROM runs WHERE id=?`, id)
	return scanRun(row)
}

// likePattern wraps query in wildcards for a substring LIKE match, escaping
// every character LIKE treats specially so the query only matches literally.
func likePattern(query string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query)
	return "%" + escaped + "%"
}

func (s *Store) SearchRuns(ctx context.Context, query, hostID, sessionID string, limit int) ([]domain.Run, error) {
	return s.SearchRunsFiltered(ctx, domain.RunSearchFilter{
		Query: query, QueryScope: "all", HostID: hostID, SessionID: sessionID, Limit: limit,
	})
}

func (s *Store) SearchRunsFiltered(ctx context.Context, filter domain.RunSearchFilter) ([]domain.Run, error) {
	where, arguments, err := runSearchWhere(filter, true)
	if err != nil {
		return nil, err
	}
	statement := `SELECT id,session_id,host_id,tool_name,tool_arguments_json,request_json,request_cipher,request_digest,status,
exit_code,stdout_redacted,stderr_redacted,stdout_cipher,stderr_cipher,error,ai_review_json,started_at,completed_at
FROM runs` + where + " ORDER BY started_at DESC,id DESC"
	if filter.Limit > 0 {
		statement += " LIMIT ?"
		arguments = append(arguments, filter.Limit)
	}
	rows, err := s.db.QueryContext(ctx, statement, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.Run, 0)
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, run)
	}
	return result, rows.Err()
}

func runSearchWhere(filter domain.RunSearchFilter, literalQuery bool) (string, []any, error) {
	statement := " WHERE 1=1"
	arguments := make([]any, 0, 16)
	if filter.SessionID != "" {
		statement += " AND session_id=?"
		arguments = append(arguments, filter.SessionID)
	}
	if filter.HostID != "" {
		statement += " AND host_id=?"
		arguments = append(arguments, filter.HostID)
	}
	if filter.ToolName != "" {
		statement += " AND tool_name=?"
		arguments = append(arguments, filter.ToolName)
	}
	if filter.Status != "" {
		statement += " AND status=?"
		arguments = append(arguments, filter.Status)
	}
	if !filter.StartedAfter.IsZero() {
		statement += " AND started_at>=?"
		arguments = append(arguments, formatTime(filter.StartedAfter.UTC()))
	}
	if !filter.StartedBefore.IsZero() {
		statement += " AND started_at<=?"
		arguments = append(arguments, formatTime(filter.StartedBefore.UTC()))
	}
	if filter.CursorStarted.IsZero() != (filter.CursorID == "") {
		return "", nil, fmt.Errorf("invalid history cursor boundary")
	}
	if !filter.CursorStarted.IsZero() {
		cursorTime := formatTime(filter.CursorStarted.UTC())
		statement += " AND (started_at,id)<(?,?)"
		arguments = append(arguments, cursorTime, filter.CursorID)
	}
	if literalQuery && filter.Query != "" {
		pattern := likePattern(filter.Query)
		switch filter.QueryScope {
		case "", "all":
			statement += ` AND (search_text LIKE ? ESCAPE '\' OR request_json LIKE ? ESCAPE '\' OR tool_arguments_json LIKE ? ESCAPE '\'
				OR stdout_redacted LIKE ? ESCAPE '\' OR stderr_redacted LIKE ? ESCAPE '\')`
			arguments = append(arguments, pattern, pattern, pattern, pattern, pattern)
		case "request":
			statement += ` AND (search_text LIKE ? ESCAPE '\' OR request_json LIKE ? ESCAPE '\' OR tool_arguments_json LIKE ? ESCAPE '\')`
			arguments = append(arguments, pattern, pattern, pattern)
		case "output":
			statement += ` AND (stdout_redacted LIKE ? ESCAPE '\' OR stderr_redacted LIKE ? ESCAPE '\')`
			arguments = append(arguments, pattern, pattern)
		default:
			return "", nil, fmt.Errorf("invalid history query_scope: use all, request, or output")
		}
	}
	return statement, arguments, nil
}

// SearchRunSummariesFilteredPage is the bounded literal history search.
// It only selects fields needed for summaries and always requires a bounded
// page size; detail and legacy CLI callers continue to use SearchRunsFiltered.
func (s *Store) SearchRunSummariesFilteredPage(ctx context.Context, filter domain.RunSearchFilter) (domain.RunSearchPage, error) {
	if filter.Limit <= 0 {
		return domain.RunSearchPage{}, fmt.Errorf("history page limit must be positive")
	}
	where, arguments, err := runSearchWhere(filter, true)
	if err != nil {
		return domain.RunSearchPage{}, err
	}
	statement := `SELECT id,session_id,host_id,tool_name,request_json,status,exit_code,ai_review_json,started_at,completed_at
FROM runs` + where + " ORDER BY started_at DESC,id DESC LIMIT ?"
	arguments = append(arguments, filter.Limit+1)
	rows, err := s.db.QueryContext(ctx, statement, arguments...)
	if err != nil {
		return domain.RunSearchPage{}, err
	}
	defer rows.Close()
	runs := make([]domain.Run, 0, filter.Limit+1)
	for rows.Next() {
		run, err := scanRunSummary(rows)
		if err != nil {
			return domain.RunSearchPage{}, err
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return domain.RunSearchPage{}, err
	}
	page := domain.RunSearchPage{Runs: runs}
	if len(page.Runs) > filter.Limit {
		page.HasMore = true
		page.Runs = page.Runs[:filter.Limit]
	}
	if page.HasMore && len(page.Runs) > 0 {
		last := page.Runs[len(page.Runs)-1]
		page.NextStartedAt, page.NextID = last.StartedAt, last.ID
	}
	return page, nil
}

func scanRunSummary(row scanner) (domain.Run, error) {
	var run domain.Run
	var reviewJSON string
	var started string
	var completed sql.NullString
	err := row.Scan(&run.ID, &run.SessionID, &run.HostID, &run.ToolName, &run.RequestJSON,
		&run.Status, &run.ExitCode, &reviewJSON, &started, &completed)
	if err != nil {
		return domain.Run{}, err
	}
	if reviewJSON != "" {
		var review domain.CommandReview
		if json.Unmarshal([]byte(reviewJSON), &review) == nil {
			run.AIReview = &review
		}
	}
	run.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
	if completed.Valid {
		run.CompletedAt, _ = time.Parse(time.RFC3339Nano, completed.String)
	}
	return run, nil
}

func scanRunRegexCandidate(row scanner) (domain.Run, error) {
	var run domain.Run
	var started string
	var completed sql.NullString
	err := row.Scan(&run.ID, &run.SessionID, &run.HostID, &run.ToolName, &run.ToolArgumentsJSON,
		&run.RequestJSON, &run.Status, &run.ExitCode, &run.StdoutRedacted, &run.StderrRedacted,
		&started, &completed)
	if err != nil {
		return domain.Run{}, err
	}
	run.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
	if completed.Valid {
		run.CompletedAt, _ = time.Parse(time.RFC3339Nano, completed.String)
	}
	return run, nil
}

func runMatchesHistoryRegex(expression *regexp.Regexp, run domain.Run, queryScope string) (bool, error) {
	var parts []string
	switch queryScope {
	case "", "all":
		parts = []string{run.RequestJSON, run.ToolArgumentsJSON, run.StdoutRedacted, run.StderrRedacted}
	case "request":
		parts = []string{run.RequestJSON, run.ToolArgumentsJSON}
	case "output":
		parts = []string{run.StdoutRedacted, run.StderrRedacted}
	default:
		return false, fmt.Errorf("invalid history query_scope: use all, request, or output")
	}
	if queryScope != "output" {
		var request domain.ExecRequest
		if json.Unmarshal([]byte(run.RequestJSON), &request) == nil {
			parts = append(parts, request.SearchText())
		}
	}
	return expression.MatchString(strings.Join(parts, "\n")), nil
}

// SearchRunSummariesRegexFilteredPage streams regex candidates instead of
// materializing every complete Run. ScanLimit bounds work even when matches
// are rare; NextStartedAt/NextID then point after the last inspected row.
func (s *Store) SearchRunSummariesRegexFilteredPage(ctx context.Context, pattern string, filter domain.RunSearchFilter) (domain.RunSearchPage, error) {
	if filter.Limit <= 0 {
		return domain.RunSearchPage{}, fmt.Errorf("history page limit must be positive")
	}
	expression, err := regexp.CompilePOSIX(pattern)
	if err != nil {
		return domain.RunSearchPage{}, fmt.Errorf("invalid POSIX history regex: %w", err)
	}
	where, arguments, err := runSearchWhere(filter, false)
	if err != nil {
		return domain.RunSearchPage{}, err
	}
	statement := `SELECT id,session_id,host_id,tool_name,tool_arguments_json,request_json,status,exit_code,
stdout_redacted,stderr_redacted,started_at,completed_at FROM runs` + where + " ORDER BY started_at DESC,id DESC"
	rows, err := s.db.QueryContext(ctx, statement, arguments...)
	if err != nil {
		return domain.RunSearchPage{}, err
	}
	defer rows.Close()
	page := domain.RunSearchPage{Runs: make([]domain.Run, 0, filter.Limit+1)}
	var lastScanned domain.Run
	scanned := 0
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return domain.RunSearchPage{}, err
		}
		run, err := scanRunRegexCandidate(rows)
		if err != nil {
			return domain.RunSearchPage{}, err
		}
		lastScanned = run
		scanned++
		matched, err := runMatchesHistoryRegex(expression, run, filter.QueryScope)
		if err != nil {
			return domain.RunSearchPage{}, err
		}
		if matched {
			// Search output is a summary; discard candidate-only large fields.
			run.ToolArgumentsJSON = ""
			run.StdoutRedacted = ""
			run.StderrRedacted = ""
			page.Runs = append(page.Runs, run)
			if len(page.Runs) > filter.Limit {
				page.HasMore = true
				page.Runs = page.Runs[:filter.Limit]
				last := page.Runs[len(page.Runs)-1]
				page.NextStartedAt, page.NextID = last.StartedAt, last.ID
				return page, nil
			}
		}
		if filter.ScanLimit > 0 && scanned >= filter.ScanLimit {
			if rows.Next() {
				page.HasMore = true
				page.ScanLimited = true
				page.NextStartedAt, page.NextID = lastScanned.StartedAt, lastScanned.ID
			} else if err := rows.Err(); err != nil {
				return domain.RunSearchPage{}, err
			}
			return page, nil
		}
	}
	if err := rows.Err(); err != nil {
		return domain.RunSearchPage{}, err
	}
	return page, nil
}

func scanRun(row scanner) (domain.Run, error) {
	var run domain.Run
	var started string
	var completed sql.NullString
	err := row.Scan(&run.ID, &run.SessionID, &run.HostID, &run.ToolName, &run.ToolArgumentsJSON, &run.RequestJSON, &run.RequestCipher, &run.RequestDigest,
		&run.Status, &run.ExitCode, &run.StdoutRedacted, &run.StderrRedacted, &run.StdoutCipher,
		&run.StderrCipher, &run.Error, &run.AIReviewJSON, &started, &completed)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Run{}, ErrNotFound
	}
	if err != nil {
		return domain.Run{}, err
	}
	if run.AIReviewJSON != "" {
		var review domain.CommandReview
		if json.Unmarshal([]byte(run.AIReviewJSON), &review) == nil {
			run.AIReview = &review
		}
	}
	run.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
	if completed.Valid {
		run.CompletedAt, _ = time.Parse(time.RFC3339Nano, completed.String)
	}
	return run, nil
}
