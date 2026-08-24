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

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/ids"
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

func (s *Service) EditRemoteFile(ctx context.Context, hostID, path, oldText, newText, validatorID string, elevated bool, reason, actor string) (domain.ExecResult, error) {
	if err := validateRemoteFilePath(path); err != nil {
		return domain.ExecResult{}, err
	}
	if len(oldText)+len(newText) > 1<<20 {
		return domain.ExecResult{}, fmt.Errorf("file edit exceeds 1 MiB")
	}
	if strings.TrimSpace(reason) == "" {
		return domain.ExecResult{}, fmt.Errorf("reason is required")
	}
	editContent := oldText + "\n" + newText
	if strings.Contains(editContent, "[REDACTED]") || s.redactor.Redact(editContent) != editContent {
		return domain.ExecResult{}, fmt.Errorf("file edit contains a secret or redaction placeholder; use a change that does not expose or overwrite secret values")
	}
	if _, err := s.validatorCommandFor(validatorID, "remote", path, path); err != nil {
		return domain.ExecResult{}, err
	}
	edit, change, err := buildTextEdit(path, oldText, newText)
	if err != nil {
		return domain.ExecResult{}, err
	}
	result, submitErr := s.Submit(ctx, domain.ExecRequest{
		HostID: hostID, Mode: domain.ExecRemoteEdit, Change: &change, TextEdit: &edit, Elevated: elevated, Reason: reason,
		RemotePath: path, Validator: validatorID,
	}, actor)
	result.Change = &change
	if result.Stdout != "" {
		metadata := parseFileEditOutput(path, validatorID, result.Stdout, result.Status == "completed")
		result.File = &metadata
	}
	if result.ExitCode == 74 {
		return result, fmt.Errorf("validation failed; the target file was not changed")
	}
	if result.ExitCode == 75 {
		return result, fmt.Errorf("file edit conflict: old_text no longer matches exactly one block in the current file")
	}
	return result, submitErr
}

func (s *Service) prepareRemoteFileChange(req domain.ExecRequest) (domain.ExecRequest, error) {
	if req.Change == nil {
		return req, fmt.Errorf("remote file change is missing")
	}
	if req.TextEdit == nil {
		return req, fmt.Errorf("remote text edit is missing")
	}
	if err := validateTextEditChange(req.RemotePath, *req.TextEdit, *req.Change); err != nil {
		return req, err
	}
	suffix := time.Now().UTC().Format("20060102T150405Z") + "-" + ids.New("file")
	tempPath := posixpath.Join(posixpath.Dir(req.RemotePath), ".opsnerva-"+posixpath.Base(req.RemotePath)+"-"+suffix+".tmp")
	validatorCommand, err := s.validatorCommandFor(req.Validator, "remote", req.RemotePath, tempPath)
	if err != nil {
		return req, err
	}
	prepared := req
	prepared.Mode = domain.ExecScript
	prepared.Script = buildRemoteFileChangeScript(req.RemotePath, tempPath, *req.TextEdit, validatorCommand)
	return prepared, nil
}

