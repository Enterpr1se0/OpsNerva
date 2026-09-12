package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

const maxSSHShellInputBytes = 64 << 10

func (s *Service) SendSSHShellInput(ctx context.Context, id, expectedSessionID, input, reason, actor string) error {
	if input == "" {
		return fmt.Errorf("input is required")
	}
	if len(input) > maxSSHShellInputBytes {
		return fmt.Errorf("shell input must contain 1-%d bytes", maxSSHShellInputBytes)
	}
	state, session, _, err := s.shells.running(id, expectedSessionID)
	if err != nil {
		return err
	}
	persistent := state.history.Persistent()
	if persistent && strings.ContainsRune(input, '\x00') {
		return fmt.Errorf("Agent shell input must not contain NUL characters")
	}
	if persistent && s.redactor.Redact(input) != input {
		return fmt.Errorf("shell input appears to contain a credential; the operator must use the private Web terminal input")
	}
	if persistent {
		s.setSSHShellOutputOwner(ctx, id, expectedSessionID, actor)
	}
	state.mu.Lock()
	secretPrompt := state.secretPrompt
	shell := state.shell
	state.mu.Unlock()
	if shell.Kind == domain.SSHShellKindSSH {
		if err := s.requireCurrentAgentSSHAccess(ctx, actor, shell.HostID, shell.Elevated); err != nil {
			return err
		}
	}
	if persistent && secretPrompt {
		return fmt.Errorf("the remote terminal is requesting a credential; wait for the operator to use the private Web terminal input")
	}
	if reason = strings.TrimSpace(reason); len(reason) > maxSSHShellReasonBytes {
		return fmt.Errorf("reason must not exceed %d bytes", maxSSHShellReasonBytes)
	}
	source := "operator"
	if actor == "eino-agent" || actor == "mcp-client" {
		source = "agent"
	}
	if persistent {
		s.appendSSHShellInputEvent(state, source, s.redactor.Redact(input), false, len(input))
	}
	if _, err := session.Write([]byte(input)); err != nil {
		return fmt.Errorf("write SSH shell input: %w", err)
	}
	if persistent {
		s.audit(context.WithoutCancel(ctx), shell.RunID, interactiveShellComponent(shell.Kind)+"_input", actor, map[string]any{
			"shell_id": shell.ID, "host_id": shell.HostID, "input": s.redactor.Redact(input),
			"source": source, "reason": s.redactor.Redact(reason),
		})
	}
	return nil
}

func (s *Service) WriteSensitiveSSHShellInput(ctx context.Context, id, input, actor string) error {
	if input == "" {
		return fmt.Errorf("sensitive input is required")
	}
	if len(input) > maxSSHShellInputBytes {
		return fmt.Errorf("sensitive shell input must contain 1-%d bytes", maxSSHShellInputBytes)
	}
	state, session, _, err := s.shells.running(id, "")
	if err != nil {
		return err
	}
	if !state.history.Persistent() {
		if _, err := session.Write([]byte(input)); err != nil {
			return fmt.Errorf("write terminal input: %w", err)
		}
		return nil
	}
	if strings.ContainsRune(input, '\x00') {
		return fmt.Errorf("sensitive Agent shell input must not contain NUL characters")
	}
	secret := strings.TrimRight(input, "\r\n")
	if secret != "" {
		state.eventMu.Lock()
		state.secrets = appendUniqueSecret(state.secrets, secret)
		state.eventMu.Unlock()
	}
	state.mu.Lock()
	shell := state.shell
	state.mu.Unlock()
	s.appendSSHShellInputEvent(state, "operator", "", true, len(input))
	if _, err := session.Write([]byte(input)); err != nil {
		return fmt.Errorf("write sensitive SSH shell input: %w", err)
	}
	s.audit(context.WithoutCancel(ctx), shell.RunID, interactiveShellComponent(shell.Kind)+"_sensitive_input", actor, map[string]any{
		"shell_id": shell.ID, "host_id": shell.HostID, "bytes": len(input), "source": "operator",
	})
	return nil
}

func (s *Service) ResizeSSHShell(ctx context.Context, id string, cols, rows int, actor string) (domain.SSHShell, error) {
	state, session, _, err := s.shells.running(id, "")
	if err != nil {
		return domain.SSHShell{}, err
	}
	if err := session.Resize(cols, rows); err != nil {
		return domain.SSHShell{}, err
	}
	state.lifecycleMu.Lock()
	defer state.lifecycleMu.Unlock()
	state.mu.Lock()
	if state.session != session || state.shell.Status != "running" {
		state.mu.Unlock()
		return domain.SSHShell{}, context.Canceled
	}
	state.shell.Cols, state.shell.Rows = cols, rows
	shell := state.shell
	state.mu.Unlock()
	history := state.history
	if err := history.Update(ctx, shell); err != nil {
		return domain.SSHShell{}, err
	}
	if history.Persistent() {
		s.audit(context.WithoutCancel(ctx), shell.RunID, interactiveShellComponent(shell.Kind)+"_resized", actor, map[string]any{
			"shell_id": shell.ID, "cols": cols, "rows": rows,
		})
	}
	return shell, nil
}

func (s *Service) InterruptSSHShell(ctx context.Context, id, expectedSessionID, reason, actor string) (domain.SSHShell, error) {
	state, session, _, err := s.shells.running(id, expectedSessionID)
	if err != nil {
		return domain.SSHShell{}, err
	}
	if reason = strings.TrimSpace(reason); len(reason) > maxSSHShellReasonBytes {
		return domain.SSHShell{}, fmt.Errorf("reason must not exceed %d bytes", maxSSHShellReasonBytes)
	}
	source := "operator"
	if actor == "eino-agent" || actor == "mcp-client" {
		source = "agent"
	}
	history := state.history
	if history.Persistent() {
		s.appendSSHShellInputEvent(state, source, "\x03", false, 1)
	}
	if err := session.Interrupt(); err != nil {
		return domain.SSHShell{}, err
	}
	state.mu.Lock()
	shell := state.shell
	state.mu.Unlock()
	if history.Persistent() {
		s.audit(context.WithoutCancel(ctx), shell.RunID, interactiveShellComponent(shell.Kind)+"_interrupted", actor, map[string]any{
			"shell_id": shell.ID, "input": "Ctrl+C", "reason": s.redactor.Redact(reason),
		})
	}
	return shell, nil
}
