package service

import (
	"bytes"
	"fmt"
	"path"
	"strings"

	"eino-ops-agent/internal/domain"

	"mvdan.cc/sh/v3/syntax"
)

// ExecutionToolSelectionError distinguishes an invalid execution mode from a
// malformed argument. Agent-facing adapters use the suggested tool and example
// to recover without repeating the same request.
type ExecutionToolSelectionError struct {
	Message       string
	SuggestedTool string
	NextAction    string
	Example       map[string]any
}

func (err *ExecutionToolSelectionError) Error() string { return err.Message }

func executionToolSelectionError(req domain.ExecRequest, program string) error {
	exampleReason := strings.TrimSpace(req.Reason)
	if exampleReason == "" {
		exampleReason = "run an interactive remote program"
	}
	if isShellInterpreter(program) && len(req.Args) > 0 {
		example := map[string]any{
			"host_id": req.HostID,
			"script":  "<Bash script body>",
			"reason":  exampleReason,
		}
		if body, ok := shellCommandBody(req.Args); ok {
			example["script"] = body
		}
		return &ExecutionToolSelectionError{
			Message:       fmt.Sprintf("ssh_exec does not accept shell interpreter %q; pass the script body directly to ssh_run_script", program),
			SuggestedTool: "ssh_run_script",
			NextAction:    "call ssh_run_script with script set to the Bash body; do not wrap it in bash -c or sh -c",
			Example:       example,
		}
	}
	return &ExecutionToolSelectionError{
		Message:       fmt.Sprintf("ssh_exec cannot run interactive program %q because it has no PTY; use ssh_shell", program),
		SuggestedTool: "ssh_shell",
		NextAction:    "call ssh_shell with action=start (a login shell is already running), then action=input; use output as needed and close the shell when done",
		Example: map[string]any{
			"action": "start", "host_id": req.HostID, "reason": exampleReason,
		},
	}
}

func isShellInterpreter(program string) bool {
	switch program {
	case "bash", "sh", "zsh", "fish":
		return true
	default:
		return false
	}
}

func shellCommandBody(args []string) (string, bool) {
	for index, argument := range args {
		if argument == "-c" && index+1 < len(args) {
			return args[index+1], true
		}
		if strings.HasPrefix(argument, "-") && !strings.HasPrefix(argument, "--") && strings.Contains(strings.TrimPrefix(argument, "-"), "c") && index+1 < len(args) {
			return args[index+1], true
		}
	}
	return "", false
}

// containsShellProgram detects direct program invocation without classifying
// the operation. It exists only to enforce managed sudo semantics.
func containsShellProgram(req domain.ExecRequest, name string) (bool, error) {
	if req.Mode == domain.ExecProgram {
		return path.Base(strings.TrimSpace(req.Program)) == name, nil
	}
	if req.Mode != domain.ExecScript && req.Mode != domain.ExecWorkspaceShell {
		return false, nil
	}
	if strings.TrimSpace(req.Script) == "" {
		return false, fmt.Errorf("script is empty")
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(req.Script), "operation.sh")
	if err != nil {
		return false, err
	}
	found := false
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		var printed bytes.Buffer
		if err := syntax.NewPrinter().Print(&printed, call.Args[0]); err == nil && path.Base(strings.Trim(printed.String(), "'\"")) == name {
			found = true
			return false
		}
		return true
	})
	return found, nil
}