func buildRemoteFileChangeScript(path, tempPath string, edit domain.TextEdit, validatorCommand string) string {
	// The diff is review-only. Locate the approved lines, then splice the original
	// bytes so untouched BOM, line endings, and final-newline state remain intact.
	pathQ, tempQ := shellQuote(path), shellQuote(tempPath)
	oldPath := tempPath + ".old"
	oldQ := shellQuote(oldPath)
	newPath := tempPath + ".new"
	newQ := shellQuote(newPath)
	outputPath := tempPath + ".output"
	outputQ := shellQuote(outputPath)
	lines := []string{
		"set -eu",
		"test ! -e " + tempQ,
		"test ! -e " + oldQ,
		"test ! -e " + newQ,
		"test ! -e " + outputQ,
		"trap " + shellQuote("test ! -e "+tempQ+" || unlink -- "+tempQ+"; test ! -e "+oldQ+" || unlink -- "+oldQ+"; test ! -e "+newQ+" || unlink -- "+newQ+"; test ! -e "+outputQ+" || unlink -- "+outputQ) + " EXIT",
		writeRemoteText(edit.OldText, oldQ),
		writeRemoteText(edit.NewText, newQ),
	}
	if edit.OldText == "" {
		lines = append(lines,
			"if [ -e "+pathQ+" ]; then printf 'file edit conflict: create target already exists; read it and retry\\n' >&2; exit 75; fi",
			"umask 077",
			"cat -- "+newQ+" > "+tempQ,
		)
	} else {
		lines = append(lines,
			"test -f "+pathQ,
			"original_sha256=$(sha256sum -- "+pathQ+")",
			"original_sha256=${original_sha256%% *}",
			"cp -p -- "+pathQ+" "+tempQ,
			"file_size=$(wc -c < "+pathQ+")",
			"match_info=$(LC_ALL=C awk -v old_path="+oldQ+" -v old_count="+strconv.Itoa(len(strings.Split(edit.OldText, "\n")))+" -v file_size=\"$file_size\" "+shellQuote(remoteTextEditLocateProgram())+" "+pathQ+")",
			"set -- $match_info",
			"match_start=$1; match_end=$2; delete_start=$3; delete_end=$4; match_eol=$5",
		)
		if edit.NewText == "" {
			lines = append(lines, "splice_start=$delete_start", "splice_end=$delete_end")
		} else {
			lines = append(lines, "splice_start=$match_start", "splice_end=$match_end")
		}
		lines = append(lines,
			"{",
			"  head -c \"$splice_start\" -- "+pathQ,
		)
		if edit.NewText != "" {
			lines = append(lines, "  LC_ALL=C awk -v eol=\"$match_eol\" "+shellQuote(remoteTextEditOutputProgram())+" "+newQ)
		}
		lines = append(lines,
			"  tail -c +$((splice_end + 1)) -- "+pathQ,
			"} > "+outputQ,
			"cat -- "+outputQ+" > "+tempQ,
			"unlink -- "+outputQ,
		)
	}
	lines = append(lines, "unlink -- "+oldQ, "unlink -- "+newQ)
	lines = append(lines, "sync -f -- "+tempQ)
	if validatorCommand != "" {
		lines = append(lines, "if ! "+validatorCommand+"; then", "  unlink -- "+tempQ, "  exit 74", "fi")
	}
	if edit.OldText == "" {
		lines = append(lines,
			"if ! ln -- "+tempQ+" "+pathQ+"; then printf 'file edit conflict: create target appeared during validation; read it and retry\\n' >&2; exit 75; fi",
			"unlink -- "+tempQ,
		)
	} else {
		lines = append(lines,
			"current_sha256=$(sha256sum -- "+pathQ+")",
			"current_sha256=${current_sha256%% *}",
			"if [ \"$current_sha256\" != \"$original_sha256\" ]; then",
			"  printf 'file edit conflict: target changed during validation; read the current file and retry\\n' >&2",
			"  exit 75",
			"fi",
			"mv -f -- "+tempQ+" "+pathQ,
		)
	}
	if validatorCommand != "" {
		lines = append(lines, "printf '"+fileValidationMarker+"\\n'")
	}
	lines = append(lines, "trap - EXIT", "sync -f -- "+pathQ, "sync -f -- "+shellQuote(posixpath.Dir(path)), "printf '"+fileAfterMarker+"\\n'", "sha256sum -- "+pathQ)
	return strings.Join(lines, "\n")
}

func writeRemoteText(value, quotedPath string) string {
	return "printf '%s' " + shellQuote(value) + " > " + quotedPath
}

func buildTextEdit(path, oldText, newText string) (domain.TextEdit, domain.FileChange, error) {
	oldText, err := normalizeTextEditBlock(oldText, true)
	if err != nil {
		return domain.TextEdit{}, domain.FileChange{}, fmt.Errorf("invalid old_text: %w", err)
	}
	newText, err = normalizeTextEditBlock(newText, true)
	if err != nil {
		return domain.TextEdit{}, domain.FileChange{}, fmt.Errorf("invalid new_text: %w", err)
	}
	if oldText == newText {
		return domain.TextEdit{}, domain.FileChange{}, fmt.Errorf("old_text and new_text must be different")
	}

	var oldLines []string
	if oldText != "" {
		oldLines = strings.Split(oldText, "\n")
	}
	var newLines []string
	if newText != "" {
		newLines = strings.Split(newText, "\n")
	}
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix && oldLines[len(oldLines)-1-suffix] == newLines[len(newLines)-1-suffix] {
		suffix++
	}
	oldChanged := oldLines[prefix : len(oldLines)-suffix]
	newChanged := newLines[prefix : len(newLines)-suffix]
	body := make([]string, 0, len(oldLines)+len(newChanged))
	for _, line := range oldLines[:prefix] {
		body = append(body, " "+line)
	}
	for _, line := range oldChanged {
		body = append(body, "-"+line)
	}
	for _, line := range newChanged {
		body = append(body, "+"+line)
	}
	if suffix > 0 {
		for _, line := range oldLines[len(oldLines)-suffix:] {
			body = append(body, " "+line)
		}
	}
	diff := "--- " + path + "\n+++ " + path + "\n@@ unique block @@\n" + strings.Join(body, "\n") + "\n"
	return domain.TextEdit{OldText: oldText, NewText: newText}, domain.FileChange{
		Diff: diff, Additions: len(newChanged), Deletions: len(oldChanged),
	}, nil
}

func validateTextEditChange(path string, edit domain.TextEdit, change domain.FileChange) error {
	normalizedEdit, expectedChange, err := buildTextEdit(path, edit.OldText, edit.NewText)
	if err != nil {
		return fmt.Errorf("invalid persisted text edit: %w", err)
	}
	if normalizedEdit != edit || expectedChange != change {
		return fmt.Errorf("file edit approval data does not match the generated change")
	}
	return nil
}

