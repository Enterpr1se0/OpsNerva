package agenttool

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

type workspaceFileRecorder struct {
	WorkspaceFileService
	workspaceID string
	path        string
	maxBytes    int
}

func (service *workspaceFileRecorder) ReadWorkspaceFileAdvanced(
	_ context.Context, workspaceID, path string, maxBytes int, _ int64, _ int, _ string,
) (domain.ExecResult, error) {
	service.workspaceID = workspaceID
	service.path = path
	service.maxBytes = maxBytes
	return domain.ExecResult{Status: "completed", Stdout: "content"}, nil
}

func TestWorkspaceShellActionValidationRejectsRunFieldsOnInput(t *testing.T) {
	input := WorkspaceShellInput{
		Action: "input", ShellID: "shell-1", Input: "go test ./...", Submit: true,
		Script: "pwd", TimeoutSeconds: 30,
	}
	err := validateWorkspaceShellActionFields(
		input, "input", []string{"action", "shell_id", "input", "submit", "reason"},
		map[string]any{"action": "input", "shell_id": "shell_xxx", "input": "go test ./...", "submit": true},
	)
	var validation *InputError
	if !errors.As(err, &validation) || validation.Validation() == nil {
		t.Fatalf("structured validation error was not returned: %v", err)
	}
	details := validation.Validation()
	if strings.Join(details.UnexpectedFields, ",") != "script,timeout_seconds" {
		t.Fatalf("unexpected Workspace shell fields = %#v", details)
	}
}

func TestWorkspaceFileReadUsesResolvedWorkspaceAndDefaultPage(t *testing.T) {
	files := &workspaceFileRecorder{}
	workspace := NewWorkspace(WorkspaceDependencies{
		Resolve: func(context.Context) (string, error) { return "project", nil },
		Files:   files,
		Results: passthroughResultPolicy{},
	})
	result, err := workspace.RunFileRead(context.Background(), WorkspaceReadInput{Path: "README.md"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if files.workspaceID != "project" || files.path != "README.md" || files.maxBytes != 128<<10 {
		t.Fatalf("Workspace read mapping = id=%q path=%q max=%d", files.workspaceID, files.path, files.maxBytes)
	}
	if result.Status != "completed" || result.Stdout != "content" {
		t.Fatalf("Workspace read result = %#v", result)
	}
}
