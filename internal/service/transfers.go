package service

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"
)

var transferSHA256Pattern = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)

func (s *Service) TransferFileBetweenHosts(ctx context.Context, sourceHostID, sourcePath, expectedSHA256, destinationHostID, destinationPath, expectedDestinationSHA256 string, timeoutSeconds int, reason, actor string) (domain.ExecResult, error) {
	return s.Submit(ctx, domain.ExecRequest{
		HostID: destinationHostID, Mode: domain.ExecSSHFileTransfer,
		SourceHostID: sourceHostID, SourcePath: sourcePath, ExpectedSHA256: expectedSHA256,
		RemotePath: destinationPath, ExpectedDestinationSHA256: expectedDestinationSHA256,
		TimeoutSeconds: timeoutSeconds, Reason: reason,
	}, actor)
}

func (s *Service) bindSSHFileTransfer(ctx context.Context, destination domain.Host, req *domain.ExecRequest, actor string) (domain.Host, error) {
	if err := validateSSHFileTransferRequest(*req); err != nil {
		return domain.Host{}, err
	}
	source, err := s.store.GetHost(ctx, req.SourceHostID)
	if err != nil {
		return domain.Host{}, fmt.Errorf("load source SSH host: %w", err)
	}
	destinationConnection, destinationDigest, err := s.resolveSSHConnection(ctx, destination)
	if err != nil {
		return domain.Host{}, fmt.Errorf("resolve destination SSH connection: %w", err)
	}
	if err := requireAgentSSHAccess(actor, destinationConnection); err != nil {
		return domain.Host{}, err
	}
	sourceConnection, sourceDigest, err := s.resolveSSHConnection(ctx, source)
	if err != nil {
		return domain.Host{}, fmt.Errorf("resolve source SSH connection: %w", err)
	}
	if err := requireAgentSSHAccess(actor, sourceConnection); err != nil {
		return domain.Host{}, err
	}
	bindSSHRequest(req, destinationDigest)
	bindSSHTransferSource(req, sourceDigest)
	return source, nil
}

func validateSSHFileTransferRequest(req domain.ExecRequest) error {
	if strings.TrimSpace(req.SourceHostID) == "" || strings.TrimSpace(req.HostID) == "" {
		return fmt.Errorf("source_host_id and destination_host_id are required")
	}
	if req.SourceHostID == req.HostID {
		return fmt.Errorf("source and destination SSH hosts must be different")
	}
	if !cleanAbsoluteRemotePath(req.SourcePath) {
		return fmt.Errorf("source_path must be a clean absolute path")
	}
	if !cleanAbsoluteRemotePath(req.RemotePath) {
		return fmt.Errorf("destination_path must be a clean absolute path")
	}
	if !transferSHA256Pattern.MatchString(req.ExpectedSHA256) {
		return fmt.Errorf("expected_sha256 must be the 64-character SHA256 returned for the source file")
	}
	if req.ExpectedDestinationSHA256 != "" && !transferSHA256Pattern.MatchString(req.ExpectedDestinationSHA256) {
		return fmt.Errorf("expected_destination_sha256 must be a 64-character SHA256 when replacing an existing destination")
	}
	return nil
}

func cleanAbsoluteRemotePath(value string) bool {
	return path.IsAbs(value) && path.Clean(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

func (s *Service) executeSSHFileTransfer(ctx context.Context, run domain.Run, req domain.ExecRequest) (sshx.RawResult, error) {
	destinationHost, err := s.store.GetHost(ctx, req.HostID)
	if err != nil {
		return sshx.RawResult{}, fmt.Errorf("reload destination SSH host: %w", err)
	}
	destination, destinationDigest, err := s.resolveSSHConnection(ctx, destinationHost)
	if err != nil {
		return sshx.RawResult{}, fmt.Errorf("resolve destination SSH connection: %w", err)
	}
	if err := verifySSHRequestBinding(req, destinationDigest); err != nil {
		return sshx.RawResult{}, err
	}
	destination, err = s.hydrateSSHConnection(destination, false)
	if err != nil {
		return sshx.RawResult{}, fmt.Errorf("prepare destination SSH credentials: %w", err)
	}

	sourceHost, err := s.store.GetHost(ctx, req.SourceHostID)
	if err != nil {
		return sshx.RawResult{}, fmt.Errorf("reload source SSH host: %w", err)
	}
	source, sourceDigest, err := s.resolveSSHConnection(ctx, sourceHost)
	if err != nil {
		return sshx.RawResult{}, fmt.Errorf("resolve source SSH connection: %w", err)
	}
	if err := verifySSHTransferSourceBinding(req, sourceDigest); err != nil {
		return sshx.RawResult{}, err
	}
	source, err = s.hydrateSSHConnection(source, false)
	if err != nil {
		return sshx.RawResult{}, fmt.Errorf("prepare source SSH credentials: %w", err)
	}

	transport, ok := s.transport.(sshx.HostFileTransferTransport)
	if !ok {
		return sshx.RawResult{}, fmt.Errorf("configured SSH transport does not support host-to-host file transfer")
	}
	return transport.TransferFile(ctx, source, destination, req, func(transferredBytes, totalBytes int64) {
		s.publishExecutionEvent(ExecutionEvent{
			SessionID: run.SessionID, RunID: run.ID, Stream: "progress", Status: "running",
			TransferredBytes: transferredBytes, TotalBytes: totalBytes,
		})
	})
}
