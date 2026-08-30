package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Enterpr1se0/opsnerva/internal/agenttool"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/service"
	"github.com/Enterpr1se0/opsnerva/internal/store"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
)

const (
	defaultWorkspaceFileReadBytes = 128 << 10
	defaultHistorySearchLimit     = 20
	maxHistorySearchLimit         = 100
	defaultHistoryOutputBytes     = 16 << 10
	maxHistoryOutputBytes         = 64 << 10
	maxHistoryStructuredBytes     = 8 << 10
	maxHistoryErrorBytes          = 4 << 10
	maxHistoryRegexBytes          = 512
	maxHistoryQueryBytes          = 4 << 10
	historyRegexScanLimit         = 2000
	maxHistoryOperationBytes      = 512
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
				excerpt = agenttool.UTF8Prefix(excerpt, remaining)
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

func historyRunMatches(run domain.Run, query string, matchMode domain.FileSearchMatchMode, queryScope string, maxBytes, matchLimit int) (agenttool.HistoryRunMatch, error) {
	expression, err := compileHistoryMatcher(query, matchMode)
	if err != nil {
		return agenttool.HistoryRunMatch{}, err
	}
	result := agenttool.HistoryRunMatch{HistoryRunSummary: historyRunSummary(run), MatchMode: matchMode, QueryScope: queryScope, MatchLimit: matchLimit}
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
			"preview":        agenttool.LimitText(raw, maxHistoryStructuredBytes, "head_tail"),
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

func historyRunSummary(run domain.Run) agenttool.HistoryRunSummary {
	var request domain.ExecRequest
	_ = json.Unmarshal([]byte(run.RequestJSON), &request)
	duration := int64(0)
	if !run.CompletedAt.IsZero() && !run.StartedAt.IsZero() && !run.CompletedAt.Before(run.StartedAt) {
		duration = run.CompletedAt.Sub(run.StartedAt).Milliseconds()
	}
	operation := historyOperation(run, request)
	if len(operation) > maxHistoryOperationBytes {
		operation = agenttool.UTF8Prefix(operation, maxHistoryOperationBytes-3) + "..."
	}
	return agenttool.HistoryRunSummary{
		ID: run.ID, HostID: run.HostID, ToolName: run.ToolName, Mode: string(request.Mode),
		Operation: operation, Status: run.Status, ExitCode: run.ExitCode,
		DurationMS: duration, StartedAt: run.StartedAt, CompletedAt: run.CompletedAt,
	}
}

func historyRunDetail(run domain.Run, stdoutOffset, stderrOffset, maxBytes int, view string) (agenttool.HistoryRunDetail, error) {
	selected, err := agenttool.SelectExecOutput(domain.ExecResult{Stdout: run.StdoutRedacted, Stderr: run.StderrRedacted},
		stdoutOffset, stderrOffset, maxBytes, view, true)
	if err != nil {
		return agenttool.HistoryRunDetail{}, err
	}
	errorText := run.Error
	errorLimited := false
	if len(errorText) > maxHistoryErrorBytes {
		errorText = agenttool.LimitText(errorText, maxHistoryErrorBytes, "head_tail")
		errorLimited = true
	}
	detail := agenttool.HistoryRunDetail{
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

func historyRunSummaries(runs []domain.Run) []agenttool.HistoryRunSummary {
	result := make([]agenttool.HistoryRunSummary, len(runs))
	for index, run := range runs {
		result[index] = historyRunSummary(run)
	}
	return result
}

func normalizeToolStatus(meta *domain.ToolMeta, status string) {
	*meta = domain.ToolMeta{}
	switch status {
	case "completed", "running", "partial", "approval_required", "cancelled":
		meta.OK = true
	}
}

func normalizeExecResult(result domain.ExecResult, err error) (domain.ExecResult, error) {
	if err == nil {
		normalizeToolStatus(&result.ToolMeta, result.Status)
		return result, nil
	}
	if errors.Is(err, context.Canceled) {
		return result, err
	}
	result.OK = false
	result.Message = err.Error()
	var validationErr *agenttool.InputError
	if errors.As(err, &validationErr) {
		result.Code = "validation_failed"
		result.NextAction = "correct the function tool input using the validation details; do not repeat unchanged input"
		result.Validation = validationErr.Validation()
		if result.Status == "" {
			result.Status = "failed"
		}
		return result, nil
	}
	result.Code, result.Retryable, result.NextAction = classifyToolError(err)
	var selectionErr *service.ExecutionToolSelectionError
	if errors.As(err, &selectionErr) {
		result.Validation = &domain.ToolValidationDetails{
			SuggestedTool: selectionErr.SuggestedTool,
			Example:       selectionErr.Example,
		}
	}
	if result.Status == "" {
		result.Status = "failed"
	}
	return result, nil
}

func CompactExecToolResult(result domain.ExecResult, err error) (agenttool.ExecResult, error) {
	normalized, normalizedErr := normalizeExecResult(result, err)
	return agenttool.ProjectExecResult(normalized), normalizedErr
}

func RunWorkspaceFileReadTool(ctx context.Context, svc *service.Service, input agenttool.WorkspaceReadInput, actor string) (agenttool.ExecResult, error) {
	workspace, err := svc.SessionWorkspace(ctx)
	if err != nil {
		return CompactExecToolResult(domain.ExecResult{}, err)
	}
	searching := input.Pattern != ""
	if searching && (input.FullContent || input.MaxBytes != 0 || input.OffsetBytes != 0 || input.TailLines != 0) {
		return CompactExecToolResult(domain.ExecResult{}, fmt.Errorf("invalid Workspace file read input: pattern cannot be combined with full_content, max_bytes, offset_bytes, or tail_lines"))
	}
	if searching && input.MatchMode == "" {
		return CompactExecToolResult(domain.ExecResult{}, fmt.Errorf("invalid Workspace file read input: match_mode is required with pattern"))
	}
	if !searching && (input.MatchMode != "" || input.ContextLines != 0) {
		return CompactExecToolResult(domain.ExecResult{}, fmt.Errorf("invalid Workspace file read input: match_mode and context_lines require pattern"))
	}
	if searching {
		result, err := svc.SearchWorkspace(ctx, workspace.ID, input.Path, input.Pattern, input.MatchMode, input.ContextLines, actor)
		return CompactExecToolResult(result, err)
	}
	if input.FullContent && (input.MaxBytes != 0 || input.OffsetBytes != 0 || input.TailLines != 0) {
		return CompactExecToolResult(domain.ExecResult{}, fmt.Errorf("invalid Workspace file read input: full_content cannot be combined with max_bytes, offset_bytes, or tail_lines"))
	}
	if input.MaxBytes < 0 || input.TailLines < 0 || (input.OffsetBytes != 0 && input.TailLines != 0) {
		return CompactExecToolResult(domain.ExecResult{}, fmt.Errorf("invalid Workspace file read range: max_bytes and tail_lines must be non-negative; tail_lines cannot be combined with offset_bytes"))
	}
	if !input.FullContent && input.MaxBytes == 0 && input.TailLines == 0 {
		input.MaxBytes = defaultWorkspaceFileReadBytes
	}
	result, err := svc.ReadWorkspaceFileAdvanced(ctx, workspace.ID, input.Path, input.MaxBytes, input.OffsetBytes, input.TailLines, actor)
	return CompactExecToolResult(result, err)
}

func ReadHistoryTool(ctx context.Context, svc *service.Service, input agenttool.HistorySearchInput) (agenttool.HistoryOutput, error) {
	runID := strings.TrimSpace(input.RunID)
	if runID != "" {
		if input.HostID != "" || input.ToolName != "" || input.Status != "" || input.StartedAfter != "" || input.StartedBefore != "" || input.Cursor != "" {
			return agenttool.HistoryOutput{}, fmt.Errorf("invalid history input: run_id cannot be combined with list filters or cursor")
		}
		if input.AfterStdout < 0 || input.AfterStderr < 0 {
			return agenttool.HistoryOutput{}, fmt.Errorf("invalid history input: output byte offsets must be non-negative")
		}
		maxOutput := input.MaxOutput
		if maxOutput == 0 {
			maxOutput = defaultHistoryOutputBytes
		}
		if maxOutput < 1024 || maxOutput > maxHistoryOutputBytes {
			return agenttool.HistoryOutput{}, fmt.Errorf("invalid history input: max_output_bytes must be between 1024 and %d", maxHistoryOutputBytes)
		}
		matchMode, queryScope, err := normalizeHistoryMatch(input.Query, input.MatchMode, input.QueryScope)
		if err != nil {
			return agenttool.HistoryOutput{}, err
		}
		if strings.TrimSpace(input.Query) != "" {
			if input.AfterStdout != 0 || input.AfterStderr != 0 || input.OutputView != "" {
				return agenttool.HistoryOutput{}, fmt.Errorf("invalid history input: run_id query cannot be combined with output offsets or output_view")
			}
			matchLimit := input.Limit
			if matchLimit == 0 {
				matchLimit = defaultHistorySearchLimit
			}
			if matchLimit < 1 || matchLimit > maxHistorySearchLimit {
				return agenttool.HistoryOutput{}, fmt.Errorf("invalid history input: limit must be between 1 and %d", maxHistorySearchLimit)
			}
			result, err := svc.GetRun(ctx, runID, false)
			if err != nil {
				return agenttool.HistoryOutput{}, err
			}
			matched, err := historyRunMatches(result.Run, input.Query, matchMode, queryScope, maxOutput, matchLimit)
			if err != nil {
				return agenttool.HistoryOutput{}, err
			}
			return agenttool.HistoryOutput{Match: &matched}, nil
		}
		if input.Limit != 0 {
			return agenttool.HistoryOutput{}, fmt.Errorf("invalid history input: limit requires query when run_id is set")
		}
		outputView := strings.ToLower(strings.TrimSpace(input.OutputView))
		if outputView == "" && (input.AfterStdout > 0 || input.AfterStderr > 0) {
			outputView = "head"
		}
		if (input.AfterStdout > 0 || input.AfterStderr > 0) && outputView != "head" {
			return agenttool.HistoryOutput{}, fmt.Errorf("invalid history input: output byte offsets require output_view=head")
		}
		result, err := svc.GetRun(ctx, runID, false)
		if err != nil {
			return agenttool.HistoryOutput{}, err
		}
		detail, err := historyRunDetail(result.Run, input.AfterStdout, input.AfterStderr, maxOutput, outputView)
		if err != nil {
			return agenttool.HistoryOutput{}, fmt.Errorf("invalid history input: %w", err)
		}
		return agenttool.HistoryOutput{Run: &detail}, nil
	}
	if input.AfterStdout != 0 || input.AfterStderr != 0 || input.MaxOutput != 0 || input.OutputView != "" {
		return agenttool.HistoryOutput{}, fmt.Errorf("invalid history input: output paging fields require run_id")
	}
	limit := input.Limit
	if limit == 0 {
		limit = defaultHistorySearchLimit
	}
	if limit < 1 || limit > maxHistorySearchLimit {
		return agenttool.HistoryOutput{}, fmt.Errorf("invalid history input: limit must be between 1 and %d", maxHistorySearchLimit)
	}
	matchMode, queryScope, err := normalizeHistoryMatch(input.Query, input.MatchMode, input.QueryScope)
	if err != nil {
		return agenttool.HistoryOutput{}, err
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
		return agenttool.HistoryOutput{}, err
	}
	startedBefore, err := parseBound("started_before", input.StartedBefore)
	if err != nil {
		return agenttool.HistoryOutput{}, err
	}
	if !startedAfter.IsZero() && !startedBefore.IsZero() && startedAfter.After(startedBefore) {
		return agenttool.HistoryOutput{}, fmt.Errorf("invalid history input: started_after must not be later than started_before")
	}
	cursorStarted, cursorID, err := decodeHistoryCursor(input.Cursor)
	if err != nil {
		return agenttool.HistoryOutput{}, err
	}
	page, err := svc.SearchRunSummariesMatchingPage(ctx, domain.RunSearchFilter{
		Query: input.Query, QueryScope: queryScope, HostID: strings.TrimSpace(input.HostID),
		ToolName: strings.TrimSpace(input.ToolName), Status: strings.TrimSpace(input.Status), StartedAfter: startedAfter,
		StartedBefore: startedBefore, CursorStarted: cursorStarted, CursorID: cursorID,
		Limit: limit, ScanLimit: historyRegexScanLimit,
	}, matchMode)
	if err != nil {
		return agenttool.HistoryOutput{}, err
	}
	summaries := historyRunSummaries(page.Runs)
	return agenttool.HistoryOutput{Runs: &summaries, HasMore: page.HasMore,
		NextCursor: encodeHistoryCursor(page.NextStartedAt, page.NextID), ScanLimited: page.ScanLimited}, nil
}

func RunWorkspaceShellTool(ctx context.Context, svc *service.Service, input agenttool.WorkspaceShellInput, actor string) (any, error) {
	action := strings.ToLower(strings.TrimSpace(input.Action))
	sessionID := service.SessionIDFromContext(ctx)
	if sessionID == "" {
		return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShell{}, agenttool.InvalidInput("workspace_shell requires an Agent conversation"))
	}
	workspace, err := svc.SessionWorkspace(ctx)
	if err != nil {
		return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShell{}, err)
	}
	switch action {
	case "run":
		allowed := []string{"action", "script", "cwd", "env", "timeout_seconds", "reason"}
		example := map[string]any{"action": "run", "script": "go test ./...", "reason": "run the project tests"}
		if err := validateWorkspaceShellActionFields(input, action, allowed, example); err != nil {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.ExecResult{}, err)
		}
		if strings.TrimSpace(input.Script) == "" || strings.TrimSpace(input.Reason) == "" {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.ExecResult{}, invalidWorkspaceShellValue(input, action, "action=run requires script and reason", allowed, example))
		}
		result, err := svc.RunWorkspaceShell(ctx, workspace.ID, input.Script, input.Cwd, input.Env, input.TimeoutSeconds, input.Reason, actor)
		return CompactExecToolResult(result, err)
	case "start":
		allowed := []string{"action", "cwd", "env", "reason"}
		example := map[string]any{"action": "start", "reason": "open an interactive project shell"}
		if err := validateWorkspaceShellActionFields(input, action, allowed, example); err != nil {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.ExecResult{}, err)
		}
		if strings.TrimSpace(input.Reason) == "" {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.ExecResult{}, invalidWorkspaceShellValue(input, action, "action=start requires reason", allowed, example))
		}
		result, err := svc.StartWorkspaceShell(ctx, workspace.ID, input.Cwd, input.Env, 120, 32, input.Reason, actor)
		return CompactExecToolResult(result, err)
	case "input":
		allowed := []string{"action", "shell_id", "input", "submit", "wait_seconds", "max_output_bytes", "reason"}
		example := map[string]any{"action": "input", "shell_id": "shell_xxx", "input": "go test ./...", "submit": true}
		if err := validateWorkspaceShellActionFields(input, action, allowed, example); err != nil {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShellSnapshot{}, err)
		}
		if strings.TrimSpace(input.ShellID) == "" || input.Input == "" {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShellSnapshot{}, invalidWorkspaceShellValue(input, action, "action=input requires shell_id and input", allowed, example))
		}
		shellInput := input.Input
		if input.Submit && !strings.HasSuffix(shellInput, "\r") && !strings.HasSuffix(shellInput, "\n") {
			shellInput += "\r"
		}
		if len(shellInput) > 64<<10 || len(input.Reason) > 500 {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShellSnapshot{}, invalidWorkspaceShellValue(input, action, "input must not exceed 65536 bytes and reason must not exceed 500 bytes", allowed, example))
		}
		queryDelay, maxBytes, policyErr := agenttool.ShellOutputPolicy(input.WaitSeconds, input.MaxOutputBytes)
		if policyErr != nil {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShellSnapshot{}, invalidWorkspaceShellValue(input, action, policyErr.Error(), allowed, example))
		}
		page, err := svc.WriteWorkspaceShellPage(ctx, input.ShellID, sessionID, workspace.ID, shellInput, queryDelay, maxBytes, input.Reason, actor)
		return normalizeValueToolResult(ctx, "workspace_shell", newSSHTools(svc).FormatShellPage(ctx, page, agenttool.ShellSnapshotAfter(page.Snapshot), true), err)
	case "output":
		allowed := []string{"action", "shell_id", "after_sequence", "wait_seconds", "max_output_bytes", "reason"}
		example := map[string]any{"action": "output", "shell_id": "shell_xxx", "wait_seconds": 10}
		if err := validateWorkspaceShellActionFields(input, action, allowed, example); err != nil {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShellSnapshot{}, err)
		}
		if strings.TrimSpace(input.ShellID) == "" {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShellSnapshot{}, invalidWorkspaceShellValue(input, action, "action=output requires shell_id", allowed, example))
		}
		if len(input.Reason) > 500 {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShellSnapshot{}, invalidWorkspaceShellValue(input, action, "reason must not exceed 500 bytes", allowed, example))
		}
		queryDelay, maxBytes, policyErr := agenttool.ShellOutputPolicy(input.WaitSeconds, input.MaxOutputBytes)
		if policyErr != nil {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShellSnapshot{}, invalidWorkspaceShellValue(input, action, policyErr.Error(), allowed, example))
		}
		page, err := svc.QueryWorkspaceShellOutput(ctx, input.ShellID, sessionID, workspace.ID, input.AfterSequence, queryDelay, maxBytes, input.Reason, actor)
		return normalizeValueToolResult(ctx, "workspace_shell", newSSHTools(svc).FormatShellPage(ctx, page, agenttool.ShellSnapshotAfter(page.Snapshot), false), err)
	case "list":
		allowed := []string{"action", "reason"}
		example := map[string]any{"action": "list"}
		if err := validateWorkspaceShellActionFields(input, action, allowed, example); err != nil {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShellList{}, err)
		}
		if len(input.Reason) > 500 {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShellList{}, invalidWorkspaceShellValue(input, action, "reason must not exceed 500 bytes", allowed, example))
		}
		result, err := svc.ListWorkspaceShells(ctx, sessionID, workspace.ID, input.Reason, actor)
		return normalizeValueToolResult(ctx, "workspace_shell", result, err)
	case "interrupt":
		allowed := []string{"action", "shell_id", "reason"}
		example := map[string]any{"action": "interrupt", "shell_id": "shell_xxx"}
		if err := validateWorkspaceShellActionFields(input, action, allowed, example); err != nil {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShell{}, err)
		}
		if strings.TrimSpace(input.ShellID) == "" || len(input.Reason) > 500 {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShell{}, invalidWorkspaceShellValue(input, action, "action=interrupt requires shell_id and reason must not exceed 500 bytes", allowed, example))
		}
		result, err := svc.InterruptWorkspaceShell(ctx, input.ShellID, sessionID, workspace.ID, input.Reason, actor)
		return normalizeValueToolResult(ctx, "workspace_shell", result, err)
	case "close":
		allowed := []string{"action", "shell_id", "reason"}
		example := map[string]any{"action": "close", "shell_id": "shell_xxx"}
		if err := validateWorkspaceShellActionFields(input, action, allowed, example); err != nil {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShell{}, err)
		}
		if strings.TrimSpace(input.ShellID) == "" || len(input.Reason) > 500 {
			return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShell{}, invalidWorkspaceShellValue(input, action, "action=close requires shell_id and reason must not exceed 500 bytes", allowed, example))
		}
		result, err := svc.CloseWorkspaceShell(ctx, input.ShellID, sessionID, workspace.ID, input.Reason, actor)
		return normalizeValueToolResult(ctx, "workspace_shell", result, err)
	default:
		return normalizeValueToolResult(ctx, "workspace_shell", domain.SSHShell{}, agenttool.StructuredInputError(
			"invalid action: use run, start, input, output, list, interrupt, or close",
			domain.ToolValidationDetails{Action: action, AllowedFields: []string{"action"}, GotFields: workspaceShellProvidedFields(input), Example: map[string]any{"action": "list"}},
		))
	}
}

