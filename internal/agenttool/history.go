package agenttool

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

type HistoryService interface {
	GetRunRecord(context.Context, string) (domain.Run, error)
	SearchRunSummariesMatchingPage(context.Context, domain.RunSearchFilter, domain.FileSearchMatchMode) (domain.RunSearchPage, error)
}

type History struct {
	service HistoryService
}

func NewHistory(service HistoryService) *History {
	return &History{service: service}
}

const (
	defaultHistorySearchLimit = 20
	maxHistorySearchLimit     = 100
	defaultHistoryOutputBytes = 16 << 10
	maxHistoryOutputBytes     = 64 << 10
	maxHistoryStructuredBytes = 8 << 10
	maxHistoryErrorBytes      = 4 << 10
	maxHistoryRegexBytes      = 512
	maxHistoryQueryBytes      = 4 << 10
	historyRegexScanLimit     = 2000
	maxHistoryOperationBytes  = 512
)

type historySearchCursor struct {
	StartedAt string `json:"started_at"`
	ID        string `json:"id"`
}

func encodeHistoryCursor(startedAt time.Time, id string) string {
	if startedAt.IsZero() || id == "" {
		return ""
	}
	encoded, _ := json.Marshal(historySearchCursor{StartedAt: startedAt.UTC().Format(time.RFC3339Nano), ID: id})
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func decodeHistoryCursor(value string) (time.Time, string, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return time.Time{}, "", fmt.Errorf("invalid history cursor: %w", err)
	}
	var cursor historySearchCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil {
		return time.Time{}, "", fmt.Errorf("invalid history cursor: %w", err)
	}
	startedAt, err := time.Parse(time.RFC3339Nano, cursor.StartedAt)
	if err != nil || strings.TrimSpace(cursor.ID) == "" {
		return time.Time{}, "", fmt.Errorf("invalid history cursor boundary")
	}
	return startedAt.UTC(), strings.TrimSpace(cursor.ID), nil
}

func normalizeHistoryMatch(query string, matchMode domain.FileSearchMatchMode, queryScope string) (domain.FileSearchMatchMode, string, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		if matchMode != "" || strings.TrimSpace(queryScope) != "" {
			return "", "", fmt.Errorf("invalid history input: query is required with match_mode or query_scope")
		}
		return domain.FileSearchLiteral, "all", nil
	}
	if len(query) > maxHistoryQueryBytes {
		return "", "", fmt.Errorf("invalid history input: query must not exceed %d bytes", maxHistoryQueryBytes)
	}
	if matchMode == "" {
		matchMode = domain.FileSearchLiteral
	}
	if matchMode != domain.FileSearchLiteral && matchMode != domain.FileSearchRegex {
		return "", "", fmt.Errorf("invalid history input: match_mode must be literal or regex")
	}
	if matchMode == domain.FileSearchRegex && len(query) > maxHistoryRegexBytes {
		return "", "", fmt.Errorf("invalid history input: regex query must not exceed %d bytes", maxHistoryRegexBytes)
	}
	queryScope = strings.TrimSpace(queryScope)
	if queryScope == "" {
		queryScope = "all"
	}
	if queryScope != "all" && queryScope != "request" && queryScope != "output" {
		return "", "", fmt.Errorf("invalid history input: query_scope must be all, request, or output")
	}
	return matchMode, queryScope, nil
}

func compileHistoryMatcher(query string, matchMode domain.FileSearchMatchMode) (*regexp.Regexp, error) {
	if matchMode == domain.FileSearchRegex {
		expression, err := regexp.CompilePOSIX(query)
		if err != nil {
			return nil, fmt.Errorf("invalid POSIX history regex: %w", err)
		}
		return expression, nil
	}
	return regexp.Compile(regexp.QuoteMeta(query))
}

