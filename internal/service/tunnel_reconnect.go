package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/observability"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"
	"github.com/Enterpr1se0/opsnerva/internal/store"
)

const (
	sshTunnelReconnectInitialDelay   = time.Second
	sshTunnelReconnectMaximumDelay   = 30 * time.Second
	sshTunnelReconnectAttemptTimeout = 30 * time.Second
)

// RetryOperatorSSHTunnel wakes the existing reconnect worker. Concurrent clicks
// coalesce into one pending attempt; they never create a second listener.
func (s *Service) RetryOperatorSSHTunnel(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.tunnelMu.RLock()
	defer s.tunnelMu.RUnlock()
	state := s.tunnels[strings.TrimSpace(id)]
	if state == nil {
		return store.ErrNotFound
	}
	if state.tunnel.Status != "retrying" || state.ctx.Err() != nil {
		return fmt.Errorf("invalid tunnel status %q: only disconnected tunnels can be retried", state.tunnel.Status)
	}
	select {
	case state.retry <- struct{}{}:
	default:
	}
	return nil
}

func (s *Service) reconnectSSHTunnel(state *sshTunnelState) *sshTunnelRuntime {
	transport, ok := s.transport.(sshx.TunnelTransport)
	if !ok {
		return nil
	}
	for attempt := 1; ; attempt++ {
		delay := sshTunnelReconnectDelay(attempt)
		s.tunnelMu.Lock()
		current, exists := s.tunnels[state.tunnel.ID]
		if !exists || current != state || state.tunnel.Status != "retrying" {
			s.tunnelMu.Unlock()
			return nil
		}
		state.tunnel.ReconnectAttempt = attempt
		snapshot := tunnelSnapshot(state)
		s.tunnelMu.Unlock()
		s.publishTunnelState(snapshot, false)

		timer := time.NewTimer(delay)
		select {
		case <-state.ctx.Done():
			timer.Stop()
			return nil
		case <-state.retry:
		case <-timer.C:
		}
		timer.Stop()

		attemptCtx, cancelAttempt := context.WithTimeout(state.ctx, sshTunnelReconnectAttemptTimeout)
		host, err := s.store.GetHost(attemptCtx, snapshot.HostID)
		var connection sshx.ConnectionSpec
		if err == nil {
			connection, _, err = s.resolveSSHConnection(attemptCtx, host)
		}
		if err == nil {
			connection, err = s.hydrateSSHConnection(connection, false)
		}
		var runtime *sshTunnelRuntime
		var localPort, remotePort int
		if err == nil {
			req := domain.ExecRequest{
				Mode: domain.ExecSSHTunnelStart, TunnelDirection: snapshot.Direction,
				TunnelLocalHost: snapshot.LocalHost, TunnelLocalPort: snapshot.LocalPort,
				TunnelRemoteHost: snapshot.RemoteHost, TunnelRemotePort: snapshot.RemotePort,
			}
			runtime, localPort, remotePort, err = openSSHTunnelRuntime(attemptCtx, state.ctx, transport, connection, req)
		}
		cancelAttempt()
		if state.ctx.Err() != nil {
			if runtime != nil {
				runtime.close()
			}
			return nil
		}
		if err != nil {
			failure := s.redactor.Redact(err.Error())
			s.tunnelMu.Lock()
			if current := s.tunnels[state.tunnel.ID]; current == state && state.tunnel.Status == "retrying" {
				state.tunnel.Error = failure
			}
			failedAttempt := tunnelSnapshot(state)
			s.tunnelMu.Unlock()
			s.publishTunnelState(failedAttempt, false)
			observability.FromContext(context.Background()).WarnContext(context.Background(), "SSH tunnel reconnect failed",
				"component", "ssh_tunnel", "tunnel_id", state.tunnel.ID, "host_id", state.tunnel.HostID,
				"attempt", attempt, "attempt_delay", delay, "error", failure)
			continue
		}
		if !state.installRuntime(runtime) {
			runtime.close()
			return nil
		}

		var reconnected domain.SSHTunnel
		s.tunnelMu.Lock()
		current, exists = s.tunnels[state.tunnel.ID]
		connected := exists && current == state && state.tunnel.Status == "retrying" && state.ctx.Err() == nil
		if connected {
			select {
			case <-state.retry:
			default:
			}
			state.tunnel.HostName = host.Name
			state.tunnel.LocalPort = localPort
			state.tunnel.RemotePort = remotePort
			state.tunnel.ProxyUsed = connection.Target.ProxyURL != "" || len(connection.Jumps) > 0
			state.tunnel.Status = "running"
			state.tunnel.Error = ""
			state.tunnel.ReconnectAttempt = 0
			reconnected = state.tunnel
		}
		s.tunnelMu.Unlock()
		if !connected {
			state.closeRuntime(runtime)
			return nil
		}
		s.publishTunnelState(reconnected, false)

		observability.FromContext(context.Background()).InfoContext(context.Background(), "SSH tunnel reconnected",
			"component", "ssh_tunnel", "tunnel_id", reconnected.ID, "host_id", reconnected.HostID,
			"attempt", attempt, "direction", reconnected.Direction,
			"local_host", reconnected.LocalHost, "local_port", localPort,
			"remote_host", reconnected.RemoteHost, "remote_port", remotePort)
		return runtime
	}
}

func sshTunnelReconnectDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := sshTunnelReconnectInitialDelay
	for index := 1; index < attempt && delay < sshTunnelReconnectMaximumDelay; index++ {
		delay *= 2
	}
	if delay > sshTunnelReconnectMaximumDelay {
		return sshTunnelReconnectMaximumDelay
	}
	return delay
}
