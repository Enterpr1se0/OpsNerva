package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/config"
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

func TestWorkspaceEditRechecksAccessAfterApproval(t *testing.T) {
	svc, root := newWorkspaceService(t, "read_write")
	ctx := context.Background()
	target := filepath.Join(root, "app.conf")
	if err := os.WriteFile(target, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pending, err := svc.EditWorkspaceFile(ctx, "project", "app.conf", "original", "updated", "", "update config", "eino-agent")
	if err != nil || pending.Status != "approval_required" {
		t.Fatalf("pending edit: %+v err=%v", pending, err)
	}
	if _, err := svc.UpdateAdminWorkspace(ctx, "project", domain.WorkspaceInput{Access: "read_only"}, "operator"); err != nil {
		t.Fatal(err)
	}
	if result, err := svc.Approve(ctx, pending.ApprovalID, "reviewed", "operator"); err == nil || result.Status == "completed" {
		t.Fatalf("edit used stale access: %+v err=%v", result, err)
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "original\n" {
		t.Fatalf("revoked write changed file: %q err=%v", content, err)
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
	if _, err := applyTextEdit("a\n", domain.TextEdit{OldText: "wrong", NewText: "b"}); err == nil || !strings.Contains(err.Error(), "matched 0") || !strings.Contains(err.Error(), "preserving all leading whitespace") {
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
	yaml := "tasks:\n    - name: install package\n      module: apt\n"
	if _, err := applyTextEdit(yaml, domain.TextEdit{OldText: "- name: install package\n      module: apt", NewText: "- name: update package\n      module: apt"}); err == nil || !strings.Contains(err.Error(), "matched 0") {
		t.Fatalf("unindented YAML block was accepted: %v", err)
	}
	updated, err = applyTextEdit(yaml, domain.TextEdit{OldText: "    - name: install package\n      module: apt", NewText: "    - name: update package\n      module: apt"})
	if err != nil || updated != "tasks:\n    - name: update package\n      module: apt\n" {
		t.Fatalf("exactly indented YAML block failed: updated=%q err=%v", updated, err)
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
	svc.validators["fixture"] = config.Validator{ID: "fixture", Scope: "workspace", Program: validator, Args: []string{"{{path}}"}, TimeoutSeconds: 5, PathPatterns: []string{filepath.Join(svc.workspaceRoot, "project", "**")}}
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
	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(path)
		if statErr != nil || info.Mode().Perm() != 0o640 {
			t.Fatalf("failed validation changed file mode: info=%#v err=%v", info, statErr)
		}
	}
}