func historyMatchWindow(value string, matchStart, matchEnd, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	windowStart := matchStart - maxBytes/3
	if windowStart < 0 {
		windowStart = 0
	}
	windowEnd := windowStart + maxBytes
	if windowEnd < matchEnd {
		windowEnd = matchEnd
		windowStart = max(0, windowEnd-maxBytes)
	}
	if windowEnd > len(value) {
		windowEnd = len(value)
		windowStart = max(0, windowEnd-maxBytes)
	}
	for windowStart < len(value) && !utf8.RuneStart(value[windowStart]) {
		windowStart++
	}
	for windowEnd > windowStart && windowEnd < len(value) && !utf8.RuneStart(value[windowEnd]) {
		windowEnd--
	}
	prefix, suffix := "", ""
	if windowStart > 0 {
		prefix = "..."
	}
	if windowEnd < len(value) {
		suffix = "..."
	}
	return prefix + value[windowStart:windowEnd] + suffix
}

func historyMatchExcerpt(value string, expression *regexp.Regexp, maxBytes, maxMatches int) (string, bool, bool) {
	if value == "" {
		return "", false, false
	}
	matches := expression.FindAllStringIndex(value, maxMatches+1)
	if len(matches) == 0 {
		return "", false, false
	}
	var result strings.Builder
	limited := len(matches) > maxMatches
	if limited {
		matches = matches[:maxMatches]
	}
	lastStart, lastEnd := -1, -1
	lastIncludedWholeLine := false
	for _, match := range matches {
		matchStart, matchEnd := match[0], match[1]
		lineStart := strings.LastIndex(value[:matchStart], "\n") + 1
		lineEnd := len(value)
		if newline := strings.IndexByte(value[matchEnd:], '\n'); newline >= 0 {
			lineEnd = matchEnd + newline
		}
		if lineStart != lastStart || lineEnd != lastEnd || !lastIncludedWholeLine {
			remaining := maxBytes - result.Len()
			separator := ""
			if result.Len() > 0 {
				separator = "\n"
				remaining--
			}
			if remaining <= 0 {
				limited = true
				break
			}
			excerpt := historyMatchWindow(value[lineStart:lineEnd], matchStart-lineStart, matchEnd-lineStart, remaining)
			if len(excerpt) > remaining {
				excerpt = UTF8Prefix(excerpt, remaining)
				limited = true
			}
			result.WriteString(separator)
			result.WriteString(excerpt)
			lastStart, lastEnd = lineStart, lineEnd
			lastIncludedWholeLine = lineEnd-lineStart <= remaining
		}
	}
	return result.String(), true, limited
}

func historyRunMatches(run domain.Run, query string, matchMode domain.FileSearchMatchMode, queryScope string, maxBytes, matchLimit int) (HistoryRunMatch, error) {
	expression, err := compileHistoryMatcher(query, matchMode)
	if err != nil {
		return HistoryRunMatch{}, err
	}
	result := HistoryRunMatch{HistoryRunSummary: historyRunSummary(run), MatchMode: matchMode, QueryScope: queryScope, MatchLimit: matchLimit}
	if queryScope != "output" {
		var request domain.ExecRequest
		_ = json.Unmarshal([]byte(run.RequestJSON), &request)
		result.RequestMatched = expression.MatchString(run.RequestJSON) || expression.MatchString(request.SearchText())
		result.ToolArgumentsMatched = expression.MatchString(run.ToolArgumentsJSON)
	}
	if queryScope != "request" {
		stdout, stdoutFound, stdoutLimited := historyMatchExcerpt(run.StdoutRedacted, expression, maxBytes, matchLimit)
		stderr, stderrFound, stderrLimited := historyMatchExcerpt(run.StderrRedacted, expression, maxBytes, matchLimit)
		result.StdoutExcerpt, result.StderrExcerpt = stdout, stderr
		result.OutputLimited = stdoutLimited || stderrLimited
		result.Found = stdoutFound || stderrFound
	}
	result.Found = result.Found || result.RequestMatched || result.ToolArgumentsMatched
	return result, nil
}

