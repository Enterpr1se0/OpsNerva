package service

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"eino-ops-agent/internal/config"
	"eino-ops-agent/internal/domain"
)

func TestRemoteFileReadScriptDoesNotExposeMetadataMarkersOnMissingFile(t *testing.T) {
	target := filepath.Join(t.TempDir(), "missing.conf")
	script := buildRemoteFileReadScript(domain.ExecRequest{
		Mode: domain.ExecRemoteRead, RemotePath: target,
	})
	command := exec.Command("bash", "-se")
	command.Stdin = strings.NewReader(script)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr

	err := command.Run()
	if err == nil {
		t.Fatal("missing remote file read succeeded")
	}
	if stdout.Len() != 0 {
		t.Fatalf("missing remote file produced stdout that would be classified as partial: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "No such file") {
		t.Fatalf("missing remote file returned unclear stderr: %q", stderr.String())
	}
}

func TestRemoteFileReadScriptKeepsParseableMetadata(t *testing.T) {
	target := filepath.Join(t.TempDir(), "config.conf")
	content := "enabled=true\n"
	if err := os.WriteFile(target, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	script := buildRemoteFileReadScript(domain.ExecRequest{
		Mode: domain.ExecRemoteRead, RemotePath: target,
	})
	command := exec.Command("bash", "-se")
	command.Stdin = strings.NewReader(script)
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	metadata, parsedContent := parseFileReadOutput(target, string(output))
	if parsedContent != content || metadata.Path != target || metadata.Size != int64(len(content)) || metadata.SHA256 == "" {
		t.Fatalf("remote file output was not parseable: metadata=%#v content=%q", metadata, parsedContent)
	}
}

func TestDecorateFileReadPageReportsNextOffset(t *testing.T) {
	metadata := domain.FileMetadata{Size: 300000, OffsetBytes: 131072}
	decorateFileReadPage(&metadata, 131072, 0)
	if !metadata.HasMore || metadata.NextOffset != 262144 {
		t.Fatalf("next file page was not exposed: %#v", metadata)
	}

	lastPage := domain.FileMetadata{Size: 200000, OffsetBytes: 131072}
	decorateFileReadPage(&lastPage, 131072, 0)
	if lastPage.HasMore || lastPage.NextOffset != 0 {
		t.Fatalf("completed file read incorrectly advertised another page: %#v", lastPage)
	}
}

func TestRemoteFileEditBuildsReviewedDiffAndScriptAfterApproval(t *testing.T) {
	svc, transport, host := newTestService(t)
	svc.validators["nginx"] = config.Validator{ID: "nginx", Scope: "remote", Program: "nginx", Args: []string{"-t", "-c", "{{path}}"}, TimeoutSeconds: 15, PathPatterns: []string{"/etc/nginx/**"}}
	transport.stdout = []byte(fileValidationMarker + "\n" + fileAfterMarker + "\n" + strings.Repeat("a", 64) + "  /etc/nginx/nginx.conf\n")
	pending, err := svc.EditRemoteFile(context.Background(), host.ID, "/etc/nginx/nginx.conf", "events {}", "events { worker_connections 1024; }", "nginx", false, "update nginx", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != "approval_required" || len(transport.calls) != 0 || pending.Change == nil || pending.Change.Additions != 1 || pending.Change.Deletions != 1 {
		t.Fatalf("declarative edit did not wait for approval: %#v", pending)
	}
	run, err := svc.store.GetRun(context.Background(), pending.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var approved domain.ExecRequest
	if err := json.Unmarshal([]byte(run.RequestJSON), &approved); err != nil {
		t.Fatal(err)
	}
	if approved.Mode != domain.ExecRemoteEdit || approved.Script != "" || approved.Change == nil || approved.Change.Diff == "" || approved.TextEdit == nil || approved.TextEdit.OldText != "events {}" || approved.ExpectedSHA256 != "" {
		t.Fatalf("approval persisted execution internals or removed fields: %#v", approved)
	}
	if _, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed", "operator"); err != nil {
		t.Fatal(err)
	}
	if len(transport.calls) != 1 {
		t.Fatalf("edit executed %d remote calls", len(transport.calls))
	}
	script := transport.calls[0].Script
	for _, required := range []string{"awk ", "patch --batch --forward --fuzz=0", "nginx", "sync -f", "mv -f", fileAfterMarker, "-events {}", "+events { worker_connections 1024; }"} {
		if !strings.Contains(script, required) {
			t.Fatalf("edit script missing %q:\n%s", required, script)
		}
	}
	for _, removed := range []string{"sha256sum -c", "cmp -s", ".bak", "__OPS_FILE_BEFORE__", "__OPS_FILE_BACKUP__"} {
		if removed != "" && strings.Contains(script, removed) {
			t.Fatalf("removed conflict/backup logic %q remains:\n%s", removed, script)
		}
	}
}

func TestRemoteFileSearchSupportsExplicitModesAndNoMatchSuccess(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(target, []byte("port: 7890\nsocks-port: 7891\nport|socks: literal\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	run := func(matchMode domain.FileSearchMatchMode, pattern string) ([]byte, error) {
		t.Helper()
		script := buildRemoteFileSearchScript(domain.ExecRequest{
			Mode: domain.ExecRemoteSearch, RemotePath: target, SearchPattern: pattern, SearchMatchMode: matchMode,
		})
		if strings.Contains(script, "head -n") {
			t.Fatalf("remote search still truncates output:\n%s", script)
		}
		command := exec.Command("bash", "-se")
		command.Stdin = strings.NewReader(script)
		return command.CombinedOutput()
	}
	literal, err := run(domain.FileSearchLiteral, "port|socks")
	if err != nil || string(literal) != "3:port|socks: literal\n" {
		t.Fatalf("literal search output=%q err=%v", literal, err)
	}
	regex, err := run(domain.FileSearchRegex, "^(port|socks-port):")
	if err != nil || !strings.Contains(string(regex), "1:port: 7890") || !strings.Contains(string(regex), "2:socks-port: 7891") {
		t.Fatalf("regex search output=%q err=%v", regex, err)
	}
	noMatches, err := run(domain.FileSearchLiteral, "absent")
	if err != nil || len(noMatches) != 0 {
		t.Fatalf("no-match search output=%q err=%v", noMatches, err)
	}
	missingScript := buildRemoteFileSearchScript(domain.ExecRequest{
		Mode: domain.ExecRemoteSearch, RemotePath: filepath.Join(directory, "missing"), SearchPattern: "x", SearchMatchMode: domain.FileSearchLiteral,
	})
	missingCommand := exec.Command("bash", "-se")
	missingCommand.Stdin = strings.NewReader(missingScript)
	missingOutput, err := missingCommand.CombinedOutput()
	if err == nil || !strings.Contains(string(missingOutput), "No such file") {
		t.Fatalf("missing file was not preserved as a real search error: output=%q err=%v", missingOutput, err)
	}
}

func TestRemoteFileSearchReturnsStructuredNoMatchResult(t *testing.T) {
	svc, transport, host := newTestService(t)
	transport.stdout = []byte{}
	pending, err := svc.SearchFile(context.Background(), host.ID, "/etc/app.conf", "port|socks", domain.FileSearchRegex, 0, false, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != "approval_required" {
		t.Fatalf("remote search did not require approval: %#v", pending)
	}
	result, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" || result.ExitCode != 0 || result.Search == nil || result.Search.Found || result.Search.MatchMode != domain.FileSearchRegex || result.Message != "no matches found" {
		t.Fatalf("remote no-match result = %#v", result)
	}
	if len(transport.calls) != 1 || !strings.Contains(transport.calls[0].Script, "grep -n -E") {
		t.Fatalf("remote regex search transport request = %#v", transport.calls)
	}
	transport.stderr = []byte("grep: /etc/app.conf: Permission denied\n")
	transport.exitCode = 2
	failedPending, err := svc.SearchFile(context.Background(), host.ID, "/etc/app.conf", "port", domain.FileSearchLiteral, 0, false, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	failed, err := svc.Approve(context.Background(), failedPending.ApprovalID, "reviewed", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != "failed" || failed.ExitCode != 2 || failed.Stderr != "grep: /etc/app.conf: Permission denied\n" || failed.Search != nil {
		t.Fatalf("remote search did not preserve grep failure: %#v", failed)
	}
	if _, err := svc.SearchFile(context.Background(), host.ID, "/etc/app.conf", "[", domain.FileSearchRegex, 0, false, "test"); err == nil || !strings.Contains(err.Error(), "POSIX") {
		t.Fatalf("remote search accepted invalid regex: %v", err)
	}
	if _, err := svc.SearchFile(context.Background(), host.ID, "/etc/app.conf", "port", "", 0, false, "test"); err == nil || !strings.Contains(err.Error(), "match_mode") {
		t.Fatalf("remote search accepted a missing match mode: %v", err)
	}
}

func TestFileEditHeredocMarkerCannotTerminateFromDiff(t *testing.T) {
	edit, change, err := buildTextEdit("/etc/app.conf", "old", "__OPS_FILE_EDIT_known__")
	if err != nil {
		t.Fatal(err)
	}
	script := buildRemoteFileChangeScript("/etc/app.conf", "/etc/.app.tmp", change, edit, "")
	if strings.Contains(script, "<<'__OPS_FILE_EDIT_known__'") {
		t.Fatal("edit reused a delimiter controlled by diff content")
	}
	if !strings.Contains(script, change.Diff) {
		t.Fatal("edit lost the normalized diff")
	}
	for _, required := range []string{"head -c 3", "efbbbf", "sed $'s/\\r$//'"} {
		if !strings.Contains(script, required) {
			t.Fatalf("remote edit script does not normalize Windows text with %q", required)
		}
	}
}

func TestRemoteFileEditRejectsSecretsAndInvalidReplacements(t *testing.T) {
	svc, transport, host := newTestService(t)
	for _, testCase := range []struct{ oldText, newText string }{
		{oldText: "password=old", newText: "password=super-secret"},
		{oldText: "token=old", newText: "token=[REDACTED]"},
		{oldText: "", newText: "new"},
		{oldText: "same", newText: "same"},
	} {
		if _, err := svc.EditRemoteFile(context.Background(), host.ID, "/etc/app.conf", testCase.oldText, testCase.newText, "", false, "change", "test"); err == nil {
			t.Fatalf("invalid replacement was accepted: %#v", testCase)
		}
	}
	if len(transport.calls) != 0 {
		t.Fatal("rejected input reached SSH transport")
	}
}

func TestBuildTextEditNormalizesInputAndBuildsMinimalDiff(t *testing.T) {
	edit, change, err := buildTextEdit("app.conf", "\ufeffa\r\nb\r\n", "a\r\nc\r\nd\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if edit.OldText != "a\nb" || edit.NewText != "a\nc\nd" || change.Additions != 2 || change.Deletions != 1 || !strings.Contains(change.Diff, "@@ -1,2 +1,3 @@\n a\n-b\n+c\n+d\n") || strings.ContainsAny(change.Diff, "\ufeff\r") {
		t.Fatalf("unexpected normalized edit=%#v change=%#v", edit, change)
	}
	if err := validateTextEditChange("app.conf", edit, change); err != nil {
		t.Fatalf("generated edit failed consistency check: %v", err)
	}
	change.Diff = strings.Replace(change.Diff, "+c", "+other", 1)
	if err := validateTextEditChange("app.conf", edit, change); err == nil {
		t.Fatal("mismatched approval diff was accepted")
	}
}

func TestRemoteFileChangeScriptsApplyWithoutPersistentBackups(t *testing.T) {
	if _, err := exec.LookPath("patch"); err != nil {
		t.Skip("patch is unavailable")
	}
	directory := t.TempDir()
	target := filepath.Join(directory, "app.conf")
	if err := os.WriteFile(target, []byte("header\n状态=关闭\nfooter\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	edit, change, err := buildTextEdit(target, "状态=关闭", "状态=开启")
	if err != nil {
		t.Fatal(err)
	}
	script := buildRemoteFileChangeScript(target, filepath.Join(directory, ".edit.tmp"), change, edit, "")
	command := exec.Command("bash", "-se")
	command.Stdin = strings.NewReader(script)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("edit script failed: %v\n%s\n%s", err, output, script)
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "header\n状态=开启\nfooter\n" {
		t.Fatalf("edited content=%q err=%v", content, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".bak") || strings.HasSuffix(entry.Name(), ".orig") || strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("file change left a backup or temporary file: %s", entry.Name())
		}
	}
}

func TestRemoteFileChangeRejectsAmbiguousOldText(t *testing.T) {
	if _, err := exec.LookPath("patch"); err != nil {
		t.Skip("patch is unavailable")
	}
	directory := t.TempDir()
	target := filepath.Join(directory, "app.conf")
	if err := os.WriteFile(target, []byte("enabled=false\nenabled=false\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	edit, change, err := buildTextEdit(target, "enabled=false", "enabled=true")
	if err != nil {
		t.Fatal(err)
	}
	script := buildRemoteFileChangeScript(target, filepath.Join(directory, ".edit.tmp"), change, edit, "")
	command := exec.Command("bash", "-se")
	command.Stdin = strings.NewReader(script)
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "matched 2 blocks") {
		t.Fatalf("ambiguous edit output=%q err=%v", output, err)
	}
	content, readErr := os.ReadFile(target)
	if readErr != nil || string(content) != "enabled=false\nenabled=false\n" {
		t.Fatalf("ambiguous edit touched target: content=%q err=%v", content, readErr)
	}
}
