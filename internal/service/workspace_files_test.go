package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestWorkspaceFilesystemErrorsKeepServiceClassification(t *testing.T) {
	svc, _ := newWorkspaceService(t, "read_write")
	ctx := context.Background()
	for name, operation := range map[string]func() error{
		"read": func() error { _, err := svc.ReadWorkspaceFile(ctx, "project", "../escape", 0, 0, "test"); return err },
		"list": func() error { _, err := svc.ListWorkspaceFiles(ctx, "project", "/workspace", "test"); return err },
		"search": func() error {
			_, err := svc.SearchWorkspace(ctx, "project", "../escape", "text", domain.FileSearchLiteral, 0, "test")
			return err
		},
		"preview":  func() error { _, err := svc.PreviewAdminWorkspaceFile("project", "../escape"); return err },
		"download": func() error { _, err := svc.OpenAdminWorkspaceFile("project", "../escape"); return err },
		"save": func() error {
			_, err := svc.SaveAdminWorkspaceTextFile(ctx, "project", "../escape", "content")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := operation()
			var invalid *InputValidationError
			if !errors.As(err, &invalid) {
				t.Fatalf("input error lost its tool classification: %v", err)
			}
		})
	}
	_, err := svc.PreviewAdminWorkspaceFile("project", "missing.txt")
	var invalid *InputValidationError
	if !errors.Is(err, os.ErrNotExist) || errors.As(err, &invalid) {
		t.Fatalf("filesystem error misclassified: %v", err)
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
