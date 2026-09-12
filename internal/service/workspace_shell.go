package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"
	"github.com/Enterpr1se0/opsnerva/internal/store"
	"github.com/Enterpr1se0/opsnerva/internal/workspacefs"
)

// RunWorkspaceShell resolves the administrator-selected backend before
// submission so the exact host or sandbox boundary is approval-bound.
func (s *Service) RunWorkspaceShell(ctx context.Context, workspaceID, script, cwd string, env map[string]string, timeoutSeconds int, reason, actor string) (domain.ExecResult, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	workspace, ok := s.workspaces.Get(workspaceID)
	if !ok {
		return domain.ExecResult{}, fmt.Errorf("workspace %q not found", workspaceID)
	}
	backend, err := s.configuredWorkspaceShellBackend(ctx)
	if err != nil {
		return domain.ExecResult{}, err
	}
	if backend == domain.WorkspaceShellModeHost && workspace.Access != "read_write" {
		return domain.ExecResult{}, fmt.Errorf("host shell is unavailable for read_only workspace %q", workspaceID)
	}
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		cwd = "."
	}
	resolvedCwd, err := resolveWorkspacePath(workspace, cwd, false)
	if err != nil {
		return domain.ExecResult{}, err
	}
	if info, statErr := os.Stat(resolvedCwd); statErr != nil || !info.IsDir() {
		return domain.ExecResult{}, fmt.Errorf("workspace shell cwd is not a directory")
	}
	host, err := s.workspaceHost(ctx, workspaceID)
	if err != nil {
		return domain.ExecResult{}, err
	}
	return s.Submit(ctx, domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecWorkspaceShell, WorkspaceID: workspaceID,
		WorkspaceShellBackend: backend,
		Script:                script, Cwd: cwd, Env: env, TimeoutSeconds: timeoutSeconds,
		Reason: reason,
	}, actor)
}

// StartWorkspaceShell opens a persistent PTY in the Workspace bound to an
// Agent conversation. The selected backend is captured in the approved
// request, just like a one-shot workspace_shell execution.
func (s *Service) StartWorkspaceShell(ctx context.Context, workspaceID, cwd string, env map[string]string, cols, rows int, reason, actor string) (domain.ExecResult, error) {
	if SessionIDFromContext(ctx) == "" {
		return domain.ExecResult{}, fmt.Errorf("interactive Workspace shells require an Agent conversation")
	}
	req, err := s.workspaceShellStartRequest(ctx, workspaceID, cwd, env, cols, rows, reason)
	if err != nil {
		return domain.ExecResult{}, err
	}
	return s.Submit(ctx, req, actor)
}

// StartOperatorWorkspaceShell opens a PTY after an authenticated operator
// explicitly selects a Workspace in the Web console.
func (s *Service) StartOperatorWorkspaceShell(ctx context.Context, workspaceID, cwd, actor string) (domain.SSHShell, error) {
	req, err := s.workspaceShellStartRequest(ctx, workspaceID, cwd, nil, 120, 32, webOperatorReason)
	if err != nil {
		return domain.SSHShell{}, err
	}
	req.ShellSurface = domain.WorkspaceShellSurfaceOperator
	host, err := s.workspaceHost(ctx, req.WorkspaceID)
	if err != nil {
		return domain.SSHShell{}, err
	}
	release, err := s.acquire(ctx, host.ID)
	if err != nil {
		return domain.SSHShell{}, err
	}
	defer release()
	return s.openOperatorWorkspaceTerminal(ctx, host, req, actor)
}

