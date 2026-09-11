package service

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestWorkspaceShellRunsInApprovalGatedSandbox(t *testing.T) {
	requireRunnableWorkspaceSandbox(t)
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
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap sandbox is Linux-only")
	}
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
	requireRunnableWorkspaceSandbox(t)
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
	requireBashWorkspaceHost(t)
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
	requireBashWorkspaceHost(t)
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
	requireBashWorkspaceHost(t)
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
	requireRunnableWorkspaceSandbox(t)
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
