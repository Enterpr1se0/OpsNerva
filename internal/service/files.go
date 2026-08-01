package service

import (
	"context"
	"fmt"
	posixpath "path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"eino-ops-agent/internal/config"
	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/ids"
)

const (
	fileMetaMarker       = "__OPS_FILE_META__"
	fileContentMarker    = "__OPS_FILE_CONTENT__"
	fileAfterMarker      = "__OPS_FILE_AFTER__"
	fileValidationMarker = "__OPS_FILE_VALIDATION_OK__"
)

// ValidatorIDs returns the configured validator identifiers for one execution
// scope. Validators are configuration-owned argv templates, never Tool-supplied
// shell commands.
func (s *Service) ValidatorIDs(scope string) []string {
	ids := make([]string, 0, len(s.validators))
	for _, validator := range s.validators {
		if validator.Scope == scope {
			ids = append(ids, validator.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

func (s *Service) ReadFileAdvanced(ctx context.Context, hostID, path string, metadataOnly bool, maxBytes int, offsetBytes int64, tailLines int, elevated bool, actor string) (domain.ExecResult, error) {
	if err := validateRemoteFilePath(path); err != nil {
		return domain.ExecResult{}, err
	}
	if metadataOnly && (maxBytes != 0 || offsetBytes != 0 || tailLines != 0) {
		return domain.ExecResult{}, fmt.Errorf("invalid file read range: metadata_only cannot be combined with max_bytes, offset_bytes, or tail_lines")
	}
	if maxBytes < 0 || tailLines < 0 || (offsetBytes != 0 && tailLines > 0) {
		return domain.ExecResult{}, fmt.Errorf("invalid file range: max_bytes and tail_lines must be non-negative; tail_lines cannot be combined with offset_bytes")
	}
	result, err := s.Submit(ctx, domain.ExecRequest{
		HostID: hostID, Mode: domain.ExecRemoteRead, RemotePath: path, MetadataOnly: metadataOnly,
		MaxBytes: maxBytes, OffsetBytes: offsetBytes, TailLines: tailLines, Elevated: elevated,
		Reason: "read a bounded remote file with version metadata",
	}, actor)
	if result.Stdout != "" {
		metadata, content := parseFileReadOutput(path, result.Stdout)
		metadata.OffsetBytes = resolvedFileOffset(metadata.Size, offsetBytes)
		metadata.ReturnedBytes = len(content)
		decorateFileReadPage(&metadata, maxBytes, tailLines)
		metadata.Sensitive = strings.Contains(content, "[REDACTED]")
		result.File = &metadata
		result.Stdout = content
	}
	if metadataOnly {
		result.Stdout = ""
	}
	return result, err
}

func decorateFileReadPage(metadata *domain.FileMetadata, maxBytes, tailLines int) {
	if metadata == nil || maxBytes <= 0 || tailLines > 0 {
		return
	}
	next := metadata.OffsetBytes + int64(maxBytes)
	if next < metadata.Size {
		metadata.HasMore = true
		metadata.NextOffset = next
	}
}

func buildRemoteFileReadScript(req domain.ExecRequest) string {
	quoted := shellQuote(req.RemotePath)
	lines := []string{
		"set -e",
		"file_stat=$(stat -Lc '%s\t%a\t%U\t%G\t%Y' -- " + quoted + ")",
		"file_sha256=$(sha256sum -- " + quoted + ")",
		"printf '" + fileMetaMarker + "\\n'",
		"printf '%s\\n' \"$file_stat\"",
		"printf '%s\\n' \"$file_sha256\"",
		"printf '" + fileContentMarker + "\\n'",
	}
	switch {
	case req.MetadataOnly:
		lines = append(lines, "head -c 1 -- "+quoted)
	case req.TailLines > 0:
		command := "tail -n " + strconv.Itoa(req.TailLines) + " -- " + quoted
		if req.MaxBytes > 0 {
			command += " | head -c " + strconv.Itoa(req.MaxBytes)
		}
		lines = append(lines, command)
	case req.OffsetBytes < 0:
		command := "tail -c " + strings.TrimPrefix(strconv.FormatInt(req.OffsetBytes, 10), "-") + " -- " + quoted
		if req.MaxBytes > 0 {
			command += " | head -c " + strconv.Itoa(req.MaxBytes)
		}
		lines = append(lines, command)
	case req.OffsetBytes > 0:
		command := "tail -c +" + strconv.FormatInt(req.OffsetBytes+1, 10) + " -- " + quoted
		if req.MaxBytes > 0 {
			command += " | head -c " + strconv.Itoa(req.MaxBytes)
		}
		lines = append(lines, command)
	default:
		if req.MaxBytes > 0 {
			lines = append(lines, "head -c "+strconv.Itoa(req.MaxBytes)+" -- "+quoted)
		} else {
			lines = append(lines, "cat -- "+quoted)
		}
	}
	return strings.Join(lines, "\n")
}

func buildRemoteFileSearchScript(req domain.ExecRequest) string {
	matchFlag := "-F"
	if req.SearchMatchMode == domain.FileSearchRegex {
		matchFlag = "-E"
	}
	grep := "grep -n " + matchFlag + " -C " + strconv.Itoa(req.ContextLines) + " -- " + shellQuote(req.SearchPattern) + " " + shellQuote(req.RemotePath)
	return strings.Join([]string{
		"if " + grep + "; then",
		"  exit 0",
		"else",
		"  search_status=$?",
		`  if [ "$search_status" -eq 1 ]; then`,
		"    exit 0",
		"  fi",
		`  exit "$search_status"`,
		"fi",
	}, "\n")
}

func resolvedFileOffset(size, requested int64) int64 {
	if requested >= 0 {
		return requested
	}
	if size <= 0 || requested < -size {
		return 0
	}
	return size + requested
}

func validateFileSearchInput(pattern string, matchMode domain.FileSearchMatchMode, contextLines int) error {
	if strings.TrimSpace(pattern) == "" || len(pattern) > 512 || strings.ContainsAny(pattern, "\x00\r\n") {
		return fmt.Errorf("invalid search pattern: use 1-512 characters on one line")
	}
	if contextLines < 0 {
		return fmt.Errorf("search context_lines must be non-negative")
	}
	switch matchMode {
	case domain.FileSearchLiteral:
		return nil
	case domain.FileSearchRegex:
		if _, err := regexp.CompilePOSIX(pattern); err != nil {
			return fmt.Errorf("invalid POSIX search regex: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("invalid search match_mode: use literal or regex")
	}
}

func (s *Service) SearchFile(ctx context.Context, hostID, path, pattern string, matchMode domain.FileSearchMatchMode, contextLines int, elevated bool, actor string) (domain.ExecResult, error) {
	if err := validateRemoteFilePath(path); err != nil {
		return domain.ExecResult{}, err
	}
	if err := validateFileSearchInput(pattern, matchMode, contextLines); err != nil {
		return domain.ExecResult{}, err
	}
	result, err := s.Submit(ctx, domain.ExecRequest{
		HostID: hostID, Mode: domain.ExecRemoteSearch, RemotePath: path, SearchPattern: pattern,
		SearchMatchMode: matchMode, ContextLines: contextLines, Elevated: elevated, Reason: "search matches in a remote file",
	}, actor)
	decorateFileSearchResult(&result, pattern, matchMode, contextLines)
	return result, err
}

func decorateFileSearchResult(result *domain.ExecResult, pattern string, matchMode domain.FileSearchMatchMode, contextLines int) {
	if result.Status != "completed" {
		return
	}
	result.Search = &domain.FileSearchResult{
		Found:        result.Stdout != "",
		Pattern:      pattern,
		MatchMode:    matchMode,
		ContextLines: contextLines,
	}
	if !result.Search.Found {
		result.Message = "no matches found"
	}
}

func (s *Service) EditRemoteFile(ctx context.Context, hostID, path, diff, validatorID string, elevated bool, reason, actor string) (domain.ExecResult, error) {
	if err := validateRemoteFilePath(path); err != nil {
		return domain.ExecResult{}, err
	}
	if len(diff) > 1<<20 {
		return domain.ExecResult{}, fmt.Errorf("file edit exceeds 1 MiB")
	}
	if strings.TrimSpace(reason) == "" {
		return domain.ExecResult{}, fmt.Errorf("reason is required")
	}
	if strings.Contains(diff, "[REDACTED]") || s.redactor.Redact(diff) != diff {
		return domain.ExecResult{}, fmt.Errorf("file edit contains a secret or redaction placeholder; use a change that does not expose or overwrite secret values")
	}
	if _, err := s.validatorCommandFor(validatorID, "remote", path, path); err != nil {
		return domain.ExecResult{}, err
	}
	change, err := buildEditChange(path, diff)
	if err != nil {
		return domain.ExecResult{}, err
	}
	result, submitErr := s.Submit(ctx, domain.ExecRequest{
		HostID: hostID, Mode: domain.ExecRemoteEdit, Change: &change, Elevated: elevated, Reason: reason,
		RemotePath: path, Validator: validatorID,
	}, actor)
	result.Change = &change
	if result.Stdout != "" {
		metadata := parseFileEditOutput(path, validatorID, result.Stdout)
		result.File = &metadata
	}
	if result.ExitCode == 74 {
		return result, fmt.Errorf("validation failed; the target file was not changed")
	}
	return result, submitErr
}

func (s *Service) prepareRemoteFileChange(req domain.ExecRequest) (domain.ExecRequest, error) {
	if req.Change == nil {
		return req, fmt.Errorf("remote file change is missing")
	}
	suffix := time.Now().UTC().Format("20060102T150405Z") + "-" + ids.New("file")
	tempPath := posixpath.Join(posixpath.Dir(req.RemotePath), ".opspilot-"+posixpath.Base(req.RemotePath)+"-"+suffix+".tmp")
	validatorCommand, err := s.validatorCommandFor(req.Validator, "remote", req.RemotePath, tempPath)
	if err != nil {
		return req, err
	}
	prepared := req
	prepared.Mode = domain.ExecScript
	prepared.Script = buildRemoteFileChangeScript(req.RemotePath, tempPath, *req.Change, validatorCommand)
	return prepared, nil
}

func buildRemoteFileChangeScript(path, tempPath string, change domain.FileChange, validatorCommand string) string {
	pathQ, tempQ := shellQuote(path), shellQuote(tempPath)
	marker := fileEditHeredocMarker(change.Diff)
	lines := []string{
		"set -eu",
		"test ! -e " + tempQ,
		"trap " + shellQuote("test ! -e "+tempQ+" || unlink -- "+tempQ) + " EXIT",
		"test -f " + pathQ,
		"cp -p -- " + pathQ + " " + tempQ,
		"patch --batch --forward --no-backup-if-mismatch " + tempQ + " <<'" + marker + "'",
		change.Diff,
		marker,
	}
	lines = append(lines, "sync -f -- "+tempQ)
	if validatorCommand != "" {
		lines = append(lines, "if ! "+validatorCommand+"; then", "  unlink -- "+tempQ, "  exit 74", "fi", "printf '"+fileValidationMarker+"\\n'")
	}
	lines = append(lines, "mv -f -- "+tempQ+" "+pathQ)
	lines = append(lines, "trap - EXIT", "sync -f -- "+pathQ, "sync -f -- "+shellQuote(posixpath.Dir(path)), "printf '"+fileAfterMarker+"\\n'", "sha256sum -- "+pathQ)
	return strings.Join(lines, "\n")
}

func buildEditChange(path, diff string) (domain.FileChange, error) {
	diff = strings.ReplaceAll(diff, "\r\n", "\n")
	if strings.ContainsAny(diff, "\x00\r") {
		return domain.FileChange{}, fmt.Errorf("unified diff contains unsupported control characters")
	}
	lines := strings.Split(diff, "\n")
	hunks := make([]string, 0, len(lines))
	seenHunk := false
	oldExpected, newExpected := 0, 0
	oldSeen, newSeen := 0, 0
	additions, deletions := 0, 0
	finishHunk := func() error {
		if seenHunk && (oldSeen != oldExpected || newSeen != newExpected) {
			return fmt.Errorf("unified diff hunk line counts do not match its header")
		}
		return nil
	}
	for index, line := range lines {
		if match := hunkHeader.FindStringSubmatch(line); match != nil {
			if err := finishHunk(); err != nil {
				return domain.FileChange{}, err
			}
			seenHunk = true
			oldExpected = patchHunkCount(match[2])
			newExpected = patchHunkCount(match[4])
			oldSeen, newSeen = 0, 0
			hunks = append(hunks, line)
			continue
		}
		if !seenHunk {
			if line == "" || strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") {
				continue
			}
			return domain.FileChange{}, fmt.Errorf("unified diff contains unsupported data before its first hunk")
		}
		if line == "" && index == len(lines)-1 {
			continue
		}
		if line == "\\ No newline at end of file" {
			hunks = append(hunks, line)
			continue
		}
		if line == "" {
			return domain.FileChange{}, fmt.Errorf("unified diff contains an unprefixed empty line")
		}
		switch line[0] {
		case ' ':
			oldSeen++
			newSeen++
		case '-':
			oldSeen++
			deletions++
		case '+':
			newSeen++
			additions++
		default:
			return domain.FileChange{}, fmt.Errorf("invalid unified diff line")
		}
		hunks = append(hunks, line)
	}
	if !seenHunk {
		return domain.FileChange{}, fmt.Errorf("unified diff contains no hunks")
	}
	if err := finishHunk(); err != nil {
		return domain.FileChange{}, err
	}
	normalized := "--- " + path + "\n+++ " + path + "\n" + strings.Join(hunks, "\n") + "\n"
	return domain.FileChange{Diff: normalized, Additions: additions, Deletions: deletions}, nil
}

func fileEditHeredocMarker(content string) string {
	for {
		marker := "__OPS_FILE_EDIT_" + strings.TrimPrefix(ids.New("edit"), "edit_") + "__"
		conflict := false
		for _, line := range strings.Split(content, "\n") {
			if line == marker {
				conflict = true
				break
			}
		}
		if !conflict {
			return marker
		}
	}
}

func (s *Service) validatorCommandFor(id, scope, allowedPath, executionPath string) (string, error) {
	if strings.TrimSpace(id) == "" {
		return "", nil
	}
	validator, ok := s.validators[id]
	if !ok || validator.Scope != scope {
		available := s.ValidatorIDs(scope)
		if len(available) == 0 {
			return "", fmt.Errorf("invalid validator_id %q: no %s validator IDs are configured; omit validator_id", id, scope)
		}
		return "", fmt.Errorf("invalid validator_id %q for %s operations; available IDs: %s", id, scope, strings.Join(available, ", "))
	}
	if !validatorAllowsPath(validator, allowedPath) {
		return "", fmt.Errorf("validator_id %q is not allowed for path %s", id, allowedPath)
	}
	parts := []string{"timeout", "--signal=KILL", strconv.Itoa(validator.TimeoutSeconds) + "s", shellQuote(validator.Program)}
	for _, argument := range validator.Args {
		parts = append(parts, shellQuote(strings.ReplaceAll(argument, "{{path}}", executionPath)))
	}
	return strings.Join(parts, " "), nil
}

func validatorAllowsPath(validator config.Validator, path string) bool {
	if len(validator.PathPatterns) == 0 {
		return false
	}
	clean := posixpath.Clean(path)
	for _, pattern := range validator.PathPatterns {
		pattern = posixpath.Clean(pattern)
		if strings.HasSuffix(pattern, "/**") {
			root := strings.TrimSuffix(pattern, "/**")
			if clean == root || strings.HasPrefix(clean, root+"/") {
				return true
			}
		} else if matched, _ := posixpath.Match(pattern, clean); matched {
			return true
		}
	}
	return false
}

func validateRemoteFilePath(path string) error {
	if !posixpath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") || posixpath.Clean(path) != path {
		return fmt.Errorf("remote file path must be a clean absolute path")
	}
	return nil
}

func parseFileReadOutput(path, output string) (domain.FileMetadata, string) {
	metadata := domain.FileMetadata{Path: path}
	metaIndex := strings.Index(output, fileMetaMarker+"\n")
	contentIndex := strings.Index(output, fileContentMarker+"\n")
	if metaIndex < 0 || contentIndex < 0 || contentIndex <= metaIndex {
		return metadata, output
	}
	metaLines := strings.Split(strings.TrimSpace(output[metaIndex+len(fileMetaMarker)+1:contentIndex]), "\n")
	if len(metaLines) > 0 {
		fields := strings.Split(metaLines[0], "\t")
		if len(fields) >= 5 {
			metadata.Size, _ = strconv.ParseInt(fields[0], 10, 64)
			metadata.Mode, metadata.Owner, metadata.Group = fields[1], fields[2], fields[3]
			metadata.ModifiedUnix, _ = strconv.ParseInt(fields[4], 10, 64)
		}
	}
	if len(metaLines) > 1 {
		fields := strings.Fields(metaLines[1])
		if len(fields) > 0 {
			metadata.SHA256 = fields[0]
		}
	}
	return metadata, output[contentIndex+len(fileContentMarker)+1:]
}

func parseFileEditOutput(path, validatorID, output string) domain.FileMetadata {
	metadata := domain.FileMetadata{Path: path, Validator: validatorID, ValidationOK: validatorID == "" || strings.Contains(output, fileValidationMarker)}
	lines := strings.Split(output, "\n")
	for index, line := range lines {
		if index+1 >= len(lines) {
			continue
		}
		value := strings.TrimSpace(lines[index+1])
		switch strings.TrimSpace(line) {
		case fileAfterMarker:
			fields := strings.Fields(value)
			if len(fields) > 0 {
				metadata.SHA256 = fields[0]
			}
		}
	}
	return metadata
}
