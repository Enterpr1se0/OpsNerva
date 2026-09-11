package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	posixpath "path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/security"
)

func normalizeRequest(req *domain.ExecRequest, limits config.Limits) {
	if req.Mode == "" {
		if req.Script != "" {
			req.Mode = domain.ExecScript
		} else {
			req.Mode = domain.ExecProgram
		}
	}
	interactiveShell := req.Mode == domain.ExecSSHShellStart || req.Mode == domain.ExecWorkspaceShellStart
	if req.TimeoutSeconds <= 0 && !interactiveShell {
		if req.Background && limits.MaxTimeoutSeconds > 0 {
			req.TimeoutSeconds = limits.MaxTimeoutSeconds
		} else {
			req.TimeoutSeconds = limits.SyncTimeoutSeconds
		}
	}
	if req.TimeoutSeconds > limits.MaxTimeoutSeconds {
		req.TimeoutSeconds = limits.MaxTimeoutSeconds
	}
	if req.Env == nil {
		req.Env = map[string]string{}
	}
	if req.Mode == domain.ExecSSHTunnelStart {
		req.TunnelDirection = domain.SSHTunnelDirection(strings.ToLower(strings.TrimSpace(string(req.TunnelDirection))))
		if req.TunnelDirection == "" {
			req.TunnelDirection = domain.SSHTunnelDirectionLocal
		}
		req.TunnelLocalHost = strings.Trim(strings.TrimSpace(req.TunnelLocalHost), "[]")
		if req.TunnelLocalHost == "" {
			req.TunnelLocalHost = sshTunnelDefaultHost
		}
		req.TunnelRemoteHost = strings.Trim(strings.TrimSpace(req.TunnelRemoteHost), "[]")
		if req.TunnelRemoteHost == "" {
			req.TunnelRemoteHost = sshTunnelDefaultHost
		}
	}
	if req.Mode == domain.ExecSSHShellStart || req.Mode == domain.ExecWorkspaceShellStart {
		if req.ShellCols == 0 {
			req.ShellCols = 120
		}
		if req.ShellRows == 0 {
			req.ShellRows = 32
		}
	}
}

func canonicalRequest(req domain.ExecRequest) (string, string, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return "", "", err
	}
	digest := sha256.Sum256(data)
	return string(data), hex.EncodeToString(digest[:]), nil
}

func validateExecutionRequest(host domain.Host, req domain.ExecRequest) (err error) {
	defer func() { err = asInputValidationError(err) }()
	if isWorkspaceMode(req.Mode) {
		if host.AuthType != "workspace" || req.Elevated {
			return fmt.Errorf("invalid workspace execution target")
		}
		if req.WorkspaceID == "" {
			return fmt.Errorf("workspace operation requires a workspace")
		}
		if req.Mode == domain.ExecWorkspaceShellStart {
			return validateInteractiveShellSize(req)
		}
		if req.Mode == domain.ExecWorkspaceSearch {
			if req.WorkspaceID == "" || req.RelativePath == "" {
				return fmt.Errorf("workspace file search requires a workspace, path, and pattern")
			}
			if err := validateFileSearchInput(req.SearchPattern, req.SearchMatchMode, req.ContextLines); err != nil {
				return fmt.Errorf("invalid Workspace file search: %w", err)
			}
		}
		if req.Mode == domain.ExecWorkspaceRead && (req.MaxBytes < 0 || req.TailLines < 0 || (req.OffsetBytes != 0 && req.TailLines != 0)) {
			return fmt.Errorf("invalid Workspace file read range")
		}
		if req.Mode == domain.ExecWorkspaceEdit && (req.RelativePath == "" || req.Change == nil || req.TextEdit == nil) {
			return fmt.Errorf("workspace file edit requires a path, generated change, and text edit")
		}
		return nil
	}
	switch req.Mode {
	case domain.ExecRemoteRead:
		if req.RemotePath == "" {
			return fmt.Errorf("remote file read requires an absolute path")
		}
		if req.MetadataOnly && (req.MaxBytes != 0 || req.OffsetBytes != 0 || req.TailLines != 0) {
			return fmt.Errorf("metadata_only cannot be combined with max_bytes, offset_bytes, or tail_lines")
		}
		if req.MaxBytes < 0 || req.TailLines < 0 || (req.OffsetBytes != 0 && req.TailLines != 0) {
			return fmt.Errorf("invalid remote file read range")
		}
	case domain.ExecRemoteSearch:
		if req.RemotePath == "" || req.SearchPattern == "" {
			return fmt.Errorf("remote file search requires an absolute path and pattern")
		}
		if err := validateFileSearchInput(req.SearchPattern, req.SearchMatchMode, req.ContextLines); err != nil {
			return fmt.Errorf("invalid remote file search: %w", err)
		}
	case domain.ExecRemoteEdit:
		if req.RemotePath == "" || req.Change == nil || req.TextEdit == nil {
			return fmt.Errorf("remote file edit requires a path, generated change, and text edit")
		}
	case domain.ExecWorkspaceDownload:
		if req.WorkspaceID == "" || req.RelativePath == "" {
			return fmt.Errorf("workspace download requires a workspace destination path")
		}
		if err := validateRemoteFilePath(req.RemotePath); err != nil {
			return err
		}
		if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(strings.ToLower(strings.TrimSpace(req.ExpectedSHA256))) {
			return fmt.Errorf("workspace download requires expected_sha256 from ssh_file_read")
		}
	case domain.ExecSSHTunnelStart:
		if err := validateSSHTunnelRequest(req); err != nil {
			return err
		}
	case domain.ExecSSHShellStart:
		if err := validateSSHShellRequest(req); err != nil {
			return err
		}
	}
	if req.Mode == domain.ExecSSHFileTransfer && req.Elevated {
		return fmt.Errorf("elevated mode is not supported for SFTP transfers")
	}
	usesSudo, err := containsShellProgram(req, "sudo")
	if err == nil && usesSudo {
		return fmt.Errorf("do not invoke sudo directly; set elevated=true and provide the underlying program")
	}
	if !req.Elevated {
		return nil
	}
	if req.Mode == domain.ExecWorkspaceUpload || req.Mode == domain.ExecWorkspaceDownload || req.Mode == domain.ExecSSHFileTransfer {
		return fmt.Errorf("elevated mode is not supported for SFTP transfers")
	}
	if host.SudoMode == "none" || host.SudoMode == "" {
		return fmt.Errorf("host %q does not allow managed sudo; edit the host sudo mode first", host.Name)
	}
	if host.SudoMode == "password" && host.SudoCipher == "" {
		return fmt.Errorf("host %q has no encrypted sudo password", host.Name)
	}
	return nil
}