func historyJSON(raw string) any {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	if len(raw) > maxHistoryStructuredBytes {
		return map[string]any{
			"output_limited": true,
			"original_bytes": len(raw),
			"preview":        LimitText(raw, maxHistoryStructuredBytes, "head_tail"),
		}
	}
	var value any
	if json.Unmarshal([]byte(raw), &value) == nil {
		return value
	}
	return raw
}

func historyOperation(run domain.Run, request domain.ExecRequest) string {
	switch request.Mode {
	case domain.ExecProgram:
		return strings.Join(append([]string{request.Program}, request.Args...), " ")
	case domain.ExecScript, domain.ExecWorkspaceShell:
		if strings.TrimSpace(request.Reason) != "" {
			return request.Reason
		}
	case domain.ExecRemoteRead:
		return "read " + request.RemotePath
	case domain.ExecRemoteSearch:
		return "search " + request.RemotePath
	case domain.ExecRemoteEdit:
		return "edit " + request.RemotePath
	case domain.ExecWorkspaceRead:
		return "read " + request.WorkspaceID + ":" + request.RelativePath
	case domain.ExecWorkspaceSearch:
		return "search " + request.WorkspaceID + ":" + request.RelativePath
	case domain.ExecWorkspaceEdit:
		return "edit " + request.WorkspaceID + ":" + request.RelativePath
	case domain.ExecWorkspaceDelete:
		return "delete " + request.WorkspaceID + ":" + request.RelativePath
	case domain.ExecWorkspaceDirectoryList:
		return "list " + request.WorkspaceID + ":" + request.RelativePath
	case domain.ExecWorkspaceUpload:
		return request.WorkspaceID + ":" + request.RelativePath + " -> " + request.RemotePath
	case domain.ExecWorkspaceDownload:
		return request.RemotePath + " -> " + request.WorkspaceID + ":" + request.RelativePath
	case domain.ExecSSHFileTransfer:
		return request.SourceHostID + ":" + request.SourcePath + " -> " + request.HostID + ":" + request.RemotePath
	case domain.ExecSSHTunnelStart:
		localHost := request.TunnelLocalHost
		if localHost == "" {
			localHost = "127.0.0.1"
		}
		if request.TunnelDirection == domain.SSHTunnelDirectionReverse {
			return fmt.Sprintf("%s:%d <- %s:%d", localHost, request.TunnelLocalPort, request.TunnelRemoteHost, request.TunnelRemotePort)
		}
		return fmt.Sprintf("%s:%d -> %s:%d", localHost, request.TunnelLocalPort, request.TunnelRemoteHost, request.TunnelRemotePort)
	case domain.ExecSSHShellStart, domain.ExecWorkspaceShellStart:
		return string(request.Mode)
	}
	if request.Mode != "" {
		return string(request.Mode)
	}
	if run.ToolName != "" {
		return run.ToolName
	}
	return "execution"
}

func historyRunSummary(run domain.Run) HistoryRunSummary {
	var request domain.ExecRequest
	_ = json.Unmarshal([]byte(run.RequestJSON), &request)
	duration := int64(0)
	if !run.CompletedAt.IsZero() && !run.StartedAt.IsZero() && !run.CompletedAt.Before(run.StartedAt) {
		duration = run.CompletedAt.Sub(run.StartedAt).Milliseconds()
	}
	operation := historyOperation(run, request)
	if len(operation) > maxHistoryOperationBytes {
		operation = UTF8Prefix(operation, maxHistoryOperationBytes-3) + "..."
	}
	return HistoryRunSummary{
		ID: run.ID, HostID: run.HostID, ToolName: run.ToolName, Mode: string(request.Mode),
		Operation: operation, Status: run.Status, ExitCode: run.ExitCode,
		DurationMS: duration, StartedAt: run.StartedAt, CompletedAt: run.CompletedAt,
	}
}