func classifyToolError(err error) (string, bool, string) {
	var selectionErr *service.ExecutionToolSelectionError
	var inputValidation *service.InputValidationError
	if errors.As(err, &selectionErr) {
		return "wrong_tool", false, selectionErr.NextAction
	}
	if errors.As(err, &inputValidation) {
		return "validation_failed", false, "correct the tool input using the error message; do not repeat unchanged input"
	}
	if errors.Is(err, service.ErrAgentHostAccessDenied) || errors.Is(err, service.ErrAgentRootAccessDenied) || errors.Is(err, service.ErrHostAgentRootUnavailable) {
		return "denied", false, "respect the host Agent and root access settings; do not retry unchanged input"
	}
	message := strings.ToLower(err.Error())
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "not_found", false, "verify the identifier or list available resources; do not retry the same missing identifier"
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(message, "timed out"), strings.Contains(message, "timeout"):
		return "timeout", true, "narrow the operation or set background=true on ssh_exec or ssh_run_script for a long-running command"
	case strings.Contains(message, "denied"), strings.Contains(message, "forbidden"):
		return "denied", false, "respect the denial and choose a permitted operation"
	case strings.Contains(message, "required"), strings.Contains(message, "invalid"), strings.Contains(message, "unsupported"):
		return "validation_failed", false, "correct the tool input using the error message; do not repeat unchanged input"
	case strings.Contains(message, "changed"), strings.Contains(message, "conflict"):
		return "conflict", true, "read the current state again before proposing another change"
	case strings.Contains(message, "constraint failed"):
		return "internal_error", false, "stop the affected workflow and report the control-plane persistence failure"
	default:
		return "remote_failed", true, "inspect stderr and gather narrower read-only details before retrying"
	}
}

