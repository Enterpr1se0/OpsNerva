package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/ids"
	"github.com/Enterpr1se0/opsnerva/internal/observability"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"
)

func (s *Service) Submit(ctx context.Context, req domain.ExecRequest, actor string) (domain.ExecResult, error) {
	return s.submit(ctx, req, actor, executionObserver{})
}

func (s *Service) submit(ctx context.Context, req domain.ExecRequest, actor string, observer executionObserver) (domain.ExecResult, error) {
	normalizeRequest(&req, s.limits)
	if err := validateRequestLimits(req, s.limits, s.redactor); err != nil {
		return domain.ExecResult{}, err
	}
	if strings.TrimSpace(req.Reason) == "" {
		return domain.ExecResult{}, fmt.Errorf("reason is required")
	}
	if req.Mode == domain.ExecWorkspaceUpload {
		if _, err := s.prepareWorkspaceUpload(req); err != nil {
			return domain.ExecResult{}, err
		}
	}
	host, err := s.store.GetHost(ctx, req.HostID)
	if err != nil {
		return domain.ExecResult{}, err
	}
	var rootConnections []sshx.ConnectionSpec
	if isWorkspaceMode(req.Mode) {
		if req.SSHConnectionDigest != "" || req.SourceConnectionDigest != "" {
			return domain.ExecResult{}, fmt.Errorf("SSH connection binding is invalid for local Workspace operations")
		}
	} else if req.Mode == domain.ExecSSHFileTransfer {
		connections, bindErr := s.bindSSHFileTransfer(ctx, host, &req, actor)
		err = bindErr
		if err != nil {
			return domain.ExecResult{}, err
		}
		rootConnections = append(rootConnections, connections.Destination, connections.Source)
	} else {
		if req.SourceConnectionDigest != "" {
			return domain.ExecResult{}, fmt.Errorf("source SSH connection binding is only valid for host-to-host transfers")
		}
		connection, digest, connectionErr := s.resolveSSHConnection(ctx, host)
		if connectionErr != nil {
			return domain.ExecResult{}, connectionErr
		}
		if err := requireAgentSSHAccess(actor, connection); err != nil {
			return domain.ExecResult{}, err
		}
		bindSSHRequest(&req, digest)
		rootConnections = append(rootConnections, connection)
	}
	if err := authorizeAgentRootConnections(actor, req.Elevated, rootConnections...); err != nil {
		return domain.ExecResult{}, err
	}
	if err := validateExecutionRequest(host, req); err != nil {
		return domain.ExecResult{}, err
	}
	requestJSON, digest, err := canonicalRequest(req)
	if err != nil {
		return domain.ExecResult{}, err
	}
	sessionID := SessionIDFromContext(ctx)
	if (req.Mode == domain.ExecSSHShellStart || req.Mode == domain.ExecWorkspaceShellStart) && sessionID == "" {
		return domain.ExecResult{}, fmt.Errorf("interactive shells require an Agent conversation")
	}
	settings, settingsErr := s.store.GetSystemSettings(ctx)
	if settingsErr != nil {
		return domain.ExecResult{}, settingsErr
	}
	llmRequest := actor == "eino-agent" || actor == "mcp-client"
	approvalRequired := llmRequest && settings.ApprovalMode != domain.ApprovalModeFullAccess
	requestCipher, err := s.encryptor.Encrypt([]byte(requestJSON))
	if err != nil {
		return domain.ExecResult{}, err
	}
	requestRedacted := s.redactor.Redact(requestJSON)
	now := time.Now().UTC()
	var commandExplanation *domain.CommandReview
	var explanationInput *domain.CommandReviewInput
	var reviewer ApprovalReviewer
	autoRejected := false
	if approvalRequired {
		if llmRequest && settings.ApprovalMode == domain.ApprovalModeAuto {
			input := s.automaticApprovalInput(ctx, req, host, digest, sessionID)
			review := s.reviewForAutomaticApproval(ctx, s.automaticApprovalReviewer(), input, settings.SubagentTimeoutSeconds)
			commandExplanation = &review
			switch {
			case review.Status == "completed" && review.Decision == domain.ApprovalAgentAllow:
				approvalRequired = false
			case review.Status == "completed" && review.Decision == domain.ApprovalAgentReject:
				autoRejected = true
			case review.Status == "completed" && review.Decision == domain.ApprovalAgentManual:
				approvalRequired = true
			}
		} else {
			reviewer = s.approvalReviewer()
			if settings.ApprovalExplanationsEnabled && reviewer != nil {
				input := s.commandReviewInput(ctx, req, host, digest, sessionID)
				explanationInput = &input
				commandExplanation = &domain.CommandReview{Status: "pending"}
			}
		}
	}
	reviewJSON := ""
	if commandExplanation != nil {
		if encoded, marshalErr := json.Marshal(commandExplanation); marshalErr == nil {
			reviewJSON = string(encoded)
		}
	}
	run := domain.Run{
		ID: ids.New("run"), SessionID: sessionID, HostID: host.ID, RequestJSON: requestRedacted, RequestCipher: requestCipher,
		SearchText: s.redactor.Redact(req.SearchText()), RequestDigest: digest,
		Status: "created", AIReviewJSON: reviewJSON, AIReview: commandExplanation, StartedAt: now,
	}
	if owner, ok := executionOwnerFromContext(ctx); ok {
		run.ToolName = owner.ToolName
		run.ToolArgumentsJSON = s.redactor.Redact(owner.Arguments)
	}
	logger := observability.FromContext(ctx).With(
		"session_id", sessionID, "host_id", host.ID,
		"mode", req.Mode, "program", req.Program, "elevated", req.Elevated,
		"actor", actor, "run_id", run.ID,
	)
	logger.DebugContext(ctx, "approval route selected", "approval_mode", settings.ApprovalMode, "approval_required", approvalRequired, "request_digest", digest)
	if autoRejected {
		run.Status = "rejected"
		run.Error = commandExplanation.Reason
		run.CompletedAt = time.Now().UTC()
		if err := s.store.CreateRun(ctx, run); err != nil {
			return domain.ExecResult{}, err
		}
		if observer.RunStarted != nil {
			observer.RunStarted(run)
		}
		s.audit(ctx, run.ID, "auto_approval_agent_rejected", "auto-approval-agent", map[string]any{
			"reason": commandExplanation.Reason, "model": commandExplanation.Model,
		})
		logger.With("component", "approval").InfoContext(ctx, "Auto approval Agent rejected execution", "model", commandExplanation.Model)
		return execResultFromRun(run, "", ""), nil
	}
	if approvalRequired {
		run.Status = "approval_required"
		if err := s.store.CreateRun(ctx, run); err != nil {
			return domain.ExecResult{}, err
		}
		if owner, ok := executionOwnerFromContext(ctx); ok {
			s.bindExecutionOwner(ctx, run.ID, sessionID, owner)
		}
		approval := domain.Approval{
			ID: ids.New("approval"), RunID: run.ID, HostID: host.ID, RequestJSON: requestRedacted, RequestCipher: requestCipher,
			RequestDigest: digest, Status: domain.ApprovalStatusPending,
			CreatedAt: now,
		}
		if continuation, ok := agentApprovalContinuationFromContext(ctx); ok {
			approval.Status = domain.ApprovalStatusPreparing
			approval.ContinuationKind = domain.ApprovalContinuationAgent
			approval.CheckpointID = continuation.CheckpointID
		}
		if err := s.store.CreateApproval(ctx, approval); err != nil {
			s.clearExecutionOwner(run.ID)
			return domain.ExecResult{}, err
		}
		s.audit(ctx, run.ID, "approval_requested", actor, map[string]any{"approval_id": approval.ID, "mode": settings.ApprovalMode})
		if commandExplanation != nil && commandExplanation.Status == "completed" && commandExplanation.Decision == domain.ApprovalAgentManual {
			s.audit(ctx, run.ID, "auto_approval_agent_requested_manual_review", "auto-approval-agent", map[string]any{
				"approval_id": approval.ID, "reason": commandExplanation.Reason, "model": commandExplanation.Model,
			})
		}
		logger.With("component", "approval").InfoContext(ctx, "execution awaiting approval", "approval_id", approval.ID, "approval_mode", settings.ApprovalMode)
		if commandExplanation != nil && commandExplanation.Status == "pending" && explanationInput != nil && reviewer != nil {
			s.startPendingApprovalExplanation(ctx, approval, *explanationInput, reviewer, settings.SubagentTimeoutSeconds)
		}
		if observer.RunStarted != nil {
			observer.RunStarted(run)
		}
		return domain.ExecResult{RunID: run.ID, Status: run.Status, ApprovalID: approval.ID}, nil
	}
	run.Status = "running"
	if err := s.store.CreateRun(ctx, run); err != nil {
		return domain.ExecResult{}, err
	}
	if owner, ok := executionOwnerFromContext(ctx); ok {
		s.bindExecutionOwner(ctx, run.ID, sessionID, owner)
	}
	if commandExplanation != nil && commandExplanation.Status == "completed" && commandExplanation.Decision == domain.ApprovalAgentAllow {
		s.audit(ctx, run.ID, "auto_approval_agent_granted", "auto-approval-agent", map[string]any{
			"reason": commandExplanation.Reason, "model": commandExplanation.Model,
		})
	}
	if observer.RunStarted != nil {
		observer.RunStarted(run)
	}
	return s.execute(ctx, host, req, run, actor, observer.Output)
}

