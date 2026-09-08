package sshx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/pkg/sftp"
)

const sftpUploadCleanupTimeout = 5 * time.Second

func (t *NativeSSHTransport) OpenSFTPFile(ctx context.Context, connection ConnectionSpec, remotePath string) (SFTPDownload, error) {
	remotePath, err := cleanSFTPPath(remotePath)
	if err != nil {
		return SFTPDownload{}, err
	}
	lease, err := t.openSFTP(ctx, connection)
	if err != nil {
		return SFTPDownload{}, err
	}
	sftpClient := lease.client
	info, err := sftpClient.Lstat(remotePath)
	if err != nil {
		_ = lease.Close()
		return SFTPDownload{}, fmt.Errorf("inspect remote file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		_ = lease.Close()
		return SFTPDownload{}, fmt.Errorf("remote path is not a regular non-symbolic file")
	}
	file, err := sftpClient.Open(remotePath)
	if err != nil {
		_ = lease.Close()
		return SFTPDownload{}, fmt.Errorf("open remote file: %w", err)
	}
	reader := &sftpDownloadReader{file: file, lease: lease}
	return SFTPDownload{Entry: sftpFileEntry(remotePath, info), Reader: reader}, nil
}

func (t *NativeSSHTransport) UploadSFTPFile(ctx context.Context, connection ConnectionSpec, remotePath string, source io.Reader, overwrite bool) (_ SFTPFileEntry, resultErr error) {
	remotePath, err := cleanSFTPPath(remotePath)
	if err != nil {
		return SFTPFileEntry{}, err
	}
	lease, err := t.openSFTP(ctx, connection)
	if err != nil {
		return SFTPFileEntry{}, err
	}
	defer lease.Close()
	sftpClient := lease.client
	mode := os.FileMode(0o644)
	if info, statErr := sftpClient.Lstat(remotePath); statErr == nil {
		if !overwrite {
			return SFTPFileEntry{}, fmt.Errorf("remote path already exists")
		}
		if !info.Mode().IsRegular() {
			return SFTPFileEntry{}, fmt.Errorf("remote path is not a regular file")
		}
		mode = info.Mode().Perm()
	} else if !os.IsNotExist(statErr) {
		return SFTPFileEntry{}, fmt.Errorf("inspect remote destination: %w", statErr)
	}
	tempPath, remote, err := createTransferTemp(sftpClient, remotePath)
	if err != nil {
		return SFTPFileEntry{}, fmt.Errorf("create remote temporary file: %w", err)
	}
	tempExists := true
	defer func() {
		if !tempExists {
			return
		}
		// Release the upload slot before acquiring a cleanup lease. Cleanup has
		// its own bounded context and works even when the original session died.
		_ = lease.Close()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), sftpUploadCleanupTimeout)
		defer cancel()
		cleanupLease, cleanupErr := t.openSFTP(cleanupCtx, connection)
		if cleanupErr == nil {
			cleanupErr = cleanupLease.client.Remove(tempPath)
			_ = cleanupLease.Close()
		}
		if cleanupErr != nil && !os.IsNotExist(cleanupErr) {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove remote upload temporary file: %w", cleanupErr))
		}
	}()
	_, copyErr := remote.ReadFromWithConcurrency(source, sftpRequestsPerFile)
	closeErr := remote.Close()
	if closeErr != nil {
		lease.session.cancel()
	}
	if err := errors.Join(copyErr, closeErr); err != nil {
		return SFTPFileEntry{}, fmt.Errorf("upload remote file: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return SFTPFileEntry{}, err
	}
	if err := sftpClient.Chmod(tempPath, mode); err != nil {
		return SFTPFileEntry{}, fmt.Errorf("set remote file mode: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return SFTPFileEntry{}, err
	}
	if overwrite {
		if err := sftpClient.PosixRename(tempPath, remotePath); err != nil {
			if renameErr := sftpClient.Rename(tempPath, remotePath); renameErr != nil {
				return SFTPFileEntry{}, fmt.Errorf("replace remote file: %w", errors.Join(err, renameErr))
			}
		}
	} else if err := sftpClient.Rename(tempPath, remotePath); err != nil {
		return SFTPFileEntry{}, fmt.Errorf("create remote file: %w", err)
	}
	tempExists = false
	info, err := sftpClient.Stat(remotePath)
	if err != nil {
		return SFTPFileEntry{}, fmt.Errorf("inspect uploaded remote file: %w", err)
	}
	return sftpFileEntry(remotePath, info), nil
}

type sftpDownloadReader struct {
	file  *sftp.File
	lease *sftpLease
	once  sync.Once
	err   error
}

func (reader *sftpDownloadReader) Read(data []byte) (int, error) {
	return reader.file.Read(data)
}

// Preserve the underlying pipelined reads when io.Copy streams to HTTP.
func (reader *sftpDownloadReader) WriteTo(destination io.Writer) (int64, error) {
	return reader.file.WriteTo(destination)
}

func (reader *sftpDownloadReader) Close() error {
	reader.once.Do(func() {
		reader.err = reader.file.Close()
		if reader.err != nil {
			reader.lease.session.cancel()
		}
		_ = reader.lease.Close()
	})
	return reader.err
}

var _ io.ReadCloser = (*sftpDownloadReader)(nil)
var _ io.WriterTo = (*sftpDownloadReader)(nil)