func (s *Service) workspaceShellStartRequest(ctx context.Context, workspaceID, cwd string, env map[string]string, cols, rows int, reason string) (domain.ExecRequest, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	workspace, ok := s.workspaces.Get(workspaceID)
	if !ok {
		return domain.ExecRequest{}, fmt.Errorf("workspace %q not found", workspaceID)
	}
	backend, err := s.configuredWorkspaceShellBackend(ctx)
	if err != nil {
		return domain.ExecRequest{}, err
	}
	if backend == domain.WorkspaceShellModeHost && workspace.Access != "read_write" {
		return domain.ExecRequest{}, fmt.Errorf("host shell is unavailable for read_only workspace %q", workspaceID)
	}
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		cwd = "."
	}
	resolvedCwd, err := resolveWorkspacePath(workspace, cwd, false)
	if err != nil {
		return domain.ExecRequest{}, err
	}
	if info, statErr := os.Stat(resolvedCwd); statErr != nil || !info.IsDir() {
		return domain.ExecRequest{}, fmt.Errorf("workspace shell cwd is not a directory")
	}
	host, err := s.workspaceHost(ctx, workspaceID)
	if err != nil {
		return domain.ExecRequest{}, err
	}
	return domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecWorkspaceShellStart, WorkspaceID: workspaceID,
		WorkspaceShellBackend: backend, Cwd: cwd, Env: env,
		ShellCols: cols, ShellRows: rows, Reason: strings.TrimSpace(reason),
	}, nil
}

func (s *Service) openWorkspaceShell(ctx context.Context, host domain.Host, req domain.ExecRequest, run domain.Run, actor string) (domain.SSHShell, error) {
	return s.openWorkspaceShellRuntime(ctx, host, req, run, actor, false)
}

func (s *Service) openOperatorWorkspaceTerminal(ctx context.Context, host domain.Host, req domain.ExecRequest, actor string) (domain.SSHShell, error) {
	return s.openWorkspaceShellRuntime(ctx, host, req, domain.Run{}, actor, true)
}

func (s *Service) openWorkspaceShellRuntime(ctx context.Context, host domain.Host, req domain.ExecRequest, run domain.Run, actor string, transient bool) (domain.SSHShell, error) {
	options, opener, err := s.workspaceInteractiveShell(ctx, host, req, transient)
	if err != nil {
		return domain.SSHShell{}, err
	}
	return s.openInteractiveShell(ctx, host, req, run, actor, options, opener)
}

func (s *Service) workspaceInteractiveShell(ctx context.Context, host domain.Host, req domain.ExecRequest, transient bool) (interactiveShellOptions, interactiveShellOpener, error) {
	if req.Mode != domain.ExecWorkspaceShellStart {
		return interactiveShellOptions{}, nil, fmt.Errorf("invalid Workspace shell request mode")
	}
	if err := validateInteractiveShellSize(req); err != nil {
		return interactiveShellOptions{}, nil, err
	}
	workspace, ok := s.workspaces.Get(req.WorkspaceID)
	if !ok {
		return interactiveShellOptions{}, nil, fmt.Errorf("workspace %q not found", req.WorkspaceID)
	}
	configuredBackend, err := s.configuredWorkspaceShellBackend(ctx)
	if err != nil {
		return interactiveShellOptions{}, nil, err
	}
	if req.WorkspaceShellBackend == "" || req.WorkspaceShellBackend != configuredBackend {
		return interactiveShellOptions{}, nil, fmt.Errorf("approved workspace shell backend %q is no longer enabled", req.WorkspaceShellBackend)
	}
	program, args, directory, environment, err := s.workspacePTYCommand(workspace, req)
	if err != nil {
		return interactiveShellOptions{}, nil, err
	}
	user := strings.TrimSpace(os.Getenv("USER"))
	if runtime.GOOS == "windows" {
		user = strings.TrimSpace(os.Getenv("USERNAME"))
	}
	if user == "" {
		user = "local"
	}
	return interactiveShellOptions{
		kind: domain.SSHShellKindWorkspace, workspaceID: workspace.ID,
		backend: req.WorkspaceShellBackend, user: user, transient: transient,
	}, func(shellCtx context.Context, output func(string, []byte)) (sshx.ShellSession, error) {
		return startWorkspacePTY(shellCtx, program, args, directory, environment, req.ShellCols, req.ShellRows, output)
	}, nil
}

func (s *Service) workspaceShellTarget(ctx context.Context, shellID, sessionID, workspaceID string) (domain.SSHShell, error) {
	shell, err := s.store.GetSSHShell(ctx, strings.TrimSpace(shellID))
	if err != nil {
		return domain.SSHShell{}, err
	}
	if shell.Kind != domain.SSHShellKindWorkspace || shell.SessionID != strings.TrimSpace(sessionID) || shell.WorkspaceID != strings.TrimSpace(workspaceID) {
		return domain.SSHShell{}, store.ErrNotFound
	}
	return shell, nil
}

