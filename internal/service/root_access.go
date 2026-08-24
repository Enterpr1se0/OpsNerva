package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"
)

var (
	ErrAgentRootAccessDenied    = errors.New("Agent root access is disabled")
	ErrHostAgentRootUnavailable = errors.New("host does not support Agent root access")
)

func hostSupportsRoot(host domain.Host) bool {
	if host.AuthType == "workspace" {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(host.User), "root") {
		return true
	}
	switch host.SudoMode {
	case "nopasswd":
		return true
	case "password":
		return host.SudoCipher != "" || host.HasSudoPassword
	default:
		return false
	}
}

func requireAgentRootAccess(actor string, host domain.Host, elevated bool) error {
	rootOperation := elevated || strings.EqualFold(strings.TrimSpace(host.User), "root")
	if actor != "eino-agent" || !rootOperation {
		return nil
	}
	if !host.AgentRootEnabled || !hostSupportsRoot(host) {
		return fmt.Errorf("%w for host %q; enable Agent root access for this host", ErrAgentRootAccessDenied, host.ID)
	}
	return nil
}

func authorizeAgentRootConnections(actor string, elevated bool, connections ...sshx.ConnectionSpec) error {
	for index, connection := range connections {
		if err := requireAgentRootAccess(actor, connection.Target, index == 0 && elevated); err != nil {
			return err
		}
		for _, jump := range connection.Jumps {
			if err := requireAgentRootAccess(actor, jump, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) requireCurrentAgentSSHAccess(ctx context.Context, actor, hostID string, elevated bool) error {
	if actor != "eino-agent" {
		return nil
	}
	host, err := s.store.GetHost(ctx, hostID)
	if err != nil {
		return err
	}
	if err := requireAgentHostAccess(actor, host); err != nil {
		return err
	}
	return requireAgentRootAccess(actor, host, elevated)
}

func (s *Service) authorizeApprovedAgentSSHExecution(ctx context.Context, actor string, host domain.Host, req domain.ExecRequest) error {
	if actor != "eino-agent" || isWorkspaceMode(req.Mode) {
		return nil
	}
	if req.Mode == domain.ExecSSHFileTransfer {
		connections, err := s.resolveSSHFileTransferConnections(ctx, host, req.SourceHostID)
		if err != nil {
			return err
		}
		if err := requireAgentSSHAccess(actor, connections.Destination); err != nil {
			return err
		}
		if err := requireAgentSSHAccess(actor, connections.Source); err != nil {
			return err
		}
		return authorizeAgentRootConnections(actor, req.Elevated, connections.Destination, connections.Source)
	}
	connection, _, err := s.resolveSSHConnection(ctx, host)
	if err != nil {
		return err
	}
	if err := requireAgentSSHAccess(actor, connection); err != nil {
		return err
	}
	return authorizeAgentRootConnections(actor, req.Elevated, connection)
}
