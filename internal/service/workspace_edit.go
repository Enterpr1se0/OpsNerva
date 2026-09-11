package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/ids"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"
	"github.com/Enterpr1se0/opsnerva/internal/workspacefs"
)

func (s *Service) EditWorkspaceFile(ctx context.Context, workspaceID, relativePath, oldText, newText, validatorID, reason, actor string) (domain.ExecResult, error) {
	workspace, ok := s.workspaceByID(workspaceID)
	if !ok {
		return domain.ExecResult{}, fmt.Errorf("workspace %q not found", workspaceID)
	}
	if workspace.Access != "read_write" {
		return domain.ExecResult{}, fmt.Errorf("workspace %q is read_only", workspaceID)
	}
	editContent := oldText + "\n" + newText
	if len(editContent) > 1<<20 || strings.Contains(editContent, "[REDACTED]") || s.redactor.Redact(editContent) != editContent {
		return domain.ExecResult{}, fmt.Errorf("workspace edit is too large or contains sensitive content")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return domain.ExecResult{}, fmt.Errorf("reason is required")
	}
	if _, err := s.workspaceValidator(validatorID, workspace, relativePath); err != nil {
		return domain.ExecResult{}, err
	}
	edit, change, err := buildTextEdit(relativePath, oldText, newText)
	if err != nil {
		return domain.ExecResult{}, err
	}
	host, err := s.workspaceHost(ctx, workspaceID)
	if err != nil {
		return domain.ExecResult{}, err
	}
	result, submitErr := s.Submit(ctx, domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecWorkspaceEdit, WorkspaceID: workspaceID, RelativePath: relativePath,
		Change: &change, TextEdit: &edit, Validator: validatorID, Reason: reason,
	}, actor)
	result.Change = &change
	if result.Stdout != "" {
		metadata := parseFileEditOutput(relativePath, validatorID, result.Stdout, result.Status == "completed")
		result.File = &metadata
	}
	if result.ExitCode == 74 {
		return result, fmt.Errorf("workspace validation failed; the target file was not changed")
	}
	if result.ExitCode == 75 {
		return result, fileEditConflictError(result, "workspace file edit conflict: "+fileEditRetryAdvice)
	}
	return result, submitErr
}

