package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestWorkspaceDownloadUsesVersionBoundAtomicDestination(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	const sessionID = "workspace-download-progress"
	events, unsubscribe := svc.SubscribeExecutionEvents(sessionID)
	defer unsubscribe()
	executionCtx := WithExecutionOwner(WithSessionID(context.Background(), sessionID), "call-workspace-download", "workspace_file_download", `{}`)
	content := []byte("downloaded over SFTP\n")
	remotePath := "/tmp/report.txt"
	transport := &workspaceDownloadTransport{fakeTransport: &fakeTransport{}, content: content, remotePath: remotePath}
	svc.transport = transport
	host, err := svc.SaveHost(context.Background(), domain.HostInput{
		Name: "source", Address: "192.0.2.50", Port: 22, User: "ops", AuthType: "agent", SudoMode: "none",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(content))
	pending, err := svc.DownloadHostFileToWorkspace(context.Background(), host.ID, remotePath, digest, "project", "downloads/report.txt", 30, "download the reviewed report", "eino-agent")
	if err == nil || !strings.Contains(err.Error(), "parent directory") {
		t.Fatalf("download accepted a missing Workspace parent: result=%#v err=%v", pending, err)
	}
	if err := os.Mkdir(filepath.Join(root, "downloads"), 0o755); err != nil {
		t.Fatal(err)
	}
	pending, err = svc.DownloadHostFileToWorkspace(executionCtx, host.ID, remotePath, digest, "project", "downloads/report.txt", 30, "download the reviewed report", "eino-agent")
	if err != nil || pending.Status != "approval_required" {
		t.Fatalf("Workspace download did not require approval: result=%#v err=%v", pending, err)
	}
	approval, err := svc.Store().GetApproval(context.Background(), pending.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(approval.RequestJSON, root) || !strings.Contains(approval.RequestJSON, digest) || !strings.Contains(approval.RequestJSON, `"mode":"workspace_download"`) {
		t.Fatalf("download approval did not bind safe paths and SHA256: %s", approval.RequestJSON)
	}
	approved, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed source and destination", "operator")
	if err != nil || approved.Status != "completed" || approved.File == nil || approved.File.SHA256 != digest {
		t.Fatalf("approved Workspace download failed: result=%#v err=%v", approved, err)
	}
	expectWorkspaceTransferProgress(t, events, pending.RunID, int64(len(content)))
	stored, err := os.ReadFile(filepath.Join(root, "downloads", "report.txt"))
	if err != nil || !bytes.Equal(stored, content) {
		t.Fatalf("downloaded content=%q err=%v", stored, err)
	}
	if _, err := svc.DownloadHostFileToWorkspace(context.Background(), host.ID, remotePath, digest, "project", "downloads/report.txt", 30, "avoid overwrite", "eino-agent"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("Workspace download overwrote an existing file: %v", err)
	}
}

func TestWorkspaceDownloadRechecksAccessDestinationAndSourceAfterApproval(t *testing.T) {
	for _, change := range []string{"access", "destination", "source"} {
		t.Run(change, func(t *testing.T) {
			svc, root := newWorkspaceService(t, "read_write")
			ctx := context.Background()
			content := []byte("original\n")
			transport := &workspaceDownloadTransport{fakeTransport: &fakeTransport{}, content: content, remotePath: "/tmp/report.txt"}
			svc.transport = transport
			host, err := svc.SaveHost(ctx, domain.HostInput{Name: "source", Address: "192.0.2.50", Port: 22, User: "ops", AuthType: "agent", SudoMode: "none"}, "test")
			if err != nil {
				t.Fatal(err)
			}
			digest := fmt.Sprintf("%x", sha256.Sum256(content))
			pending, err := svc.DownloadHostFileToWorkspace(ctx, host.ID, transport.remotePath, digest, "project", "report.txt", 30, "download reviewed version", "eino-agent")
			if err != nil || pending.Status != "approval_required" {
				t.Fatalf("download skipped approval: %+v err=%v", pending, err)
			}
			target := filepath.Join(root, "report.txt")
			wantError := ""
			switch change {
			case "access":
				wantError = "read_only"
				if _, err := svc.UpdateAdminWorkspace(ctx, "project", domain.WorkspaceInput{Access: "read_only"}, "operator"); err != nil {
					t.Fatal(err)
				}
			case "destination":
				wantError = "already exists"
				if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "source":
				wantError = "remote download source version conflict"
				transport.content = []byte("changed\n")
			}
			result, err := svc.Approve(ctx, pending.ApprovalID, "reviewed", "operator")
			if err == nil || result.Status != "failed" || !strings.Contains(err.Error(), wantError) {
				t.Fatalf("stale download was accepted: %+v err=%v", result, err)
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			if change == "destination" {
				stored, err := os.ReadFile(target)
				if err != nil || string(stored) != "keep" || len(entries) != 1 {
					t.Fatalf("conflict changed target or leaked staging: %q %v err=%v", stored, entries, err)
				}
			} else if len(entries) != 0 {
				t.Fatalf("failed download left files: %v", entries)
			}
			events, err := svc.ListAudit(ctx, "", 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event.Type == "workspace_file_downloaded" {
					t.Fatalf("failed download wrote success audit: %+v", event)
				}
			}
		})
	}
}

func TestWorkspaceUploadRejectsReadOnlyWorkspace(t *testing.T) {
	svc, _ := newWorkspaceService(t, "read_only")
	if _, err := svc.UploadWorkspaceFile(context.Background(), "project", "file.txt", "file.txt", bytes.NewBufferString("x"), "admin-web"); err == nil || !strings.Contains(err.Error(), "read_only") {
		t.Fatalf("read-only Workspace accepted upload: %v", err)
	}
	if _, err := svc.DeleteAdminWorkspaceEntry(context.Background(), "project", "file.txt", "admin-web"); err == nil || !strings.Contains(err.Error(), "read_only") {
		t.Fatalf("read-only Workspace accepted delete: %v", err)
	}
}

func TestWorkspaceDirectUploadUsesOneVersionBoundApproval(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_only")
	const sessionID = "workspace-upload-progress"
	events, unsubscribe := svc.SubscribeExecutionEvents(sessionID)
	defer unsubscribe()
	executionCtx := WithExecutionOwner(WithSessionID(context.Background(), sessionID), "call-workspace-upload", "workspace_file_upload", `{}`)
	transport := &fakeTransport{}
	svc.transport = transport
	host, err := svc.SaveHost(context.Background(), domain.HostInput{
		Name: "destination", Address: "192.0.2.40", Port: 22, User: "ops", AuthType: "agent", SudoMode: "none",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("version: 1\n")
	localPath := filepath.Join(root, "deploy.yaml")
	if err := os.WriteFile(localPath, content, 0o640); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(content))
	pending, err := svc.UploadWorkspaceFileToHost(executionCtx, host.ID, "project", "deploy.yaml", digest, "/tmp/deploy.yaml", "deploy exact fixture", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != "approval_required" {
		t.Fatalf("direct Workspace upload bypassed one approval: %#v", pending)
	}
	approval, err := svc.Store().GetApproval(context.Background(), pending.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(approval.RequestJSON, root) || !strings.Contains(approval.RequestJSON, digest) || !strings.Contains(approval.RequestJSON, `"mode":"workspace_upload"`) {
		t.Fatalf("approval did not bind the safe source version without exposing its root: %s", approval.RequestJSON)
	}
	approved, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed exact source and destination", "operator")
	if err != nil || approved.Status != "completed" {
		t.Fatalf("approved direct upload failed: result=%#v err=%v", approved, err)
	}
	expectWorkspaceTransferProgress(t, events, pending.RunID, 12)
	if len(transport.calls) != 1 || transport.calls[0].Mode != domain.ExecWorkspaceUpload || !sameWorkspaceFile(transport.calls[0].LocalPath, localPath) || transport.calls[0].ExpectedSHA256 != digest {
		t.Fatalf("transport did not receive the resolved version-bound source: %#v", transport.calls)
	}

	stale, err := svc.UploadWorkspaceFileToHost(context.Background(), host.ID, "project", "deploy.yaml", digest, "/tmp/deploy-2.yaml", "detect source change", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(localPath, []byte("version: 2\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Approve(context.Background(), stale.ApprovalID, "reviewed before source changed", "operator"); err == nil || !strings.Contains(err.Error(), "version conflict") {
		t.Fatalf("changed Workspace source was uploaded after approval: %v", err)
	}
	if len(transport.calls) != 1 {
		t.Fatalf("version-conflicted source reached transport: %#v", transport.calls)
	}
}

func expectWorkspaceTransferProgress(t *testing.T, events <-chan ExecutionEvent, runID string, total int64) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case event := <-events:
			if event.RunID == runID && event.Stream == "progress" && event.TransferredBytes == total && event.TotalBytes == total {
				return
			}
		case <-deadline:
			t.Fatalf("Workspace transfer %q did not publish completed byte progress", runID)
		}
	}
}
