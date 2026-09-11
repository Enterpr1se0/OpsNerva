package service

import (
	"context"
	"encoding/json"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func (s *Service) RecoverInterruptedTasks(ctx context.Context) error {
	if err := s.store.InterruptActiveTasks(ctx); err != nil {
		return err
	}
	if err := s.store.InterruptActiveRuns(ctx, "control plane restarted before the operation completed"); err != nil {
		return err
	}
	if err := s.store.InterruptActiveSSHShells(ctx); err != nil {
		return err
	}
	if err := s.store.AbortUnactivatedAgentApprovals(ctx, "control plane restarted before the Agent approval checkpoint became resumable"); err != nil {
		return err
	}
	if err := s.store.FailPendingChatMessages(ctx); err != nil {
		return err
	}
	if err := s.recoverChatToolCalls(ctx); err != nil {
		return err
	}
	if err := s.store.InterruptRunningMCPToolCalls(ctx); err != nil {
		return err
	}
	_, err := s.store.PruneChatTurnsExcludedFromContext(ctx, "")
	return err
}

func (s *Service) recoverChatToolCalls(ctx context.Context) error {
	calls, err := s.store.ListRunningChatToolCalls(ctx)
	if err != nil {
		return err
	}
	for _, call := range calls {
		status := domain.ChatToolCallUnknown
		content := ""
		if call.RunID != "" {
			run, runErr := s.store.GetRun(ctx, call.RunID)
			if runErr == nil && run.Status == "approval_required" {
				continue
			}
			if runErr == nil {
				if terminal := persistedToolExecutionStatus(run.Status); terminal != "" {
					status = terminal
					if encoded, marshalErr := json.Marshal(execResultFromRun(run, "", "")); marshalErr == nil {
						content = string(encoded)
					}
				}
			}
		}
		if _, err := s.store.FinishChatToolCall(ctx, call.SessionID, call.ToolCallID, call.RunID, status, content, ""); err != nil {
			return err
		}
	}
	return nil
}