func (s *Service) editWorkspaceFile(ctx context.Context, workspace config.Workspace, path string, req domain.ExecRequest) (sshx.RawResult, error) {
	started := time.Now()
	if req.Change == nil {
		return sshx.RawResult{}, fmt.Errorf("workspace file change is missing")
	}
	if req.TextEdit == nil {
		return sshx.RawResult{}, fmt.Errorf("workspace text edit is missing")
	}
	if err := validateTextEditChange(req.RelativePath, *req.TextEdit, *req.Change); err != nil {
		return sshx.RawResult{}, err
	}
	info, statErr := os.Stat(path)
	existed := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return sshx.RawResult{}, statErr
	}
	creating := req.TextEdit.OldText == ""
	if !existed && !creating {
		return sshx.RawResult{ExitCode: 1, Stderr: []byte("workspace edit target does not exist"), Duration: time.Since(started)}, fmt.Errorf("workspace edit target does not exist")
	}
	if existed && creating {
		message := "workspace create target already exists"
		return sshx.RawResult{ExitCode: 75, Stderr: []byte(message), Duration: time.Since(started)}, fmt.Errorf("%s", message)
	}
	if existed && !info.Mode().IsRegular() {
		return sshx.RawResult{}, fmt.Errorf("workspace edit target is not a regular file")
	}
	mode := os.FileMode(0o600)
	var original []byte
	var originalDigest [sha256.Size]byte
	var updated []byte
	var err error
	if existed {
		mode = info.Mode().Perm()
		original, err = os.ReadFile(path)
		if err != nil {
			return sshx.RawResult{}, err
		}
		originalDigest = sha256.Sum256(original)
		updated, err = applyTextEditBytes(original, *req.TextEdit)
		if err != nil {
			return sshx.RawResult{ExitCode: 75, Stderr: []byte(err.Error()), Duration: time.Since(started)}, err
		}
	} else {
		updated = []byte(req.TextEdit.NewText)
	}
	suffix := time.Now().UTC().Format("20060102T150405Z") + "-" + ids.New("file")
	temporary := filepath.Join(filepath.Dir(path), ".opsnerva-"+filepath.Base(path)+"-"+suffix+".tmp")
	if err := workspacefs.WriteSyncedFile(temporary, updated, mode); err != nil {
		return sshx.RawResult{}, err
	}
	defer os.Remove(temporary)
	validationOutput, err := s.runWorkspaceValidator(ctx, req.Validator, workspace, req.RelativePath, temporary)
	if err != nil {
		_ = os.Remove(temporary)
		return sshx.RawResult{ExitCode: 74, Stdout: validationOutput, Stderr: []byte(err.Error()), Duration: time.Since(started)}, err
	}
	if existed {
		current, readErr := os.ReadFile(path)
		if readErr != nil {
			return sshx.RawResult{}, readErr
		}
		if sha256.Sum256(current) != originalDigest {
			message := "workspace file edit conflict: target changed during validation"
			return sshx.RawResult{ExitCode: 75, Stderr: []byte(message), Duration: time.Since(started)}, fmt.Errorf("%s", message)
		}
		if err = os.Rename(temporary, path); err != nil {
			return sshx.RawResult{}, err
		}
	} else {
		if err = os.Link(temporary, path); err != nil {
			if errors.Is(err, os.ErrExist) {
				message := "workspace file edit conflict: create target appeared during validation"
				return sshx.RawResult{ExitCode: 75, Stderr: []byte(message), Duration: time.Since(started)}, fmt.Errorf("%s", message)
			}
			return sshx.RawResult{}, err
		}
		if err = os.Remove(temporary); err != nil {
			return sshx.RawResult{}, err
		}
	}
	_ = os.Remove(temporary)
	if err := workspacefs.SyncDirectory(filepath.Dir(path)); err != nil {
		return sshx.RawResult{ExitCode: 74, Stderr: []byte(err.Error()), Duration: time.Since(started)}, err
	}
	afterDigest := sha256.Sum256(updated)
	stdout := ""
	if req.Validator != "" {
		stdout = fileValidationMarker + "\n"
	}
	stdout += fmt.Sprintf("%s\n%x  %s\n", fileAfterMarker, afterDigest, req.RelativePath)
	stdout += string(validationOutput)
	return sshx.RawResult{ExitCode: 0, Stdout: []byte(stdout), Duration: time.Since(started)}, nil
}

func (s *Service) workspaceValidator(id string, workspace config.Workspace, relative string) (config.Validator, error) {
	if id == "" {
		return config.Validator{}, nil
	}
	validator, ok := s.validators[id]
	if !ok || validator.Scope != "workspace" {
		available := s.ValidatorIDs("workspace")
		if len(available) == 0 {
			return config.Validator{}, fmt.Errorf("invalid validator_id %q: no Workspace validator IDs are configured; omit validator_id", id)
		}
		return config.Validator{}, fmt.Errorf("invalid Workspace validator_id %q; available IDs: %s", id, strings.Join(available, ", "))
	}
	if !workspaceValidatorAllowsPath(validator, filepath.Join(workspace.Root, relative)) && !workspaceValidatorAllowsPath(validator, relative) {
		return config.Validator{}, fmt.Errorf("validator_id %q is not allowed for Workspace path %s", id, relative)
	}
	return validator, nil
}

func workspaceValidatorAllowsPath(validator config.Validator, path string) bool {
	validator.PathPatterns = append([]string(nil), validator.PathPatterns...)
	for index, pattern := range validator.PathPatterns {
		validator.PathPatterns[index] = filepath.ToSlash(pattern)
	}
	return validatorAllowsPath(validator, filepath.ToSlash(path))
}

func (s *Service) runWorkspaceValidator(ctx context.Context, id string, workspace config.Workspace, relative, path string) ([]byte, error) {
	validator, err := s.workspaceValidator(id, workspace, relative)
	if err != nil || id == "" {
		return nil, err
	}
	args := make([]string, len(validator.Args))
	for index, argument := range validator.Args {
		args[index] = strings.ReplaceAll(argument, "{{path}}", path)
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(validator.TimeoutSeconds)*time.Second)
	defer cancel()
	command := exec.CommandContext(timeoutCtx, validator.Program, args...)
	command.Dir = workspace.Root
	command.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	err = command.Run()
	return output.Bytes(), err
}