func historyRunDetail(run domain.Run, stdoutOffset, stderrOffset, maxBytes int, view string) (HistoryRunDetail, error) {
	selected, err := SelectExecOutput(domain.ExecResult{Stdout: run.StdoutRedacted, Stderr: run.StderrRedacted},
		stdoutOffset, stderrOffset, maxBytes, view, true)
	if err != nil {
		return HistoryRunDetail{}, err
	}
	errorText := run.Error
	errorLimited := false
	if len(errorText) > maxHistoryErrorBytes {
		errorText = LimitText(errorText, maxHistoryErrorBytes, "head_tail")
		errorLimited = true
	}
	detail := HistoryRunDetail{
		HistoryRunSummary: historyRunSummary(run), ToolArguments: historyJSON(run.ToolArgumentsJSON),
		Request: historyJSON(run.RequestJSON), Stdout: selected.Stdout, Stderr: selected.Stderr, Error: errorText,
		OutputView: selected.OutputView, OutputLimited: selected.OutputLimited,
		StdoutTotalBytes: selected.StdoutTotalBytes, StderrTotalBytes: selected.StderrTotalBytes,
		StdoutOmittedBytes: selected.StdoutOmittedBytes, StderrOmittedBytes: selected.StderrOmittedBytes,
		StdoutOffsetBytes: selected.StdoutOffsetBytes, StderrOffsetBytes: selected.StderrOffsetBytes,
		ErrorLimited: errorLimited,
	}
	if errorLimited {
		detail.ErrorTotalBytes = len(run.Error)
	}
	if selected.OutputView == "head" {
		if next := stdoutOffset + len(selected.Stdout); next < selected.StdoutTotalBytes {
			detail.StdoutNextOffset = next
		}
		if next := stderrOffset + len(selected.Stderr); next < selected.StderrTotalBytes {
			detail.StderrNextOffset = next
		}
	}
	return detail, nil
}

func historyRunSummaries(runs []domain.Run) []HistoryRunSummary {
	result := make([]HistoryRunSummary, len(runs))
	for index, run := range runs {
		result[index] = historyRunSummary(run)
	}
	return result
}

