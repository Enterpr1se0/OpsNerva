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

const (
	sshTunnelDefaultHost             = "127.0.0.1"
	sshTunnelReconnectInitialDelay   = time.Second
	sshTunnelReconnectMaximumDelay   = 30 * time.Second
	sshTunnelReconnectAttemptTimeout = 30 * time.Second
)

type sshTunnelStateIDContextKey struct{}

var (
	sshTunnelHostnameRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9_.-]{0,251}[A-Za-z0-9])?$`)
	// ErrHostHasActiveTunnel prevents deleting connection settings still used by a forwarding worker.
	ErrHostHasActiveTunnel = errors.New("host has an active SSH tunnel")
)

type sshTunnelState struct {
	tunnel          domain.SSHTunnel
	ctx             context.Context
	cancel          context.CancelFunc
	runtimeMu       sync.Mutex
	runtime         *sshTunnelRuntime
	stopped         bool
	connections     sync.WaitGroup
	connectionMu    sync.Mutex
	openConnections map[net.Conn]struct{}
	active          atomic.Int64
	total           atomic.Int64
	sent            atomic.Int64
	received        atomic.Int64
}

type sshTunnelRuntime struct {
	ctx       context.Context
	cancel    context.CancelFunc
	listener  net.Listener
	client    sshx.TunnelClient
	closeOnce sync.Once
}

func (runtime *sshTunnelRuntime) close() {
	runtime.closeOnce.Do(func() {
		runtime.cancel()
		_ = runtime.listener.Close()
		_ = runtime.client.Close()
	})
}

func (state *sshTunnelState) installRuntime(runtime *sshTunnelRuntime) bool {
	state.runtimeMu.Lock()
	defer state.runtimeMu.Unlock()
	if state.stopped || state.runtime != nil {
		return false
	}
	state.runtime = runtime
	return true
}

func (state *sshTunnelState) currentRuntime() *sshTunnelRuntime {
	state.runtimeMu.Lock()
	defer state.runtimeMu.Unlock()
	return state.runtime
}

func (state *sshTunnelState) closeRuntime(runtime *sshTunnelRuntime) {
	state.runtimeMu.Lock()
	if state.runtime == runtime {
		state.runtime = nil
	}
	state.runtimeMu.Unlock()
	runtime.close()
	state.closeConnections()
}

func (state *sshTunnelState) stop() {
	state.runtimeMu.Lock()
	state.stopped = true
	runtime := state.runtime
	state.runtime = nil
	state.runtimeMu.Unlock()
	state.cancel()
	if runtime != nil {
		runtime.close()
	}
	state.closeConnections()
}

func (state *sshTunnelState) closeConnections() {
	state.connectionMu.Lock()
	connections := make([]net.Conn, 0, len(state.openConnections))
	for connection := range state.openConnections {
		connections = append(connections, connection)
	}
	state.connectionMu.Unlock()
	for _, connection := range connections {
		_ = connection.Close()
	}
}

func (state *sshTunnelState) trackConnection(runtime *sshTunnelRuntime, connection net.Conn) bool {
	state.runtimeMu.Lock()
	active := !state.stopped && state.runtime == runtime
	if active {
		state.connectionMu.Lock()
		state.openConnections[connection] = struct{}{}
		state.connectionMu.Unlock()
	}
	state.runtimeMu.Unlock()
	if !active {
		_ = connection.Close()
		return false
	}
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
	if previous.Status != "running" {
		return domain.SSHTunnel{}, fmt.Errorf("invalid tunnel status %q: only running tunnels can be edited", previous.Status)
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

	state.stop()

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
	runtime, localPort, remotePort, err := openSSHTunnelRuntime(ctx, tunnelCtx, transport, connection, req)
	if err != nil {
		cancelTunnel()
		return domain.SSHTunnel{}, err
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
		ctx: tunnelCtx, cancel: cancelTunnel, openConnections: make(map[net.Conn]struct{}),
	}
	if !state.installRuntime(runtime) {
		runtime.close()
		cancelTunnel()
		return domain.SSHTunnel{}, fmt.Errorf("initialize SSH tunnel runtime")
	}
	s.tunnelMu.Lock()
	s.tunnels[state.tunnel.ID] = state
	startedTunnel := tunnelSnapshot(state)
	s.tunnelMu.Unlock()
	workerStarted = true
	go s.runSSHTunnel(state)

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

func openSSHTunnelRuntime(startupParent, tunnelCtx context.Context, transport sshx.TunnelTransport, connection sshx.ConnectionSpec, req domain.ExecRequest) (*sshTunnelRuntime, int, int, error) {
	runtimeCtx, cancelRuntime := context.WithCancel(tunnelCtx)
	startupCtx, cancelStartup := context.WithCancel(startupParent)
	stopStartup := context.AfterFunc(runtimeCtx, cancelStartup)
	client, err := transport.OpenTunnel(startupCtx, connection)
	stopStartup()
	cancelStartup()
	if err != nil {
		cancelRuntime()
		return nil, 0, 0, err
	}
	if err := runtimeCtx.Err(); err != nil {
		cancelRuntime()
		_ = client.Close()
		return nil, 0, 0, err
	}

	var listener net.Listener
	switch req.TunnelDirection {
	case domain.SSHTunnelDirectionLocal:
		listener, err = net.Listen("tcp", net.JoinHostPort(req.TunnelLocalHost, strconv.Itoa(req.TunnelLocalPort)))
		if err != nil {
			cancelRuntime()
			_ = client.Close()
			return nil, 0, 0, fmt.Errorf("listen on local endpoint %s:%d: %w", req.TunnelLocalHost, req.TunnelLocalPort, err)
		}
	case domain.SSHTunnelDirectionReverse:
		reverseClient, ok := client.(sshx.ReverseTunnelClient)
		if !ok {
			cancelRuntime()
			_ = client.Close()
			return nil, 0, 0, fmt.Errorf("configured SSH transport does not support reverse port forwarding")
		}
		listener, err = reverseClient.Listen("tcp", net.JoinHostPort(req.TunnelRemoteHost, strconv.Itoa(req.TunnelRemotePort)))
		if err != nil {
			cancelRuntime()
			_ = client.Close()
			return nil, 0, 0, fmt.Errorf("listen on remote endpoint %s:%d: %w", req.TunnelRemoteHost, req.TunnelRemotePort, err)
		}
	default:
		cancelRuntime()
		_ = client.Close()
		return nil, 0, 0, fmt.Errorf("invalid SSH tunnel direction %q", req.TunnelDirection)
	}
	runtime := &sshTunnelRuntime{ctx: runtimeCtx, cancel: cancelRuntime, listener: listener, client: client}
	_, listenerPortText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		runtime.close()
		return nil, 0, 0, fmt.Errorf("resolve SSH tunnel listener address: %w", err)
	}
	listenerPort, err := strconv.Atoi(listenerPortText)
	if err != nil {
		runtime.close()
		return nil, 0, 0, fmt.Errorf("resolve SSH tunnel listener port: %w", err)
	}
	localPort, remotePort := req.TunnelLocalPort, req.TunnelRemotePort
	if req.TunnelDirection == domain.SSHTunnelDirectionLocal {
		localPort = listenerPort
	} else {
		remotePort = listenerPort
	}
	return runtime, localPort, remotePort, nil
}

func (s *Service) runSSHTunnel(state *sshTunnelState) {
	defer s.executionWG.Done()
	defer func() {
		state.stop()
		s.tunnelMu.Lock()
		if current := s.tunnels[state.tunnel.ID]; current == state {
			delete(s.tunnels, state.tunnel.ID)
		}
		s.tunnelMu.Unlock()
	}()
	runtime := state.currentRuntime()
	for runtime != nil {
		terminalErr := s.serveSSHTunnelRuntime(state, runtime)
		unexpected := state.ctx.Err() == nil
		state.closeRuntime(runtime)
		state.connections.Wait()
		if !unexpected {
			return
		}

		failure := "SSH tunnel connection closed"
		if terminalErr != nil {
			failure = s.redactor.Redact(terminalErr.Error())
		}
		s.tunnelMu.Lock()
		current, exists := s.tunnels[state.tunnel.ID]
		if !exists || current != state || state.tunnel.Status == "stopping" || state.tunnel.Status == "stopped" {
			s.tunnelMu.Unlock()
			return
		}
		state.tunnel.Status = "retrying"
		state.tunnel.Error = failure
		state.tunnel.ReconnectAttempt = 0
		s.tunnelMu.Unlock()

		observability.FromContext(context.Background()).WarnContext(context.Background(), "SSH tunnel disconnected; reconnecting",
			"component", "ssh_tunnel", "tunnel_id", state.tunnel.ID, "host_id", state.tunnel.HostID, "error", failure)
		runtime = s.reconnectSSHTunnel(state)
	}
}

func (s *Service) serveSSHTunnelRuntime(state *sshTunnelState, runtime *sshTunnelRuntime) error {
	acceptErrors := make(chan error, 1)
	clientErrors := make(chan error, 1)
	state.connections.Add(1)
	go func() {
		defer state.connections.Done()
		acceptErrors <- s.acceptSSHTunnelConnections(runtime.ctx, state, runtime)
	}()
	go func() { clientErrors <- runtime.client.Wait() }()

	select {
	case <-runtime.ctx.Done():
		return nil
	case err := <-acceptErrors:
		return err
	case err := <-clientErrors:
		return err
	}
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
		snapshot := state.tunnel
		s.tunnelMu.Unlock()

		timer := time.NewTimer(delay)
		select {
		case <-state.ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil
		case <-timer.C:
		}

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
			s.tunnelMu.Unlock()
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

func (s *Service) acceptSSHTunnelConnections(ctx context.Context, state *sshTunnelState, runtime *sshTunnelRuntime) error {
	targetAddress := net.JoinHostPort(state.tunnel.RemoteHost, strconv.Itoa(state.tunnel.RemotePort))
	acceptSide := "local"
	if state.tunnel.Direction == domain.SSHTunnelDirectionReverse {
		targetAddress = net.JoinHostPort(state.tunnel.LocalHost, strconv.Itoa(state.tunnel.LocalPort))
		acceptSide = "remote"
	}
	for {
		inbound, err := runtime.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept %s tunnel connection: %w", acceptSide, err)
		}
		if !state.trackConnection(runtime, inbound) {
			return nil
		}
		state.total.Add(1)
		state.active.Add(1)
		state.connections.Add(1)
		go s.forwardSSHTunnelConnection(ctx, state, runtime, inbound, targetAddress)
	}
}

func (s *Service) forwardSSHTunnelConnection(ctx context.Context, state *sshTunnelState, runtime *sshTunnelRuntime, inbound net.Conn, targetAddress string) {
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
		target, err = runtime.client.Dial("tcp", targetAddress)
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
	if !state.trackConnection(runtime, target) {
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
		if state.tunnel.HostID == hostID {
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
