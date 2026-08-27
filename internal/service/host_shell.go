package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"
)

const (
	hostShellDetectionTimeout = 15 * time.Second
	hostShellStorageTimeout   = 5 * time.Second
)

func (s *Service) prepareSSHExecutionConnection(
	ctx context.Context,
	connection sshx.ConnectionSpec,
	binding string,
	includeSudo bool,
	requireShell bool,
) (sshx.ConnectionSpec, error) {
	hydrated, err := s.hydrateSSHConnection(connection, includeSudo)
	if err != nil {
		return sshx.ConnectionSpec{}, err
	}
	if !requireShell || detectedShellName(hydrated.ShellPath) != unknownHostShell {
		return hydrated, nil
	}
	shellPath, err := s.detectAndStoreHostShell(ctx, connection.Target.ID, binding, hydrated)
	if err != nil {
		return sshx.ConnectionSpec{}, err
	}
	hydrated.ShellPath = shellPath
	return hydrated, nil
}

func (s *Service) detectAndStoreHostShell(ctx context.Context, hostID, binding string, connection sshx.ConnectionSpec) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	result := s.hostShellProbes.DoChan(binding, func() (any, error) {
		s.executionMu.Lock()
		if s.executionClosed {
			s.executionMu.Unlock()
			return "", fmt.Errorf("service is shutting down")
		}
		s.executionWG.Add(1)
		s.executionMu.Unlock()
		defer s.executionWG.Done()

		probeCtx, cancel := context.WithTimeout(s.executionCtx, hostShellDetectionTimeout)
		defer cancel()
		info, probeErr := s.transport.Probe(probeCtx, connection)
		if probeErr != nil {
			return "", probeErr
		}
		shellPath, detectErr := detectedShellPathFromHostInfo(info)
		if detectErr != nil {
			return "", detectErr
		}
		if storeErr := s.storeDetectedHostShell(probeCtx, hostID, binding, shellPath); storeErr != nil {
			return "", storeErr
		}
		return shellPath, nil
	})
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case completed := <-result:
		if completed.Err != nil {
			return "", completed.Err
		}
		shellPath, ok := completed.Val.(string)
		if !ok {
			return "", fmt.Errorf("SSH shell detection returned an invalid result")
		}
		return shellPath, nil
	}
}

func detectedShellPathFromHostInfo(info sshx.HostInfo) (string, error) {
	shellPath := strings.TrimSpace(info.ShellPath)
	if detectedShellName(shellPath) == unknownHostShell {
		return "", fmt.Errorf("SSH probe did not return a supported shell path")
	}
	return shellPath, nil
}

func (s *Service) storeDetectedHostShell(ctx context.Context, hostID, binding, shellPath string) error {
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hostShellStorageTimeout)
	defer cancel()
	if err := s.store.SetHostDetectedShell(persistCtx, hostID, binding, shellPath); err != nil {
		return fmt.Errorf("store detected shell for host %q: %w", hostID, err)
	}
	return nil
}

func requiresDetectedShell(requestMode, transportMode domain.ExecMode) bool {
	return requestMode == domain.ExecSSHShellStart || transportMode == domain.ExecScript
}
