package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/fileedit"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"
	"github.com/Enterpr1se0/opsnerva/internal/workspacefs"
)

func (s *Service) EditWorkspaceFile(ctx context.Context, workspaceID, relativePath, oldText, newText, validatorID, reason, actor string) (domain.ExecResult, error) {
	workspace, ok := s.workspaces.Get(workspaceID)
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
	edit, change, err := fileedit.Build(relativePath, oldText, newText)
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
		return result, fileEditConflictError(result, "workspace file edit conflict: "+fileedit.RetryAdvice)
	}
	return result, submitErr
}

func (s *Service) editWorkspaceFile(ctx context.Context, workspace config.Workspace, req domain.ExecRequest) (sshx.RawResult, error) {
	if req.Change == nil {
		return sshx.RawResult{}, fmt.Errorf("workspace file change is missing")
	}
	if req.TextEdit == nil {
		return sshx.RawResult{}, fmt.Errorf("workspace text edit is missing")
	}
	if err := fileedit.ValidateChange(req.RelativePath, *req.TextEdit, *req.Change); err != nil {
		return sshx.RawResult{}, err
	}
	edit, err := workspacefs.New(workspace.Root).PrepareEdit(ctx, req.RelativePath, *req.TextEdit)
	if err != nil {
		return workspaceEditFailure(workspaceFileError(err))
	}
	defer edit.Close()
	validationOutput, err := s.runWorkspaceValidator(ctx, req.Validator, workspace, req.RelativePath, edit.StagedPath())
	if err != nil {
		return sshx.RawResult{ExitCode: 74, Stdout: validationOutput, Stderr: []byte(err.Error())}, err
	}
	result, err := edit.Commit(ctx)
	if err != nil {
		return workspaceEditFailure(err)
	}
	stdout := ""
	if req.Validator != "" {
		stdout = fileValidationMarker + "\n"
	}
	stdout += fmt.Sprintf("%s\n%s  %s\n", fileAfterMarker, result.SHA256, req.RelativePath)
	stdout += string(validationOutput)
	return sshx.RawResult{ExitCode: 0, Stdout: []byte(stdout)}, nil
}

// Exit codes and tool output belong to Service, not the filesystem transaction.
func workspaceEditFailure(err error) (sshx.RawResult, error) {
	exitCode := 1
	var conflict *workspacefs.EditConflictError
	if errors.As(err, &conflict) {
		exitCode = 75
	}
	return sshx.RawResult{ExitCode: exitCode, Stderr: []byte(err.Error())}, err
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
