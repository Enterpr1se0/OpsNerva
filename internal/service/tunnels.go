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

const sshTunnelLocalHost = "127.0.0.1"

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

func (s *Service) StartSSHTunnel(ctx context.Context, hostID, remoteHost string, remotePort, localPort int, reason, actor string) (domain.ExecResult, error) {
	remoteHost = strings.TrimSpace(remoteHost)
	if remoteHost == "" {
		remoteHost = "127.0.0.1"
	}
	remoteHost = strings.Trim(remoteHost, "[]")
	return s.Submit(ctx, domain.ExecRequest{
		HostID:           strings.TrimSpace(hostID),
		Mode:             domain.ExecSSHTunnelStart,
		Reason:           strings.TrimSpace(reason),
		TunnelRemoteHost: remoteHost,
		TunnelRemotePort: remotePort,
		TunnelLocalPort:  localPort,
	}, actor)
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
		"tunnel_id": id, "host_id": stopped.HostID, "local_port": stopped.LocalPort,
		"remote_host": stopped.RemoteHost, "remote_port": stopped.RemotePort,
	})
	observability.FromContext(ctx).InfoContext(ctx, "SSH tunnel stopped",
		"component", "ssh_tunnel", "tunnel_id", id, "host_id", stopped.HostID,
		"local_port", stopped.LocalPort, "remote_host", stopped.RemoteHost, "remote_port", stopped.RemotePort)
	return stopped, nil
}

func (s *Service) openSSHTunnel(ctx context.Context, host domain.Host, connection sshx.ConnectionSpec, req domain.ExecRequest, actor string) (domain.SSHTunnel, error) {
	transport, ok := s.transport.(sshx.TunnelTransport)
	if !ok {
		return domain.SSHTunnel{}, fmt.Errorf("configured SSH transport does not support local port forwarding")
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

	listener, err := net.Listen("tcp4", net.JoinHostPort(sshTunnelLocalHost, strconv.Itoa(req.TunnelLocalPort)))
	if err != nil {
		cancelTunnel()
		_ = client.Close()
		return domain.SSHTunnel{}, fmt.Errorf("listen on local port %d: %w", req.TunnelLocalPort, err)
	}
	_, localPortText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		cancelTunnel()
		_ = listener.Close()
		_ = client.Close()
		return domain.SSHTunnel{}, fmt.Errorf("resolve local tunnel address: %w", err)
	}
	localPort, err := strconv.Atoi(localPortText)
	if err != nil {
		cancelTunnel()
		_ = listener.Close()
		_ = client.Close()
		return domain.SSHTunnel{}, fmt.Errorf("resolve local tunnel port: %w", err)
	}

	proxyUsed := connection.Target.ProxyURL != "" || len(connection.Jumps) > 0
	state := &sshTunnelState{
		tunnel: domain.SSHTunnel{
			ID: ids.New("tunnel"), HostID: host.ID, HostName: host.Name,
			LocalHost: sshTunnelLocalHost, LocalPort: localPort,
			RemoteHost: req.TunnelRemoteHost, RemotePort: req.TunnelRemotePort,
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
		"tunnel_id": startedTunnel.ID, "host_id": host.ID, "local_port": localPort,
		"remote_host": req.TunnelRemoteHost, "remote_port": req.TunnelRemotePort, "proxy_used": proxyUsed,
	})
	observability.FromContext(ctx).InfoContext(ctx, "SSH tunnel started",
		"component", "ssh_tunnel", "tunnel_id", startedTunnel.ID, "host_id", host.ID,
		"local_port", localPort, "remote_host", req.TunnelRemoteHost,
		"remote_port", req.TunnelRemotePort, "proxy_used", proxyUsed)
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
	remoteAddress := net.JoinHostPort(state.tunnel.RemoteHost, strconv.Itoa(state.tunnel.RemotePort))
	for {
		local, err := state.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept local tunnel connection: %w", err)
		}
		if !state.trackConnection(local) {
			return nil
		}
		state.total.Add(1)
		state.active.Add(1)
		state.connections.Add(1)
		go s.forwardSSHTunnelConnection(state, local, remoteAddress)
	}
}

func (s *Service) forwardSSHTunnelConnection(state *sshTunnelState, local net.Conn, remoteAddress string) {
	defer state.connections.Done()
	defer state.active.Add(-1)
	defer state.untrackConnection(local)
	remote, err := state.client.Dial("tcp", remoteAddress)
	if err != nil {
		_ = local.Close()
		s.tunnelMu.Lock()
		if current := s.tunnels[state.tunnel.ID]; current == state && state.tunnel.Status == "running" {
			state.tunnel.Error = s.redactor.Redact(fmt.Sprintf("connect remote endpoint %s: %v", remoteAddress, err))
		}
		s.tunnelMu.Unlock()
		return
	}
	if !state.trackConnection(remote) {
		return
	}
	defer state.untrackConnection(remote)
	s.tunnelMu.Lock()
	if current := s.tunnels[state.tunnel.ID]; current == state && state.tunnel.Status == "running" {
		state.tunnel.Error = ""
	}
	s.tunnelMu.Unlock()
	defer local.Close()
	defer remote.Close()

	var relay sync.WaitGroup
	relay.Add(2)
	go func() {
		defer relay.Done()
		_, _ = io.Copy(countingWriter{writer: remote, total: &state.sent}, local)
		closeWrite(remote)
	}()
	go func() {
		defer relay.Done()
		_, _ = io.Copy(countingWriter{writer: local, total: &state.received}, remote)
		closeWrite(local)
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
	host := strings.Trim(strings.TrimSpace(req.TunnelRemoteHost), "[]")
	if host == "" {
		return fmt.Errorf("remote_host is required")
	}
	if len(host) > 253 || strings.ContainsAny(host, "\x00\r\n\t /\\") {
		return fmt.Errorf("invalid remote_host")
	}
	if net.ParseIP(host) == nil && !sshTunnelHostnameRE.MatchString(host) {
		return fmt.Errorf("invalid remote_host")
	}
	if req.TunnelRemotePort < 1 || req.TunnelRemotePort > 65535 {
		return fmt.Errorf("remote_port must be between 1 and 65535")
	}
	if req.TunnelLocalPort < 0 || req.TunnelLocalPort > 65535 {
		return fmt.Errorf("local_port must be between 0 and 65535")
	}
	if req.Elevated {
		return fmt.Errorf("SSH tunnel requests cannot use elevated mode")
	}
	return nil
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
