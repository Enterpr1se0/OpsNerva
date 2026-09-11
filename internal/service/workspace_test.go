package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/security"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"
	"github.com/Enterpr1se0/opsnerva/internal/store"
)

type zeroReader struct{}

func (zeroReader) Read(buffer []byte) (int, error) {
	clear(buffer)
	return len(buffer), nil
}

func requireRunnableWorkspaceSandbox(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap sandbox is Linux-only")
	}
	sandbox, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bubblewrap is not installed")
	}
	// Mirror the runtime mounts used by workspaceSandboxCommand so the preflight
	// verifies bwrap can actually run the production sandbox. /usr/bin/true is
	// dynamically linked, so /lib and /lib64 must be visible inside the sandbox.
	args := []string{
		"--unshare-all", "--unshare-user", "--cap-drop", "ALL",
		"--ro-bind", "/usr", "/usr",
		"--ro-bind-try", "/lib", "/lib",
		"--ro-bind-try", "/lib64", "/lib64",
		"--symlink", "usr/bin", "/bin", "--symlink", "usr/sbin", "/sbin",
		"--proc", "/proc", "--dev", "/dev", "--",
		"/usr/bin/true",
	}
	if workspaceSandboxSupportsDisableUserns(sandbox) {
		args = append([]string{"--disable-userns"}, args...)
	}
	if output, err := exec.Command(sandbox, args...).CombinedOutput(); err != nil {
		message := string(output)
		if strings.Contains(message, "Operation not permitted") || strings.Contains(message, "No permissions to creating new namespace") {
			t.Skipf("bubblewrap namespaces are unavailable: %s", strings.TrimSpace(message))
		}
		t.Fatalf("bubblewrap preflight failed: %v: %s", err, strings.TrimSpace(message))
	}
}

func requireBashWorkspaceHost(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test exercises the Bash Workspace host shell")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
}

func sameWorkspaceFile(first, second string) bool {
	firstInfo, firstErr := os.Stat(first)
	secondInfo, secondErr := os.Stat(second)
	return firstErr == nil && secondErr == nil && os.SameFile(firstInfo, secondInfo)
}

func newWorkspaceService(t *testing.T, access string) (*Service, string) {
	t.Helper()
	ctx := context.Background()
	workspaceRoot := t.TempDir()
	dataDir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dataDir, "workspace.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	encryptor, err := security.NewEncryptor("", dataDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.DataDir = dataDir
	svc := New(st, nil, encryptor, security.NewRedactor(), cfg.Limits, cfg)
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := svc.Shutdown(shutdownCtx); err != nil {
			t.Errorf("shutdown service: %v", err)
		}
	})
	if err := svc.InitializeWorkspaces(ctx, workspaceRoot); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteAdminWorkspace(ctx, "default", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateAdminWorkspace(ctx, domain.WorkspaceInput{ID: "project", Access: access}, "test"); err != nil {
		t.Fatal(err)
	}
	return svc, filepath.Join(workspaceRoot, "project")
}

type workspaceDownloadTransport struct {
	*fakeTransport
	content    []byte
	remotePath string
}

func (transport *workspaceDownloadTransport) OpenSFTPFile(_ context.Context, _ sshx.ConnectionSpec, remotePath string) (sshx.SFTPDownload, error) {
	if remotePath != transport.remotePath {
		return sshx.SFTPDownload{}, fmt.Errorf("unexpected remote path %q", remotePath)
	}
	return sshx.SFTPDownload{
		Entry:  sshx.SFTPFileEntry{Name: filepath.Base(remotePath), Path: remotePath, Type: "file", Size: int64(len(transport.content)), Mode: "-rw-r--r--"},
		Reader: io.NopCloser(bytes.NewReader(transport.content)),
	}, nil
}

func (*workspaceDownloadTransport) ListSFTPFiles(context.Context, sshx.ConnectionSpec, string) (sshx.SFTPFileList, error) {
	return sshx.SFTPFileList{}, errors.New("not implemented")
}

func (*workspaceDownloadTransport) UploadSFTPFile(context.Context, sshx.ConnectionSpec, string, io.Reader, bool) (sshx.SFTPFileEntry, error) {
	return sshx.SFTPFileEntry{}, errors.New("not implemented")
}

func (*workspaceDownloadTransport) CreateSFTPDirectory(context.Context, sshx.ConnectionSpec, string) (sshx.SFTPFileEntry, error) {
	return sshx.SFTPFileEntry{}, errors.New("not implemented")
}

func (*workspaceDownloadTransport) RenameSFTPEntry(context.Context, sshx.ConnectionSpec, string, string) (sshx.SFTPFileEntry, error) {
	return sshx.SFTPFileEntry{}, errors.New("not implemented")
}

func (*workspaceDownloadTransport) RemoveSFTPEntry(context.Context, sshx.ConnectionSpec, string, bool, func(sshx.SFTPDeleteProgress)) (sshx.SFTPFileEntry, error) {
	return sshx.SFTPFileEntry{}, errors.New("not implemented")
}