func NormalizeWebSearchToolResult(result domain.WebSearchResponse, err error) (domain.WebSearchResponse, error) {
	result.ToolVersion = "1.1"
	result.ContentIsUntrusted = true
	if err == nil {
		result.OK = true
		result.Code = "completed"
		return result, nil
	}
	if errors.Is(err, context.Canceled) {
		return result, err
	}
	result.OK = false
	result.Message = err.Error()
	switch {
	case errors.Is(err, service.ErrWebSearchDisabled):
		result.Code = "configuration_required"
		result.NextAction = "tell the operator that Tavily Web must be enabled and configured in Settings; do not retry"
	case errors.Is(err, context.DeadlineExceeded):
		result.Code = "timeout"
		result.Retryable = true
		result.NextAction = "retry once with a narrower query or fewer results"
	case errors.Is(err, service.ErrWebSearchUpstream):
		result.Code, result.Retryable, result.NextAction = classifyWebProviderToolError(err)
	case strings.Contains(strings.ToLower(err.Error()), "timeout"):
		result.Code = "timeout"
		result.Retryable = true
		result.NextAction = "retry once with a narrower query or fewer results"
	default:
		result.Code, result.Retryable, result.NextAction = classifyToolError(err)
	}
	return result, nil
}

func NormalizeWebExtractToolResult(result domain.WebExtractResponse, err error) (domain.WebExtractResponse, error) {
	result.ToolVersion = "1.1"
	result.ContentIsUntrusted = true
	if err == nil {
		result.OK = true
		result.Code = "completed"
		if len(result.FailedResults) > 0 {
			result.Code = "partial"
			result.Message = "some URLs could not be extracted"
			result.NextAction = "use the successful pages and retry only failed URLs when they are still necessary"
		}
		return result, nil
	}
	if errors.Is(err, context.Canceled) {
		return result, err
	}
	result.OK = false
	result.Message = err.Error()
	switch {
	case errors.Is(err, service.ErrWebSearchDisabled):
		result.Code = "configuration_required"
		result.NextAction = "tell the operator that Tavily Web must be enabled and configured in Settings; do not retry"
	case errors.Is(err, context.DeadlineExceeded):
		result.Code = "timeout"
		result.Retryable = true
		result.NextAction = "retry once with fewer URLs"
	case errors.Is(err, service.ErrWebSearchUpstream):
		result.Code, result.Retryable, result.NextAction = classifyWebProviderToolError(err)
	case strings.Contains(strings.ToLower(err.Error()), "timeout"):
		result.Code = "timeout"
		result.Retryable = true
		result.NextAction = "retry once with fewer URLs"
	default:
		result.Code, result.Retryable, result.NextAction = classifyToolError(err)
	}
	return result, nil
}