func normalizeTextEditBlock(value string, allowEmpty bool) (string, error) {
	value = strings.TrimPrefix(value, "\ufeff")
	value = strings.ReplaceAll(value, "\r\n", "\n")
	if strings.ContainsAny(value, "\x00\r") {
		return "", fmt.Errorf("contains unsupported control characters")
	}
	value = strings.TrimSuffix(value, "\n")
	if value == "" && !allowEmpty {
		return "", fmt.Errorf("must contain at least one complete line")
	}
	return value, nil
}

func remoteTextEditLocateProgram() string {
	return strings.Join([]string{
		"BEGIN {",
		"  while ((read_status = getline line < old_path) > 0) { expected[++expected_count] = line }",
		"  close(old_path)",
		"  if (read_status < 0) { print \"file edit failed to read old_text\" > \"/dev/stderr\"; exit 76 }",
		"  if (expected_count != old_count || expected_count == 0) { print \"file edit old_text encoding is invalid\" > \"/dev/stderr\"; exit 76 }",
		"  bom = sprintf(\"%c%c%c\", 239, 187, 191)",
		"  byte_offset = 0",
		"  previous_content_end = 0",
		"}",
		"{",
		"  raw = $0",
		"  record_has_newline = (byte_offset + length(raw) < file_size)",
		"  has_crlf = record_has_newline && length(raw) > 0 && substr(raw, length(raw), 1) == \"\\r\"",
		"  normalized = has_crlf ? substr(raw, 1, length(raw) - 1) : raw",
		"  line_start = byte_offset",
		"  if (NR == 1 && substr(normalized, 1, 3) == bom) { normalized = substr(normalized, 4); line_start += 3 }",
		"  line_content_end = byte_offset + length(raw) - (has_crlf ? 1 : 0)",
		"  line_after_end = byte_offset + length(raw) + (record_has_newline ? 1 : 0)",
		"  slot = (NR - 1) % expected_count",
		"  window[slot] = normalized",
		"  starts[slot] = line_start",
		"  content_ends[slot] = line_content_end",
		"  after_ends[slot] = line_after_end",
		"  before_ends[slot] = previous_content_end",
		"  eols[slot] = has_crlf ? 2 : (record_has_newline ? 1 : 0)",
		"  if (NR >= expected_count) {",
		"    matched = 1",
		"    for (offset = 1; offset <= expected_count; offset++) {",
		"      slot = (NR - expected_count + offset - 1) % expected_count",
		"      if (window[slot] != expected[offset]) { matched = 0; break }",
		"    }",
		"    if (matched) {",
		"      matches++",
		"      first_slot = (NR - expected_count) % expected_count",
		"      last_slot = (NR - 1) % expected_count",
		"      match_start = starts[first_slot]",
		"      match_end = content_ends[last_slot]",
		"      delete_start = match_start",
		"      delete_end = content_ends[last_slot]",
		"      if (after_ends[last_slot] > content_ends[last_slot]) { delete_end = after_ends[last_slot] }",
		"      else if (match_start > 0) { delete_start = before_ends[first_slot] }",
		"      match_eol = 0",
		"      for (offset = 1; offset <= expected_count; offset++) {",
		"        slot = (NR - expected_count + offset - 1) % expected_count",
		"        if (match_eol == 0 && eols[slot] > 0) { match_eol = eols[slot] }",
		"      }",
		"      if (match_eol == 0 && match_start > before_ends[first_slot]) { match_eol = match_start - before_ends[first_slot] }",
		"      if (match_eol != 2) { match_eol = 1 }",
		"    }",
		"  }",
		"  previous_content_end = line_content_end",
		"  byte_offset = line_after_end",
		"}",
		"END {",
		"  if (read_status < 0 || expected_count != old_count || expected_count == 0) { exit 76 }",
		"  if (matches != 1) {",
		"    printf \"file edit conflict: old_text matched %d blocks; read the current file and retry with a unique block\\n\", matches > \"/dev/stderr\"",
		"    exit 75",
		"  }",
		"  print match_start, match_end, delete_start, delete_end, match_eol",
		"}",
	}, "\n")
}

func remoteTextEditOutputProgram() string {
	return strings.Join([]string{
		"BEGIN {",
		"  separator = eol == 2 ? \"\\r\\n\" : \"\\n\"",
		"}",
		"{ if (NR > 1) { printf \"%s\", separator }; printf \"%s\", $0 }",
	}, "\n")
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
			if len(fields) >= 6 {
				metadata.OffsetBytes, _ = strconv.ParseInt(fields[5], 10, 64)
			}
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

func parseFileEditOutput(path, validatorID, output string, succeeded bool) domain.FileMetadata {
	metadata := domain.FileMetadata{Path: path, Validator: validatorID, ValidationOK: succeeded && validatorID != "" && strings.Contains(output, fileValidationMarker)}
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