func (s *Service) ListWorkspaceShells(ctx context.Context, sessionID, workspaceID, reason, actor string) (domain.SSHShellList, error) {
	shells, err := s.store.ListSSHShells(ctx, strings.TrimSpace(sessionID), true)
	if err != nil {
		return domain.SSHShellList{}, err
	}
	filtered := shells[:0]
	for _, shell := range shells {
		if shell.Kind == domain.SSHShellKindWorkspace && shell.WorkspaceID == strings.TrimSpace(workspaceID) {
			filtered = append(filtered, shell)
		}
	}
	if reason = strings.TrimSpace(reason); reason != "" {
		if len(reason) > maxSSHShellReasonBytes {
			return domain.SSHShellList{}, fmt.Errorf("reason must not exceed %d bytes", maxSSHShellReasonBytes)
		}
		s.audit(context.WithoutCancel(ctx), "", "workspace_shell_list", actor, map[string]any{
			"session_id": strings.TrimSpace(sessionID), "workspace_id": strings.TrimSpace(workspaceID), "reason": s.redactor.Redact(reason),
		})
	}
	return domain.SSHShellList{Shells: filtered, Count: len(filtered), WorkspaceID: strings.TrimSpace(workspaceID)}, nil
}

func (s *Service) WriteWorkspaceShell(ctx context.Context, shellID, sessionID, workspaceID, input, reason, actor string) (domain.SSHShellSnapshot, error) {
	if _, err := s.workspaceShellTarget(ctx, shellID, sessionID, workspaceID); err != nil {
		return domain.SSHShellSnapshot{}, err
	}
	return s.WriteSSHShell(ctx, shellID, sessionID, input, reason, actor)
}

func (s *Service) WriteWorkspaceShellPage(ctx context.Context, shellID, sessionID, workspaceID, input string, queryDelay time.Duration, maxOutputBytes int, reason, actor string) (domain.SSHShellOutputPage, error) {
	if _, err := s.workspaceShellTarget(ctx, shellID, sessionID, workspaceID); err != nil {
		return domain.SSHShellOutputPage{}, err
	}
	return s.WriteSSHShellPage(ctx, shellID, sessionID, input, queryDelay, maxOutputBytes, reason, actor)
}

func (s *Service) WaitWorkspaceShellOutput(ctx context.Context, shellID, sessionID, workspaceID, reason, actor string) (domain.SSHShellSnapshot, error) {
	if _, err := s.workspaceShellTarget(ctx, shellID, sessionID, workspaceID); err != nil {
		return domain.SSHShellSnapshot{}, err
	}
	return s.WaitSSHShellOutput(ctx, shellID, sessionID, reason, actor)
}

func (s *Service) QueryWorkspaceShellOutput(ctx context.Context, shellID, sessionID, workspaceID string, afterSequence *uint64, queryDelay time.Duration, maxOutputBytes int, reason, actor string) (domain.SSHShellOutputPage, error) {
	if _, err := s.workspaceShellTarget(ctx, shellID, sessionID, workspaceID); err != nil {
		return domain.SSHShellOutputPage{}, err
	}
	return s.QuerySSHShellOutput(ctx, shellID, sessionID, afterSequence, queryDelay, maxOutputBytes, reason, actor)
}

func (s *Service) GetWorkspaceShellSnapshot(ctx context.Context, shellID, sessionID, workspaceID string, after uint64, wait time.Duration, coalesce bool, reason, actor string) (domain.SSHShellSnapshot, error) {
	if _, err := s.workspaceShellTarget(ctx, shellID, sessionID, workspaceID); err != nil {
		return domain.SSHShellSnapshot{}, err
	}
	return s.GetSSHShellSnapshot(ctx, shellID, sessionID, after, wait, coalesce, reason, actor)
}