func classifyWebProviderToolError(err error) (string, bool, string) {
	var providerError *service.WebSearchProviderError
	if !errors.As(err, &providerError) {
		return "provider_failed", true, "retry once only when the provider failure appears transient"
	}
	switch providerError.Code {
	case service.WebSearchErrorInvalidRequest:
		return providerError.Code, false, "correct the search or extraction parameters; do not repeat unchanged input"
	case service.WebSearchErrorAuthenticationFailed:
		return providerError.Code, false, "tell the operator to verify the Tavily API key in Settings; do not retry"
	case service.WebSearchErrorQuotaExhausted:
		return providerError.Code, false, "tell the operator that Tavily quota is exhausted; do not retry"
	case service.WebSearchErrorRateLimited:
		if providerError.Retryable {
			return providerError.Code, true, "retry once after a short delay with fewer results or URLs"
		}
		return providerError.Code, false, "do not retry in this turn; continue with sources already available"
	case service.WebSearchErrorTimeout:
		return providerError.Code, providerError.Retryable, "retry once with fewer results or URLs only when the operation is still necessary"
	case service.WebSearchErrorProviderUnavailable:
		return providerError.Code, providerError.Retryable, "retry once only when the operation is still necessary; otherwise report the provider outage"
	default:
		return "provider_failed", providerError.Retryable, "retry once only when the provider failure appears transient"
	}
}

