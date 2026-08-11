package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/ids"
	"eino-ops-agent/internal/observability"
	"eino-ops-agent/internal/sshx"
	"eino-ops-agent/internal/store"
)

const sshTunnelDefaultHost = "127.0.0.1"

type sshTunnelStateIDContextKey struct{}

var (
	sshTunnelHostnameRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9_.-]{0,251}[A-Za-z0-9])?$`)
	// ErrHostHasActiveTunnel prevents deleting connection settings still used by a forwarding worker.
	ErrHostHasActiveTunnel = errors.New("host has an active SSH tunnel")
)

type sshTunnelState struct {
	tunnel          domain.SSHTunnel
	listener        net.Listener
	client          sshx.TunnelClient
	cancel          context.CancelFunc
	closeOnce       sync.Once
	connections     sync.WaitGroup
	connectionMu    sync.Mutex
	openConnections map[net.Conn]struct{}
	closing         bool
	active          atomic.Int64
	total           atomic.Int64
	sent            atomic.Int64
	received        atomic.Int64
}

func (state *sshTunnelState) closeResources() {
	state.closeOnce.Do(func() {
		state.cancel()
		state.connectionMu.Lock()
		state.closing = true
		connections := make([]net.Conn, 0, len(state.openConnections))
		for connection := range state.openConnections {
			connections = append(connections, connection)
		}
		state.connectionMu.Unlock()
		_ = state.listener.Close()
		_ = state.client.Close()
		for _, connection := range connections {
			_ = connection.Close()
		}
	})
}

func (state *sshTunnelState) trackConnection(connection net.Conn) bool {
	state.connectionMu.Lock()
	defer state.connectionMu.Unlock()
	if state.closing {
		_ = connection.Close()
		return false
	}
	state.openConnections[connection] = struct{}{}
	return true
}

func (state *sshTunnelState) untrackConnection(connection net.Conn) {
	state.connectionMu.Lock()
	delete(state.openConnections, connection)
	state.connectionMu.Unlock()
}

func (s *Service) StartSSHTunnel(ctx context.Context, hostID string, config domain.SSHTunnelConfig, reason, actor string) (domain.ExecResult, error) {
	return s.Submit(ctx, domain.ExecRequest{
		HostID:           strings.TrimSpace(hostID),
		Mode:             domain.ExecSSHTunnelStart,
		Reason:           strings.TrimSpace(reason),
		TunnelDirection:  config.Direction,
		TunnelLocalHost:  config.LocalHost,
		TunnelLocalPort:  config.LocalPort,
		TunnelRemoteHost: config.RemoteHost,
		TunnelRemotePort: config.RemotePort,
	}, actor)
}

// StartOperatorSSHTunnel starts a tunnel after an authenticated Web operator
// explicitly submits the form, without entering the Agent approval path.
func (s *Service) StartOperatorSSHTunnel(ctx context.Context, hostID string, config domain.SSHTunnelConfig, actor string) (domain.SSHTunnel, error) {
	return s.startOperatorSSHTunnel(ctx, hostID, config, actor, "")
}

func (s *Service) startOperatorSSHTunnel(ctx context.Context, hostID string, config domain.SSHTunnelConfig, actor, stateID string) (domain.SSHTunnel, error) {
	if stateID = strings.TrimSpace(stateID); stateID != "" {
		ctx = context.WithValue(ctx, sshTunnelStateIDContextKey{}, stateID)
	}
	result, err := s.executeOperatorRun(ctx, domain.ExecRequest{
		HostID:           strings.TrimSpace(hostID),
		Mode:             domain.ExecSSHTunnelStart,
		Reason:           webOperatorReason,
		TunnelDirection:  config.Direction,
		TunnelLocalHost:  config.LocalHost,
		TunnelLocalPort:  config.LocalPort,
		TunnelRemoteHost: config.RemoteHost,
		TunnelRemotePort: config.RemotePort,
	}, actor)
	if err != nil {
		return domain.SSHTunnel{}, err
	}
	if result.Tunnel == nil {
		return domain.SSHTunnel{}, fmt.Errorf("SSH tunnel start completed without tunnel state")
	}
	return *result.Tunnel, nil
}

// UpdateOperatorSSHTunnel replaces an operator tunnel. Invalid target host or
// forwarding input is rejected before the existing listener is touched;
// runtime replacement failures trigger a best-effort rollback.
func (s *Service) UpdateOperatorSSHTunnel(ctx context.Context, id, hostID string, config domain.SSHTunnelConfig, actor string) (domain.SSHTunnel, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return domain.SSHTunnel{}, fmt.Errorf("tunnel_id is required")
	}
	hostID = strings.TrimSpace(hostID)
	if hostID == "" {
		return domain.SSHTunnel{}, fmt.Errorf("host_id is required")
	}
	config, err := s.normalizedSSHTunnelConfig(config)
	if err != nil {
		return domain.SSHTunnel{}, err
	}

	s.tunnelMu.RLock()
	state, ok := s.tunnels[id]
	if !ok {
		s.tunnelMu.RUnlock()
		return domain.SSHTunnel{}, store.ErrNotFound
	}
	previous := tunnelSnapshot(state)
	s.tunnelMu.RUnlock()
	if previous.Status != "running" && previous.Status != "failed" {
		return domain.SSHTunnel{}, fmt.Errorf("invalid tunnel status %q: only running or failed tunnels can be edited", previous.Status)
	}
	if previous.HostID == hostID && previous.Direction == config.Direction && previous.LocalHost == config.LocalHost && previous.LocalPort == config.LocalPort &&
		previous.RemoteHost == config.RemoteHost && previous.RemotePort == config.RemotePort {
		return previous, nil
	}
	if err := s.validateOperatorSSHTunnelTarget(ctx, hostID, config); err != nil {
		return domain.SSHTunnel{}, err
	}

	if _, err := s.StopSSHTunnel(ctx, id, actor); err != nil {
		return domain.SSHTunnel{}, err
	}
	replacement, err := s.StartOperatorSSHTunnel(ctx, hostID, config, actor)
	if err == nil {
		s.audit(context.WithoutCancel(ctx), "", "ssh_tunnel_updated", actor, map[string]any{
			"tunnel_id": replacement.ID, "previous_tunnel_id": previous.ID, "host_id": replacement.HostID,
			"direction": replacement.Direction, "local_host": replacement.LocalHost, "local_port": replacement.LocalPort,
			"remote_host": replacement.RemoteHost, "remote_port": replacement.RemotePort,
		})
		return replacement, nil
	}

	updateErr := err
	rollbackCtx, cancelRollback := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancelRollback()
	restored, rollbackErr := s.startOperatorSSHTunnel(rollbackCtx, previous.HostID, domain.SSHTunnelConfig{
		Direction: previous.Direction, LocalHost: previous.LocalHost, LocalPort: previous.LocalPort,
		RemoteHost: previous.RemoteHost, RemotePort: previous.RemotePort,
	}, actor, previous.ID)
	if rollbackErr == nil {
		s.audit(context.WithoutCancel(ctx), "", "ssh_tunnel_update_rolled_back", actor, map[string]any{
			"tunnel_id": restored.ID, "previous_tunnel_id": previous.ID, "host_id": restored.HostID,
		})
		return domain.SSHTunnel{}, fmt.Errorf("update SSH tunnel: %w; previous tunnel restored", updateErr)
	}

	previous.Status = "failed"
	previous.Error = s.redactor.Redact(fmt.Sprintf("update failed: %v; restore failed: %v", updateErr, rollbackErr))
	s.tunnelMu.Lock()
	if _, exists := s.tunnels[previous.ID]; !exists {
		state.tunnel = previous
		s.tunnels[previous.ID] = state
	}
	s.tunnelMu.Unlock()
	return domain.SSHTunnel{}, fmt.Errorf("update SSH tunnel: %w; restore previous tunnel: %v", updateErr, rollbackErr)
}

func (s *Service) validateOperatorSSHTunnelTarget(ctx context.Context, hostID string, config domain.SSHTunnelConfig) error {
	host, err := s.store.GetHost(ctx, hostID)
	if err != nil {
		return err
	}
	_, connectionDigest, err := s.resolveSSHConnection(ctx, host)
	if err != nil {
		return err
	}
	req := domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecSSHTunnelStart, Reason: "started directly by the operator from the Web console",
		TunnelDirection: config.Direction, TunnelLocalHost: config.LocalHost, TunnelLocalPort: config.LocalPort,
		TunnelRemoteHost: config.RemoteHost, TunnelRemotePort: config.RemotePort,
	}
	bindSSHRequest(&req, connectionDigest)
	return validateExecutionRequest(host, req)
}

func (s *Service) normalizedSSHTunnelConfig(config domain.SSHTunnelConfig) (domain.SSHTunnelConfig, error) {
	req := domain.ExecRequest{
		Mode: domain.ExecSSHTunnelStart, TunnelDirection: config.Direction,
		TunnelLocalHost: config.LocalHost, TunnelLocalPort: config.LocalPort,
		TunnelRemoteHost: config.RemoteHost, TunnelRemotePort: config.RemotePort,
	}
	normalizeRequest(&req, s.limits)
	if err := validateSSHTunnelRequest(req); err != nil {
		return domain.SSHTunnelConfig{}, err
	}
	return domain.SSHTunnelConfig{
		Direction: req.TunnelDirection, LocalHost: req.TunnelLocalHost, LocalPort: req.TunnelLocalPort,
		RemoteHost: req.TunnelRemoteHost, RemotePort: req.TunnelRemotePort,
	}, nil
}

func (s *Service) ListSSHTunnels() domain.SSHTunnelList {
	s.tunnelMu.RLock()
	tunnels := make([]domain.SSHTunnel, 0, len(s.tunnels))
	for _, state := range s.tunnels {
		tunnels = append(tunnels, tunnelSnapshot(state))
	}
	s.tunnelMu.RUnlock()
	sort.Slice(tunnels, func(left, right int) bool {
		return tunnels[left].StartedAt.Before(tunnels[right].StartedAt)
	})
	return domain.SSHTunnelList{Tunnels: tunnels, Count: len(tunnels)}
}

func (s *Service) StopSSHTunnel(ctx context.Context, id, actor string) (domain.SSHTunnel, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return domain.SSHTunnel{}, fmt.Errorf("tunnel_id is required")
	}
	s.tunnelMu.Lock()
	state, ok := s.tunnels[id]
	if !ok {
		s.tunnelMu.Unlock()
		return domain.SSHTunnel{}, store.ErrNotFound
	}
	state.tunnel.Status = "stopping"
	s.tunnelMu.Unlock()

	state.closeResources()

	s.tunnelMu.Lock()
	state.tunnel.Status = "stopped"
	stopped := tunnelSnapshot(state)
	delete(s.tunnels, id)
	s.tunnelMu.Unlock()
	s.audit(context.WithoutCancel(ctx), "", "ssh_tunnel_stopped", actor, map[string]any{
		"tunnel_id": id, "host_id": stopped.HostID, "direction": stopped.Direction,
		"local_host": stopped.LocalHost, "local_port": stopped.LocalPort,
		"remote_host": stopped.RemoteHost, "remote_port": stopped.RemotePort,
	})
	observability.FromContext(ctx).InfoContext(ctx, "SSH tunnel stopped",
		"component", "ssh_tunnel", "tunnel_id", id, "host_id", stopped.HostID,
		"direction", stopped.Direction, "local_host", stopped.LocalHost, "local_port", stopped.LocalPort,
		"remote_host", stopped.RemoteHost, "remote_port", stopped.RemotePort)
	return stopped, nil
}

// RetryOperatorSSHTunnel replaces a failed tunnel while retaining its failure
// record until the replacement has connected successfully.
func (s *Service) RetryOperatorSSHTunnel(ctx context.Context, id, actor string) (domain.SSHTunnel, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return domain.SSHTunnel{}, fmt.Errorf("tunnel_id is required")
	}

	s.tunnelMu.Lock()
	state, ok := s.tunnels[id]
	if !ok {
		s.tunnelMu.Unlock()
		return domain.SSHTunnel{}, store.ErrNotFound
	}
	if state.tunnel.Status != "failed" {
		status := state.tunnel.Status
		s.tunnelMu.Unlock()
		return domain.SSHTunnel{}, fmt.Errorf("invalid tunnel status %q: only failed tunnels can be retried", status)
	}
	failed := tunnelSnapshot(state)
	state.tunnel.Status = "retrying"
	state.tunnel.Error = ""
	s.tunnelMu.Unlock()

	retried, err := s.StartOperatorSSHTunnel(ctx, failed.HostID, domain.SSHTunnelConfig{
		Direction: failed.Direction, LocalHost: failed.LocalHost, LocalPort: failed.LocalPort,
		RemoteHost: failed.RemoteHost, RemotePort: failed.RemotePort,
	}, actor)
	if err != nil {
		s.tunnelMu.Lock()
		if current, exists := s.tunnels[id]; exists && current == state && state.tunnel.Status == "retrying" {
			state.tunnel.Status = "failed"
			state.tunnel.Error = s.redactor.Redact(err.Error())
		}
		s.tunnelMu.Unlock()
		return domain.SSHTunnel{}, err
	}

	s.tunnelMu.Lock()
	current, exists := s.tunnels[id]
	committed := exists && current == state && state.tunnel.Status == "retrying"
	if committed {
		delete(s.tunnels, id)
	}
	s.tunnelMu.Unlock()
	if !committed {
		_, _ = s.StopSSHTunnel(context.WithoutCancel(ctx), retried.ID, actor)
		return domain.SSHTunnel{}, fmt.Errorf("SSH tunnel retry was canceled: %w", store.ErrNotFound)
	}

	s.audit(context.WithoutCancel(ctx), "", "ssh_tunnel_retried", actor, map[string]any{
		"tunnel_id": retried.ID, "previous_tunnel_id": id, "host_id": retried.HostID,
		"direction": retried.Direction, "local_host": retried.LocalHost, "local_port": retried.LocalPort,
		"remote_host": retried.RemoteHost, "remote_port": retried.RemotePort,
	})
	observability.FromContext(ctx).InfoContext(ctx, "SSH tunnel retried",
		"component", "ssh_tunnel", "tunnel_id", retried.ID, "previous_tunnel_id", id,
		"host_id", retried.HostID, "direction", retried.Direction,
		"local_host", retried.LocalHost, "local_port", retried.LocalPort,
		"remote_host", retried.RemoteHost, "remote_port", retried.RemotePort)
	return retried, nil
}

func (s *Service) openSSHTunnel(ctx context.Context, host domain.Host, connection sshx.ConnectionSpec, req domain.ExecRequest, actor string) (domain.SSHTunnel, error) {
	transport, ok := s.transport.(sshx.TunnelTransport)
	if !ok {
		return domain.SSHTunnel{}, fmt.Errorf("configured SSH transport does not support port forwarding")
	}
	if err := validateSSHTunnelRequest(req); err != nil {
		return domain.SSHTunnel{}, err
	}

	s.executionMu.Lock()
	if s.executionClosed {
		s.executionMu.Unlock()
		return domain.SSHTunnel{}, fmt.Errorf("service is shutting down")
	}
	s.executionWG.Add(1)
	s.executionMu.Unlock()
	workerStarted := false
	defer func() {
		if !workerStarted {
			s.executionWG.Done()
		}
	}()

	tunnelCtx, cancelTunnel := context.WithCancel(s.executionCtx)
	startupCtx, cancelStartup := context.WithCancel(ctx)
	stopStartup := context.AfterFunc(tunnelCtx, cancelStartup)
	client, err := transport.OpenTunnel(startupCtx, connection)
	stopStartup()
	cancelStartup()
	if err != nil {
		cancelTunnel()
		return domain.SSHTunnel{}, err
	}

	var listener net.Listener
	switch req.TunnelDirection {
	case domain.SSHTunnelDirectionLocal:
		listener, err = net.Listen("tcp", net.JoinHostPort(req.TunnelLocalHost, strconv.Itoa(req.TunnelLocalPort)))
		if err != nil {
			cancelTunnel()
			_ = client.Close()
			return domain.SSHTunnel{}, fmt.Errorf("listen on local endpoint %s:%d: %w", req.TunnelLocalHost, req.TunnelLocalPort, err)
		}
	case domain.SSHTunnelDirectionReverse:
		reverseClient, ok := client.(sshx.ReverseTunnelClient)
		if !ok {
			cancelTunnel()
			_ = client.Close()
			return domain.SSHTunnel{}, fmt.Errorf("configured SSH transport does not support reverse port forwarding")
		}
		listener, err = reverseClient.Listen("tcp", net.JoinHostPort(req.TunnelRemoteHost, strconv.Itoa(req.TunnelRemotePort)))
		if err != nil {
			cancelTunnel()
			_ = client.Close()
			return domain.SSHTunnel{}, fmt.Errorf("listen on remote endpoint %s:%d: %w", req.TunnelRemoteHost, req.TunnelRemotePort, err)
		}
	default:
		cancelTunnel()
		_ = client.Close()
		return domain.SSHTunnel{}, fmt.Errorf("invalid SSH tunnel direction %q", req.TunnelDirection)
	}
	_, listenerPortText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		cancelTunnel()
		_ = listener.Close()
		_ = client.Close()
		return domain.SSHTunnel{}, fmt.Errorf("resolve SSH tunnel listener address: %w", err)
	}
	listenerPort, err := strconv.Atoi(listenerPortText)
	if err != nil {
		cancelTunnel()
		_ = listener.Close()
		_ = client.Close()
		return domain.SSHTunnel{}, fmt.Errorf("resolve SSH tunnel listener port: %w", err)
	}
	localPort, remotePort := req.TunnelLocalPort, req.TunnelRemotePort
	if req.TunnelDirection == domain.SSHTunnelDirectionLocal {
		localPort = listenerPort
	} else {
		remotePort = listenerPort
	}

	proxyUsed := connection.Target.ProxyURL != "" || len(connection.Jumps) > 0
	tunnelID, _ := ctx.Value(sshTunnelStateIDContextKey{}).(string)
	tunnelID = strings.TrimSpace(tunnelID)
	if tunnelID == "" {
		tunnelID = ids.New("tunnel")
	}
	state := &sshTunnelState{
		tunnel: domain.SSHTunnel{
			ID: tunnelID, HostID: host.ID, HostName: host.Name,
			Direction: req.TunnelDirection, LocalHost: req.TunnelLocalHost, LocalPort: localPort,
			RemoteHost: req.TunnelRemoteHost, RemotePort: remotePort,
			Status: "running", ProxyUsed: proxyUsed, StartedAt: time.Now().UTC(),
		},
		listener: listener, client: client, cancel: cancelTunnel, openConnections: make(map[net.Conn]struct{}),
	}
	s.tunnelMu.Lock()
	s.tunnels[state.tunnel.ID] = state
	startedTunnel := tunnelSnapshot(state)
	s.tunnelMu.Unlock()
	workerStarted = true
	go s.runSSHTunnel(tunnelCtx, state)

	s.audit(context.WithoutCancel(ctx), "", "ssh_tunnel_started", actor, map[string]any{
		"tunnel_id": startedTunnel.ID, "host_id": host.ID, "direction": startedTunnel.Direction,
		"local_host": startedTunnel.LocalHost, "local_port": startedTunnel.LocalPort,
		"remote_host": startedTunnel.RemoteHost, "remote_port": startedTunnel.RemotePort, "proxy_used": proxyUsed,
	})
	observability.FromContext(ctx).InfoContext(ctx, "SSH tunnel started",
		"component", "ssh_tunnel", "tunnel_id", startedTunnel.ID, "host_id", host.ID,
		"direction", startedTunnel.Direction, "local_host", startedTunnel.LocalHost,
		"local_port", startedTunnel.LocalPort, "remote_host", startedTunnel.RemoteHost,
		"remote_port", startedTunnel.RemotePort, "proxy_used", proxyUsed)
	return startedTunnel, nil
}

func (s *Service) runSSHTunnel(ctx context.Context, state *sshTunnelState) {
	defer s.executionWG.Done()
	acceptErrors := make(chan error, 1)
	clientErrors := make(chan error, 1)
	state.connections.Add(1)
	go func() {
		defer state.connections.Done()
		acceptErrors <- s.acceptSSHTunnelConnections(ctx, state)
	}()
	go func() { clientErrors <- state.client.Wait() }()

	var terminalErr error
	select {
	case <-ctx.Done():
	case terminalErr = <-acceptErrors:
	case terminalErr = <-clientErrors:
	}
	unexpected := ctx.Err() == nil
	state.closeResources()
	state.connections.Wait()

	s.tunnelMu.Lock()
	current, exists := s.tunnels[state.tunnel.ID]
	if exists && current == state && state.tunnel.Status != "stopping" && state.tunnel.Status != "stopped" {
		if unexpected {
			state.tunnel.Status = "failed"
			if terminalErr != nil {
				state.tunnel.Error = s.redactor.Redact(terminalErr.Error())
			} else {
				state.tunnel.Error = "SSH tunnel connection closed"
			}
		} else {
			delete(s.tunnels, state.tunnel.ID)
		}
	}
	s.tunnelMu.Unlock()
	if terminalErr != nil && unexpected {
		observability.FromContext(context.Background()).WarnContext(context.Background(), "SSH tunnel stopped unexpectedly",
			"component", "ssh_tunnel", "tunnel_id", state.tunnel.ID, "host_id", state.tunnel.HostID,
			"error", s.redactor.Redact(terminalErr.Error()))
	}
}

func (s *Service) acceptSSHTunnelConnections(ctx context.Context, state *sshTunnelState) error {
	targetAddress := net.JoinHostPort(state.tunnel.RemoteHost, strconv.Itoa(state.tunnel.RemotePort))
	acceptSide := "local"
	if state.tunnel.Direction == domain.SSHTunnelDirectionReverse {
		targetAddress = net.JoinHostPort(state.tunnel.LocalHost, strconv.Itoa(state.tunnel.LocalPort))
		acceptSide = "remote"
	}
	for {
		inbound, err := state.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept %s tunnel connection: %w", acceptSide, err)
		}
		if !state.trackConnection(inbound) {
			return nil
		}
		state.total.Add(1)
		state.active.Add(1)
		state.connections.Add(1)
		go s.forwardSSHTunnelConnection(ctx, state, inbound, targetAddress)
	}
}

func (s *Service) forwardSSHTunnelConnection(ctx context.Context, state *sshTunnelState, inbound net.Conn, targetAddress string) {
	defer state.connections.Done()
	defer state.active.Add(-1)
	defer state.untrackConnection(inbound)
	var target net.Conn
	var err error
	endpointSide := "remote"
	if state.tunnel.Direction == domain.SSHTunnelDirectionReverse {
		endpointSide = "local"
		target, err = (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", targetAddress)
	} else {
		target, err = state.client.Dial("tcp", targetAddress)
	}
	if err != nil {
		_ = inbound.Close()
		s.tunnelMu.Lock()
		if current := s.tunnels[state.tunnel.ID]; current == state && state.tunnel.Status == "running" {
			state.tunnel.Error = s.redactor.Redact(fmt.Sprintf("connect %s endpoint %s: %v", endpointSide, targetAddress, err))
		}
		s.tunnelMu.Unlock()
		return
	}
	if !state.trackConnection(target) {
		return
	}
	defer state.untrackConnection(target)
	s.tunnelMu.Lock()
	if current := s.tunnels[state.tunnel.ID]; current == state && state.tunnel.Status == "running" {
		state.tunnel.Error = ""
	}
	s.tunnelMu.Unlock()
	defer inbound.Close()
	defer target.Close()

	inboundTotal, targetTotal := &state.sent, &state.received
	if state.tunnel.Direction == domain.SSHTunnelDirectionReverse {
		inboundTotal, targetTotal = &state.received, &state.sent
	}

	var relay sync.WaitGroup
	relay.Add(2)
	go func() {
		defer relay.Done()
		_, _ = io.Copy(countingWriter{writer: target, total: inboundTotal}, inbound)
		closeWrite(target)
	}()
	go func() {
		defer relay.Done()
		_, _ = io.Copy(countingWriter{writer: inbound, total: targetTotal}, target)
		closeWrite(inbound)
	}()
	relay.Wait()
}

type countingWriter struct {
	writer io.Writer
	total  *atomic.Int64
}

func (writer countingWriter) Write(data []byte) (int, error) {
	written, err := writer.writer.Write(data)
	writer.total.Add(int64(written))
	return written, err
}

func closeWrite(connection net.Conn) {
	if halfCloser, ok := connection.(interface{ CloseWrite() error }); ok {
		_ = halfCloser.CloseWrite()
		return
	}
	_ = connection.Close()
}

func tunnelSnapshot(state *sshTunnelState) domain.SSHTunnel {
	result := state.tunnel
	result.ActiveConnections = state.active.Load()
	result.TotalConnections = state.total.Load()
	result.BytesSent = state.sent.Load()
	result.BytesReceived = state.received.Load()
	return result
}

func validateSSHTunnelRequest(req domain.ExecRequest) error {
	if req.Mode != domain.ExecSSHTunnelStart {
		return fmt.Errorf("invalid SSH tunnel request mode")
	}
	switch req.TunnelDirection {
	case domain.SSHTunnelDirectionLocal:
		if err := validateSSHTunnelHost("local_host", req.TunnelLocalHost, true); err != nil {
			return err
		}
		if err := validateSSHTunnelHost("remote_host", req.TunnelRemoteHost, false); err != nil {
			return err
		}
		if req.TunnelLocalPort < 0 || req.TunnelLocalPort > 65535 {
			return fmt.Errorf("local_port must be between 0 and 65535")
		}
		if req.TunnelRemotePort < 1 || req.TunnelRemotePort > 65535 {
			return fmt.Errorf("remote_port must be between 1 and 65535")
		}
	case domain.SSHTunnelDirectionReverse:
		if err := validateSSHTunnelHost("local_host", req.TunnelLocalHost, false); err != nil {
			return err
		}
		if err := validateSSHTunnelHost("remote_host", req.TunnelRemoteHost, true); err != nil {
			return err
		}
		if req.TunnelLocalPort < 1 || req.TunnelLocalPort > 65535 {
			return fmt.Errorf("local_port must be between 1 and 65535")
		}
		if req.TunnelRemotePort < 0 || req.TunnelRemotePort > 65535 {
			return fmt.Errorf("remote_port must be between 0 and 65535")
		}
	default:
		return fmt.Errorf("direction must be local or reverse")
	}
	if req.Elevated {
		return fmt.Errorf("SSH tunnel requests cannot use elevated mode")
	}
	return nil
}

func validateSSHTunnelHost(field, value string, requireIP bool) error {
	host := strings.Trim(strings.TrimSpace(value), "[]")
	if host == "" {
		return fmt.Errorf("%s is required", field)
	}
	if len(host) > 253 || strings.ContainsAny(host, "\x00\r\n\t /\\") {
		return fmt.Errorf("invalid %s", field)
	}
	if net.ParseIP(host) != nil {
		return nil
	}
	if requireIP {
		return fmt.Errorf("%s must be an IP address", field)
	}
	if !sshTunnelHostnameRE.MatchString(host) {
		return fmt.Errorf("invalid %s", field)
	}
	return nil
}

func sshTunnelFieldsSet(req domain.ExecRequest) bool {
	return req.TunnelDirection != "" || req.TunnelLocalHost != "" || req.TunnelLocalPort != 0 ||
		req.TunnelRemoteHost != "" || req.TunnelRemotePort != 0
}

func (s *Service) hasSSHTunnelForHost(hostID string) bool {
	s.tunnelMu.RLock()
	defer s.tunnelMu.RUnlock()
	for _, state := range s.tunnels {
		if state.tunnel.HostID == hostID && state.tunnel.Status != "failed" {
			return true
		}
	}
	return false
}

func marshalSSHTunnel(tunnel domain.SSHTunnel) ([]byte, error) {
	data, err := json.Marshal(tunnel)
	if err != nil {
		return nil, fmt.Errorf("encode SSH tunnel state: %w", err)
	}
	return data, nil
}