func (s *Service) InterruptWorkspaceShell(ctx context.Context, shellID, sessionID, workspaceID, reason, actor string) (domain.SSHShell, error) {
	if _, err := s.workspaceShellTarget(ctx, shellID, sessionID, workspaceID); err != nil {
		return domain.SSHShell{}, err
	}
	return s.InterruptSSHShell(ctx, shellID, sessionID, reason, actor)
}

func (s *Service) CloseWorkspaceShell(ctx context.Context, shellID, sessionID, workspaceID, reason, actor string) (domain.SSHShell, error) {
	if _, err := s.workspaceShellTarget(ctx, shellID, sessionID, workspaceID); err != nil {
		return domain.SSHShell{}, err
	}
	return s.CloseSSHShell(ctx, shellID, sessionID, reason, actor)
}

func (s *Service) workspacePTYCommand(workspace config.Workspace, req domain.ExecRequest) (string, []string, string, []string, error) {
	switch req.WorkspaceShellBackend {
	case domain.WorkspaceShellModeSandbox:
		program, args, environment, err := s.workspaceSandboxCommand(workspace, req, true)
		return program, args, "", environment, err
	case domain.WorkspaceShellModeHost:
		if workspace.Access != "read_write" {
			return "", nil, "", nil, fmt.Errorf("host shell is unavailable for read_only workspace %q", workspace.ID)
		}
		shell, _, err := workspaceHostShellExecutable()
		if err != nil {
			return "", nil, "", nil, err
		}
		resolvedCwd, err := resolveWorkspacePath(workspace, req.Cwd, false)
		if err != nil {
			return "", nil, "", nil, err
		}
		args := []string{"--noprofile", "--norc", "-i"}
		if runtime.GOOS == "windows" {
			args = []string{"-NoLogo", "-NoProfile", "-NoExit", "-Command", workspacePowerShellUTF8Preamble}
		}
		return shell, args, resolvedCwd, workspaceHostEnvironment(workspace.Root, req.Env), nil
	default:
		return "", nil, "", nil, fmt.Errorf("unsupported workspace shell backend %q", req.WorkspaceShellBackend)
	}
}

func (s *Service) decorateWorkspaceShellSettings(settings domain.SystemSettings) domain.SystemSettings {
	if settings.WorkspaceShellMode == "" {
		settings.WorkspaceShellMode = domain.DefaultWorkspaceShellMode(runtime.GOOS)
	}
	settings.WorkspaceShellPlatform = runtime.GOOS
	_, sandboxErr := s.workspaceSandboxExecutable()
	_, hostName, hostErr := workspaceHostShellExecutable()
	settings.WorkspaceSandboxAvailable = sandboxErr == nil
	settings.WorkspaceHostShellAvailable = hostErr == nil
	switch settings.WorkspaceShellMode {
	case domain.WorkspaceShellModeSandbox:
		if sandboxErr == nil {
			settings.WorkspaceShellBackend = domain.WorkspaceShellModeSandbox
			settings.WorkspaceShellName = "bash"
		}
	case domain.WorkspaceShellModeHost:
		if hostErr == nil {
			settings.WorkspaceShellBackend = domain.WorkspaceShellModeHost
			settings.WorkspaceShellName = hostName
		}
	}
	return settings
}

func (s *Service) configuredWorkspaceShellBackend(ctx context.Context) (string, error) {
	settings, err := s.store.GetSystemSettings(ctx)
	if err != nil {
		return "", err
	}
	if settings.WorkspaceShellMode == "" {
		settings.WorkspaceShellMode = domain.DefaultWorkspaceShellMode(runtime.GOOS)
	}
	switch settings.WorkspaceShellMode {
	case domain.WorkspaceShellModeDisabled:
		return "", fmt.Errorf("workspace shell is disabled in System settings")
	case domain.WorkspaceShellModeSandbox:
		if _, err := s.workspaceSandboxExecutable(); err != nil {
			return "", err
		}
		return domain.WorkspaceShellModeSandbox, nil
	case domain.WorkspaceShellModeHost:
		if _, _, err := workspaceHostShellExecutable(); err != nil {
			return "", err
		}
		return domain.WorkspaceShellModeHost, nil
	default:
		return "", fmt.Errorf("invalid workspace shell mode %q", settings.WorkspaceShellMode)
	}
}

