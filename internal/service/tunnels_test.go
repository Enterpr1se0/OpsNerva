package service

import (
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"eino-ops-agent/internal/domain"
)

func TestSSHTunnelApprovalReusesResolvedProxyConnectionAndStops(t *testing.T) {
	svc, transport, host := newTestService(t)
	ctx := context.Background()
	jump, err := svc.SaveHost(ctx, hostInputForTunnelTest("jump", ""), "test")
	if err != nil {
		t.Fatal(err)
	}
	targetInput := hostInputForTunnelTest(host.Name, host.ID)
	targetInput.ProxyJumpHostID = jump.ID
	proxy, err := svc.SaveProxy(ctx, domain.ProxyInput{
		Name: "tunnel proxy", URL: "socks5://127.0.0.1:1080", Username: "proxy-user", Password: "proxy-secret",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	targetInput.ProxyID = proxy.ID
	target, err := svc.SaveHost(ctx, targetInput, "test")
	if err != nil {
		t.Fatal(err)
	}

	pending, err := svc.StartSSHTunnel(ctx, target.ID, "", 5432, 0, "access the remote database locally", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != "approval_required" || pending.ApprovalID == "" {
		t.Fatalf("tunnel start bypassed approval: %#v", pending)
	}
	transport.mu.Lock()
	beforeApproval := len(transport.tunnelSpecs)
	transport.mu.Unlock()
	if beforeApproval != 0 {
		t.Fatal("SSH tunnel connected before approval")
	}

	approved, err := svc.Approve(ctx, pending.ApprovalID, "reviewed local and remote ports", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != "completed" || approved.Tunnel == nil || approved.Tunnel.LocalPort == 0 || approved.Tunnel.RemoteHost != "127.0.0.1" || approved.Tunnel.RemotePort != 5432 || !approved.Tunnel.ProxyUsed {
		t.Fatalf("unexpected approved tunnel result: %#v", approved)
	}
	transport.mu.Lock()
	if len(transport.tunnelSpecs) != 1 {
		transport.mu.Unlock()
		t.Fatalf("tunnel transport calls=%d, want 1", len(transport.tunnelSpecs))
	}
	connection := transport.tunnelSpecs[0]
	transport.mu.Unlock()
	if connection.Target.ProxyURL != "socks5://127.0.0.1:1080" || connection.Target.ProxyPassword != "proxy-secret" || len(connection.Jumps) != 1 || connection.Jumps[0].ID != jump.ID {
		t.Fatalf("configured proxy connection was not reused: %#v", connection)
	}

	list := svc.ListSSHTunnels()
	if list.Count != 1 || list.Tunnels[0].ID != approved.Tunnel.ID || list.Tunnels[0].Status != "running" {
		t.Fatalf("unexpected tunnel list: %#v", list)
	}
	if err := svc.DeleteHost(ctx, target.ID, "test"); err == nil || !strings.Contains(err.Error(), "active SSH tunnel") {
		t.Fatalf("active tunnel did not protect host deletion: %v", err)
	}
	stopped, err := svc.StopSSHTunnel(ctx, approved.Tunnel.ID, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Status != "stopped" || svc.ListSSHTunnels().Count != 0 {
		t.Fatalf("tunnel did not stop cleanly: stopped=%#v list=%#v", stopped, svc.ListSSHTunnels())
	}
}

func TestSSHTunnelInputValidation(t *testing.T) {
	svc, _, host := newTestService(t)
	for _, test := range []struct {
		name       string
		remoteHost string
		remotePort int
		localPort  int
	}{
		{name: "invalid remote host", remoteHost: "bad host", remotePort: 80},
		{name: "missing remote port", remoteHost: "localhost"},
		{name: "invalid local port", remoteHost: "localhost", remotePort: 80, localPort: 65536},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := svc.StartSSHTunnel(context.Background(), host.ID, test.remoteHost, test.remotePort, test.localPort, "test invalid tunnel", "test"); err == nil {
				t.Fatal("invalid tunnel input was accepted")
			}
		})
	}
}

func TestSSHTunnelForwardsLocalTraffic(t *testing.T) {
	svc, _, host := newTestService(t)
	remoteListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer remoteListener.Close()
	go func() {
		connection, acceptErr := remoteListener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		_, _ = io.Copy(connection, connection)
	}()
	remotePort := remoteListener.Addr().(*net.TCPAddr).Port

	pending, err := svc.StartSSHTunnel(context.Background(), host.ID, "127.0.0.1", remotePort, 0, "verify local forwarding", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	approved, err := svc.Approve(context.Background(), pending.ApprovalID, "approved test tunnel", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if approved.Tunnel == nil {
		t.Fatalf("missing tunnel result: %#v", approved)
	}
	forwarded, err := net.DialTimeout("tcp", net.JoinHostPort(approved.Tunnel.LocalHost, strconv.Itoa(approved.Tunnel.LocalPort)), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer forwarded.Close()
	if err := forwarded.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	payload := []byte("forwarded-through-local-listener")
	if _, err := forwarded.Write(payload); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, len(payload))
	if _, err := io.ReadFull(forwarded, reply); err != nil {
		t.Fatal(err)
	}
	if string(reply) != string(payload) {
		t.Fatalf("unexpected forwarded reply %q", reply)
	}
	var current domain.SSHTunnelList
	deadline := time.Now().Add(time.Second)
	for {
		current = svc.ListSSHTunnels()
		if current.Count == 1 && len(current.Tunnels) == 1 &&
			current.Tunnels[0].TotalConnections == 1 &&
			current.Tunnels[0].BytesSent >= int64(len(payload)) &&
			current.Tunnels[0].BytesReceived >= int64(len(payload)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("unexpected tunnel counters: %#v", current)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := svc.StopSSHTunnel(context.Background(), approved.Tunnel.ID, "operator"); err != nil {
		t.Fatal(err)
	}
	if _, err := forwarded.Read(make([]byte, 1)); err == nil {
		t.Fatal("stopping the tunnel left the active local connection open")
	}
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), time.Second)
	defer cancelShutdown()
	if err := svc.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("active tunnel connection delayed service shutdown: %v", err)
	}
}

func hostInputForTunnelTest(name, id string) domain.HostInput {
	return domain.HostInput{
		ID: id, Name: name, Address: "127.0.0.1", Port: 22, User: "test",
		AuthType: "agent", SudoMode: "none",
	}
}
