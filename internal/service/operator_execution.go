package service

import (
	"context"
	"fmt"
	"time"

	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/ids"
)

const webOperatorReason = "started directly by the operator from the Web console"

// executeOperatorRun persists operator-managed connections that need durable
// lifecycle state. Interactive App terminals use the separate in-memory shell
// runtime and must not pass through this Run/Audit path.
func (s *Service) executeOperatorRun(ctx context.Context, req domain.ExecRequest, actor string) (domain.ExecResult, error) {
	normalizeRequest(&req, s.limits)
	if req.Mode != domain.ExecSSHTunnelStart {
		return domain.ExecResult{}, fmt.Errorf("invalid operator connection mode")
	}
	if err := validateRequestLimits(req, s.limits, s.redactor); err != nil {
		return domain.ExecResult{}, err
	}
	host, err := s.store.GetHost(ctx, req.HostID)
	if err != nil {
		return domain.ExecResult{}, err
	}
	if req.Mode != domain.ExecWorkspaceShellStart {
		_, connectionDigest, err := s.resolveSSHConnection(ctx, host)
		if err != nil {
			return domain.ExecResult{}, err
		}
		bindSSHRequest(&req, connectionDigest)
	}
	if err := validateExecutionRequest(host, req); err != nil {
		return domain.ExecResult{}, err
	}
	requestJSON, requestDigest, err := canonicalRequest(req)
	if err != nil {
		return domain.ExecResult{}, err
	}
	requestCipher, err := s.encryptor.Encrypt([]byte(requestJSON))
	if err != nil {
		return domain.ExecResult{}, err
	}
	run := domain.Run{
		ID: ids.New("run"), HostID: host.ID,
		RequestJSON: s.redactor.Redact(requestJSON), RequestCipher: requestCipher,
		SearchText: s.redactor.Redact(req.SearchText()), RequestDigest: requestDigest,
		Status: "running", StartedAt: time.Now().UTC(),
	}
	if err := s.store.CreateRun(ctx, run); err != nil {
		return domain.ExecResult{}, err
	}
	s.audit(context.WithoutCancel(ctx), run.ID, "operator_connection_requested", actor, map[string]any{
		"host_id": host.ID, "mode": req.Mode,
	})
	return s.execute(ctx, host, req, run, actor, nil)
}
