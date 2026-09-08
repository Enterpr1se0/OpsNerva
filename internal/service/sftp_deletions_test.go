package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/sshx"
	"github.com/Enterpr1se0/opsnerva/internal/store"
)

type deletionTransport struct {
	*workspaceDownloadTransport
	remove func(context.Context, func(sshx.SFTPDeleteProgress)) error
}

func (transport *deletionTransport) RemoveSFTPEntry(ctx context.Context, _ sshx.ConnectionSpec, target string, _ bool, report func(sshx.SFTPDeleteProgress)) (sshx.SFTPFileEntry, error) {
	err := transport.remove(ctx, report)
	return sshx.SFTPFileEntry{Path: target, Type: "directory"}, err
}

func awaitSFTPDeletion(t *testing.T, events <-chan StateEvent, id, status string) SFTPDeletion {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			if event.SFTPDeletion != nil && event.SFTPDeletion.ID == id && event.SFTPDeletion.Status == status {
				return *event.SFTPDeletion
			}
		case <-timer.C:
			t.Fatalf("timed out awaiting %s: %s", id, status)
		}
	}
}

func TestSFTPDeletionSurvivesRequestCancellationAndPublishesResult(t *testing.T) {
	svc, fake, host := newTestService(t)
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	jobContext := make(chan context.Context, 1)
	release := make(chan struct{})
	svc.transport = &deletionTransport{workspaceDownloadTransport: &workspaceDownloadTransport{fakeTransport: fake}, remove: func(ctx context.Context, report func(sshx.SFTPDeleteProgress)) error {
		jobContext <- ctx
		report(sshx.SFTPDeleteProgress{Discovered: 5, Removed: 2})
		select {
		case <-release:
			report(sshx.SFTPDeleteProgress{Discovered: 5, Removed: 5, RootRemoved: true, ScanComplete: true})
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	before, err := svc.store.ListAudit(context.Background(), "", 100)
	if err != nil {
		t.Fatal(err)
	}
	events, _, unsubscribe := svc.SubscribeStateEvents()
	defer unsubscribe()
	initial, err := svc.StartSFTPDeletion(requestCtx, host.ID, "/tree", true)
	if err != nil {
		t.Fatal(err)
	}
	var ctx context.Context
	select {
	case ctx = <-jobContext:
	case <-time.After(3 * time.Second):
		t.Fatal("task did not start")
	}
	cancelRequest()
	if ctx.Err() != nil {
		t.Fatalf("HTTP cancellation cancelled deletion: %v", ctx.Err())
	}
	if _, err := svc.StartSFTPDeletion(context.Background(), host.ID, "/other", true); !errors.Is(err, ErrSFTPDeletionBusy) {
		t.Fatalf("same-host deletion accepted: %v", err)
	}
	close(release)
	final := awaitSFTPDeletion(t, events, initial.ID, "completed")
	if final.Revision <= initial.Revision || final.Progress.Removed != 5 || !final.Progress.RootRemoved {
		t.Fatalf("final result = %+v", final)
	}
	jobs := svc.ListSFTPDeletions()
	if len(jobs) != 1 || jobs[0] != final {
		t.Fatalf("snapshot = %+v", jobs)
	}
	after, err := svc.store.ListAudit(context.Background(), "", 100)
	if err != nil || len(before) != len(after) {
		t.Fatalf("operator deletion wrote audit: %d -> %d, %v", len(before), len(after), err)
	}
	runs, err := svc.store.SearchRuns(context.Background(), "", "", "", 100)
	if err != nil || len(runs) != 0 {
		t.Fatalf("operator deletion created Agent runs: %+v, %v", runs, err)
	}
}

func TestSFTPDeletionCancellationAndShutdown(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprintf("shutdown=%v", shutdown), func(t *testing.T) {
			svc, fake, host := newTestService(t)
			reported := make(chan struct{})
			svc.transport = &deletionTransport{workspaceDownloadTransport: &workspaceDownloadTransport{fakeTransport: fake}, remove: func(ctx context.Context, report func(sshx.SFTPDeleteProgress)) error {
				report(sshx.SFTPDeleteProgress{Discovered: 10, Removed: 3})
				close(reported)
				<-ctx.Done()
				return ctx.Err()
			}}
			events, _, unsubscribe := svc.SubscribeStateEvents()
			defer unsubscribe()
			job, err := svc.StartSFTPDeletion(context.Background(), host.ID, "/tree", true)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-reported:
			case <-time.After(3 * time.Second):
				t.Fatal("no progress")
			}
			if shutdown {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if err := svc.Shutdown(ctx); err != nil {
					t.Fatal(err)
				}
				if _, err := svc.StartSFTPDeletion(context.Background(), host.ID, "/another", true); err == nil {
					t.Fatal("shutdown accepted task")
				}
			} else {
				stopping, err := svc.CancelSFTPDeletion(job.ID)
				if err != nil || stopping.Status != "stopping" {
					t.Fatalf("cancel = %+v, %v", stopping, err)
				}
			}
			final := awaitSFTPDeletion(t, events, job.ID, "cancelled")
			if final.Progress.Removed != 3 || final.Error != "" {
				t.Fatalf("partial progress lost on cancel: %+v", final)
			}
			if repeated, err := svc.CancelSFTPDeletion(job.ID); err != nil || repeated != final {
				t.Fatalf("terminal cancellation = %+v, %v", repeated, err)
			}
			if _, err := svc.CancelSFTPDeletion("missing"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("missing cancellation = %v", err)
			}
		})
	}
}