func (s *Service) workspaceSandboxExecutable() (string, error) {
	if runtime.GOOS != "linux" {
		return "", fmt.Errorf("workspace sandbox requires Linux; select Host shell or Disabled in System settings")
	}
	configured := strings.TrimSpace(s.workspaceSandboxPath)
	if configured == "" {
		return "", fmt.Errorf("workspace shell sandbox is disabled; configure workspace_sandbox_path")
	}
	path, err := exec.LookPath(configured)
	if err != nil {
		return "", fmt.Errorf("workspace shell sandbox %q is unavailable; install bubblewrap or configure workspace_sandbox_path: %w", configured, err)
	}
	return filepath.Abs(path)
}

func workspaceSandboxSupportsDisableUserns(sandbox string) bool {
	output, err := exec.Command(sandbox, "--help").CombinedOutput()
	return err == nil && bytes.Contains(output, []byte("--disable-userns"))
}

func workspaceHostShellExecutable() (string, string, error) {
	candidates := []string{"bash"}
	if runtime.GOOS == "windows" {
		candidates = []string{"pwsh.exe", "powershell.exe"}
	}
	for _, candidate := range candidates {
		path, err := exec.LookPath(candidate)
		if err == nil {
			absolute, absErr := filepath.Abs(path)
			if absErr != nil {
				return "", "", absErr
			}
			return absolute, strings.TrimSuffix(strings.TrimSuffix(filepath.Base(path), ".exe"), ".EXE"), nil
		}
	}
	return "", "", fmt.Errorf("host shell is unavailable on %s", runtime.GOOS)
}

type workspaceSandboxMask struct {
	path      string
	directory bool
}

