package service

import (
	"context"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/observability"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"
)

const (
	hostCatalogProbeTimeout    = 5 * time.Second
	maxConcurrentCatalogProbes = 4
	unknownHostShell           = "unknown"
)

func (s *Service) ListHostCapabilities(ctx context.Context) ([]domain.HostCapability, error) {
	hosts, err := s.store.ListHosts(ctx)
	if err != nil {
		return nil, err
	}
	type candidate struct {
		host       domain.Host
		connection sshx.ConnectionSpec
		binding    string
		index      int
	}
	result := make([]domain.HostCapability, 0, len(hosts))
	candidates := make([]candidate, 0, len(hosts))
	for _, host := range hosts {
		if !host.AgentEnabled {
			continue
		}
		connection, binding, resolveErr := s.resolveSSHConnection(ctx, host)
		if resolveErr != nil || requireAgentSSHAccess("eino-agent", connection) != nil || authorizeAgentRootConnections("eino-agent", false, connection) != nil {
			continue
		}
		capability := hostCapability(host)
		capability.Shell = detectedShellName(connection.ShellPath)
		result = append(result, capability)
		if capability.Shell == unknownHostShell {
			candidates = append(candidates, candidate{host: host, connection: connection, binding: binding, index: len(result) - 1})
		}
	}

	probeCtx, cancelProbes := context.WithTimeout(ctx, hostCatalogProbeTimeout)
	defer cancelProbes()
	probeSlots := make(chan struct{}, maxConcurrentCatalogProbes)
	var probes sync.WaitGroup
	for candidateIndex := range candidates {
		candidateIndex := candidateIndex
		probes.Add(1)
		go func() {
			defer probes.Done()
			candidate := candidates[candidateIndex]
			select {
			case probeSlots <- struct{}{}:
				prepared, detectErr := s.prepareSSHExecutionConnection(probeCtx, candidate.connection, candidate.binding, false, true)
				<-probeSlots
				if detectErr == nil {
					result[candidate.index].Shell = detectedShellName(prepared.ShellPath)
				}
				if detectErr != nil {
					observability.FromContext(ctx).WarnContext(ctx, "SSH shell detection failed",
						"component", "host_catalog", "host_id", candidate.host.ID, "error", detectErr)
				}
			case <-probeCtx.Done():
			}
		}()
	}
	probes.Wait()
	return result, nil
}

func detectedShellName(shellPath string) string {
	shell := path.Base(strings.TrimSpace(shellPath))
	if shell == "bash" || shell == "sh" {
		return shell
	}
	return unknownHostShell
}
