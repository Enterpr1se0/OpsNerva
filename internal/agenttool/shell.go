package agenttool

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

const defaultShellOutputBytes = 128 << 10

func ShellOutputPolicy(waitSeconds, maxOutputBytes *int) (time.Duration, int, error) {
	wait := domain.DefaultShellQueryDelaySeconds
	if waitSeconds != nil {
		wait = *waitSeconds
	}
	if wait < 0 || wait > domain.MaxShellQueryDelaySeconds {
		return 0, 0, fmt.Errorf("wait_seconds must be between 0 and %d", domain.MaxShellQueryDelaySeconds)
	}
	maxBytes := defaultShellOutputBytes
	if maxOutputBytes != nil {
		maxBytes = *maxOutputBytes
	}
	if maxBytes < 4<<10 || maxBytes > maxOutputViewBytes {
		return 0, 0, fmt.Errorf("max_output_bytes must be between 4096 and %d", maxOutputViewBytes)
	}
	return time.Duration(wait) * time.Second, maxBytes, nil
}

func sshShellProvidedFields(input SSHShellInput) []string {
	fields := []string{"action"}
	if input.HostID != "" {
		fields = append(fields, "host_id")
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
	if input.Elevated {
		fields = append(fields, "elevated")
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

func validateSSHShellActionFields(input SSHShellInput, action string, allowed []string, example map[string]any) error {
	return ValidateActionFields(action, sshShellProvidedFields(input), allowed, example)
}

func invalidSSHShellValue(input SSHShellInput, action, message string, allowed []string, example map[string]any) error {
	return StructuredInputError(message, domain.ToolValidationDetails{
		Action: action, AllowedFields: allowed, GotFields: sshShellProvidedFields(input), Example: example,
	})
}

func (ssh *SSH) readableShellSnapshot(ctx context.Context, snapshot domain.SSHShellSnapshot, after uint64) domain.SSHShellSnapshot {
	readable, err := ssh.dependencies.Shells.ReadableSSHShellSnapshot(ctx, snapshot, after)
	if err == nil {
		return readable
	}
	// Never expose raw terminal escapes when a readable replay cannot be
	// produced. recent_output remains available as a bounded snapshot.
	for index := range snapshot.Events {
		if snapshot.Events[index].Stream == "stdout" || snapshot.Events[index].Stream == "stderr" {
			snapshot.Events[index].Content = ""
		}
	}
	return snapshot
}

type ShellOutputChunk struct {
	FirstSequence uint64 `json:"first_sequence,omitempty"`
	Sequence      uint64 `json:"sequence"`
	Stream        string `json:"stream"`
	Content       string `json:"content"`
}

type ShellResult struct {
	ShellID           string             `json:"shell_id"`
	HostID            string             `json:"host_id,omitempty"`
	HostName          string             `json:"host_name,omitempty"`
	WorkspaceID       string             `json:"workspace_id,omitempty"`
	Status            string             `json:"status"`
	Chunks            []ShellOutputChunk `json:"chunks,omitempty"`
	NextSequence      uint64             `json:"next_sequence"`
	OutputBytes       int                `json:"output_bytes,omitempty"`
	HasMore           bool               `json:"has_more,omitempty"`
	ExitCode          *int               `json:"exit_code,omitempty"`
	TerminationReason string             `json:"termination_reason,omitempty"`
	Error             string             `json:"error,omitempty"`
}

func (ssh *SSH) FormatShellPage(ctx context.Context, page domain.SSHShellOutputPage, after uint64, stripInputEcho bool) ShellResult {
	readable := ssh.readableShellSnapshot(ctx, page.Snapshot, after)
	shell := readable.Shell
	chunks := ModelShellChunks(readable.Events, stripInputEcho)
	outputBytes := 0
	for _, chunk := range chunks {
		outputBytes += len(chunk.Content)
	}
	return ShellResult{
		ShellID: shell.ID, HostID: shell.HostID, HostName: shell.HostName,
		WorkspaceID: shell.WorkspaceID, Status: shell.Status,
		Chunks: chunks, NextSequence: readable.NextSequence, OutputBytes: outputBytes,
		HasMore: page.HasMore, ExitCode: shell.ExitCode,
		TerminationReason: shell.TerminationReason, Error: shell.Error,
	}
}

func ModelShellChunks(events []domain.SSHShellEvent, stripInputEcho bool) []ShellOutputChunk {
	start := 0
	input := ""
	if stripInputEcho {
		for index, event := range events {
			if event.Stream == "input" && event.Source == "agent" {
				start = index + 1
				input = event.Content
			}
		}
	}
	chunks := make([]ShellOutputChunk, 0)
	for _, event := range events[start:] {
		if (event.Stream != "stdout" && event.Stream != "stderr") || event.Content == "" {
			continue
		}
		content := normalizeShellNewlines(event.Content)
		if len(chunks) > 0 && chunks[len(chunks)-1].Stream == event.Stream {
			chunks[len(chunks)-1].Content += content
			chunks[len(chunks)-1].Sequence = event.Sequence
			continue
		}
		chunks = append(chunks, ShellOutputChunk{
			FirstSequence: event.Sequence, Sequence: event.Sequence, Stream: event.Stream, Content: content,
		})
	}
	var combined strings.Builder
	for _, chunk := range chunks {
		combined.WriteString(chunk.Content)
	}
	cleaned := cleanModelShellResponse(combined.String(), input)
	removeBytes := combined.Len() - len(cleaned)
	for removeBytes > 0 && len(chunks) > 0 {
		if removeBytes >= len(chunks[0].Content) {
			removeBytes -= len(chunks[0].Content)
			chunks = chunks[1:]
			continue
		}
		chunks[0].Content = chunks[0].Content[removeBytes:]
		removeBytes = 0
	}
	for len(chunks) > 0 && chunks[0].Content == "" {
		chunks = chunks[1:]
	}
	for index := range chunks {
		if chunks[index].FirstSequence == chunks[index].Sequence {
			chunks[index].FirstSequence = 0
		}
	}
	return chunks
}

func ModelShellOutput(events []domain.SSHShellEvent) string {
	start := 0
	input := ""
	for index, event := range events {
		if event.Stream == "input" && event.Source == "agent" {
			start = index + 1
			input = event.Content
		}
	}
	var output strings.Builder
	for _, event := range events[start:] {
		if event.Stream == "stdout" || event.Stream == "stderr" {
			output.WriteString(event.Content)
		}
	}
	return cleanModelShellResponse(output.String(), input)
}

// cleanModelShellResponse removes the terminal driver's echo of the input line
// from the model-facing result. Raw PTY events remain unchanged for the Web
// terminal. ConPTY may prefix the echoed command with the PowerShell prompt,
// while Unix PTYs usually echo only the command itself.
func cleanModelShellResponse(output, input string) string {
	output = normalizeShellNewlines(output)
	command := strings.TrimRight(normalizeShellNewlines(input), "\n")
	if output == "" || command == "" || strings.Contains(command, "\n") {
		return output
	}
	lineEnd := strings.IndexByte(output, '\n')
	firstLine := output
	remainder := ""
	if lineEnd >= 0 {
		firstLine = output[:lineEnd]
		remainder = output[lineEnd+1:]
	}
	if firstLine == command || shellPromptEcho(firstLine, command) {
		return strings.TrimPrefix(remainder, "\n")
	}
	return output
}

func normalizeShellNewlines(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	return strings.ReplaceAll(value, "\r", "\n")
}

func shellPromptEcho(line, command string) bool {
	if !strings.HasSuffix(line, command) {
		return false
	}
	prefix := strings.TrimSpace(strings.TrimSuffix(line, command))
	if prefix == "" {
		return true
	}
	return strings.HasSuffix(prefix, ">") || strings.HasSuffix(prefix, "$") ||
		strings.HasSuffix(prefix, "#") || strings.HasSuffix(prefix, "%")
}

func ShellSnapshotAfter(snapshot domain.SSHShellSnapshot) uint64 {
	if len(snapshot.Events) == 0 {
		return snapshot.NextSequence
	}
	sequence := snapshot.Events[0].Sequence
	if snapshot.Events[0].FirstSequence != 0 {
		sequence = snapshot.Events[0].FirstSequence
	}
	if sequence == 0 {
		return 0
	}
	return sequence - 1
}

func (ssh *SSH) RunShell(ctx context.Context, sessionID string, input SSHShellInput, actor string) (any, error) {
	action := strings.ToLower(strings.TrimSpace(input.Action))
	if sessionID == "" {
		return ssh.dependencies.Results.Value(ctx, "ssh_shell", domain.SSHShell{}, InvalidInput("ssh_shell requires an Agent or MCP session"))
	}
	switch action {
	case "start":
		allowed := []string{"action", "host_id", "cwd", "elevated", "reason"}
		example := map[string]any{"action": "start", "host_id": "host_xxx", "reason": "open an interactive diagnostic shell"}
		if err := validateSSHShellActionFields(input, action, allowed, example); err != nil {
			return ssh.dependencies.Results.Value(ctx, "ssh_shell", domain.ExecResult{}, err)
		}
		if strings.TrimSpace(input.HostID) == "" || strings.TrimSpace(input.Reason) == "" {
			return ssh.dependencies.Results.Value(ctx, "ssh_shell", domain.ExecResult{}, invalidSSHShellValue(input, action, "action=start requires host_id and reason", allowed, example))
		}
		if len(input.Reason) > 500 {
			return ssh.dependencies.Results.Value(ctx, "ssh_shell", domain.ExecResult{}, invalidSSHShellValue(input, action, "reason must not exceed 500 bytes", allowed, example))
		}
		result, err := ssh.dependencies.Shells.StartSSHShell(ctx, input.HostID, input.Cwd, input.Elevated, 120, 32, input.Reason, actor)
		return ssh.execResult(result, err)
	case "input":
		allowed := []string{"action", "shell_id", "input", "submit", "wait_seconds", "max_output_bytes", "reason"}
		example := map[string]any{"action": "input", "shell_id": "shell_xxx", "input": "whoami", "submit": true}
		if err := validateSSHShellActionFields(input, action, allowed, example); err != nil {
			return ssh.dependencies.Results.Value(ctx, "ssh_shell", domain.SSHShellSnapshot{}, err)
		}
		if strings.TrimSpace(input.ShellID) == "" || input.Input == "" {
			return ssh.dependencies.Results.Value(ctx, "ssh_shell", domain.SSHShellSnapshot{}, invalidSSHShellValue(input, action, "action=input requires shell_id and input", allowed, example))
		}
		shellInput := input.Input
		if input.Submit && !strings.HasSuffix(shellInput, "\r") && !strings.HasSuffix(shellInput, "\n") {
			shellInput += "\r"
		}
		if len(shellInput) > 64<<10 || len(input.Reason) > 500 {
			return ssh.dependencies.Results.Value(ctx, "ssh_shell", domain.SSHShellSnapshot{}, invalidSSHShellValue(input, action, "input must not exceed 65536 bytes and reason must not exceed 500 bytes", allowed, example))
		}
		queryDelay, maxBytes, policyErr := ShellOutputPolicy(input.WaitSeconds, input.MaxOutputBytes)
		if policyErr != nil {
			return ssh.dependencies.Results.Value(ctx, "ssh_shell", domain.SSHShellSnapshot{}, invalidSSHShellValue(input, action, policyErr.Error(), allowed, example))
		}
		page, err := ssh.dependencies.Shells.WriteSSHShellPage(ctx, input.ShellID, sessionID, shellInput, queryDelay, maxBytes, input.Reason, actor)
		return ssh.dependencies.Results.Value(ctx, "ssh_shell", ssh.FormatShellPage(ctx, page, ShellSnapshotAfter(page.Snapshot), true), err)
	case "output":
		allowed := []string{"action", "shell_id", "after_sequence", "wait_seconds", "max_output_bytes", "reason"}
		example := map[string]any{"action": "output", "shell_id": "shell_xxx", "wait_seconds": 10}
		if err := validateSSHShellActionFields(input, action, allowed, example); err != nil {
			return ssh.dependencies.Results.Value(ctx, "ssh_shell", domain.SSHShellSnapshot{}, err)
		}
		if strings.TrimSpace(input.ShellID) == "" {
			return ssh.dependencies.Results.Value(ctx, "ssh_shell", domain.SSHShellSnapshot{}, invalidSSHShellValue(input, action, "action=output requires shell_id", allowed, example))
		}
		if len(input.Reason) > 500 {
			return ssh.dependencies.Results.Value(ctx, "ssh_shell", domain.SSHShellSnapshot{}, invalidSSHShellValue(input, action, "reason must not exceed 500 bytes", allowed, example))
		}
		queryDelay, maxBytes, policyErr := ShellOutputPolicy(input.WaitSeconds, input.MaxOutputBytes)
		if policyErr != nil {
			return ssh.dependencies.Results.Value(ctx, "ssh_shell", domain.SSHShellSnapshot{}, invalidSSHShellValue(input, action, policyErr.Error(), allowed, example))
		}
		page, err := ssh.dependencies.Shells.QuerySSHShellOutput(ctx, input.ShellID, sessionID, input.AfterSequence, queryDelay, maxBytes, input.Reason, actor)
		return ssh.dependencies.Results.Value(ctx, "ssh_shell", ssh.FormatShellPage(ctx, page, ShellSnapshotAfter(page.Snapshot), false), err)
	case "list":
		allowed := []string{"action", "reason"}
		example := map[string]any{"action": "list"}
		if err := validateSSHShellActionFields(input, action, allowed, example); err != nil {
			return ssh.dependencies.Results.Value(ctx, "ssh_shell", domain.SSHShellList{}, err)
		}
		if len(input.Reason) > 500 {
			return ssh.dependencies.Results.Value(ctx, "ssh_shell", domain.SSHShellList{}, invalidSSHShellValue(input, action, "reason must not exceed 500 bytes", allowed, example))
		}
		result, err := ssh.dependencies.Shells.ListSSHShells(ctx, sessionID, true, input.Reason, actor)
		return ssh.dependencies.Results.Value(ctx, "ssh_shell", result, err)
	case "interrupt":
		allowed := []string{"action", "shell_id", "reason"}
		example := map[string]any{"action": "interrupt", "shell_id": "shell_xxx"}
		if err := validateSSHShellActionFields(input, action, allowed, example); err != nil {
			return ssh.dependencies.Results.Value(ctx, "ssh_shell", domain.SSHShell{}, err)
		}
		if strings.TrimSpace(input.ShellID) == "" {
			return ssh.dependencies.Results.Value(ctx, "ssh_shell", domain.SSHShell{}, invalidSSHShellValue(input, action, "action=interrupt requires shell_id", allowed, example))
		}
		if len(input.Reason) > 500 {
			return ssh.dependencies.Results.Value(ctx, "ssh_shell", domain.SSHShell{}, invalidSSHShellValue(input, action, "reason must not exceed 500 bytes", allowed, example))
		}
		result, err := ssh.dependencies.Shells.InterruptSSHShell(ctx, input.ShellID, sessionID, input.Reason, actor)
		return ssh.dependencies.Results.Value(ctx, "ssh_shell", result, err)
	case "close":
		allowed := []string{"action", "shell_id", "reason"}
		example := map[string]any{"action": "close", "shell_id": "shell_xxx"}
		if err := validateSSHShellActionFields(input, action, allowed, example); err != nil {
			return ssh.dependencies.Results.Value(ctx, "ssh_shell", domain.SSHShell{}, err)
		}
		if strings.TrimSpace(input.ShellID) == "" {
			return ssh.dependencies.Results.Value(ctx, "ssh_shell", domain.SSHShell{}, invalidSSHShellValue(input, action, "action=close requires shell_id", allowed, example))
		}
		if len(input.Reason) > 500 {
			return ssh.dependencies.Results.Value(ctx, "ssh_shell", domain.SSHShell{}, invalidSSHShellValue(input, action, "reason must not exceed 500 bytes", allowed, example))
		}
		result, err := ssh.dependencies.Shells.CloseSSHShell(ctx, input.ShellID, sessionID, input.Reason, actor)
		return ssh.dependencies.Results.Value(ctx, "ssh_shell", result, err)
	default:
		return ssh.dependencies.Results.Value(ctx, "ssh_shell", domain.SSHShell{}, StructuredInputError(
			"invalid action: use start, input, output, list, interrupt, or close",
			domain.ToolValidationDetails{
				Action: action, AllowedFields: []string{"action"}, GotFields: sshShellProvidedFields(input),
				Example: map[string]any{"action": "list"},
			},
		))
	}
}