func execResultFromRun(run domain.Run, approvalID, operatorInstruction string) domain.ExecResult {
	stderr := run.StderrRedacted
	if stderr == "" && run.Error != "" {
		stderr = run.Error
	}
	duration := time.Duration(0)
	if !run.CompletedAt.IsZero() {
		duration = run.CompletedAt.Sub(run.StartedAt)
	}
	result := domain.ExecResult{
		RunID: run.ID, Status: run.Status, ApprovalID: approvalID,
		AutoApproved:        autoApprovedRun(run),
		OperatorInstruction: operatorInstruction, ExitCode: run.ExitCode,
		Stdout: run.StdoutRedacted, Stderr: stderr,
		Duration: duration, CompletedAt: run.CompletedAt,
	}
	var request domain.ExecRequest
	if json.Unmarshal([]byte(run.RequestJSON), &request) == nil && request.Mode == domain.ExecSSHTunnelStart {
		var tunnel domain.SSHTunnel
		if json.Unmarshal([]byte(run.StdoutRedacted), &tunnel) == nil && tunnel.ID != "" {
			result.Tunnel = &tunnel
		}
	}
	if request.Mode == domain.ExecSSHShellStart || request.Mode == domain.ExecWorkspaceShellStart {
		var shell domain.SSHShell
		if json.Unmarshal([]byte(run.StdoutRedacted), &shell) == nil && shell.ID != "" {
			result.Shell = &shell
		}
		result.ShellUsage = sshShellUsage()
	}
	return result
}