func validateRequestLimits(req domain.ExecRequest, limits config.Limits, redactor *security.Redactor) (err error) {
	defer func() { err = asInputValidationError(err) }()
	if req.Mode == domain.ExecSSHTunnelStart {
		if req.Program != "" || len(req.Args) != 0 || req.Script != "" || req.Cwd != "" || len(req.Env) != 0 ||
			req.RemotePath != "" || req.SourceHostID != "" || req.SourcePath != "" || req.WorkspaceID != "" ||
			req.RelativePath != "" || req.Change != nil || req.TextEdit != nil {
			return fmt.Errorf("SSH tunnel requests cannot include command, file, transfer, or Workspace fields")
		}
	} else if sshTunnelFieldsSet(req) {
		return fmt.Errorf("SSH tunnel fields are only valid for ssh_tunnel_start requests")
	}
	if req.Mode == domain.ExecSSHShellStart {
		if req.Program != "" || len(req.Args) != 0 || req.Script != "" || len(req.Env) != 0 ||
			req.RemotePath != "" || req.SourceHostID != "" || req.SourcePath != "" || req.WorkspaceID != "" ||
			req.RelativePath != "" || req.Change != nil || req.TextEdit != nil || req.TunnelRemoteHost != "" ||
			req.TunnelRemotePort != 0 || req.TunnelLocalPort != 0 {
			return fmt.Errorf("SSH shell requests cannot include command, file, transfer, Workspace, environment, or tunnel fields")
		}
	} else if req.Mode == domain.ExecWorkspaceShellStart {
		if req.Program != "" || len(req.Args) != 0 || req.Script != "" || req.RemotePath != "" ||
			req.SourceHostID != "" || req.SourcePath != "" || req.RelativePath != "" || req.Change != nil || req.TextEdit != nil ||
			req.TunnelRemoteHost != "" || req.TunnelRemotePort != 0 || req.TunnelLocalPort != 0 || req.Elevated {
			return fmt.Errorf("Workspace shell start requests cannot include command, remote file, transfer, change, tunnel, or elevated fields")
		}
	} else if req.ShellCols != 0 || req.ShellRows != 0 {
		return fmt.Errorf("interactive shell fields are only valid for shell start requests")
	}
	if req.Mode == domain.ExecWorkspaceShell || req.Mode == domain.ExecWorkspaceShellStart {
		switch req.WorkspaceShellBackend {
		case domain.WorkspaceShellModeSandbox, domain.WorkspaceShellModeHost:
		default:
			return fmt.Errorf("workspace_shell_backend must be sandbox or host")
		}
	} else if req.WorkspaceShellBackend != "" {
		return fmt.Errorf("workspace_shell_backend is only valid for workspace shell requests")
	}
	changeTooLarge := false
	if req.Change != nil {
		changeTooLarge = len(req.Change.Diff) > 2<<20
	}
	textEditTooLarge := false
	if req.TextEdit != nil {
		textEditTooLarge = len(req.TextEdit.OldText)+len(req.TextEdit.NewText) > 1<<20
		if req.Mode != domain.ExecRemoteEdit && req.Mode != domain.ExecWorkspaceEdit {
			return fmt.Errorf("text_edit is only valid for file edit requests")
		}
	}
	if len(req.Program) > 512 || len(req.Args) > 128 || len(req.Env) > 64 || len(req.Script) > 1<<20 || changeTooLarge || textEditTooLarge {
		return fmt.Errorf("execution request exceeds program, argument, environment, or 1 MiB content limits")
	}
	for _, argument := range req.Args {
		if len(argument) > 32<<10 || strings.ContainsRune(argument, '\x00') {
			return fmt.Errorf("invalid command argument")
		}
	}
	if req.Cwd != "" {
		if req.Mode == domain.ExecWorkspaceShell || req.Mode == domain.ExecWorkspaceShellStart {
			if filepath.IsAbs(req.Cwd) || filepath.Clean(req.Cwd) != req.Cwd || strings.ContainsAny(req.Cwd, "\x00\r\n") {
				return fmt.Errorf("workspace shell cwd must be a clean relative path")
			}
		} else if !posixpath.IsAbs(req.Cwd) || posixpath.Clean(req.Cwd) != req.Cwd || strings.ContainsAny(req.Cwd, "\x00\r\n") {
			return fmt.Errorf("cwd must be a clean absolute remote path")
		}
	}
	if req.RemotePath != "" && (!posixpath.IsAbs(req.RemotePath) || posixpath.Clean(req.RemotePath) != req.RemotePath || strings.ContainsAny(req.RemotePath, "\x00\r\n")) {
		return fmt.Errorf("remote_path must be a clean absolute path")
	}
	if req.SourcePath != "" && (!posixpath.IsAbs(req.SourcePath) || posixpath.Clean(req.SourcePath) != req.SourcePath || strings.ContainsAny(req.SourcePath, "\x00\r\n")) {
		return fmt.Errorf("source_path must be a clean absolute path")
	}
	for key, value := range req.Env {
		if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`).MatchString(key) || len(value) > 32<<10 || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("invalid environment variable %q", key)
		}
		if redactor != nil && redactor.Redact(key+"="+value) != key+"="+value {
			return fmt.Errorf("environment variable %q appears to contain a secret; credentials must be managed by the control plane", key)
		}
	}
	if req.Mode != domain.ExecProgram {
		return nil
	}
	program := strings.ToLower(posixpath.Base(req.Program))
	interactive := map[string]bool{"bash": true, "sh": true, "zsh": true, "fish": true, "su": true, "vi": true, "vim": true, "nano": true, "emacs": true, "less": true, "more": true, "man": true, "htop": true, "watch": true, "tmux": true, "screen": true}
	if interactive[program] {
		return executionToolSelectionError(req, program)
	}
	if program == "top" && !hasAnyArg(req.Args, "-b", "--batch") {
		return &ExecutionToolSelectionError{
			Message:       "ssh_exec requires top batch mode because it has no PTY",
			SuggestedTool: "ssh_exec",
			NextAction:    "for a snapshot retry ssh_exec with program=top and args=[\"-b\",\"-n\",\"1\"]; use ssh_shell only for interactive top",
			Example: map[string]any{
				"host_id": req.HostID, "program": "top", "args": []string{"-b", "-n", "1"}, "reason": req.Reason,
			},
		}
	}
	if program == "systemctl" && len(req.Args) > 0 && req.Args[0] == "edit" {
		return fmt.Errorf("interactive systemctl edit is unsupported; use ssh_file_edit on the unit or override file")
	}
	if packageMutation(req.Args) {
		requiredFlag := ""
		switch program {
		case "apt", "apt-get":
			requiredFlag = "-y or --assume-yes"
			if hasAnyArg(req.Args, "-y", "--yes", "--assume-yes") {
				return nil
			}
		case "dnf", "yum":
			requiredFlag = "-y or --assumeyes"
			if hasAnyArg(req.Args, "-y", "--assumeyes") {
				return nil
			}
		case "pacman":
			requiredFlag = "--noconfirm"
			if hasAnyArg(req.Args, "--noconfirm") {
				return nil
			}
		default:
			return nil
		}
		return fmt.Errorf("package operation may wait for interactive input; add %s and keep the exact package list in args", requiredFlag)
	}
	_ = limits
	return nil
}

func packageMutation(args []string) bool {
	for _, argument := range args {
		switch strings.ToLower(argument) {
		case "install", "remove", "upgrade", "full-upgrade", "dist-upgrade", "-s", "-r", "-u", "-sy", "-syu":
			return true
		}
	}
	return false
}

func hasAnyArg(args []string, candidates ...string) bool {
	for _, argument := range args {
		for _, candidate := range candidates {
			if argument == candidate {
				return true
			}
		}
	}
	return false
}