func (history *History) Read(ctx context.Context, input HistorySearchInput) (HistoryOutput, error) {
	runID := strings.TrimSpace(input.RunID)
	if runID != "" {
		if input.HostID != "" || input.ToolName != "" || input.Status != "" || input.StartedAfter != "" || input.StartedBefore != "" || input.Cursor != "" {
			return HistoryOutput{}, fmt.Errorf("invalid history input: run_id cannot be combined with list filters or cursor")
		}
		if input.AfterStdout < 0 || input.AfterStderr < 0 {
			return HistoryOutput{}, fmt.Errorf("invalid history input: output byte offsets must be non-negative")
		}
		maxOutput := input.MaxOutput
		if maxOutput == 0 {
			maxOutput = defaultHistoryOutputBytes
		}
		if maxOutput < 1024 || maxOutput > maxHistoryOutputBytes {
			return HistoryOutput{}, fmt.Errorf("invalid history input: max_output_bytes must be between 1024 and %d", maxHistoryOutputBytes)
		}
		matchMode, queryScope, err := normalizeHistoryMatch(input.Query, input.MatchMode, input.QueryScope)
		if err != nil {
			return HistoryOutput{}, err
		}
		if strings.TrimSpace(input.Query) != "" {
			if input.AfterStdout != 0 || input.AfterStderr != 0 || input.OutputView != "" {
				return HistoryOutput{}, fmt.Errorf("invalid history input: run_id query cannot be combined with output offsets or output_view")
			}
			matchLimit := input.Limit
			if matchLimit == 0 {
				matchLimit = defaultHistorySearchLimit
			}
			if matchLimit < 1 || matchLimit > maxHistorySearchLimit {
				return HistoryOutput{}, fmt.Errorf("invalid history input: limit must be between 1 and %d", maxHistorySearchLimit)
			}
			run, err := history.service.GetRunRecord(ctx, runID)
			if err != nil {
				return HistoryOutput{}, err
			}
			matched, err := historyRunMatches(run, input.Query, matchMode, queryScope, maxOutput, matchLimit)
			if err != nil {
				return HistoryOutput{}, err
			}
			return HistoryOutput{Match: &matched}, nil
		}
		if input.Limit != 0 {
			return HistoryOutput{}, fmt.Errorf("invalid history input: limit requires query when run_id is set")
		}
		outputView := strings.ToLower(strings.TrimSpace(input.OutputView))
		if outputView == "" && (input.AfterStdout > 0 || input.AfterStderr > 0) {
			outputView = "head"
		}
		if (input.AfterStdout > 0 || input.AfterStderr > 0) && outputView != "head" {
			return HistoryOutput{}, fmt.Errorf("invalid history input: output byte offsets require output_view=head")
		}
		run, err := history.service.GetRunRecord(ctx, runID)
		if err != nil {
			return HistoryOutput{}, err
		}
		detail, err := historyRunDetail(run, input.AfterStdout, input.AfterStderr, maxOutput, outputView)
		if err != nil {
			return HistoryOutput{}, fmt.Errorf("invalid history input: %w", err)
		}
		return HistoryOutput{Run: &detail}, nil
	}
	if input.AfterStdout != 0 || input.AfterStderr != 0 || input.MaxOutput != 0 || input.OutputView != "" {
		return HistoryOutput{}, fmt.Errorf("invalid history input: output paging fields require run_id")
	}
	limit := input.Limit
	if limit == 0 {
		limit = defaultHistorySearchLimit
	}
	if limit < 1 || limit > maxHistorySearchLimit {
		return HistoryOutput{}, fmt.Errorf("invalid history input: limit must be between 1 and %d", maxHistorySearchLimit)
	}
	matchMode, queryScope, err := normalizeHistoryMatch(input.Query, input.MatchMode, input.QueryScope)
	if err != nil {
		return HistoryOutput{}, err
	}
	parseBound := func(name, value string) (time.Time, error) {
		if strings.TrimSpace(value) == "" {
			return time.Time{}, nil
		}
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid history input: %s must be RFC3339: %w", name, err)
		}
		return parsed.UTC(), nil
	}
	startedAfter, err := parseBound("started_after", input.StartedAfter)
	if err != nil {
		return HistoryOutput{}, err
	}
	startedBefore, err := parseBound("started_before", input.StartedBefore)
	if err != nil {
		return HistoryOutput{}, err
	}
	if !startedAfter.IsZero() && !startedBefore.IsZero() && startedAfter.After(startedBefore) {
		return HistoryOutput{}, fmt.Errorf("invalid history input: started_after must not be later than started_before")
	}
	cursorStarted, cursorID, err := decodeHistoryCursor(input.Cursor)
	if err != nil {
		return HistoryOutput{}, err
	}
	page, err := history.service.SearchRunSummariesMatchingPage(ctx, domain.RunSearchFilter{
		Query: input.Query, QueryScope: queryScope, HostID: strings.TrimSpace(input.HostID),
		ToolName: strings.TrimSpace(input.ToolName), Status: strings.TrimSpace(input.Status), StartedAfter: startedAfter,
		StartedBefore: startedBefore, CursorStarted: cursorStarted, CursorID: cursorID,
		Limit: limit, ScanLimit: historyRegexScanLimit,
	}, matchMode)
	if err != nil {
		return HistoryOutput{}, err
	}
	summaries := historyRunSummaries(page.Runs)
	return HistoryOutput{
		Runs: &summaries, HasMore: page.HasMore,
		NextCursor: encodeHistoryCursor(page.NextStartedAt, page.NextID), ScanLimited: page.ScanLimited,
	}, nil
}
