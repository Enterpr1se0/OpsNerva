package agent

import (
	"github.com/Enterpr1se0/opsnerva/internal/agenttool"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func workspaceShellProvidedFields(input agenttool.WorkspaceShellInput) []string {
	fields := []string{"action"}
	if input.Script != "" {
		fields = append(fields, "script")
	}
	if input.ShellID != "" {
		fields = append(fields, "shell_id")
	}
	if input.Input != "" {
		fields = append(fields, "input")
	}
	if input.Submit {
		fields = append(fields, "submit")
	}
	if input.Cwd != "" {
		fields = append(fields, "cwd")
	}
	if len(input.Env) != 0 {
		fields = append(fields, "env")
	}
	if input.TimeoutSeconds != 0 {
		fields = append(fields, "timeout_seconds")
	}
	if input.AfterSequence != nil {
		fields = append(fields, "after_sequence")
	}
	if input.WaitSeconds != nil {
		fields = append(fields, "wait_seconds")
	}
	if input.MaxOutputBytes != nil {
		fields = append(fields, "max_output_bytes")
	}
	if input.Reason != "" {
		fields = append(fields, "reason")
	}
	return fields
}

func validateWorkspaceShellActionFields(input agenttool.WorkspaceShellInput, action string, allowed []string, example map[string]any) error {
	return agenttool.ValidateActionFields(action, workspaceShellProvidedFields(input), allowed, example)
}

func invalidWorkspaceShellValue(input agenttool.WorkspaceShellInput, action, message string, allowed []string, example map[string]any) error {
	return agenttool.StructuredInputError(message, domain.ToolValidationDetails{
		Action: action, AllowedFields: allowed, GotFields: workspaceShellProvidedFields(input), Example: example,
	})
}