func autoApprovedRun(run domain.Run) bool {
	return run.AIReview != nil && run.AIReview.Kind == domain.CommandReviewKindAutomaticApproval && run.AIReview.Status == "completed" && run.AIReview.Decision == domain.ApprovalAgentAllow
}

func (s *Service) execute(ctx context.Context, host domain.Host, req domain.ExecRequest, run domain.Run, actor string, stream func(string, []byte)) (domain.ExecResult, error) {
	ctx = s.withExecutionOwnerForRun(ctx, run.ID)
	defer s.clearExecutionOwner(run.ID)
	autoApproved := autoApprovedRun(run)
	logger := observability.FromContext(ctx).With(
		"component", "execution", "run_id", run.ID, "session_id", run.SessionID, "host_id", host.ID,
		"mode", req.Mode, "program", req.Program, "elevated", req.Elevated,
	)
	logger.InfoContext(ctx, "operation execution started")
	if req.Mode == domain.ExecWorkspaceUpload {
		prepared, prepareErr := s.prepareWorkspaceUpload(req)
		if prepareErr != nil {
			run.Status = "failed"
			run.Error = prepareErr.Error()
			run.CompletedAt = time.Now().UTC()
			_ = s.store.UpdateRun(ctx, run)
			s.publishExecutionEvent(ExecutionEvent{SessionID: run.SessionID, RunID: run.ID, Status: run.Status})
			s.audit(ctx, run.ID, "command_failed", actor, map[string]any{"error": prepareErr.Error()})
			logger.ErrorContext(ctx, "Workspace upload source validation failed", "error", prepareErr)
			return domain.ExecResult{RunID: run.ID, Status: run.Status, AutoApproved: autoApproved, Stderr: prepareErr.Error(), CompletedAt: run.CompletedAt}, prepareErr
		}
		req = prepared
	}
	approvedReq := req
	transportReq := req
	if req.Mode == domain.ExecRemoteRead {
		transportReq.Mode = domain.ExecScript
		transportReq.Script = buildRemoteFileReadScript(req)
	}
	if req.Mode == domain.ExecRemoteSearch {
		transportReq.Mode = domain.ExecScript
		transportReq.Script = buildRemoteFileSearchScript(req)
	}
	if req.Mode == domain.ExecRemoteEdit {
		prepared, prepareErr := s.prepareRemoteFileChange(req)
		if prepareErr != nil {
			run.Status = "failed"
			run.Error = prepareErr.Error()
			run.CompletedAt = time.Now().UTC()
			_ = s.store.UpdateRun(ctx, run)
			s.publishExecutionEvent(ExecutionEvent{SessionID: run.SessionID, RunID: run.ID, Status: run.Status})
			s.audit(ctx, run.ID, "command_failed", actor, map[string]any{"error": prepareErr.Error()})
			logger.ErrorContext(ctx, "remote file change preparation failed", "error", prepareErr)
			return domain.ExecResult{RunID: run.ID, Status: run.Status, AutoApproved: autoApproved, Stderr: prepareErr.Error(), Change: req.Change, CompletedAt: run.CompletedAt}, prepareErr
		}
		transportReq = prepared
	}
	hostIDs := []string{host.ID}
	if req.Mode == domain.ExecSSHFileTransfer {
		hostIDs = append(hostIDs, req.SourceHostID)
	}
	release := func() {}
	var err error
	release, err = s.acquire(ctx, hostIDs...)
	if err != nil {
		logger.WarnContext(ctx, "operation canceled before acquiring capacity", "error", err)
		return domain.ExecResult{}, err
	}
	defer release()
	var connection sshx.ConnectionSpec
	if !isWorkspaceMode(req.Mode) && req.Mode != domain.ExecSSHFileTransfer {
		var currentDigest string
		latestHost, connectionErr := s.store.GetHost(ctx, host.ID)
		if connectionErr == nil {
			connection, currentDigest, connectionErr = s.resolveSSHConnection(ctx, latestHost)
			if connectionErr == nil {
				connectionErr = verifySSHRequestBinding(req, currentDigest)
			}
		}
		if connectionErr == nil {
			connection, connectionErr = s.prepareSSHExecutionConnection(
				ctx, connection, currentDigest, req.Elevated, requiresDetectedShell(req.Mode, transportReq.Mode),
			)
		}
		err = connectionErr
	}
	if err != nil {
		run.Status = "failed"
		run.Error = err.Error()
		run.CompletedAt = time.Now().UTC()
		_ = s.store.UpdateRun(ctx, run)
		s.publishExecutionEvent(ExecutionEvent{SessionID: run.SessionID, RunID: run.ID, Status: run.Status})
		s.audit(ctx, run.ID, "command_failed", actor, map[string]any{"error": err.Error()})
		logger.ErrorContext(ctx, "SSH credential preparation failed", "error", err)
		return domain.ExecResult{RunID: run.ID, Status: run.Status, AutoApproved: autoApproved, CompletedAt: run.CompletedAt}, err
	}
	s.audit(ctx, run.ID, "command_started", actor, map[string]any{"digest": run.RequestDigest})
	s.publishExecutionEvent(ExecutionEvent{
		SessionID: run.SessionID,
		RunID:     run.ID,
		Status:    "running",
	})
	var raw sshx.RawResult
	var execErr error
	var tunnel *domain.SSHTunnel
	var shell *domain.SSHShell
	var outputSink *executionOutputSink
	if stream != nil || s.hasExecutionSubscribers(run.SessionID) || s.hasApprovalTask(run.ID) {
		outputSink = s.newExecutionOutputSink(run, stream)
	}
	if req.Mode == domain.ExecSSHTunnelStart {
		started := time.Now()
		created, tunnelErr := s.openSSHTunnel(ctx, host, connection, req, actor)
		execErr = tunnelErr
		raw.Duration = time.Since(started)
		if tunnelErr == nil {
			tunnel = &created
			raw.ExitCode = 0
			raw.Stdout, execErr = marshalSSHTunnel(created)
		} else {
			raw.ExitCode = -1
		}
	} else if req.Mode == domain.ExecSSHShellStart {
		started := time.Now()
		created, shellErr := s.openSSHShell(ctx, host, connection, req, run, actor)
		execErr = shellErr
		raw.Duration = time.Since(started)
		if shellErr == nil {
			shell = &created
			raw.ExitCode = 0
			raw.Stdout, execErr = marshalSSHShell(created)
		} else {
			raw.ExitCode = -1
		}
	} else if req.Mode == domain.ExecWorkspaceShellStart {
		started := time.Now()
		created, shellErr := s.openWorkspaceShell(ctx, host, req, run, actor)
		execErr = shellErr
		raw.Duration = time.Since(started)
		if shellErr == nil {
			shell = &created
			raw.ExitCode = 0
			raw.Stdout, execErr = marshalSSHShell(created)
		} else {
			raw.ExitCode = -1
		}
	} else if req.Mode == domain.ExecSSHFileTransfer {
		raw, execErr = s.executeSSHFileTransfer(ctx, run, req)
	} else if req.Mode == domain.ExecWorkspaceUpload {
		transport, ok := s.transport.(sshx.WorkspaceFileUploadTransport)
		if !ok {
			execErr = fmt.Errorf("configured SSH transport does not support Workspace file upload")
			raw.ExitCode = -1
		} else {
			raw, execErr = transport.UploadWorkspaceFile(ctx, connection, req, s.executionTransferReporter(run))
		}
	} else if req.Mode == domain.ExecWorkspaceDownload {
		raw, execErr = s.executeWorkspaceDownload(ctx, connection, req, run, actor)
	} else if isWorkspaceMode(req.Mode) {
		var workspaceStream func(string, []byte)
		if outputSink != nil {
			workspaceStream = outputSink.Write
		}
		raw, execErr = s.executeWorkspace(ctx, req, actor, workspaceStream)
	} else if streaming, ok := s.transport.(sshx.StreamingTransport); ok && outputSink != nil {
		raw, execErr = streaming.ExecStream(ctx, connection, transportReq, outputSink.Write)
	} else {
		raw, execErr = s.transport.Exec(ctx, connection, transportReq)
	}
	if outputSink != nil {
		outputSink.Flush()
	}
	run.ExitCode = raw.ExitCode
	run.StdoutRedacted = s.redactor.Redact(string(raw.Stdout))
	run.StderrRedacted = s.redactor.Redact(string(raw.Stderr))
	run.StdoutCipher, _ = s.encryptor.Encrypt(raw.Stdout)
	run.StderrCipher, _ = s.encryptor.Encrypt(raw.Stderr)
	run.CompletedAt = time.Now().UTC()
	if execErr != nil {
		run.Status = "failed"
		if errors.Is(execErr, context.Canceled) || errors.Is(execErr, context.DeadlineExceeded) ||
			errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			run.Status = "interrupted"
		}
		run.Error = execErr.Error()
	} else if raw.ExitCode != 0 {
		run.Error = "remote command exited with code " + strconv.Itoa(raw.ExitCode)
		if len(bytes.TrimSpace(raw.Stdout)) > 0 {
			run.Status = "partial"
		} else {
			run.Status = "failed"
		}
	} else {
		run.Status = "completed"
	}
	persistCtx := context.WithoutCancel(ctx)
	if err := s.store.UpdateRun(persistCtx, run); err != nil {
		logger.ErrorContext(ctx, "persist execution result failed", "error", err)
		return domain.ExecResult{}, err
	}
	s.publishExecutionEvent(ExecutionEvent{
		SessionID: run.SessionID,
		RunID:     run.ID,
		Status:    run.Status,
	})
	s.audit(persistCtx, run.ID, "command_completed", actor, map[string]any{"status": run.Status, "exit_code": run.ExitCode, "duration_ms": raw.Duration.Milliseconds()})
	completion := logger.InfoContext
	if run.Status == "failed" {
		completion = logger.ErrorContext
	}
	completion(ctx, "operation execution completed", "status", run.Status, "exit_code", run.ExitCode, "duration_ms", raw.Duration.Milliseconds(), "stdout_bytes", len(raw.Stdout), "stderr_bytes", len(raw.Stderr), "error", execErr)
	result := domain.ExecResult{
		RunID: run.ID, Status: run.Status, AutoApproved: autoApproved, ExitCode: run.ExitCode,
		Stdout: run.StdoutRedacted, Stderr: run.StderrRedacted,
		Duration: raw.Duration, Change: approvedReq.Change, Tunnel: tunnel, CompletedAt: run.CompletedAt,
		Shell: shell,
	}
	if (approvedReq.Mode == domain.ExecSSHShellStart || approvedReq.Mode == domain.ExecWorkspaceShellStart) && run.Status == "completed" {
		result.ShellUsage = sshShellUsage()
	}
	if run.Status == "completed" && (approvedReq.Mode == domain.ExecRemoteSearch || approvedReq.Mode == domain.ExecWorkspaceSearch) {
		decorateFileSearchResult(&result, approvedReq.SearchPattern, approvedReq.SearchMatchMode, approvedReq.ContextLines)
	}
	if run.Status == "completed" && (approvedReq.Mode == domain.ExecRemoteRead || approvedReq.Mode == domain.ExecWorkspaceRead) && result.Stdout != "" {
		path := approvedReq.RemotePath
		if approvedReq.Mode == domain.ExecWorkspaceRead {
			path = approvedReq.RelativePath
		}
		metadata, content := parseFileReadOutput(path, result.Stdout)
		if approvedReq.Mode == domain.ExecRemoteRead || approvedReq.TailLines == 0 {
			metadata.OffsetBytes = resolvedFileOffset(metadata.Size, approvedReq.OffsetBytes)
		}
		metadata.ReturnedBytes = len(content)
		decorateFileReadPage(&metadata, approvedReq.MaxBytes, approvedReq.TailLines)
		metadata.Sensitive = strings.Contains(content, "[REDACTED]")
		result.File, result.Stdout = &metadata, content
	}
	if approvedReq.Mode == domain.ExecRemoteRead && approvedReq.MetadataOnly {
		result.Stdout = ""
	}
	if approvedReq.Change != nil {
		path := approvedReq.RemotePath
		if approvedReq.Mode == domain.ExecWorkspaceEdit {
			path = approvedReq.RelativePath
		}
		metadata := parseFileEditOutput(path, approvedReq.Validator, result.Stdout, run.Status == "completed")
		result.File = &metadata
	}
	if approvedReq.Mode == domain.ExecWorkspaceDownload && run.Status == "completed" {
		var downloaded WorkspaceUploadResult
		if json.Unmarshal(raw.Stdout, &downloaded) == nil {
			result.File = &domain.FileMetadata{Path: downloaded.Path, Size: downloaded.Size, SHA256: downloaded.SHA256}
			result.Stdout = ""
		}
	}
	return result, execErr
}

