package service

import (
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

func TestRemoteFileEditApprovesDeclarativeDiffAndBuildsScriptAfterApproval(t *testing.T) {
	svc, transport, host := newTestService(t)
	svc.validators["nginx"] = config.Validator{ID: "nginx", Scope: "remote", Program: "nginx", Args: []string{"-t", "-c", "{{path}}"}, TimeoutSeconds: 15, PathPatterns: []string{"/etc/nginx/**"}}
	transport.stdout = []byte(fileValidationMarker + "\n" + fileAfterMarker + "\n" + strings.Repeat("a", 64) + "  /etc/nginx/nginx.conf\n")
	diff := "@@ -1 +1 @@\n-events {}\n+events { worker_connections 1024; }\n"
	pending, err := svc.EditRemoteFile(context.Background(), host.ID, "/etc/nginx/nginx.conf", diff, "nginx", false, "update nginx", "test")
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
	if approved.Mode != domain.ExecRemoteEdit || approved.Script != "" || approved.Change == nil || approved.Change.Diff == "" || approved.ExpectedSHA256 != "" {
		t.Fatalf("approval persisted execution internals or removed fields: %#v", approved)
	}
	if _, err := svc.Approve(context.Background(), pending.ApprovalID, "reviewed", "operator"); err != nil {
		t.Fatal(err)
	}
	if len(transport.calls) != 1 {
		t.Fatalf("edit executed %d remote calls", len(transport.calls))
	}
	script := transport.calls[0].Script
	for _, required := range []string{"patch --batch --forward", "nginx", "sync -f", "mv -f", fileAfterMarker, "-events {}", "+events { worker_connections 1024; }"} {
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
	pending, err := svc.SearchFile(context.Background(), host.ID, "/etc/app.conf", "port|socks", domain.FileSearchRegex, 0, false, "test")
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
	failedPending, err := svc.SearchFile(context.Background(), host.ID, "/etc/app.conf", "port", domain.FileSearchLiteral, 0, false, "test")
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
	change, err := buildEditChange("/etc/app.conf", "@@ -1 +1 @@\n-old\n+__OPS_FILE_EDIT_known__\n")
	if err != nil {
		t.Fatal(err)
	}
	script := buildRemoteFileChangeScript("/etc/app.conf", "/etc/.app.tmp", change, "")
	if strings.Contains(script, "<<'__OPS_FILE_EDIT_known__'") {
		t.Fatal("edit reused a delimiter controlled by diff content")
	}
	if !strings.Contains(script, change.Diff) {
		t.Fatal("edit lost the normalized diff")
	}
}

func TestRemoteFileEditRejectsSecretsAndMalformedDiffs(t *testing.T) {
	svc, transport, host := newTestService(t)
	for _, diff := range []string{
		"@@ -1 +1 @@\n-password=old\n+password=super-secret\n",
		"@@ -1 +1 @@\n-token=old\n+token=[REDACTED]\n",
		"not a unified diff",
		"@@ -1,2 +1 @@\n-old\n+new\n",
	} {
		if _, err := svc.EditRemoteFile(context.Background(), host.ID, "/etc/app.conf", diff, "", false, "change", "test"); err == nil {
			t.Fatalf("invalid diff was accepted: %q", diff)
		}
	}
	if len(transport.calls) != 0 {
		t.Fatal("rejected input reached SSH transport")
	}
}

func TestBuildEditChangeNormalizesHeadersAndCountsLines(t *testing.T) {
	change, err := buildEditChange("app.conf", "--- old\n+++ new\n@@ -1,2 +1,3 @@\n a\n-b\n+c\n+d\n")
	if err != nil {
		t.Fatal(err)
	}
	if change.Additions != 2 || change.Deletions != 1 || !strings.HasPrefix(change.Diff, "--- app.conf\n+++ app.conf\n") {
		t.Fatalf("unexpected normalized change: %#v", change)
	}
}

func TestRemoteFileChangeScriptsApplyWithoutPersistentBackups(t *testing.T) {
	if _, err := exec.LookPath("patch"); err != nil {
		t.Skip("patch is unavailable")
	}
	directory := t.TempDir()
	target := filepath.Join(directory, "app.conf")
	if err := os.WriteFile(target, []byte("enabled=false\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	change, err := buildEditChange(target, "@@ -1 +1 @@\n-enabled=false\n+enabled=true\n")
	if err != nil {
		t.Fatal(err)
	}
	script := buildRemoteFileChangeScript(target, filepath.Join(directory, ".edit.tmp"), change, "")
	command := exec.Command("bash", "-se")
	command.Stdin = strings.NewReader(script)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("edit script failed: %v\n%s\n%s", err, output, script)
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "enabled=true\n" {
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
