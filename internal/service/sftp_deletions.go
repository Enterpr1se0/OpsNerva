package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/ids"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"
	"github.com/Enterpr1se0/opsnerva/internal/store"
)

const sftpDeletionActiveLimit = 8
const sftpDeletionHistoryLimit = 64

var ErrSFTPDeletionBusy = errors.New("a deletion is already running on this host")
var ErrSFTPDeletionLimit = errors.New("too many active SFTP deletions")

type SFTPDeletion struct {
	ID        string                  `json:"id"`
	HostID    string                  `json:"host_id"`
	Path      string                  `json:"path"`
	Status    string                  `json:"status"`
	Revision  uint64                  `json:"revision"`
	Progress  sshx.SFTPDeleteProgress `json:"progress"`
	Error     string                  `json:"error,omitempty"`
	UpdatedAt time.Time               `json:"updated_at"`
}

func (job SFTPDeletion) active() bool { return job.Status == "running" || job.Status == "stopping" }

type sftpDeletionState struct {
	job    SFTPDeletion
	cancel context.CancelFunc
}

// Operator deletions are transient service resources, not Agent tasks or audit
// runs. Each host retains its latest result, bounded across all hosts.
func (s *Service) StartSFTPDeletion(ctx context.Context, hostID, remotePath string, recursive bool) (SFTPDeletion, error) {
	if err := sshx.ValidateSFTPDeletePath(remotePath); err != nil {
		return SFTPDeletion{}, err
	}
	transport, connection, err := s.operatorSFTP(ctx, hostID)
	if err != nil {
		return SFTPDeletion{}, err
	}
	s.executionMu.Lock()
	defer s.executionMu.Unlock()
	if s.executionClosed {
		return SFTPDeletion{}, fmt.Errorf("service is shutting down")
	}
	if err := ctx.Err(); err != nil {
		return SFTPDeletion{}, err
	}
	s.sftpDeletionMu.Lock()
	defer s.sftpDeletionMu.Unlock()
	hostID = connection.Target.ID
	if current := s.sftpDeletions[hostID]; current != nil && current.job.active() {
		return SFTPDeletion{}, ErrSFTPDeletionBusy
	}
	active := 0
	var oldest *sftpDeletionState
	for _, state := range s.sftpDeletions {
		if state.job.active() {
			active++
		} else if oldest == nil || state.job.UpdatedAt.Before(oldest.job.UpdatedAt) {
			oldest = state
		}
	}
	if active >= sftpDeletionActiveLimit {
		return SFTPDeletion{}, ErrSFTPDeletionLimit
	}
	if len(s.sftpDeletions) >= sftpDeletionHistoryLimit && s.sftpDeletions[hostID] == nil && oldest != nil {
		delete(s.sftpDeletions, oldest.job.HostID)
	}
	jobCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stopShutdown := context.AfterFunc(s.executionCtx, cancel)
	state := &sftpDeletionState{job: SFTPDeletion{ID: ids.New("sftp_delete"), HostID: hostID, Path: remotePath, Status: "running"}, cancel: cancel}
	if s.sftpDeletions == nil {
		s.sftpDeletions = make(map[string]*sftpDeletionState)
	}
	s.sftpDeletions[hostID] = state
	s.publishSFTPDeletionLocked(state)
	initial := state.job
	s.executionWG.Add(1)
	go func() {
		defer s.executionWG.Done()
		defer stopShutdown()
		defer cancel()
		_, err := transport.RemoveSFTPEntry(jobCtx, connection, remotePath, recursive, func(progress sshx.SFTPDeleteProgress) {
			s.sftpDeletionMu.Lock()
			state.job.Progress = progress
			s.publishSFTPDeletionLocked(state)
			s.sftpDeletionMu.Unlock()
		})
		s.sftpDeletionMu.Lock()
		defer s.sftpDeletionMu.Unlock()
		switch {
		case err == nil:
			state.job.Status = "completed"
		case jobCtx.Err() != nil:
			state.job.Status = "cancelled"
		default:
			state.job.Status = "failed"
			state.job.Error = err.Error()
		}
		state.cancel = nil
		s.publishSFTPDeletionLocked(state)
	}()
	return initial, nil
}

func (s *Service) publishSFTPDeletionLocked(state *sftpDeletionState) {
	s.sftpDeletionSequence++
	state.job.Revision = s.sftpDeletionSequence
	state.job.UpdatedAt = time.Now().UTC()
	snapshot := state.job
	s.publishStateEvent(StateEvent{Topic: StateTopicSFTPDeletions, SFTPDeletion: &snapshot})
}

func (s *Service) ListSFTPDeletions() []SFTPDeletion {
	s.sftpDeletionMu.Lock()
	defer s.sftpDeletionMu.Unlock()
	jobs := make([]SFTPDeletion, 0, len(s.sftpDeletions))
	for _, state := range s.sftpDeletions {
		jobs = append(jobs, state.job)
	}
	return jobs
}

func (s *Service) CancelSFTPDeletion(id string) (SFTPDeletion, error) {
	s.sftpDeletionMu.Lock()
	defer s.sftpDeletionMu.Unlock()
	for _, state := range s.sftpDeletions {
		if state.job.ID != id {
			continue
		}
		if state.job.Status == "running" {
			state.job.Status = "stopping"
			state.cancel()
			s.publishSFTPDeletionLocked(state)
		}
		return state.job, nil
	}
	return SFTPDeletion{}, store.ErrNotFound
}
