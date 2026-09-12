package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"
)

// UploadWorkspaceFileToHost transfers one allowlisted Workspace file directly
// to a registered host. The model provides only the Workspace-relative path;
// the absolute local path is resolved after approval and is never serialized.
func (s *Service) UploadWorkspaceFileToHost(ctx context.Context, hostID, workspaceID, relativePath, expectedSHA256, remotePath, reason, actor string) (domain.ExecResult, error) {
	return s.Submit(ctx, domain.ExecRequest{
		HostID: hostID, Mode: domain.ExecWorkspaceUpload, WorkspaceID: workspaceID, RelativePath: relativePath,
		ExpectedSHA256: strings.ToLower(strings.TrimSpace(expectedSHA256)), RemotePath: remotePath, Reason: reason,
	}, actor)
}

// DownloadHostFileToWorkspace copies one SHA256-bound remote file into a new
// path in the conversation-bound Workspace. The destination is resolved both
// before approval and immediately before the atomic local commit.
func (s *Service) DownloadHostFileToWorkspace(ctx context.Context, hostID, remotePath, expectedSHA256, workspaceID, relativePath string, timeoutSeconds int, reason, actor string) (domain.ExecResult, error) {
	workspace, ok := s.workspaceByID(workspaceID)
	if !ok {
		return domain.ExecResult{}, fmt.Errorf("workspace %q not found", workspaceID)
	}
	if err := validateRemoteFilePath(remotePath); err != nil {
		return domain.ExecResult{}, err
	}
	expectedSHA256 = strings.ToLower(strings.TrimSpace(expectedSHA256))
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(expectedSHA256) {
		return domain.ExecResult{}, fmt.Errorf("workspace download requires expected_sha256 from ssh_file_read")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return domain.ExecResult{}, fmt.Errorf("reason is required")
	}
	if timeoutSeconds < 0 || timeoutSeconds > 600 {
		return domain.ExecResult{}, fmt.Errorf("workspace download timeout_seconds must be between 1 and 600 when provided")
	}
	relativePath, err := s.validateWorkspaceFileDestination(workspace, relativePath, filepath.Base(remotePath))
	if err != nil {
		return domain.ExecResult{}, err
	}
	return s.Submit(ctx, domain.ExecRequest{
		HostID: hostID, Mode: domain.ExecWorkspaceDownload, WorkspaceID: workspaceID,
		RemotePath: remotePath, RelativePath: relativePath, ExpectedSHA256: expectedSHA256,
		TimeoutSeconds: timeoutSeconds, Reason: reason,
	}, actor)
}

func (s *Service) executeWorkspaceDownload(ctx context.Context, connection sshx.ConnectionSpec, req domain.ExecRequest, run domain.Run, actor string) (sshx.RawResult, error) {
	started := time.Now()
	transport, ok := s.transport.(sshx.SFTPTransport)
	if !ok {
		return sshx.RawResult{ExitCode: -1, Duration: time.Since(started)}, fmt.Errorf("configured SSH transport does not support SFTP")
	}
	workspace, ok := s.workspaceByID(req.WorkspaceID)
	if !ok {
		return sshx.RawResult{ExitCode: -1, Duration: time.Since(started)}, fmt.Errorf("workspace %q not found", req.WorkspaceID)
	}
	if _, err := s.validateWorkspaceFileDestination(workspace, req.RelativePath, filepath.Base(req.RemotePath)); err != nil {
		return sshx.RawResult{ExitCode: -1, Duration: time.Since(started)}, err
	}
	timeout := req.TimeoutSeconds
	if timeout <= 0 {
		timeout = s.limits.SyncTimeoutSeconds
	}
	if timeout <= 0 {
		timeout = 60
	}
	downloadCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	download, err := transport.OpenSFTPFile(downloadCtx, connection, req.RemotePath)
	if err != nil {
		return sshx.RawResult{ExitCode: -1, Duration: time.Since(started)}, err
	}
	defer download.Reader.Close()
	if download.Entry.Type != "file" {
		return sshx.RawResult{ExitCode: -1, Duration: time.Since(started)}, fmt.Errorf("remote download source is not a regular file")
	}
	stored, err := s.storeWorkspaceFile(downloadCtx, workspace, req.RelativePath, filepath.Base(req.RemotePath), download.Reader, req.ExpectedSHA256, "workspace_file_downloaded", actor, download.Entry.Size, s.executionTransferReporter(run))
	if err != nil {
		return sshx.RawResult{ExitCode: -1, Duration: time.Since(started)}, err
	}
	output, err := json.Marshal(stored)
	return sshx.RawResult{ExitCode: 0, Stdout: output, Duration: time.Since(started)}, err
}

func (s *Service) prepareWorkspaceUpload(req domain.ExecRequest) (domain.ExecRequest, error) {
	workspace, ok := s.workspaceByID(req.WorkspaceID)
	if !ok {
		return req, fmt.Errorf("workspace %q not found", req.WorkspaceID)
	}
	expected := strings.ToLower(strings.TrimSpace(req.ExpectedSHA256))
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(expected) {
		return req, fmt.Errorf("workspace upload requires the expected_sha256 returned by workspace_file_read")
	}
	path, err := resolveWorkspacePath(workspace, strings.TrimSpace(req.RelativePath), false)
	if err != nil {
		return req, err
	}
	file, err := os.Open(path)
	if err != nil {
		return req, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return req, fmt.Errorf("workspace upload source is not a regular file")
	}
	digest := sha256.New()
	_, copyErr := io.Copy(digest, file)
	closeErr := file.Close()
	if copyErr != nil {
		return req, copyErr
	}
	if closeErr != nil {
		return req, closeErr
	}
	actual := hex.EncodeToString(digest.Sum(nil))
	if actual != expected {
		return req, fmt.Errorf("workspace upload source version conflict: expected SHA256 %s, got %s", expected, actual)
	}
	req.ExpectedSHA256 = expected
	req.LocalPath = path
	return req, nil
}
