package service

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/Enterpr1se0/opsnerva/internal/sshx"
)

func (s *Service) ListOperatorSFTPFiles(ctx context.Context, hostID, remotePath string) (sshx.SFTPFileList, error) {
	transport, connection, err := s.operatorSFTP(ctx, hostID)
	if err != nil {
		return sshx.SFTPFileList{}, err
	}
	return transport.ListSFTPFiles(ctx, connection, remotePath)
}

func (s *Service) OpenOperatorSFTPFile(ctx context.Context, hostID, remotePath string) (sshx.SFTPDownload, error) {
	transport, connection, err := s.operatorSFTP(ctx, hostID)
	if err != nil {
		return sshx.SFTPDownload{}, err
	}
	return transport.OpenSFTPFile(ctx, connection, remotePath)
}

func (s *Service) UploadOperatorSFTPFile(ctx context.Context, hostID, remotePath string, source io.Reader, overwrite bool) (sshx.SFTPMutationResult, error) {
	transport, connection, err := s.operatorSFTP(ctx, hostID)
	if err != nil {
		return sshx.SFTPMutationResult{}, err
	}
	entry, err := transport.UploadSFTPFile(ctx, connection, remotePath, source, overwrite)
	return sshx.SFTPMutationResult{HostID: connection.Target.ID, Entry: entry}, err
}

func (s *Service) CreateOperatorSFTPDirectory(ctx context.Context, hostID, remotePath string) (sshx.SFTPMutationResult, error) {
	transport, connection, err := s.operatorSFTP(ctx, hostID)
	if err != nil {
		return sshx.SFTPMutationResult{}, err
	}
	entry, err := transport.CreateSFTPDirectory(ctx, connection, remotePath)
	return sshx.SFTPMutationResult{HostID: connection.Target.ID, Entry: entry}, err
}

func (s *Service) RenameOperatorSFTPEntry(ctx context.Context, hostID, sourcePath, destinationPath string) (sshx.SFTPMutationResult, error) {
	transport, connection, err := s.operatorSFTP(ctx, hostID)
	if err != nil {
		return sshx.SFTPMutationResult{}, err
	}
	entry, err := transport.RenameSFTPEntry(ctx, connection, sourcePath, destinationPath)
	return sshx.SFTPMutationResult{HostID: connection.Target.ID, Entry: entry}, err
}

func (s *Service) RemoveOperatorSFTPEntry(ctx context.Context, hostID, remotePath string, recursive bool) (sshx.SFTPMutationResult, error) {
	transport, connection, err := s.operatorSFTP(ctx, hostID)
	if err != nil {
		return sshx.SFTPMutationResult{}, err
	}
	entry, err := transport.RemoveSFTPEntry(ctx, connection, remotePath, recursive)
	return sshx.SFTPMutationResult{HostID: connection.Target.ID, Entry: entry}, err
}

func (s *Service) operatorSFTP(ctx context.Context, hostID string) (sshx.SFTPTransport, sshx.ConnectionSpec, error) {
	hostID = strings.TrimSpace(hostID)
	if hostID == "" {
		return nil, sshx.ConnectionSpec{}, fmt.Errorf("host_id is required")
	}
	transport, ok := s.transport.(sshx.SFTPTransport)
	if !ok {
		return nil, sshx.ConnectionSpec{}, fmt.Errorf("configured SSH transport does not support SFTP")
	}
	host, err := s.store.GetHost(ctx, hostID)
	if err != nil {
		return nil, sshx.ConnectionSpec{}, err
	}
	connection, _, err := s.resolveSSHConnection(ctx, host)
	if err != nil {
		return nil, sshx.ConnectionSpec{}, err
	}
	connection, err = s.hydrateSSHConnection(connection, false)
	if err != nil {
		return nil, sshx.ConnectionSpec{}, err
	}
	return transport, connection, nil
}