func workspaceSandboxMasks(root string) ([]workspaceSandboxMask, error) {
	const maxMasks = 512
	masks := make([]workspaceSandboxMask, 0)
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		isUnsafeSpecialFile := info.Mode()&(os.ModeSocket|os.ModeNamedPipe|os.ModeDevice|os.ModeCharDevice|os.ModeIrregular) != 0
		if path == root || (!workspacefs.IsSensitiveComponent(info.Name()) && !isUnsafeSpecialFile) {
			return nil
		}
		if len(masks) >= maxMasks {
			return fmt.Errorf("workspace contains more than %d sensitive paths; sandbox setup refused", maxMasks)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		masks = append(masks, workspaceSandboxMask{
			path:      filepath.Join("/workspace", relative),
			directory: info.IsDir(),
		})
		if info.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	return masks, err
}

func pathsOverlap(first, second string) bool {
	within := func(path, root string) bool {
		relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
		return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
	}
	return within(first, second) || within(second, first)
}

func (s *Service) executeWorkspaceShell(ctx context.Context, workspace config.Workspace, req domain.ExecRequest, stream func(string, []byte)) (sshx.RawResult, error) {
	configuredBackend, err := s.configuredWorkspaceShellBackend(ctx)
	if err != nil {
		return sshx.RawResult{}, err
	}
	if req.WorkspaceShellBackend == "" || req.WorkspaceShellBackend != configuredBackend {
		return sshx.RawResult{}, fmt.Errorf("approved workspace shell backend %q is no longer enabled", req.WorkspaceShellBackend)
	}
	switch req.WorkspaceShellBackend {
	case domain.WorkspaceShellModeSandbox:
		return s.executeWorkspaceSandboxShell(ctx, workspace, req, stream)
	case domain.WorkspaceShellModeHost:
		return s.executeWorkspaceHostShell(ctx, workspace, req, stream)
	default:
		return sshx.RawResult{}, fmt.Errorf("unsupported workspace shell backend %q", req.WorkspaceShellBackend)
	}
}

func (s *Service) executeWorkspaceSandboxShell(ctx context.Context, workspace config.Workspace, req domain.ExecRequest, stream func(string, []byte)) (sshx.RawResult, error) {
	sandbox, args, environment, err := s.workspaceSandboxCommand(workspace, req, false)
	if err != nil {
		return sshx.RawResult{}, err
	}

	timeout := req.TimeoutSeconds
	if timeout <= 0 {
		timeout = s.limits.SyncTimeoutSeconds
	}
	if timeout <= 0 {
		timeout = 60
	}
	execCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	command := exec.CommandContext(execCtx, sandbox, args...)
	command.Env = environment
	command.Stdin = strings.NewReader(req.Script)
	return s.runWorkspaceProcess(execCtx, command, timeout, "shell sandbox", workspace.Root, stream)
}

func (s *Service) workspaceSandboxCommand(workspace config.Workspace, req domain.ExecRequest, interactive bool) (string, []string, []string, error) {
	sandbox, err := s.workspaceSandboxExecutable()
	if err != nil {
		return "", nil, nil, err
	}
	root, err := filepath.EvalSymlinks(workspace.Root)
	if err != nil {
		return "", nil, nil, fmt.Errorf("resolve workspace root: %w", err)
	}
	for _, systemRoot := range []string{"/usr", "/lib", "/lib64"} {
		if pathsOverlap(root, systemRoot) {
			return "", nil, nil, fmt.Errorf("workspace root overlaps sandbox runtime directory %s", systemRoot)
		}
	}
	cwd := req.Cwd
	if cwd == "" {
		cwd = "."
	}
	resolvedCwd, err := resolveWorkspacePath(workspace, cwd, false)
	if err != nil {
		return "", nil, nil, err
	}
	info, err := os.Stat(resolvedCwd)
	if err != nil || !info.IsDir() {
		return "", nil, nil, fmt.Errorf("workspace shell cwd is not a directory")
	}
	relativeCwd, err := filepath.Rel(root, resolvedCwd)
	if err != nil {
		return "", nil, nil, err
	}
	masks, err := workspaceSandboxMasks(root)
	if err != nil {
		return "", nil, nil, fmt.Errorf("prepare workspace sandbox masks: %w", err)
	}
	mountMode := "--bind"
	if workspace.Access == "read_only" {
		mountMode = "--ro-bind"
	}
	args := []string{"--die-with-parent"}
	if !interactive {
		args = append(args, "--new-session")
	}
	// Interactive commands already run as the session leader of a dedicated
	// PTY. Starting another session inside Bubblewrap detaches Bash from that
	// controlling terminal and disables job control.
	args = append(args,
		"--unshare-all", "--unshare-user", "--cap-drop", "ALL",
		"--ro-bind", "/usr", "/usr",
		"--ro-bind-try", "/lib", "/lib",
		"--ro-bind-try", "/lib64", "/lib64",
		"--symlink", "usr/bin", "/bin", "--symlink", "usr/sbin", "/sbin",
		"--dir", "/etc", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp",
		"--dir", "/workspace", mountMode, root, "/workspace",
	)
	if workspaceSandboxSupportsDisableUserns(sandbox) {
		args = append([]string{"--disable-userns"}, args...)
	}
	for _, mask := range masks {
		if mask.directory {
			args = append(args, "--tmpfs", mask.path)
		} else {
			args = append(args, "--ro-bind", "/dev/null", mask.path)
		}
	}
	sandboxCwd := "/workspace"
	if relativeCwd != "." {
		sandboxCwd = filepath.ToSlash(filepath.Join("/workspace", relativeCwd))
	}
	args = append(args,
		"--chdir", sandboxCwd,
		"--clearenv",
		"--setenv", "PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"--setenv", "HOME", "/workspace",
		"--setenv", "TMPDIR", "/tmp",
		"--setenv", "TERM", "xterm-256color",
		"--setenv", "COLORTERM", "truecolor",
		"--setenv", "LANG", "C.UTF-8",
		"--setenv", "LC_ALL", "C.UTF-8",
		"--setenv", "PYTHONUTF8", "1",
		"--setenv", "PYTHONIOENCODING", "utf-8",
	)
	keys := make([]string, 0, len(req.Env))
	for key := range req.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--setenv", key, req.Env[key])
	}
	bashArgs := []string{"-se"}
	if interactive {
		bashArgs = []string{"--noprofile", "--norc", "-i"}
	}
	args = append(args, "--", "/usr/bin/bash")
	args = append(args, bashArgs...)
	environment := []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "PYTHONUTF8=1", "PYTHONIOENCODING=utf-8"}
	return sandbox, args, environment, nil
}

