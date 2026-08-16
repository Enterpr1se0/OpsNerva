package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
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

	"eino-ops-agent/internal/config"
	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/security"
	"eino-ops-agent/internal/sshx"
	"eino-ops-agent/internal/store"
)

type zeroReader struct{}

func (zeroReader) Read(buffer []byte) (int, error) {
	clear(buffer)
	return len(buffer), nil
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

func (*workspaceDownloadTransport) RemoveSFTPEntry(context.Context, sshx.ConnectionSpec, string, bool) (sshx.SFTPFileEntry, error) {
	return sshx.SFTPFileEntry{}, errors.New("not implemented")
}

func TestConversationWorkspaceBindingIsAuthoritative(t *testing.T) {
	svc, _ := newWorkspaceService(t, "read_write")
	ctx := context.Background()
	if _, err := svc.CreateAdminWorkspace(ctx, domain.WorkspaceInput{ID: "other", Access: "read_only"}, "web-user"); err != nil {
		t.Fatal(err)
	}
	session, err := svc.PrepareChatSession(ctx, "session-workspace", "project", "web-user")
	if err != nil {
		t.Fatal(err)
	}
	if session.WorkspaceID != "project" {
		t.Fatalf("bound Workspace = %q", session.WorkspaceID)
	}
	capability, err := svc.SessionWorkspace(WithSessionID(ctx, session.ID))
	if err != nil {
		t.Fatal(err)
	}
	if capability.ID != "project" || capability.Access != "read_write" {
		t.Fatalf("session capability = %#v", capability)
	}
	if _, err := svc.PrepareChatSession(ctx, session.ID, "other", "web-user"); err == nil || !strings.Contains(err.Error(), "switch it before sending") {
		t.Fatalf("mismatched request Workspace was accepted: %v", err)
	}
	unbound, err := svc.SetChatSessionWorkspace(ctx, session.ID, "", "web-user")
	if err != nil {
		t.Fatal(err)
	}
	if unbound.WorkspaceID != "" {
		t.Fatalf("unbound Workspace = %q", unbound.WorkspaceID)
	}
	if _, err := svc.SessionWorkspace(WithSessionID(ctx, session.ID)); err == nil || !strings.Contains(err.Error(), "no Workspace is bound") {
		t.Fatalf("unbound conversation resolved a Workspace: %v", err)
	}
	if _, err := svc.SetChatSessionWorkspace(ctx, session.ID, "missing", "web-user"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing Workspace was accepted: %v", err)
	}
	if _, err := svc.SetChatSessionWorkspace(ctx, session.ID, "project", "web-user"); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteAdminWorkspace(ctx, "project", "web-user"); err != nil {
		t.Fatal(err)
	}
	afterDelete, err := svc.GetChatSession(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterDelete.WorkspaceID != "" {
		t.Fatalf("deleted Workspace remains bound: %q", afterDelete.WorkspaceID)
	}
}

func runApprovedWorkspaceAccess(t *testing.T, svc *Service, invoke func(context.Context) (domain.ExecResult, error)) domain.ExecResult {
	t.Helper()
	base, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	notifications := make(chan domain.ExecResult, 1)
	ctx := WithApprovalNotifier(WithBlockingApprovals(base), func(result domain.ExecResult) {
		notifications <- result
	})
	type outcome struct {
		result domain.ExecResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := invoke(ctx)
		done <- outcome{result: result, err: err}
	}()
	var pending domain.ExecResult
	select {
	case pending = <-notifications:
	case <-base.Done():
		t.Fatal("timed out waiting for Workspace file approval")
	}
	if pending.Status != "approval_required" || pending.ApprovalID == "" {
		t.Fatalf("Workspace file access skipped approval: %#v", pending)
	}
	select {
	case early := <-done:
		t.Fatalf("Workspace file access returned before approval: %#v", early)
	default:
	}
	if _, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed file access", "operator"); err != nil {
		t.Fatal(err)
	}
	select {
	case completed := <-done:
		if completed.err != nil {
			t.Fatal(completed.err)
		}
		return completed.result
	case <-base.Done():
		t.Fatal("timed out waiting for approved Workspace file access")
		return domain.ExecResult{}
	}
}

func TestWorkspaceAdminCreateUpdateAndRemove(t *testing.T) {
	svc, projectRoot := newWorkspaceService(t, "read_write")
	created, err := svc.CreateAdminWorkspace(context.Background(), domain.WorkspaceInput{ID: "docs", Access: "read_only"}, "admin-web")
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != "docs" || created.Access != "read_only" {
		t.Fatalf("unexpected created workspace: %#v", created)
	}
	docsRoot := filepath.Join(filepath.Dir(projectRoot), "docs")
	if info, err := os.Stat(docsRoot); err != nil || !info.IsDir() {
		t.Fatalf("managed Workspace directory was not created: %v", err)
	}
	if err := os.WriteFile(filepath.Join(docsRoot, "preserved.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(docsRoot, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docsRoot, "nested", "inner.txt"), []byte("deep"), 0o600); err != nil {
		t.Fatal(err)
	}
	updated, err := svc.UpdateAdminWorkspace(context.Background(), "project", domain.WorkspaceInput{ID: "project", Access: "read_only"}, "admin-web")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Access != "read_only" {
		t.Fatalf("workspace access was not updated: %#v", updated)
	}
	if _, err := svc.UploadWorkspaceFile(context.Background(), "project", "blocked.txt", "blocked.txt", strings.NewReader("x"), "admin-web"); err == nil || !strings.Contains(err.Error(), "read_only") {
		t.Fatalf("updated read-only permission was not enforced: %v", err)
	}
	if err := svc.DeleteAdminWorkspace(context.Background(), "docs", "admin-web"); err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.workspaceByID("docs"); ok {
		t.Fatal("removed workspace remains active")
	}
	if _, err := os.Lstat(docsRoot); !os.IsNotExist(err) {
		t.Fatalf("removing the Workspace did not delete its directory: %v", err)
	}
	if _, err := svc.CreateAdminWorkspace(context.Background(), domain.WorkspaceInput{ID: "docs", Access: "read_write"}, "admin-web"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(docsRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("re-added Workspace directory is not empty: %d entries", len(entries))
	}
}

func TestWorkspaceManagedDirectoriesRejectUnsafeNamesAndSymlinks(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	for _, id := range []string{".", "..", "CON", "com1.txt", "trailing."} {
		if _, err := svc.CreateAdminWorkspace(context.Background(), domain.WorkspaceInput{ID: id, Access: "read_write"}, "admin-web"); err == nil {
			t.Fatalf("unsafe Workspace id %q was accepted", id)
		}
	}
	if _, err := svc.CreateAdminWorkspace(context.Background(), domain.WorkspaceInput{ID: "PROJECT", Access: "read_write"}, "admin-web"); err == nil {
		t.Fatal("case-insensitive duplicate Workspace id was accepted")
	}

	target := t.TempDir()
	linkedRoot := filepath.Join(filepath.Dir(root), "linked")
	if err := os.Symlink(target, linkedRoot); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}
	if err := svc.InitializeWorkspaces(context.Background(), linkedRoot); err == nil || !strings.Contains(err.Error(), "symbolic links") {
		t.Fatalf("symlinked managed root was accepted: %v", err)
	}
	if _, err := svc.CreateAdminWorkspace(context.Background(), domain.WorkspaceInput{ID: "linked", Access: "read_write"}, "admin-web"); err == nil || !strings.Contains(err.Error(), "symbolic links") {
		t.Fatalf("symlinked Workspace directory was accepted: %v", err)
	}
}

func TestWorkspaceReadPatchAndTraversalProtection(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	path := filepath.Join(root, "app.conf")
	if err := os.WriteFile(path, []byte("port=8080\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	read := runApprovedWorkspaceAccess(t, svc, func(ctx context.Context) (domain.ExecResult, error) {
		return svc.ReadWorkspaceFile(ctx, "project", "app.conf", 0, 0, "eino-agent")
	})
	if read.Status != "completed" || read.Stdout != "port=8080\n" || read.File == nil || read.File.SHA256 == "" {
		t.Fatalf("unexpected workspace read: %#v", read)
	}
	pending, err := svc.EditWorkspaceFile(context.Background(), "project", "app.conf", "port=8080", "port=9090", "", "change port", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != "approval_required" {
		t.Fatalf("workspace write skipped approval: %#v", pending)
	}
	approved, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed", "operator")
	if err != nil || approved.Status != "completed" {
		t.Fatalf("workspace patch failed: %#v err=%v", approved, err)
	}
	content, _ := os.ReadFile(path)
	if string(content) != "port=9090\n" {
		t.Fatalf("patch result = %q", content)
	}
	if _, err := svc.ReadWorkspaceFile(context.Background(), "project", "../outside", 100, 0, "test"); err == nil || !strings.Contains(err.Error(), "relative") {
		t.Fatalf("workspace traversal was not rejected: %v", err)
	}
}

func TestWorkspaceReadPreservesCompleteLargeFile(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_only")
	want := strings.Repeat("workspace-file-data\n", 20_000) + "workspace-file-end\n"
	if err := os.WriteFile(filepath.Join(root, "large.log"), []byte(want), 0o640); err != nil {
		t.Fatal(err)
	}
	result := runApprovedWorkspaceAccess(t, svc, func(ctx context.Context) (domain.ExecResult, error) {
		return svc.ReadWorkspaceFile(ctx, "project", "large.log", 0, 0, "eino-agent")
	})
	if result.Stdout != want || result.File == nil || result.File.ReturnedBytes != len(want) {
		t.Fatalf("complete workspace file was not returned: got=%d want=%d metadata=%#v", len(result.Stdout), len(want), result.File)
	}
}

func TestWorkspaceReadNegativeOffsetReadsFromFileEnd(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_only")
	if err := os.WriteFile(filepath.Join(root, "tail.log"), []byte("0123456789"), 0o640); err != nil {
		t.Fatal(err)
	}
	result := runApprovedWorkspaceAccess(t, svc, func(ctx context.Context) (domain.ExecResult, error) {
		return svc.ReadWorkspaceFile(ctx, "project", "tail.log", 0, -4, "eino-agent")
	})
	if result.Stdout != "6789" || result.File == nil || result.File.OffsetBytes != 6 || result.File.ReturnedBytes != 4 {
		t.Fatalf("negative Workspace offset returned %#v", result)
	}

	result = runApprovedWorkspaceAccess(t, svc, func(ctx context.Context) (domain.ExecResult, error) {
		return svc.ReadWorkspaceFile(ctx, "project", "tail.log", 0, -100, "eino-agent")
	})
	if result.Stdout != "0123456789" || result.File == nil || result.File.OffsetBytes != 0 {
		t.Fatalf("oversized negative Workspace offset returned %#v", result)
	}
}

func TestWorkspaceReadTailLinesMatchesRemoteSemantics(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_only")
	content := "one\r\ntwo\r\nthree\r\n"
	if err := os.WriteFile(filepath.Join(root, "tail-lines.log"), []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	result := runApprovedWorkspaceAccess(t, svc, func(ctx context.Context) (domain.ExecResult, error) {
		return svc.ReadWorkspaceFileAdvanced(ctx, "project", "tail-lines.log", 0, 0, 2, "eino-agent")
	})
	if result.Stdout != "two\r\nthree\r\n" || result.File == nil || result.File.OffsetBytes != 5 || result.File.ReturnedBytes != len("two\r\nthree\r\n") {
		t.Fatalf("Workspace tail_lines returned %#v", result)
	}
	if _, err := svc.ReadWorkspaceFileAdvanced(context.Background(), "project", "tail-lines.log", 0, 1, 2, "test"); err == nil || !strings.Contains(err.Error(), "tail_lines") {
		t.Fatalf("Workspace tail_lines accepted offset_bytes: %v", err)
	}
}

func TestWorkspaceSearchReturnsLiteralMatchesWithContext(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_only")
	content := "before\nneedle one\nmiddle\nneedle two\nafter\nport|socks\n"
	if err := os.WriteFile(filepath.Join(root, "search.log"), []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	result := runApprovedWorkspaceAccess(t, svc, func(ctx context.Context) (domain.ExecResult, error) {
		return svc.SearchWorkspace(ctx, "project", "search.log", "needle", domain.FileSearchLiteral, 1, "eino-agent")
	})
	want := "1-before\n2:needle one\n3-middle\n4:needle two\n5-after\n"
	if result.Stdout != want {
		t.Fatalf("Workspace search output = %q, want %q", result.Stdout, want)
	}
	if result.Search == nil || !result.Search.Found || result.Search.MatchMode != domain.FileSearchLiteral {
		t.Fatalf("Workspace literal search metadata = %#v", result.Search)
	}
	literalPipe := runApprovedWorkspaceAccess(t, svc, func(ctx context.Context) (domain.ExecResult, error) {
		return svc.SearchWorkspace(ctx, "project", "search.log", "port|socks", domain.FileSearchLiteral, 0, "eino-agent")
	})
	if literalPipe.Stdout != "6:port|socks\n" {
		t.Fatalf("Workspace literal search interpreted pipe as regex: %q", literalPipe.Stdout)
	}
	regex := runApprovedWorkspaceAccess(t, svc, func(ctx context.Context) (domain.ExecResult, error) {
		return svc.SearchWorkspace(ctx, "project", "search.log", "needle|port", domain.FileSearchRegex, 0, "eino-agent")
	})
	if regex.Stdout != "2:needle one\n4:needle two\n6:port|socks\n" || regex.Search == nil || !regex.Search.Found {
		t.Fatalf("Workspace regex search = %#v", regex)
	}
	noMatches := runApprovedWorkspaceAccess(t, svc, func(ctx context.Context) (domain.ExecResult, error) {
		return svc.SearchWorkspace(ctx, "project", "search.log", "absent", domain.FileSearchLiteral, 0, "eino-agent")
	})
	if noMatches.Status != "completed" || noMatches.Stdout != "" || noMatches.Search == nil || noMatches.Search.Found || noMatches.Message != "no matches found" {
		t.Fatalf("Workspace no-match result = %#v", noMatches)
	}
	if _, err := svc.SearchWorkspace(context.Background(), "project", "search.log", "needle", domain.FileSearchLiteral, -1, "test"); err == nil {
		t.Fatal("Workspace search accepted negative context_lines")
	}
	if _, err := svc.SearchWorkspace(context.Background(), "project", "search.log", "[", domain.FileSearchRegex, 0, "test"); err == nil || !strings.Contains(err.Error(), "POSIX") {
		t.Fatalf("Workspace search accepted invalid regex: %v", err)
	}
}

func TestWorkspaceFileAccessRequiresFreshApproval(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_only")
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("token: fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := WithSessionID(context.Background(), "file-read-session")
	pending, err := svc.ReadWorkspaceFile(ctx, "project", "config.yaml", 0, 0, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != "approval_required" || pending.Stdout != "" {
		t.Fatalf("file content was available before approval: %#v", pending)
	}
	approved, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed", "operator")
	if err != nil || approved.Status != "completed" || !strings.Contains(approved.Stdout, "token: fixture") {
		t.Fatalf("approved file access failed: %#v err=%v", approved, err)
	}
}

func TestWorkspacePatchUsesCurrentContextWithoutSHABinding(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	path := filepath.Join(root, "app.conf")
	_ = os.WriteFile(path, []byte("a\n"), 0o600)
	pending, err := svc.EditWorkspaceFile(context.Background(), "project", "app.conf", "a", "b", "", "change", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed", "operator")
	if err != nil || result.Status != "completed" {
		t.Fatalf("context-matched edit failed: %#v err=%v", result, err)
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "b\n" {
		t.Fatalf("edit result=%q err=%v", content, err)
	}
}

func TestWorkspaceFileEditPreservesMode(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	existingPath := filepath.Join(root, "app.conf")
	if err := os.WriteFile(existingPath, []byte("enabled=false\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	pending, err := svc.EditWorkspaceFile(context.Background(), "project", "app.conf", "enabled=false", "enabled=true", "", "enable app", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed", "operator"); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(existingPath)
	if err != nil || string(content) != "enabled=true\n" {
		t.Fatalf("replacement content=%q err=%v", content, err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(existingPath)
		if err != nil || info.Mode().Perm() != 0o640 {
			t.Fatalf("replacement mode=%v err=%v", info, err)
		}
	}
}

func TestWorkspaceFileEditPreservesUTF8BOMAndCRLF(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	path := filepath.Join(root, "windows.conf")
	original := append([]byte{0xef, 0xbb, 0xbf}, []byte("enabled=false\r\nname=demo\r\n")...)
	if err := os.WriteFile(path, original, 0o640); err != nil {
		t.Fatal(err)
	}
	pending, err := svc.EditWorkspaceFile(context.Background(), "project", "windows.conf", "\ufeffenabled=false\r\nname=demo\r\n", "enabled=true\r\nname=demo\r\n", "", "normalize Windows text", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed", "operator"); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	expected := append([]byte{0xef, 0xbb, 0xbf}, []byte("enabled=true\r\nname=demo\r\n")...)
	if err != nil || !bytes.Equal(content, expected) {
		t.Fatalf("preserved edit bytes=% x want=% x err=%v", content, expected, err)
	}
}

func TestWorkspaceFileEditRejectsInvalidReplacementAndMissingTarget(t *testing.T) {
	svc, _ := newWorkspaceService(t, "read_write")
	if _, err := svc.EditWorkspaceFile(context.Background(), "project", "app.conf", "same", "same", "", "change", "test"); err == nil || !strings.Contains(err.Error(), "different") {
		t.Fatalf("no-op replacement was accepted: %v", err)
	}
	pending, err := svc.EditWorkspaceFile(context.Background(), "project", "missing.conf", "old", "new", "", "change", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed", "operator"); err == nil || !strings.Contains(result.Stderr, "does not exist") {
		t.Fatalf("missing edit target was accepted: result=%#v err=%v", result, err)
	}
}

func TestApplyTextEditRequiresOneExactBlock(t *testing.T) {
	if _, err := applyTextEdit("a\n", domain.TextEdit{OldText: "wrong", NewText: "b"}); err == nil || !strings.Contains(err.Error(), "matched 0") {
		t.Fatalf("missing old_text was accepted: %v", err)
	}
	if _, err := applyTextEdit("a\na\n", domain.TextEdit{OldText: "a", NewText: "b"}); err == nil || !strings.Contains(err.Error(), "matched 2") {
		t.Fatalf("ambiguous old_text was accepted: %v", err)
	}
	updated, err := applyTextEdit("prefix\na\nsuffix\n", domain.TextEdit{OldText: "a", NewText: "b"})
	if err != nil || updated != "prefix\nb\nsuffix\n" {
		t.Fatalf("unique relocated edit failed: updated=%q err=%v", updated, err)
	}
	deleted, err := applyTextEdit("a\nb\n", domain.TextEdit{OldText: "a", NewText: ""})
	if err != nil || deleted != "b\n" {
		t.Fatalf("line deletion failed: updated=%q err=%v", deleted, err)
	}
	withoutFinalNewline, err := applyTextEdit("only-one-line-no-nl", domain.TextEdit{OldText: "only-one-line-no-nl", NewText: "edited"})
	if err != nil || withoutFinalNewline != "edited" {
		t.Fatalf("final-newline state changed: updated=%q err=%v", withoutFinalNewline, err)
	}
	crlf, err := applyTextEdit("a\r\nb\r\nc\r\n", domain.TextEdit{OldText: "b", NewText: "b-edited"})
	if err != nil || crlf != "a\r\nb-edited\r\nc\r\n" {
		t.Fatalf("CRLF bytes changed: updated=%q err=%v", crlf, err)
	}
}

func TestWorkspaceFileEditCreatesMissingFile(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	pending, err := svc.EditWorkspaceFile(context.Background(), "project", "created.conf", "", "enabled=true", "", "create config", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed", "operator")
	if err != nil || result.Status != "completed" {
		t.Fatalf("create result=%#v err=%v", result, err)
	}
	content, err := os.ReadFile(filepath.Join(root, "created.conf"))
	if err != nil || string(content) != "enabled=true" {
		t.Fatalf("created content=%q err=%v", content, err)
	}
}

func TestWorkspaceListHidesSensitiveControlPlaneNames(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_only")
	for _, name := range []string{"README.md", ".env", "deploy-credentials.json", "master.key"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	result, err := svc.ListWorkspaceFiles(context.Background(), "project", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Stdout, "README.md") || strings.Contains(result.Stdout, ".env") || strings.Contains(result.Stdout, "credentials") || strings.Contains(result.Stdout, "master.key") {
		t.Fatalf("workspace listing exposed sensitive names: %s", result.Stdout)
	}
	run, err := svc.store.GetRun(context.Background(), result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var request domain.ExecRequest
	if err := json.Unmarshal([]byte(run.RequestJSON), &request); err != nil {
		t.Fatal(err)
	}
	if request.RelativePath != "." {
		t.Fatalf("omitted Workspace list path was not normalized to root: %#v", request)
	}
}

func TestWorkspaceListRejectsAbsoluteDisplayPathBeforeRun(t *testing.T) {
	svc, _ := newWorkspaceService(t, "read_only")
	ctx := context.Background()
	before, err := svc.store.SearchRuns(ctx, "", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.ListWorkspaceFiles(ctx, "project", "/workspace", "test")
	if err == nil || !strings.Contains(err.Error(), `omit path or use "."`) || !strings.Contains(err.Error(), "must be relative") {
		t.Fatalf("absolute Workspace display path returned unclear error: %v", err)
	}
	after, searchErr := svc.store.SearchRuns(ctx, "", "", "", 0)
	if searchErr != nil {
		t.Fatal(searchErr)
	}
	if len(after) != len(before) {
		t.Fatalf("invalid Workspace path created an execution Run: before=%d after=%d", len(before), len(after))
	}
}

func TestWorkspacePreValidationFailureDoesNotTouchOriginal(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	path := filepath.Join(root, "app.conf")
	if err := os.WriteFile(path, []byte("port=8080\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	validator := filepath.Join(t.TempDir(), "validate-fixture")
	validatorBody := "#!/bin/sh\nexit 1\n"
	if err := os.WriteFile(validator, []byte(validatorBody), 0o700); err != nil {
		t.Fatal(err)
	}
	svc.validators["fixture"] = config.Validator{ID: "fixture", Scope: "workspace", Program: validator, Args: []string{"{{path}}"}, TimeoutSeconds: 5, PathPatterns: []string{filepath.Join(root, "**")}}
	pending, err := svc.EditWorkspaceFile(context.Background(), "project", "app.conf", "port=8080", "port=9090", "fixture", "change port", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed", "operator")
	if err == nil || result.ExitCode != 74 {
		t.Fatalf("expected pre-validation failure, result=%#v err=%v", result, err)
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil || string(content) != "port=8080\n" {
		t.Fatalf("failed validation touched the original: content=%q err=%v", content, readErr)
	}
	info, statErr := os.Stat(path)
	if statErr != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("failed validation changed file mode: info=%#v err=%v", info, statErr)
	}
}

func TestWorkspaceAdminUploadIsAtomicAndNeverOverwrites(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	content := []byte("package main\n")
	result, err := svc.UploadWorkspaceFile(context.Background(), "project", "main.go", "ignored.txt", bytes.NewReader(content), "admin-web")
	if err != nil {
		t.Fatal(err)
	}
	wantSHA := fmt.Sprintf("%x", sha256.Sum256(content))
	if result.Path != "main.go" || result.Size != int64(len(content)) || result.SHA256 != wantSHA {
		t.Fatalf("unexpected upload result: %#v", result)
	}
	stored, err := os.ReadFile(filepath.Join(root, "main.go"))
	if err != nil || !bytes.Equal(stored, content) {
		t.Fatalf("uploaded content mismatch: %q err=%v", stored, err)
	}
	info, err := os.Stat(filepath.Join(root, "main.go"))
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("uploaded mode = %v err=%v", info.Mode().Perm(), err)
	}
	if _, err := svc.UploadWorkspaceFile(context.Background(), "project", "main.go", "main.go", bytes.NewBufferString("overwrite\n"), "admin-web"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing file was overwritten: %v", err)
	}
	stored, _ = os.ReadFile(filepath.Join(root, "main.go"))
	if !bytes.Equal(stored, content) {
		t.Fatalf("failed overwrite changed existing content: %q", stored)
	}
	listing, err := svc.ListAdminWorkspaceFiles("project", ".")
	if err != nil || len(listing.Entries) != 1 || listing.Entries[0].Name != "main.go" || listing.Entries[0].Type != "file" {
		t.Fatalf("uploaded file was not visible in the admin listing: %#v err=%v", listing, err)
	}
	for _, path := range []string{"../escape", ".env.production", `nested\windows.txt`} {
		if _, err := svc.UploadWorkspaceFile(context.Background(), "project", path, "file", bytes.NewBufferString("x"), "admin-web"); err == nil {
			t.Fatalf("unsafe upload path %q was accepted", path)
		}
	}
	capabilities := svc.ListAdminWorkspaceCapabilities()
	if len(capabilities) != 1 || capabilities[0].ID != "project" {
		t.Fatalf("unexpected admin capabilities: %#v", capabilities)
	}
	preview, err := svc.PreviewAdminWorkspaceFile("project", "main.go")
	if err != nil {
		t.Fatal(err)
	}
	if preview.Path != "main.go" || preview.Content != string(content) || preview.SHA256 != wantSHA || preview.Binary {
		t.Fatalf("unexpected workspace preview: %#v", preview)
	}
	deleted, err := svc.DeleteAdminWorkspaceEntry(context.Background(), "project", "main.go", "admin-web")
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Path != "main.go" || deleted.Type != "file" || deleted.SHA256 != wantSHA {
		t.Fatalf("unexpected delete result: %#v", deleted)
	}
	if _, err := os.Stat(filepath.Join(root, "main.go")); !os.IsNotExist(err) {
		t.Fatalf("deleted file remains at its original path: %v", err)
	}
	listing, err = svc.ListAdminWorkspaceFiles("project", ".")
	if err != nil || len(listing.Entries) != 0 {
		t.Fatalf("deleted file remains in listing: %#v err=%v", listing, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".opsnerva-trash")); !os.IsNotExist(err) {
		t.Fatalf("delete created a recovery directory: %v", err)
	}
}

func TestWorkspaceAdminPreviewIsBoundedAndHashesWholeFile(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	content := bytes.Repeat([]byte("a"), int(maxAdminWorkspacePreviewBytes)+4096)
	if err := os.WriteFile(filepath.Join(root, "large.txt"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	preview, err := svc.PreviewAdminWorkspaceFile("project", "large.txt")
	if err != nil {
		t.Fatal(err)
	}
	wantSHA := fmt.Sprintf("%x", sha256.Sum256(content))
	if !preview.Truncated || len(preview.Content) != int(maxAdminWorkspacePreviewBytes) || preview.Size != int64(len(content)) || preview.SHA256 != wantSHA {
		t.Fatalf("unexpected bounded workspace preview: %#v", preview)
	}
}

func TestWorkspaceAdminUploadAcceptsFilesLargerThanLegacyLimit(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	const size = int64(100<<20) + 1

	result, err := svc.UploadWorkspaceFile(context.Background(), "project", "large.bin", "large.bin", io.LimitReader(zeroReader{}, size), "admin-web")
	if err != nil {
		t.Fatal(err)
	}
	if result.Size != size {
		t.Fatalf("uploaded size = %d, want %d", result.Size, size)
	}
	info, err := os.Stat(filepath.Join(root, "large.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != size {
		t.Fatalf("stored size = %d, want %d", info.Size(), size)
	}
}

func TestWorkspaceAdminTextEditorPreservesModeAndRejectsBinaryFiles(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	path := filepath.Join(root, "config.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	result, err := svc.SaveAdminWorkspaceTextFile(context.Background(), "project", "config.txt", "after\n")
	if err != nil {
		t.Fatal(err)
	}
	wantSHA := fmt.Sprintf("%x", sha256.Sum256([]byte("after\n")))
	if result.Path != "config.txt" || result.Size != int64(len("after\n")) || result.SHA256 != wantSHA {
		t.Fatalf("unexpected text edit result: %#v", result)
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "after\n" {
		t.Fatalf("edited content = %q, err = %v", content, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("edited mode = %v, err = %v", info.Mode().Perm(), err)
	}
	binaryPath := filepath.Join(root, "binary.dat")
	if err := os.WriteFile(binaryPath, []byte{0, 1, 2}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SaveAdminWorkspaceTextFile(context.Background(), "project", "binary.dat", "text"); err == nil || !strings.Contains(err.Error(), "binary") {
		t.Fatalf("binary file was editable: %v", err)
	}
	binary, err := os.ReadFile(binaryPath)
	if err != nil || !bytes.Equal(binary, []byte{0, 1, 2}) {
		t.Fatalf("binary file changed: %v, err = %v", binary, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".opsnerva-") {
			t.Fatalf("text editor left temporary file %q", entry.Name())
		}
	}
}

func TestWorkspaceAdminTextEditorRejectsReadOnlyWorkspace(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_only")
	if err := os.WriteFile(filepath.Join(root, "config.txt"), []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SaveAdminWorkspaceTextFile(context.Background(), "project", "config.txt", "after\n"); err == nil || !strings.Contains(err.Error(), "read_only") {
		t.Fatalf("read-only Workspace was editable: %v", err)
	}
}

func TestWorkspaceFileWatchReportsExternalDirectoryChanges(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	ctx, cancel := context.WithCancel(context.Background())
	watch, err := svc.WatchAdminWorkspaceFiles(ctx, "project", ".")
	if err != nil {
		cancel()
		t.Fatal(err)
	}

	path := filepath.Join(root, "external.txt")
	for _, change := range []func() error{
		func() error { return os.WriteFile(path, []byte("first"), 0o600) },
		func() error { return os.WriteFile(path, []byte("second version"), 0o600) },
		func() error { return os.Remove(path) },
	} {
		if err := change(); err != nil {
			cancel()
			t.Fatal(err)
		}
		select {
		case event := <-watch.Changes:
			if event.WorkspaceID != "project" || event.Path != "." {
				cancel()
				t.Fatalf("unexpected workspace change: %#v", event)
			}
		case watchErr := <-watch.Errors:
			cancel()
			t.Fatalf("workspace watcher failed: %v", watchErr)
		case <-time.After(3 * time.Second):
			cancel()
			t.Fatal("timed out waiting for workspace file change")
		}
	}

	cancel()
	select {
	case _, open := <-watch.Changes:
		if open {
			t.Fatal("workspace change channel remained open after cancellation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("workspace watcher did not stop after cancellation")
	}
}

func TestWorkspaceAdminDeletePermanentlyRemovesDirectory(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	directory := filepath.Join(root, "build")
	if err := os.MkdirAll(filepath.Join(directory, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "assets", "app.js"), []byte("console.log('ok')\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	deleted, err := svc.DeleteAdminWorkspaceEntry(context.Background(), "project", "build", "admin-web")
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Path != "build" || deleted.Type != "directory" || deleted.Size != 0 || deleted.SHA256 != "" {
		t.Fatalf("unexpected directory delete result: %#v", deleted)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("deleted directory remains at its original path: %v", err)
	}
	if _, err := svc.DeleteAdminWorkspaceEntry(context.Background(), "project", ".", "admin-web"); err == nil || !strings.Contains(err.Error(), "root cannot be deleted") {
		t.Fatalf("Workspace root deletion was accepted: %v", err)
	}
}

func TestAgentWorkspaceDeleteRequiresApprovalAndRecursiveIntent(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	directory := filepath.Join(root, "generated")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "artifact.txt"), []byte("temporary\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DeleteWorkspaceEntry(context.Background(), "project", "generated", false, "remove generated output", "eino-agent"); err == nil || !strings.Contains(err.Error(), "recursive=true") {
		t.Fatalf("non-empty directory deletion did not require recursive intent: %v", err)
	}
	pending, err := svc.DeleteWorkspaceEntry(context.Background(), "project", "generated", true, "remove generated output", "eino-agent")
	if err != nil || pending.Status != "approval_required" {
		t.Fatalf("Workspace delete did not require approval: result=%#v err=%v", pending, err)
	}
	approved, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed permanent deletion", "operator")
	if err != nil || approved.Status != "completed" || !strings.Contains(approved.Stdout, `"path":"generated"`) {
		t.Fatalf("approved Workspace delete failed: result=%#v err=%v", approved, err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("deleted Workspace directory still exists: %v", err)
	}
	if _, err := svc.DeleteWorkspaceEntry(context.Background(), "project", ".", true, "remove root", "eino-agent"); err == nil || !strings.Contains(err.Error(), "root cannot be deleted") {
		t.Fatalf("Workspace root deletion was accepted: %v", err)
	}
}

func TestWorkspaceDownloadUsesVersionBoundAtomicDestination(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
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
	pending, err = svc.DownloadHostFileToWorkspace(context.Background(), host.ID, remotePath, digest, "project", "downloads/report.txt", 30, "download the reviewed report", "eino-agent")
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
	stored, err := os.ReadFile(filepath.Join(root, "downloads", "report.txt"))
	if err != nil || !bytes.Equal(stored, content) {
		t.Fatalf("downloaded content=%q err=%v", stored, err)
	}
	if _, err := svc.DownloadHostFileToWorkspace(context.Background(), host.ID, remotePath, digest, "project", "downloads/report.txt", 30, "avoid overwrite", "eino-agent"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("Workspace download overwrote an existing file: %v", err)
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
	pending, err := svc.UploadWorkspaceFileToHost(context.Background(), host.ID, "project", "deploy.yaml", digest, "/tmp/deploy.yaml", "deploy exact fixture", "eino-agent")
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
	if len(transport.calls) != 1 || transport.calls[0].Mode != domain.ExecWorkspaceUpload || transport.calls[0].LocalPath != localPath || transport.calls[0].ExpectedSHA256 != digest {
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

func TestWorkspaceShellRunsInApprovalGatedSandbox(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bubblewrap is not installed")
	}
	svc, root := newWorkspaceService(t, "read_write")
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("WORKSPACE_SECRET=must-not-leak\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RunWorkspaceShell(context.Background(), "project", "pwd", "../", nil, 10, "invalid traversal", "test"); err == nil || !strings.Contains(err.Error(), "clean and relative") {
		t.Fatalf("workspace shell traversal cwd was not rejected before approval: %v", err)
	}

	pending, err := svc.RunWorkspaceShell(context.Background(), "project", "test ! -e /home/pig\npwd\nmkdir -p extracted\nprintf 'ready\\n' > extracted/value.txt\ncat .env || true\n", ".", nil, 10, "extract a release archive", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != "approval_required" {
		t.Fatalf("workspace shell skipped exact approval: %#v", pending)
	}
	approved, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed sandboxed extraction", "operator")
	if err != nil || approved.Status != "completed" {
		t.Fatalf("workspace shell failed: %#v err=%v", approved, err)
	}
	if !strings.Contains(approved.Stdout, "/workspace") || strings.Contains(approved.Stdout, root) || strings.Contains(approved.Stdout, "must-not-leak") {
		t.Fatalf("workspace shell exposed a host path or sensitive file: %q", approved.Stdout)
	}
	content, err := os.ReadFile(filepath.Join(root, "extracted", "value.txt"))
	if err != nil || string(content) != "ready\n" {
		t.Fatalf("workspace shell output was not persisted: %q err=%v", content, err)
	}
}

func TestWorkspaceShellFailsClosedWithoutSandbox(t *testing.T) {
	svc, _ := newWorkspaceService(t, "read_write")
	svc.workspaceSandboxPath = filepath.Join(t.TempDir(), "missing-bwrap")
	capabilities := svc.ListWorkspaceCapabilities()
	if len(capabilities) != 1 || capabilities[0].Shell {
		t.Fatalf("unavailable sandbox was advertised: %#v", capabilities)
	}
	if _, err := svc.RunWorkspaceShell(context.Background(), "project", "pwd", ".", nil, 10, "inspect workspace", "test"); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("workspace shell did not fail closed: %v", err)
	}
}

func TestReadOnlyWorkspaceShellCannotPersistChanges(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bubblewrap is not installed")
	}
	svc, root := newWorkspaceService(t, "read_only")
	pending, err := svc.RunWorkspaceShell(context.Background(), "project", "printf 'blocked\\n' > created.txt", ".", nil, 10, "verify read-only mount", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != "approval_required" {
		t.Fatalf("workspace shell skipped approval: %#v", pending)
	}
	result, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed read-only test", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "failed" || result.ExitCode == 0 {
		t.Fatalf("read-only workspace accepted a write: %#v", result)
	}
	if _, statErr := os.Stat(filepath.Join(root, "created.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("read-only workspace persisted shell output: %v", statErr)
	}
}

func TestHostWorkspaceShellRequiresFreshOneTimeApproval(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	svc, root := newWorkspaceService(t, "read_write")
	mode := domain.WorkspaceShellModeHost
	if _, err := svc.SaveSystemSettings(context.Background(), domain.SystemSettingsInput{
		AgentMaxIterations: domain.DefaultAgentMaxIterations, WorkspaceShellMode: &mode,
	}, "test"); err != nil {
		t.Fatal(err)
	}
	ctx := WithSessionID(context.Background(), "host-shell-session")
	pending, err := svc.RunWorkspaceShell(ctx, "project", "pwd\nprintf 'ok\\n' > host-created.txt", "", nil, 10, "exercise host shell", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != "approval_required" {
		t.Fatalf("host shell did not request explicit approval: %#v", pending)
	}
	approval, err := svc.Store().GetApproval(context.Background(), pending.ApprovalID)
	if err != nil || !strings.Contains(approval.RequestJSON, `"cwd":"."`) {
		t.Fatalf("Workspace shell did not bind the root cwd by default: approval=%#v err=%v", approval, err)
	}
	approved, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed once", "operator")
	if err != nil || approved.Status != "completed" {
		t.Fatalf("one-time host shell approval failed: %#v err=%v", approved, err)
	}
	if strings.Contains(approved.Stdout, root) || !strings.Contains(approved.Stdout, "$WORKSPACE") {
		t.Fatalf("host shell exposed the workspace root: %q", approved.Stdout)
	}
	if content, err := os.ReadFile(filepath.Join(root, "host-created.txt")); err != nil || string(content) != "ok\n" {
		t.Fatalf("host shell did not write the workspace fixture: content=%q err=%v", content, err)
	}
	repeated, err := svc.RunWorkspaceShell(ctx, "project", "pwd\nprintf 'ok\\n' > host-created.txt", "", nil, 10, "exercise host shell", "eino-agent")
	if err != nil || repeated.Status != "approval_required" {
		t.Fatalf("repeated host shell reused approval: %#v err=%v", repeated, err)
	}
}

func TestHostWorkspaceShellStreamsOutputBeforeCompletion(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	svc, root := newWorkspaceService(t, "read_write")
	mode := domain.WorkspaceShellModeHost
	if _, err := svc.SaveSystemSettings(context.Background(), domain.SystemSettingsInput{
		AgentMaxIterations: domain.DefaultAgentMaxIterations, WorkspaceShellMode: &mode,
	}, "test"); err != nil {
		t.Fatal(err)
	}
	const sessionID = "workspace-shell-stream"
	events, unsubscribe := svc.SubscribeExecutionEvents(sessionID)
	defer unsubscribe()
	ctx := WithExecutionOwner(WithSessionID(context.Background(), sessionID), "call-workspace-shell", "workspace_shell", `{"action":"run"}`)
	pending, err := svc.RunWorkspaceShell(ctx, "project", "pwd\nprintf 'first\\n'\nsleep 0.4\nprintf 'second\\n'", ".", nil, 10, "test streaming output", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApproveAsync(context.Background(), pending.ApprovalID, "reviewed", "operator"); err != nil {
		t.Fatal(err)
	}

	var output strings.Builder
	deadline := time.After(3 * time.Second)
	for !strings.Contains(output.String(), "first\n") {
		select {
		case event := <-events:
			if event.RunID == pending.RunID && event.Stream == "stdout" {
				output.WriteString(event.Content)
			}
		case <-deadline:
			t.Fatalf("Workspace shell output did not stream before completion: %q", output.String())
		}
	}
	run, err := svc.Store().GetRun(context.Background(), pending.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "running" {
		t.Fatalf("first output arrived after completion: status=%s output=%q", run.Status, output.String())
	}
	if strings.Contains(output.String(), root) || !strings.Contains(output.String(), "$WORKSPACE") {
		t.Fatalf("stream exposed Workspace root: %q", output.String())
	}
	completionDeadline := time.After(3 * time.Second)
	for {
		select {
		case event := <-events:
			if event.RunID != pending.RunID || !terminalExecutionStatus(event.Status) {
				continue
			}
			if event.Status != "completed" {
				t.Fatalf("Workspace shell ended with status %q", event.Status)
			}
			return
		case <-completionDeadline:
			t.Fatal("Workspace shell did not complete after streaming output")
		}
	}
}

func TestInteractiveHostWorkspaceShellStreamsInputAndOutput(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	svc, _ := newWorkspaceService(t, "read_write")
	hostMode := domain.WorkspaceShellModeHost
	if _, err := svc.SaveSystemSettings(context.Background(), domain.SystemSettingsInput{
		AgentMaxIterations: domain.DefaultAgentMaxIterations, WorkspaceShellMode: &hostMode,
	}, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PrepareChatSession(context.Background(), "workspace-pty-session", "project", "test"); err != nil {
		t.Fatal(err)
	}
	ctx := WithSessionID(context.Background(), "workspace-pty-session")
	pending, err := svc.StartWorkspaceShell(ctx, "project", ".", map[string]string{"PTY_FIXTURE": "ready"}, 100, 28, "open project terminal", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != "approval_required" {
		t.Fatalf("interactive Workspace shell skipped approval: %#v", pending)
	}
	approved, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed interactive shell", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if approved.Shell == nil {
		t.Fatalf("approved result omitted shell: %#v", approved)
	}
	shell := *approved.Shell
	if shell.Kind != domain.SSHShellKindWorkspace || shell.WorkspaceID != "project" || shell.Backend != domain.WorkspaceShellModeHost || shell.SessionID != "workspace-pty-session" || shell.Surface != domain.WorkspaceShellSurfaceAgent {
		t.Fatalf("unexpected Workspace PTY metadata: %#v", shell)
	}
	if _, err := svc.SetChatSessionWorkspace(context.Background(), shell.SessionID, "", "test"); err == nil {
		t.Fatal("active Workspace terminal allowed conversation Workspace switch")
	}
	if _, err := svc.WriteWorkspaceShellPage(ctx, shell.ID, shell.SessionID, shell.WorkspaceID, "printf 'workspace-pty:%s\\n' \"$PTY_FIXTURE\"\r", 0, 0, "", "eino-agent"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		snapshot, snapshotErr := svc.GetWorkspaceShellSnapshot(ctx, shell.ID, shell.SessionID, shell.WorkspaceID, 0, 200*time.Millisecond, true, "", "eino-agent")
		if snapshotErr != nil {
			t.Fatal(snapshotErr)
		}
		if strings.Contains(snapshot.RecentOutput, "workspace-pty:ready") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Workspace PTY output did not arrive: %#v", snapshot)
		}
	}
	if _, err := svc.WriteWorkspaceShellPage(ctx, shell.ID, shell.SessionID, shell.WorkspaceID, "sleep 10\r", 0, 0, "", "eino-agent"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := svc.InterruptWorkspaceShell(ctx, shell.ID, shell.SessionID, shell.WorkspaceID, "", "eino-agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.WriteWorkspaceShellPage(ctx, shell.ID, shell.SessionID, shell.WorkspaceID, "printf 'workspace-interrupt-ok\\n'\r", 0, 0, "", "eino-agent"); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for {
		snapshot, snapshotErr := svc.GetWorkspaceShellSnapshot(ctx, shell.ID, shell.SessionID, shell.WorkspaceID, 0, 200*time.Millisecond, true, "", "eino-agent")
		if snapshotErr != nil {
			t.Fatal(snapshotErr)
		}
		if strings.Contains(snapshot.RecentOutput, "workspace-interrupt-ok") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Workspace PTY interrupt did not restore the prompt: %#v", snapshot)
		}
	}
	if _, err := svc.CloseWorkspaceShell(ctx, shell.ID, shell.SessionID, shell.WorkspaceID, "", "eino-agent"); err != nil {
		t.Fatal(err)
	}
}

func TestOperatorCanStartWorkspaceShellWithoutApproval(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	svc, _ := newWorkspaceService(t, "read_write")
	hostMode := domain.WorkspaceShellModeHost
	if _, err := svc.SaveSystemSettings(context.Background(), domain.SystemSettingsInput{
		AgentMaxIterations: domain.DefaultAgentMaxIterations, WorkspaceShellMode: &hostMode,
	}, "test"); err != nil {
		t.Fatal(err)
	}
	shell, err := svc.StartOperatorWorkspaceShell(context.Background(), "project", ".", "admin-web")
	if err != nil {
		t.Fatal(err)
	}
	if shell.Kind != domain.SSHShellKindWorkspace || shell.WorkspaceID != "project" || shell.Surface != domain.WorkspaceShellSurfaceOperator || shell.SessionID != "" {
		t.Fatalf("unexpected operator Workspace shell: %#v", shell)
	}
	assertNoPendingApprovals(t, svc)
	if _, err := svc.UpdateAdminWorkspace(context.Background(), "project", domain.WorkspaceInput{ID: "project", Access: "read_only"}, "admin-web"); err == nil {
		t.Fatal("active Workspace terminal allowed access downgrade")
	}
	sandboxMode := domain.WorkspaceShellModeSandbox
	if _, err := svc.SaveSystemSettings(context.Background(), domain.SystemSettingsInput{
		AgentMaxIterations: domain.DefaultAgentMaxIterations, WorkspaceShellMode: &sandboxMode,
	}, "admin-web"); err == nil {
		t.Fatal("active Workspace terminal allowed backend switch")
	}
	if err := svc.DeleteAdminWorkspace(context.Background(), "project", "admin-web"); err == nil {
		t.Fatal("active Workspace terminal allowed Workspace deletion")
	}
	if _, err := svc.CloseSSHShell(context.Background(), shell.ID, "", "", "admin-web"); err != nil {
		t.Fatal(err)
	}
}

func TestInteractiveSandboxWorkspaceShellHasTTY(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bubblewrap is not installed")
	}
	svc, _ := newWorkspaceService(t, "read_write")
	shell, err := svc.StartOperatorWorkspaceShell(context.Background(), "project", ".", "admin-web")
	if err != nil {
		t.Fatal(err)
	}
	if shell.Backend != domain.WorkspaceShellModeSandbox {
		t.Fatalf("unexpected Workspace PTY backend: %#v", shell)
	}
	if err := svc.SendSSHShellInput(context.Background(), shell.ID, "", "test -t 0 && printf 'sandbox-tty-ok\\n'\r", "", "admin-web"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		snapshot, snapshotErr := svc.GetSSHShellSnapshot(context.Background(), shell.ID, "", 0, 200*time.Millisecond, true, "", "")
		if snapshotErr != nil {
			t.Fatal(snapshotErr)
		}
		if strings.Contains(snapshot.RecentOutput, "sandbox-tty-ok") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sandbox Workspace PTY has no working TTY: %#v", snapshot)
		}
	}
	if _, err := svc.CloseSSHShell(context.Background(), shell.ID, "", "", "admin-web"); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspacePowerShellScriptUsesUTF8AndCleansTemporaryDirectory(t *testing.T) {
	script := "Write-Output '中文输出'\n"
	content := workspacePowerShellScript(script)
	if !bytes.HasPrefix(content, []byte{0xef, 0xbb, 0xbf}) {
		t.Fatalf("PowerShell script is missing its UTF-8 BOM: %x", content[:3])
	}
	for _, required := range []string{
		"[System.Console]::InputEncoding = $__opsNervaUtf8Encoding",
		"[System.Console]::OutputEncoding = $__opsNervaUtf8Encoding",
		"$OutputEncoding = $__opsNervaUtf8Encoding",
		"$env:LANG = 'C.UTF-8'",
		"$env:PYTHONUTF8 = '1'",
		script,
	} {
		if !bytes.Contains(content, []byte(required)) {
			t.Fatalf("PowerShell script is missing %q", required)
		}
	}

	path, cleanup, err := createWorkspacePowerShellScript(script)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Dir(path)
	relativeToTemp, err := filepath.Rel(os.TempDir(), directory)
	if err != nil || relativeToTemp == ".." || strings.HasPrefix(relativeToTemp, ".."+string(filepath.Separator)) {
		t.Fatalf("PowerShell script was written outside the system temporary directory: %s", path)
	}
	stored, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(stored, content) {
		t.Fatalf("temporary PowerShell script content mismatch: err=%v", err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("temporary PowerShell directory was not removed: %v", err)
	}
	environment := strings.Join(workspaceHostEnvironment("workspace-root", nil), "\n")
	for _, required := range []string{"LANG=C.UTF-8", "LC_ALL=C.UTF-8", "PYTHONUTF8=1", "PYTHONIOENCODING=utf-8"} {
		if !strings.Contains(environment, required) {
			t.Fatalf("Workspace host environment is missing %q: %s", required, environment)
		}
	}
}

func TestWorkspaceHostEnvironmentPreservesWindowsProfileDirectories(t *testing.T) {
	hostValues := map[string]string{
		"HOME":         `C:\Users\operator`,
		"USERPROFILE":  `C:\Users\operator`,
		"HOMEDRIVE":    "C:",
		"HOMEPATH":     `\Users\operator`,
		"APPDATA":      `C:\Users\operator\AppData\Roaming`,
		"LOCALAPPDATA": `C:\Users\operator\AppData\Local`,
		"PSModulePath": `C:\Program Files\WindowsPowerShell\Modules`,
	}
	for key, value := range hostValues {
		t.Setenv(key, value)
	}
	environment := workspaceHostEnvironmentForPlatform("windows", `C:\OpsNerva\workspace\default`, map[string]string{
		"userprofile": `C:\OpsNerva\workspace\default`,
		"APPDATA":     `C:\OpsNerva\workspace\default\AppData`,
		"CUSTOM":      "value",
	})
	values := make(map[string]string, len(environment))
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("invalid environment entry %q", entry)
		}
		values[key] = value
	}
	for key, expected := range hostValues {
		if values[key] != expected {
			t.Fatalf("Windows host environment %s = %q, want %q", key, values[key], expected)
		}
	}
	if values["CUSTOM"] != "value" {
		t.Fatalf("custom Workspace environment was lost: %#v", values)
	}
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA"} {
		if strings.Contains(strings.ToLower(values[key]), `opsnerva\workspace`) {
			t.Fatalf("Windows profile variable %s points inside the Workspace: %q", key, values[key])
		}
	}
	if _, ok := values["userprofile"]; ok {
		t.Fatalf("case-insensitive USERPROFILE override was preserved: %#v", values)
	}
}

func TestHostWorkspaceShellRejectsReadOnlyDisabledAndBackendSwitch(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	readOnlyService, _ := newWorkspaceService(t, "read_only")
	hostMode := domain.WorkspaceShellModeHost
	if _, err := readOnlyService.SaveSystemSettings(context.Background(), domain.SystemSettingsInput{
		AgentMaxIterations: domain.DefaultAgentMaxIterations, WorkspaceShellMode: &hostMode,
	}, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := readOnlyService.RunWorkspaceShell(context.Background(), "project", "pwd", ".", nil, 10, "inspect", "test"); err == nil || !strings.Contains(err.Error(), "read_only") {
		t.Fatalf("read_only workspace accepted host shell: %v", err)
	}

	svc, _ := newWorkspaceService(t, "read_write")
	if _, err := svc.SaveSystemSettings(context.Background(), domain.SystemSettingsInput{
		AgentMaxIterations: domain.DefaultAgentMaxIterations, WorkspaceShellMode: &hostMode,
	}, "test"); err != nil {
		t.Fatal(err)
	}
	pending, err := svc.RunWorkspaceShell(context.Background(), "project", "pwd", ".", nil, 10, "inspect", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	disabledMode := domain.WorkspaceShellModeDisabled
	if _, err := svc.SaveSystemSettings(context.Background(), domain.SystemSettingsInput{
		AgentMaxIterations: domain.DefaultAgentMaxIterations, WorkspaceShellMode: &disabledMode,
	}, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed before setting changed", "operator"); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("approved host shell ran after backend was disabled: %v", err)
	}
	if _, err := svc.RunWorkspaceShell(context.Background(), "project", "pwd", ".", nil, 10, "inspect", "test"); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("disabled workspace shell created an approval: %v", err)
	}
}

func TestWorkspaceShellBackendValidation(t *testing.T) {
	limits := config.Default().Limits
	for _, req := range []domain.ExecRequest{
		{Mode: domain.ExecWorkspaceShell, WorkspaceID: "project", Script: "pwd"},
		{Mode: domain.ExecWorkspaceShell, WorkspaceID: "project", WorkspaceShellBackend: "automatic", Script: "pwd"},
		{Mode: domain.ExecWorkspaceShellStart, WorkspaceID: "project", WorkspaceShellBackend: "automatic", ShellCols: 120, ShellRows: 32},
		{Mode: domain.ExecProgram, Program: "pwd", WorkspaceShellBackend: domain.WorkspaceShellModeHost},
	} {
		if err := validateRequestLimits(req, limits, nil); err == nil {
			t.Fatalf("invalid workspace shell backend fields were accepted: %#v", req)
		}
	}
	valid := domain.ExecRequest{
		Mode: domain.ExecWorkspaceShell, WorkspaceID: "project",
		WorkspaceShellBackend: domain.WorkspaceShellModeSandbox, Script: "pwd", Cwd: ".",
	}
	if err := validateRequestLimits(valid, limits, nil); err != nil {
		t.Fatalf("valid workspace shell backend was rejected: %v", err)
	}
	validStart := domain.ExecRequest{
		Mode: domain.ExecWorkspaceShellStart, WorkspaceID: "project", WorkspaceShellBackend: domain.WorkspaceShellModeSandbox,
		Cwd: ".", ShellCols: 120, ShellRows: 32,
	}
	if err := validateRequestLimits(validStart, limits, nil); err != nil {
		t.Fatalf("valid interactive Workspace shell was rejected: %v", err)
	}
}

func TestWorkspaceCaptureBufferPreservesCompleteOutput(t *testing.T) {
	payload := bytes.Repeat([]byte("workspace-output-"), 100_000)
	buffer := &workspaceCaptureBuffer{}
	for offset := 0; offset < len(payload); offset += 8191 {
		end := min(offset+8191, len(payload))
		if _, err := buffer.Write(payload[offset:end]); err != nil {
			t.Fatal(err)
		}
	}
	if got := buffer.Bytes(); !bytes.Equal(got, payload) {
		t.Fatalf("captured workspace output differs: got=%d want=%d", len(got), len(payload))
	}
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
