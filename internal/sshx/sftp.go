package sshx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
)

type SFTPFileEntry struct {
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	Type       string    `json:"type"`
	Size       int64     `json:"size,omitempty"`
	Mode       string    `json:"mode"`
	ModifiedAt time.Time `json:"modified_at"`
}

type SFTPFileList struct {
	HostID  string          `json:"host_id"`
	Path    string          `json:"path"`
	Entries []SFTPFileEntry `json:"entries"`
}

type SFTPMutationResult struct {
	HostID string        `json:"host_id"`
	Entry  SFTPFileEntry `json:"entry"`
}

type SFTPDownload struct {
	Entry  SFTPFileEntry
	Reader io.ReadCloser
}

type SFTPTransport interface {
	ListSFTPFiles(context.Context, ConnectionSpec, string) (SFTPFileList, error)
	OpenSFTPFile(context.Context, ConnectionSpec, string) (SFTPDownload, error)
	UploadSFTPFile(context.Context, ConnectionSpec, string, io.Reader, bool) (SFTPFileEntry, error)
	CreateSFTPDirectory(context.Context, ConnectionSpec, string) (SFTPFileEntry, error)
	RenameSFTPEntry(context.Context, ConnectionSpec, string, string) (SFTPFileEntry, error)
	RemoveSFTPEntry(context.Context, ConnectionSpec, string, bool) (SFTPFileEntry, error)
}

func (t *NativeSSHTransport) ListSFTPFiles(ctx context.Context, connection ConnectionSpec, remotePath string) (SFTPFileList, error) {
	lease, err := t.openSFTP(ctx, connection)
	if err != nil {
		return SFTPFileList{}, err
	}
	defer lease.Close()
	stop := closeSFTPOnContext(ctx, lease)
	defer stop()
	sftpClient := lease.client
	if strings.TrimSpace(remotePath) == "" {
		remotePath, err = sftpClient.Getwd()
		if err != nil {
			return SFTPFileList{}, fmt.Errorf("resolve remote home directory: %w", err)
		}
	}
	remotePath, err = cleanSFTPPath(remotePath)
	if err != nil {
		return SFTPFileList{}, err
	}
	entries, err := sftpClient.ReadDir(remotePath)
	if err != nil {
		return SFTPFileList{}, fmt.Errorf("list remote directory: %w", err)
	}
	result := make([]SFTPFileEntry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, sftpFileEntry(path.Join(remotePath, entry.Name()), entry))
	}
	sort.Slice(result, func(left, right int) bool {
		leftDirectory := result[left].Type == "directory"
		rightDirectory := result[right].Type == "directory"
		if leftDirectory != rightDirectory {
			return leftDirectory
		}
		return strings.ToLower(result[left].Name) < strings.ToLower(result[right].Name)
	})
	return SFTPFileList{HostID: connection.Target.ID, Path: remotePath, Entries: result}, nil
}

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
	reader.stop = context.AfterFunc(ctx, func() { _ = reader.Close() })
	return SFTPDownload{Entry: sftpFileEntry(remotePath, info), Reader: reader}, nil
}