func (s *Service) executeWorkspaceHostShell(ctx context.Context, workspace config.Workspace, req domain.ExecRequest, stream func(string, []byte)) (sshx.RawResult, error) {
	if workspace.Access != "read_write" {
		return sshx.RawResult{}, fmt.Errorf("host shell is unavailable for read_only workspace %q", workspace.ID)
	}
	shell, _, err := workspaceHostShellExecutable()
	if err != nil {
		return sshx.RawResult{}, err
	}
	cwd := req.Cwd
	if cwd == "" {
		cwd = "."
	}
	resolvedCwd, err := resolveWorkspacePath(workspace, cwd, false)
	if err != nil {
		return sshx.RawResult{}, err
	}
	info, err := os.Stat(resolvedCwd)
	if err != nil || !info.IsDir() {
		return sshx.RawResult{}, fmt.Errorf("workspace shell cwd is not a directory")
	}
	timeout := req.TimeoutSeconds
	if timeout <= 0 {
		timeout = s.limits.SyncTimeoutSeconds
	}
	if timeout <= 0 {
		timeout = 60
	}
	execCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	args := []string{"-se"}
	var cleanupPowerShellScript func() error
	if runtime.GOOS == "windows" {
		scriptPath, cleanup, scriptErr := createWorkspacePowerShellScript(req.Script)
		if scriptErr != nil {
			return sshx.RawResult{}, scriptErr
		}
		cleanupPowerShellScript = cleanup
		args = []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", scriptPath}
	}
	command := exec.CommandContext(execCtx, shell, args...)
	command.Dir = resolvedCwd
	command.Env = workspaceHostEnvironment(workspace.Root, req.Env)
	if runtime.GOOS != "windows" {
		command.Stdin = strings.NewReader(req.Script)
	}
	result, runErr := s.runWorkspaceProcess(execCtx, command, timeout, "host shell", workspace.Root, stream)
	if cleanupPowerShellScript != nil {
		if cleanupErr := cleanupPowerShellScript(); cleanupErr != nil {
			cleanupErr = fmt.Errorf("remove temporary PowerShell script: %w", cleanupErr)
			if runErr != nil {
				return result, errors.Join(runErr, cleanupErr)
			}
			return result, cleanupErr
		}
	}
	return result, runErr
}

const workspacePowerShellUTF8Preamble = `$__opsNervaUtf8Encoding = [System.Text.UTF8Encoding]::new($false)
[System.Console]::InputEncoding = $__opsNervaUtf8Encoding
[System.Console]::OutputEncoding = $__opsNervaUtf8Encoding
$OutputEncoding = $__opsNervaUtf8Encoding
$env:LANG = 'C.UTF-8'
$env:LC_ALL = 'C.UTF-8'
$env:PYTHONUTF8 = '1'
$env:PYTHONIOENCODING = 'utf-8'
`

func workspacePowerShellScript(script string) []byte {
	const utf8BOM = "\xef\xbb\xbf"
	return []byte(utf8BOM + workspacePowerShellUTF8Preamble + script)
}

func createWorkspacePowerShellScript(script string) (string, func() error, error) {
	directory, err := os.MkdirTemp("", "opsnerva-workspace-shell-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temporary PowerShell directory: %w", err)
	}
	cleanup := func() error {
		return os.RemoveAll(directory)
	}
	path := filepath.Join(directory, "script.ps1")
	if err := os.WriteFile(path, workspacePowerShellScript(script), 0o600); err != nil {
		cleanupErr := cleanup()
		if cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove temporary PowerShell directory: %w", cleanupErr))
		}
		return "", nil, fmt.Errorf("write temporary PowerShell script: %w", err)
	}
	return path, cleanup, nil
}

