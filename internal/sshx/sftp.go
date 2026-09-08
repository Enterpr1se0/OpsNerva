package sshx

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"
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
	RemoveSFTPEntry(context.Context, ConnectionSpec, string, bool, func(SFTPDeleteProgress)) (SFTPFileEntry, error)
}

func (t *NativeSSHTransport) ListSFTPFiles(ctx context.Context, connection ConnectionSpec, remotePath string) (_ SFTPFileList, resultErr error) {
	timing := newSFTPTiming(ctx, "list", connection.Target.ID)
	defer func() { timing.finish(resultErr) }()
	lease, err := t.openSFTP(ctx, connection)
	timing.mark("open")
	if err != nil {
		return SFTPFileList{}, err
	}
	defer func() {
		_ = lease.Close()
		timing.mark("release")
	}()
	sftpClient := lease.client
	if strings.TrimSpace(remotePath) == "" {
		remotePath, err = sftpClient.Getwd()
		timing.mark("home")
		if err != nil {
			return SFTPFileList{}, fmt.Errorf("resolve remote home directory: %w", err)
		}
	}
	remotePath, err = cleanSFTPPath(remotePath)
	if err != nil {
		return SFTPFileList{}, err
	}
	entries, err := sftpClient.ReadDirContext(ctx, remotePath)
	timing.mark("read_directory")
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
	timing.mark("format_sort")
	return SFTPFileList{HostID: connection.Target.ID, Path: remotePath, Entries: result}, nil
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

func (t *NativeSSHTransport) RemoveSFTPEntry(ctx context.Context, connection ConnectionSpec, remotePath string, recursive bool, report func(SFTPDeleteProgress)) (_ SFTPFileEntry, resultErr error) {
	if err := ValidateSFTPDeletePath(remotePath); err != nil {
		return SFTPFileEntry{}, err
	}
	timing := newSFTPTiming(ctx, "delete", connection.Target.ID)
	defer func() { timing.finish(resultErr) }()
	lease, err := t.openSFTP(ctx, connection)
	timing.mark("open")
	if err != nil {
		return SFTPFileEntry{}, err
	}
	defer func() {
		_ = lease.Close()
		timing.mark("release")
	}()
	info, err := lease.client.Lstat(remotePath)
	timing.mark("inspect")
	if err != nil {
		return SFTPFileEntry{}, fmt.Errorf("inspect remote entry: %w", err)
	}
	err = deleteSFTPTree(ctx, lease.client, remotePath, info.IsDir(), recursive, report)
	timing.mark("delete")
	if err != nil {
		return SFTPFileEntry{}, err
	}
	return sftpFileEntry(remotePath, info), nil
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

var _ SFTPTransport = (*NativeSSHTransport)(nil)
