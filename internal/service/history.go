package service

import (
	"context"
	"fmt"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/store"
)

type HistoryResult struct {
	Run       domain.Run `json:"run"`
	StdoutRaw string     `json:"stdout_raw,omitempty"`
	StderrRaw string     `json:"stderr_raw,omitempty"`
}

func (s *Service) GetRun(ctx context.Context, id string, includeRaw bool) (HistoryResult, error) {
	run, err := s.GetRunRecord(ctx, id)
	if err != nil {
		return HistoryResult{}, err
	}
	result := HistoryResult{Run: run}
	if includeRaw {
		stdout, err := s.encryptor.Decrypt(run.StdoutCipher)
		if err != nil {
			return HistoryResult{}, err
		}
		stderr, err := s.encryptor.Decrypt(run.StderrCipher)
		if err != nil {
			return HistoryResult{}, err
		}
		result.StdoutRaw = string(stdout)
		result.StderrRaw = string(stderr)
	}
	return result, nil
}

func (s *Service) GetRunRecord(ctx context.Context, id string) (domain.Run, error) {
	run, err := s.store.GetRun(ctx, id)
	if err != nil {
		return domain.Run{}, err
	}
	if sessionID := SessionIDFromContext(ctx); sessionID != "" && run.SessionID != sessionID {
		return domain.Run{}, store.ErrNotFound
	}
	return run, nil
}

func (s *Service) SearchRuns(ctx context.Context, query, hostID string, limit int) ([]domain.Run, error) {
	return s.store.SearchRuns(ctx, query, hostID, SessionIDFromContext(ctx), limit)
}

func (s *Service) SearchRunSummariesMatchingPage(ctx context.Context, filter domain.RunSearchFilter, matchMode domain.FileSearchMatchMode) (domain.RunSearchPage, error) {
	filter.SessionID = SessionIDFromContext(ctx)
	switch matchMode {
	case "", domain.FileSearchLiteral:
		return s.store.SearchRunSummariesFilteredPage(ctx, filter)
	case domain.FileSearchRegex:
		return s.store.SearchRunSummariesRegexFilteredPage(ctx, filter.Query, filter)
	default:
		return domain.RunSearchPage{}, fmt.Errorf("invalid history match_mode: use literal or regex")
	}
}