func (s *Service) acquire(ctx context.Context, hostIDs ...string) (func(), error) {
	uniqueHostIDs := make([]string, 0, len(hostIDs))
	seen := make(map[string]struct{}, len(hostIDs))
	for _, hostID := range hostIDs {
		if _, exists := seen[hostID]; hostID == "" || exists {
			continue
		}
		seen[hostID] = struct{}{}
		uniqueHostIDs = append(uniqueHostIDs, hostID)
	}
	sort.Strings(uniqueHostIDs)
	s.semMu.Lock()
	hostSems := make([]chan struct{}, 0, len(uniqueHostIDs))
	for _, hostID := range uniqueHostIDs {
		hostSem := s.hostSems[hostID]
		if hostSem == nil {
			limit := s.limits.HostConcurrency
			if limit <= 0 {
				limit = 2
			}
			hostSem = make(chan struct{}, limit)
			s.hostSems[hostID] = hostSem
		}
		hostSems = append(hostSems, hostSem)
	}
	s.semMu.Unlock()
	select {
	case s.globalSem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	acquired := make([]chan struct{}, 0, len(hostSems))
	for _, hostSem := range hostSems {
		select {
		case hostSem <- struct{}{}:
			acquired = append(acquired, hostSem)
		case <-ctx.Done():
			for index := len(acquired) - 1; index >= 0; index-- {
				<-acquired[index]
			}
			<-s.globalSem
			return nil, ctx.Err()
		}
	}
	return func() {
		for index := len(acquired) - 1; index >= 0; index-- {
			<-acquired[index]
		}
		<-s.globalSem
	}, nil
}