type editableTextLine struct {
	text              string
	start, contentEnd int
	afterEnd          int
	eol               []byte
}

func editableTextLines(original []byte) ([]editableTextLine, error) {
	if bytes.HasPrefix(original, []byte{0xff, 0xfe}) || bytes.HasPrefix(original, []byte{0xfe, 0xff}) {
		return nil, fmt.Errorf("UTF-16 file editing is unsupported; convert the file to UTF-8 first")
	}
	if bytes.IndexByte(original, 0) >= 0 {
		return nil, fmt.Errorf("binary file editing is unsupported")
	}
	start := 0
	if bytes.HasPrefix(original, []byte{0xef, 0xbb, 0xbf}) {
		start = 3
	}
	lines := make([]editableTextLine, 0, bytes.Count(original[start:], []byte{'\n'})+1)
	for start < len(original) {
		newlineOffset := bytes.IndexByte(original[start:], '\n')
		if newlineOffset < 0 {
			lines = append(lines, editableTextLine{text: string(original[start:]), start: start, contentEnd: len(original), afterEnd: len(original)})
			break
		}
		newline := start + newlineOffset
		contentEnd := newline
		if contentEnd > start && original[contentEnd-1] == '\r' {
			contentEnd--
		}
		lines = append(lines, editableTextLine{
			text: string(original[start:contentEnd]), start: start, contentEnd: contentEnd, afterEnd: newline + 1,
			eol: append([]byte(nil), original[contentEnd:newline+1]...),
		})
		start = newline + 1
	}
	return lines, nil
}

func applyTextEditBytes(original []byte, edit domain.TextEdit) ([]byte, error) {
	originalLines, err := editableTextLines(original)
	if err != nil {
		return nil, err
	}
	oldLines := strings.Split(edit.OldText, "\n")
	var newLines []string
	if edit.NewText != "" {
		newLines = strings.Split(edit.NewText, "\n")
	}
	matches := make([]int, 0, 2)
	for start := 0; start+len(oldLines) <= len(originalLines); start++ {
		matched := true
		for offset := range oldLines {
			if originalLines[start+offset].text != oldLines[offset] {
				matched = false
				break
			}
		}
		if matched {
			matches = append(matches, start)
		}
	}
	if len(matches) != 1 {
		return nil, fmt.Errorf("file edit conflict: old_text matched %d blocks; %s", len(matches), fileEditRetryAdvice)
	}
	start := matches[0]
	first := originalLines[start]
	last := originalLines[start+len(oldLines)-1]
	spliceStart, spliceEnd := first.start, last.contentEnd
	if len(newLines) == 0 {
		if last.afterEnd > last.contentEnd {
			spliceEnd = last.afterEnd
		} else if start > 0 {
			spliceStart = originalLines[start-1].contentEnd
		}
		updated := make([]byte, 0, len(original)-(spliceEnd-spliceStart))
		updated = append(updated, original[:spliceStart]...)
		updated = append(updated, original[spliceEnd:]...)
		return updated, nil
	}
	eol := []byte{'\n'}
	for index := start; index <= start+len(oldLines)-1; index++ {
		if len(originalLines[index].eol) > 0 {
			eol = originalLines[index].eol
			break
		}
	}
	if len(last.eol) == 0 && start > 0 && len(originalLines[start-1].eol) > 0 {
		eol = originalLines[start-1].eol
	}
	var replacement bytes.Buffer
	for index, line := range newLines {
		if index > 0 {
			replacement.Write(eol)
		}
		replacement.WriteString(line)
	}
	updated := make([]byte, 0, len(original)-(spliceEnd-spliceStart)+replacement.Len())
	updated = append(updated, original[:spliceStart]...)
	updated = append(updated, replacement.Bytes()...)
	updated = append(updated, original[spliceEnd:]...)
	return updated, nil
}

func applyTextEdit(original string, edit domain.TextEdit) (string, error) {
	updated, err := applyTextEditBytes([]byte(original), edit)
	return string(updated), err
}