func buildAvailableTools(svc *service.Service) ([]tool.BaseTool, error) {
	var tools []tool.BaseTool
	sshTools := newSSHTools(svc)
	remoteValidatorIDs := svc.ValidatorIDs("remote")
	workspaceValidatorIDs := svc.ValidatorIDs("workspace")
	validatorHint := func(ids []string) string {
		if len(ids) == 0 {
			return " No validators; omit validator_id."
		}
		return " validator_id: " + strings.Join(ids, ", ") + "."
	}
	appendTool := func(created tool.InvokableTool, err error) error {
		if err != nil {
			return err
		}
		tools = append(tools, created)
		return nil
	}

	if err := appendTool(toolutils.InferTool("ssh_host_inspect", "Inspect one SSH host's OS, user, shell, and uptime (read-only).", func(ctx context.Context, input agenttool.HostInput) (any, error) {
		capability, err := svc.ProbeHost(ctx, input.HostID, "eino-agent")
		return normalizeValueToolResult(ctx, "ssh_host_inspect", capability, err)
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("ssh_exec", agenttool.SSHExecDescription, func(ctx context.Context, input agenttool.ExecInput) (agenttool.ExecResult, error) {
		request := domain.ExecRequest{HostID: input.HostID, Mode: domain.ExecProgram, Program: input.Program, Args: input.Args, Background: input.Background, Cwd: input.Cwd, Env: input.Env, Elevated: input.Elevated, TimeoutSeconds: input.TimeoutSeconds, MaxOutputBytes: input.MaxOutputBytes, OutputView: input.OutputView, Reason: input.Reason}
		return sshTools.RunExecution(ctx, request, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("ssh_run_script", agenttool.SSHScriptDescription, func(ctx context.Context, input agenttool.ScriptInput) (agenttool.ExecResult, error) {
		request := domain.ExecRequest{HostID: input.HostID, Mode: domain.ExecScript, Script: input.Script, Background: input.Background, Cwd: input.Cwd, Env: input.Env, Elevated: input.Elevated, TimeoutSeconds: input.TimeoutSeconds, MaxOutputBytes: input.MaxOutputBytes, OutputView: input.OutputView, Reason: input.Reason}
		return sshTools.RunExecution(ctx, request, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("ssh_tunnel", agenttool.SSHTunnelDescription, func(ctx context.Context, input agenttool.SSHTunnelInput) (any, error) {
		return sshTools.RunTunnel(ctx, input, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("ssh_shell", agenttool.SSHShellDescription, func(ctx context.Context, input agenttool.SSHShellInput) (any, error) {
		return sshTools.RunShell(ctx, service.SessionIDFromContext(ctx), input, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("ssh_task", agenttool.SSHTaskDescription, func(ctx context.Context, input agenttool.TaskInput) (agenttool.ExecResult, error) {
		return sshTools.RunTask(ctx, input, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("ssh_file_read", "Read, page, tail, inspect metadata, or search one remote file.", func(ctx context.Context, input agenttool.FileReadInput) (agenttool.ExecResult, error) {
		return sshTools.RunFileRead(ctx, input, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("ssh_file_list", "List a remote directory (read-only).", func(ctx context.Context, input agenttool.FileListInput) (agenttool.ExecResult, error) {
		return sshTools.RunFileList(ctx, input, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("ssh_file_edit", "Create a remote text file or replace/delete one exact unique line block; read existing files first."+validatorHint(remoteValidatorIDs), func(ctx context.Context, input agenttool.FileEditInput) (agenttool.ExecResult, error) {
		return sshTools.RunFileEdit(ctx, input, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("ssh_file_transfer", "Transfer one SHA256-bound file between registered SSH hosts.", func(ctx context.Context, input agenttool.SSHFileTransferInput) (agenttool.ExecResult, error) {
		return sshTools.RunFileTransfer(ctx, input, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("workspace_file_list", "List a directory in the current Workspace (read-only).", func(ctx context.Context, input agenttool.WorkspacePathInput) (agenttool.ExecResult, error) {
		workspace, err := svc.SessionWorkspace(ctx)
		if err != nil {
			return CompactExecToolResult(domain.ExecResult{}, err)
		}
		result, err := svc.ListWorkspaceFiles(ctx, workspace.ID, input.Path, "eino-agent")
		return CompactExecToolResult(result, err)
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("workspace_file_read", "Read, page, tail, or search one file in the current Workspace.", func(ctx context.Context, input agenttool.WorkspaceReadInput) (agenttool.ExecResult, error) {
		return RunWorkspaceFileReadTool(ctx, svc, input, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("workspace_file_edit", "Create a text file or replace/delete one exact unique line block in the current Workspace; read existing files first."+validatorHint(workspaceValidatorIDs), func(ctx context.Context, input agenttool.WorkspaceFileEditInput) (agenttool.ExecResult, error) {
		workspace, err := svc.SessionWorkspace(ctx)
		if err != nil {
			return CompactExecToolResult(domain.ExecResult{}, err)
		}
		result, err := svc.EditWorkspaceFile(ctx, workspace.ID, input.Path, input.OldText, input.NewText, input.ValidatorID, input.Reason, "eino-agent")
		return CompactExecToolResult(result, err)
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("workspace_file_delete", "Permanently delete a path in the current read-write Workspace.", func(ctx context.Context, input agenttool.WorkspaceFileDeleteInput) (agenttool.ExecResult, error) {
		workspace, err := svc.SessionWorkspace(ctx)
		if err != nil {
			return CompactExecToolResult(domain.ExecResult{}, err)
		}
		result, err := svc.DeleteWorkspaceEntry(ctx, workspace.ID, input.Path, input.Recursive, input.Reason, "eino-agent")
		return CompactExecToolResult(result, err)
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("workspace_file_upload", "Upload a SHA256-bound current Workspace file to an SSH host.", func(ctx context.Context, input agenttool.WorkspaceUploadInput) (agenttool.ExecResult, error) {
		workspace, err := svc.SessionWorkspace(ctx)
		if err != nil {
			return CompactExecToolResult(domain.ExecResult{}, err)
		}
		result, err := svc.UploadWorkspaceFileToHost(ctx, input.HostID, workspace.ID, input.Path, input.ExpectedSHA256, input.RemotePath, input.Reason, "eino-agent")
		return CompactExecToolResult(result, err)
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("workspace_file_download", "Download a SHA256-bound SSH file to a new current Workspace path.", func(ctx context.Context, input agenttool.WorkspaceDownloadInput) (agenttool.ExecResult, error) {
		workspace, err := svc.SessionWorkspace(ctx)
		if err != nil {
			return CompactExecToolResult(domain.ExecResult{}, err)
		}
		result, err := svc.DownloadHostFileToWorkspace(ctx, input.HostID, input.RemotePath, input.ExpectedSHA256, workspace.ID, input.Path, input.TimeoutSeconds, input.Reason, "eino-agent")
		return CompactExecToolResult(result, err)
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("workspace_shell", "Run a script or manage a PTY in the current Workspace. Use run for one-shot work; wait_seconds delays reads; continue from next_sequence.", func(ctx context.Context, input agenttool.WorkspaceShellInput) (any, error) {
		return RunWorkspaceShellTool(ctx, svc, input, "eino-agent")
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("web_search", "Search 3-5 public Web sources with Tavily. Prefer official domains and basic depth; use advanced depth only when relevant chunks are necessary. Select result URLs for web_extract and cite source URLs.", func(ctx context.Context, input agenttool.WebSearchInput) (domain.WebSearchResponse, error) {
		result, err := svc.SearchWeb(ctx, domain.WebSearchRequest{
			Query: input.Query, MaxResults: input.MaxResults, Topic: input.Topic, SearchDepth: input.SearchDepth,
			TimeRange: input.TimeRange, StartDate: input.StartDate, EndDate: input.EndDate, ChunksPerSource: input.ChunksPerSource,
			IncludeDomains: input.IncludeDomains, ExcludeDomains: input.ExcludeDomains,
		}, "eino-agent")
		if result.Query == "" {
			result.Query = input.Query
		}
		if result.Provider == "" {
			result.Provider = "tavily"
		}
		return NormalizeWebSearchToolResult(result, err)
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("web_extract", "Extract relevant Markdown from up to five selected public URLs. Search first when URLs are unknown, pass query to focus extraction, and cite each source URL.", func(ctx context.Context, input agenttool.WebExtractInput) (domain.WebExtractResponse, error) {
		result, err := svc.ExtractWeb(ctx, domain.WebExtractRequest{
			URLs: input.URLs, Query: input.Query, ExtractDepth: input.ExtractDepth, ChunksPerSource: input.ChunksPerSource,
		}, "eino-agent")
		if result.Provider == "" {
			result.Provider = "tavily"
		}
		if result.Query == "" {
			result.Query = input.Query
		}
		return NormalizeWebExtractToolResult(result, err)
	})); err != nil {
		return nil, err
	}
	if err := appendTool(toolutils.InferTool("ssh_history", "Search this conversation's audited run summaries with literal or POSIX regex matching and cursor pagination. Use run_id for a bounded redacted detail page; combine run_id and query for bounded matching excerpts, with limit as the per-stream match cap.", func(ctx context.Context, input agenttool.HistorySearchInput) (any, error) {
		result, err := ReadHistoryTool(ctx, svc, input)
		return normalizeValueToolResult(ctx, "ssh_history", result, err)
	})); err != nil {
		return nil, err
	}
	tools = append(tools, svc.MCPTools()...)
	return tools, nil
}

func BuildTools(svc *service.Service) ([]tool.BaseTool, error) {
	ctx := context.Background()
	available, err := buildAvailableTools(svc)
	if err != nil {
		return nil, err
	}
	states, err := svc.AgentToolStates(ctx)
	if err != nil {
		return nil, err
	}
	_, skillTools, err := newSkillMiddleware(ctx, svc, states)
	if err != nil {
		return nil, err
	}
	available = append(available, skillTools...)
	enabled := make([]tool.BaseTool, 0, len(available))
	for _, candidate := range available {
		info, err := candidate.Info(ctx)
		if err != nil {
			return nil, err
		}
		if value, configured := states[info.Name]; !configured || value {
			enabled = append(enabled, candidate)
		}
	}
	return enabled, nil
}

func buildToolSet(ctx context.Context, svc *service.Service) ([]tool.BaseTool, []agenttool.Descriptor, error) {
	available, err := buildAvailableTools(svc)
	if err != nil {
		return nil, nil, err
	}
	descriptors, err := agenttool.Describe(ctx, available)
	if err != nil {
		return nil, nil, err
	}
	states, err := svc.AgentToolStates(ctx)
	if err != nil {
		return nil, nil, err
	}
	enabled := make([]tool.BaseTool, 0, len(available))
	for index, candidate := range available {
		if value, configured := states[descriptors[index].Name]; configured {
			descriptors[index].Enabled = value
		}
		if descriptors[index].Enabled {
			enabled = append(enabled, candidate)
		}
	}
	return enabled, descriptors, nil
}