func workspaceHostEnvironment(workspaceRoot string, input map[string]string) []string {
	return workspaceHostEnvironmentForPlatform(runtime.GOOS, workspaceRoot, input)
}

func workspaceHostEnvironmentForPlatform(goos, workspaceRoot string, input map[string]string) []string {
	values := map[string]string{
		"PATH":             os.Getenv("PATH"),
		"TERM":             "xterm-256color",
		"COLORTERM":        "truecolor",
		"LANG":             "C.UTF-8",
		"LC_ALL":           "C.UTF-8",
		"PYTHONUTF8":       "1",
		"PYTHONIOENCODING": "utf-8",
	}
	if goos != "windows" {
		values["HOME"] = workspaceRoot
		values["TMPDIR"] = os.TempDir()
	}
	for key, value := range input {
		values[key] = value
	}
	if goos == "windows" {
		// Keep Windows profile directories outside the Workspace. PowerShell and
		// child processes create AppData and Microsoft trees under USERPROFILE.
		for _, key := range []string{
			"HOME", "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "APPDATA", "LOCALAPPDATA", "PSModulePath",
			"SystemRoot", "WINDIR", "ComSpec", "PATHEXT", "TEMP", "TMP",
		} {
			for existing := range values {
				if strings.EqualFold(existing, key) {
					delete(values, existing)
				}
			}
			if value := os.Getenv(key); value != "" {
				values[key] = value
			}
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+values[key])
	}
	return environment
}

func (s *Service) runWorkspaceProcess(execCtx context.Context, command *exec.Cmd, timeout int, operation, workspaceRoot string, stream func(string, []byte)) (sshx.RawResult, error) {
	stdout := newWorkspaceCaptureBuffer("stdout", workspaceRoot, stream)
	stderr := newWorkspaceCaptureBuffer("stderr", workspaceRoot, stream)
	command.Stdout, command.Stderr = stdout, stderr
	started := time.Now()
	runErr := command.Run()
	stdout.Flush()
	stderr.Flush()
	result := sshx.RawResult{
		ExitCode: workspaceExitCode(runErr), Stdout: stdout.Bytes(), Stderr: stderr.Bytes(),
		Duration: time.Since(started),
	}
	if errors.Is(execCtx.Err(), context.DeadlineExceeded) {
		return result, fmt.Errorf("workspace %s timed out after %s", operation, time.Duration(timeout)*time.Second)
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			return result, fmt.Errorf("start workspace %s: %w", operation, runErr)
		}
	}
	return result, nil
}

type workspaceCaptureBuffer struct {
	buffer  bytes.Buffer
	pending strings.Builder
	stream  string
	emit    func(string, []byte)
	redact  func(string) string
}

func (b *workspaceCaptureBuffer) Write(data []byte) (int, error) {
	written, err := b.buffer.Write(data)
	if written == 0 || b.emit == nil {
		return written, err
	}
	b.pending.Write(data[:written])
	value := b.pending.String()
	consumed := 0
	for consumed < len(value) {
		index := strings.IndexAny(value[consumed:], "\r\n")
		if index < 0 {
			break
		}
		end := consumed + index + 1
		if value[end-1] == '\r' && end < len(value) && value[end] == '\n' {
			end++
		}
		b.emit(b.stream, []byte(b.redact(value[consumed:end])))
		consumed = end
	}
	if consumed > 0 {
		b.pending.Reset()
		b.pending.WriteString(value[consumed:])
	}
	return written, err
}

func (b *workspaceCaptureBuffer) Bytes() []byte { return bytes.Clone(b.buffer.Bytes()) }

func (b *workspaceCaptureBuffer) Flush() {
	if b.emit == nil || b.pending.Len() == 0 {
		return
	}
	b.emit(b.stream, []byte(b.redact(b.pending.String())))
	b.pending.Reset()
}

func newWorkspaceCaptureBuffer(stream, root string, emit func(string, []byte)) *workspaceCaptureBuffer {
	roots := workspaceRedactionRoots(root)
	return &workspaceCaptureBuffer{
		stream: stream,
		emit:   emit,
		redact: func(value string) string { return redactWorkspacePaths(value, roots) },
	}
}

func workspaceExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