func TestSFTPDeletionLimitsAndPartialFailure(t *testing.T) {
	svc, fake, host := newTestService(t)
	svc.transport = &deletionTransport{workspaceDownloadTransport: &workspaceDownloadTransport{fakeTransport: fake}, remove: func(_ context.Context, report func(sshx.SFTPDeleteProgress)) error {
		report(sshx.SFTPDeleteProgress{Discovered: 5, Removed: 2, Failed: 1, Skipped: 2, FirstError: "/tree/locked: permission denied", ScanComplete: true})
		return errors.New("delete incomplete")
	}}
	svc.sftpDeletions = make(map[string]*sftpDeletionState)
	for index := range sftpDeletionActiveLimit {
		id := fmt.Sprintf("active-%d", index)
		svc.sftpDeletions[id] = &sftpDeletionState{job: SFTPDeletion{HostID: id, Status: "running"}}
	}
	if _, err := svc.StartSFTPDeletion(context.Background(), host.ID, "/tree", true); !errors.Is(err, ErrSFTPDeletionLimit) {
		t.Fatalf("active limit = %v", err)
	}
	clear(svc.sftpDeletions)
	for index := range sftpDeletionHistoryLimit {
		id := fmt.Sprintf("ended-%d", index)
		svc.sftpDeletions[id] = &sftpDeletionState{job: SFTPDeletion{HostID: id, Status: "completed", UpdatedAt: time.Unix(int64(index), 0)}}
	}
	events, _, unsubscribe := svc.SubscribeStateEvents()
	defer unsubscribe()
	job, err := svc.StartSFTPDeletion(context.Background(), host.ID, "/tree", true)
	if err != nil {
		t.Fatal(err)
	}
	final := awaitSFTPDeletion(t, events, job.ID, "failed")
	if final.Progress.Removed != 2 || final.Progress.Failed != 1 || final.Progress.FirstError == "" || final.Error == "" {
		t.Fatalf("partial failure = %+v", final)
	}
	if len(svc.ListSFTPDeletions()) != sftpDeletionHistoryLimit {
		t.Fatal("history grew past limit")
	}
	svc.sftpDeletionMu.Lock()
	_, retained := svc.sftpDeletions["ended-0"]
	svc.sftpDeletionMu.Unlock()
	if retained {
		t.Fatal("oldest terminal result was not evicted")
	}
}
