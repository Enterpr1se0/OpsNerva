package service

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/sshx"
)

var transferSHA256Pattern = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)

func (s *Service) TransferFileBetweenHosts(ctx context.Context, sourceHostID, sourcePath, expectedSHA256, destinationHostID, destinationPath string, overwrite bool, expectedDestinationSHA256 string, timeoutSeconds int, reason, actor string) (domain.ExecResult, error) {
	return s.Submit(ctx, domain.ExecRequest{
		HostID: destinationHostID, Mode: domain.ExecSSHFileTransfer,
		SourceHostID: sourceHostID, SourcePath: sourcePath, ExpectedSHA256: expectedSHA256,
		RemotePath: destinationPath, Overwrite: overwrite, ExpectedDestinationSHA256: expectedDestinationSHA256,
		TimeoutSeconds: timeoutSeconds, Reason: reason,
	}, actor)
}

func (s *Service) bindSSHFileTransfer(ctx context.Context, destination domain.Host, req *domain.ExecRequest, actor string) (domain.Host, error) {
	if err := validateSSHFileTransferRequest(*req); err != nil {
		return domain.Host{}, err
	}
	source, err := s.store.GetHost(ctx, req.SourceHostID)
	if err != nil {
		return domain.Host{}, fmt.Errorf("load source SSH host: %w", err)
	}
	destinationConnection, destinationDigest, err := s.resolveSSHConnection(ctx, destination)
	if err != nil {
		return domain.Host{}, fmt.Errorf("resolve destination SSH connection: %w", err)
	}
	if err := requireAgentSSHAccess(actor, destinationConnection); err != nil {
		return domain.Host{}, err
	}
	sourceConnection, sourceDigest, err := s.resolveSSHConnection(ctx, source)
	if err != nil {
		return domain.Host{}, fmt.Errorf("resolve source SSH connection: %w", err)
	}
	if err := requireAgentSSHAccess(actor, sourceConnection); err != nil {
		return domain.Host{}, err
	}
	bindSSHRequest(req, destinationDigest)
	bindSSHTransferSource(req, sourceDigest)
	return source, nil
}

func validateSSHFileTransferRequest(req domain.ExecRequest) error {
	if strings.TrimSpace(req.SourceHostID) == "" || strings.TrimSpace(req.HostID) == "" {
		return fmt.Errorf("source_host_id and destination_host_id are required")
	}
	if req.SourceHostID == req.HostID {
		return fmt.Errorf("source and destination SSH hosts must be different")
	}
	if !cleanAbsoluteRemotePath(req.SourcePath) {
		return fmt.Errorf("source_path must be a clean absolute path")
	}
	if !cleanAbsoluteRemotePath(req.RemotePath) {
		return fmt.Errorf("destination_path must be a clean absolute path")
	}
	if !transferSHA256Pattern.MatchString(req.ExpectedSHA256) {
		return fmt.Errorf("expected_sha256 must be the 64-character SHA256 returned for the source file")
	}
	if req.Overwrite {
		if !transferSHA256Pattern.MatchString(req.ExpectedDestinationSHA256) {
			return fmt.Errorf("expected_destination_sha256 is required when overwrite is true")
		}
	} else if req.ExpectedDestinationSHA256 != "" {
		return fmt.Errorf("expected_destination_sha256 is only valid when overwrite is true")
	}
	return nil
}

func cleanAbsoluteRemotePath(value string) bool {
	return path.IsAbs(value) && path.Clean(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

func mergeTransferDecisions(destination, source domain.Decision) domain.Decision {
	risk := destination.Risk
	if riskRank(source.Risk) > riskRank(risk) {
		risk = source.Risk
	}
	action := destination.Action
	if actionRank(source.Action) > actionRank(action) {
		action = source.Action
	}
	hits := make([]string, 0, len(destination.RuleHits)+len(source.RuleHits))
	for _, hit := range destination.RuleHits {
		hits = append(hits, "destination:"+hit)
	}
	for _, hit := range source.RuleHits {
		hits = append(hits, "source:"+hit)
	}
	sort.Strings(hits)
	reason := destination.Reason
	if actionRank(source.Action) > actionRank(destination.Action) {
		reason = source.Reason
	}
	return domain.Decision{Risk: risk, Action: action, Reason: reason, RuleHits: hits}
}

func riskRank(risk domain.RiskLevel) int {
	switch risk {
	case domain.RiskForbidden:
		return 4
	case domain.RiskCritical:
		return 3
	case domain.RiskChange:
		return 2
	case domain.RiskReadOnly:
		return 1
	default:
		return 0
	}
}

func actionRank(action domain.DecisionAction) int {
	switch action {
	case domain.ActionDeny:
		return 4
	case domain.ActionBreakGlass:
		return 3
	case domain.ActionApprove:
		return 2
	case domain.ActionAllow:
		return 1
	default:
		return 0
	}
}

func (s *Service) executeSSHFileTransfer(ctx context.Context, req domain.ExecRequest) (sshx.RawResult, error) {
	destinationHost, err := s.store.GetHost(ctx, req.HostID)
	if err != nil {
		return sshx.RawResult{}, fmt.Errorf("reload destination SSH host: %w", err)
	}
	destination, destinationDigest, err := s.resolveSSHConnection(ctx, destinationHost)
	if err != nil {
		return sshx.RawResult{}, fmt.Errorf("resolve destination SSH connection: %w", err)
	}
	if err := verifySSHRequestBinding(req, destinationDigest); err != nil {
		return sshx.RawResult{}, err
	}
	destination, err = s.hydrateSSHConnection(destination, false)
	if err != nil {
		return sshx.RawResult{}, fmt.Errorf("prepare destination SSH credentials: %w", err)
	}

	sourceHost, err := s.store.GetHost(ctx, req.SourceHostID)
	if err != nil {
		return sshx.RawResult{}, fmt.Errorf("reload source SSH host: %w", err)
	}
	source, sourceDigest, err := s.resolveSSHConnection(ctx, sourceHost)
	if err != nil {
		return sshx.RawResult{}, fmt.Errorf("resolve source SSH connection: %w", err)
	}
	if err := verifySSHTransferSourceBinding(req, sourceDigest); err != nil {
		return sshx.RawResult{}, err
	}
	source, err = s.hydrateSSHConnection(source, false)
	if err != nil {
		return sshx.RawResult{}, fmt.Errorf("prepare source SSH credentials: %w", err)
	}

	transport, ok := s.transport.(sshx.HostFileTransferTransport)
	if !ok {
		return sshx.RawResult{}, fmt.Errorf("configured SSH transport does not support host-to-host file transfer")
	}
	return transport.TransferFile(ctx, source, destination, req)
}