func (t *NativeSSHTransport) UploadSFTPFile(ctx context.Context, connection ConnectionSpec, remotePath string, source io.Reader, overwrite bool) (SFTPFileEntry, error) {
	remotePath, err := cleanSFTPPath(remotePath)
	if err != nil {
		return SFTPFileEntry{}, err
	}
	lease, err := t.openSFTP(ctx, connection)
	if err != nil {
		return SFTPFileEntry{}, err
	}
	defer lease.Close()
	stop := closeSFTPOnContext(ctx, lease)
	defer stop()
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
		if tempExists {
			_ = sftpClient.Remove(tempPath)
		}
	}()
	if _, err := io.Copy(remote, source); err != nil {
		_ = remote.Close()
		return SFTPFileEntry{}, fmt.Errorf("upload remote file: %w", err)
	}
	if err := remote.Close(); err != nil {
		return SFTPFileEntry{}, fmt.Errorf("finish remote upload: %w", err)
	}
	if err := sftpClient.Chmod(tempPath, mode); err != nil {
		return SFTPFileEntry{}, fmt.Errorf("set remote file mode: %w", err)
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

func (t *NativeSSHTransport) CreateSFTPDirectory(ctx context.Context, connection ConnectionSpec, remotePath string) (SFTPFileEntry, error) {
	remotePath, err := cleanSFTPPath(remotePath)
	if err != nil {
		return SFTPFileEntry{}, err
	}
	lease, err := t.openSFTP(ctx, connection)
	if err != nil {
		return SFTPFileEntry{}, err
	}
	defer lease.Close()
	stop := closeSFTPOnContext(ctx, lease)
	defer stop()
	sftpClient := lease.client
	if err := sftpClient.Mkdir(remotePath); err != nil {
		return SFTPFileEntry{}, fmt.Errorf("create remote directory: %w", err)
	}
	if err := sftpClient.Chmod(remotePath, 0o755); err != nil {
		_ = sftpClient.RemoveDirectory(remotePath)
		return SFTPFileEntry{}, fmt.Errorf("set remote directory mode: %w", err)
	}
	info, err := sftpClient.Stat(remotePath)
	if err != nil {
		return SFTPFileEntry{}, fmt.Errorf("inspect remote directory: %w", err)
	}
	return sftpFileEntry(remotePath, info), nil
}

func (t *NativeSSHTransport) RenameSFTPEntry(ctx context.Context, connection ConnectionSpec, sourcePath, destinationPath string) (SFTPFileEntry, error) {
	sourcePath, err := cleanSFTPPath(sourcePath)
	if err != nil {
		return SFTPFileEntry{}, err
	}
	destinationPath, err = cleanSFTPPath(destinationPath)
	if err != nil {
		return SFTPFileEntry{}, err
	}
	if sourcePath == "/" {
		return SFTPFileEntry{}, fmt.Errorf("remote root cannot be renamed")
	}
	lease, err := t.openSFTP(ctx, connection)
	if err != nil {
		return SFTPFileEntry{}, err
	}
	defer lease.Close()
	stop := closeSFTPOnContext(ctx, lease)
	defer stop()
	sftpClient := lease.client
	if _, err := sftpClient.Lstat(destinationPath); err == nil {
		return SFTPFileEntry{}, fmt.Errorf("remote destination already exists")
	} else if !os.IsNotExist(err) {
		return SFTPFileEntry{}, fmt.Errorf("inspect remote destination: %w", err)
	}
	if err := sftpClient.Rename(sourcePath, destinationPath); err != nil {
		return SFTPFileEntry{}, fmt.Errorf("rename remote entry: %w", err)
	}
	info, err := sftpClient.Lstat(destinationPath)
	if err != nil {
		return SFTPFileEntry{}, fmt.Errorf("inspect renamed remote entry: %w", err)
	}
	return sftpFileEntry(destinationPath, info), nil
}

func (t *NativeSSHTransport) RemoveSFTPEntry(ctx context.Context, connection ConnectionSpec, remotePath string, recursive bool) (SFTPFileEntry, error) {
	remotePath, err := cleanSFTPPath(remotePath)
	if err != nil {
		return SFTPFileEntry{}, err
	}
	if remotePath == "/" {
		return SFTPFileEntry{}, fmt.Errorf("remote root cannot be deleted")
	}
	lease, err := t.openSFTP(ctx, connection)
	if err != nil {
		return SFTPFileEntry{}, err
	}
	defer lease.Close()
	stop := closeSFTPOnContext(ctx, lease)
	defer stop()
	sftpClient := lease.client
	info, err := sftpClient.Lstat(remotePath)
	if err != nil {
		return SFTPFileEntry{}, fmt.Errorf("inspect remote entry: %w", err)
	}
	entry := sftpFileEntry(remotePath, info)
	if info.IsDir() {
		if recursive {
			err = removeSFTPDirectory(sftpClient, remotePath)
		} else {
			err = sftpClient.RemoveDirectory(remotePath)
		}
	} else {
		err = sftpClient.Remove(remotePath)
	}
	if err != nil {
		return SFTPFileEntry{}, fmt.Errorf("delete remote entry: %w", err)
	}
	return entry, nil
}

type sftpLease struct {
	transport *NativeSSHTransport
	entry     *sftpPoolEntry
	client    *sftp.Client
	once      sync.Once
	err       error
}

func (lease *sftpLease) Close() error {
	lease.once.Do(func() {
		lease.err = lease.client.Close()
		lease.transport.releaseSFTPConnection(lease.entry, false)
	})
	return lease.err
}

func (t *NativeSSHTransport) openSFTP(ctx context.Context, connection ConnectionSpec) (*sftpLease, error) {
	if err := validateNativeConnection(connection); err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		entry, err := t.acquireSFTPConnection(ctx, connection)
		if err != nil {
			return nil, fmt.Errorf("connect native SSH for SFTP: %w", err)
		}
		sftpClient, err := sftp.NewClient(entry.client.client)
		if err == nil {
			return &sftpLease{transport: t, entry: entry, client: sftpClient}, nil
		}
		lastErr = err
		t.releaseSFTPConnection(entry, true)
	}
	return nil, fmt.Errorf("start native SFTP: %w", lastErr)
}

func closeSFTPOnContext(ctx context.Context, lease *sftpLease) func() bool {
	return context.AfterFunc(ctx, func() { _ = lease.Close() })
}

func cleanSFTPPath(value string) (string, error) {
	if value == "" || !path.IsAbs(value) || path.Clean(value) != value || strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("remote path must be a clean absolute path")
	}
	return value, nil
}

func sftpFileEntry(remotePath string, info os.FileInfo) SFTPFileEntry {
	entryType := "file"
	if info.IsDir() {
		entryType = "directory"
	} else if info.Mode()&os.ModeSymlink != 0 {
		entryType = "symlink"
	}
	return SFTPFileEntry{
		Name:       info.Name(),
		Path:       remotePath,
		Type:       entryType,
		Size:       info.Size(),
		Mode:       info.Mode().String(),
		ModifiedAt: info.ModTime(),
	}
}

func removeSFTPDirectory(client *sftp.Client, remotePath string) error {
	entries, err := client.ReadDir(remotePath)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		child := path.Join(remotePath, entry.Name())
		if entry.IsDir() {
			if err := removeSFTPDirectory(client, child); err != nil {
				return err
			}
			continue
		}
		if err := client.Remove(child); err != nil {
			return err
		}
	}
	return client.RemoveDirectory(remotePath)
}

type sftpDownloadReader struct {
	file  *sftp.File
	lease *sftpLease
	stop  func() bool
	once  sync.Once
	err   error
}

func (reader *sftpDownloadReader) Read(data []byte) (int, error) {
	return reader.file.Read(data)
}

func (reader *sftpDownloadReader) Close() error {
	reader.once.Do(func() {
		if reader.stop != nil {
			reader.stop()
		}
		reader.err = errors.Join(reader.file.Close(), reader.lease.Close())
	})
	return reader.err
}

var _ SFTPTransport = (*NativeSSHTransport)(nil)
var _ io.ReadCloser = (*sftpDownloadReader)(nil)
