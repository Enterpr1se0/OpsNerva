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

	pending, err := svc.StartSSHTunnel(ctx, target.ID, domain.SSHTunnelConfig{RemotePort: 5432}, "access the remote database locally", "eino-agent")
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
	if approved.Status != "completed" || approved.Tunnel == nil || approved.Tunnel.Direction != domain.SSHTunnelDirectionLocal || approved.Tunnel.LocalHost != "127.0.0.1" || approved.Tunnel.LocalPort == 0 || approved.Tunnel.RemoteHost != "127.0.0.1" || approved.Tunnel.RemotePort != 5432 || !approved.Tunnel.ProxyUsed {
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
		name   string
		config domain.SSHTunnelConfig
	}{
		{name: "invalid remote host", config: domain.SSHTunnelConfig{RemoteHost: "bad host", RemotePort: 80}},
		{name: "missing remote port", config: domain.SSHTunnelConfig{RemoteHost: "localhost"}},
		{name: "invalid local port", config: domain.SSHTunnelConfig{RemoteHost: "localhost", RemotePort: 80, LocalPort: 65536}},
		{name: "local listener requires IP", config: domain.SSHTunnelConfig{LocalHost: "localhost", RemotePort: 80}},
		{name: "reverse listener requires IP", config: domain.SSHTunnelConfig{Direction: domain.SSHTunnelDirectionReverse, LocalPort: 80, RemoteHost: "localhost"}},
		{name: "reverse requires local target port", config: domain.SSHTunnelConfig{Direction: domain.SSHTunnelDirectionReverse}},
		{name: "invalid direction", config: domain.SSHTunnelConfig{Direction: "sideways", RemotePort: 80}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := svc.StartSSHTunnel(context.Background(), host.ID, test.config, "test invalid tunnel", "test"); err == nil {
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

	pending, err := svc.StartSSHTunnel(context.Background(), host.ID, domain.SSHTunnelConfig{
		LocalHost: "0.0.0.0", RemoteHost: "127.0.0.1", RemotePort: remotePort,
	}, "verify local forwarding", "eino-agent")
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
	if approved.Tunnel.Direction != domain.SSHTunnelDirectionLocal || approved.Tunnel.LocalHost != "0.0.0.0" {
		t.Fatalf("configured local bind address was not preserved: %#v", approved.Tunnel)
	}
	forwarded, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(approved.Tunnel.LocalPort)), time.Second)
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

func TestSSHTunnelForwardsReverseTraffic(t *testing.T) {
	svc, _, host := newTestService(t)
	localListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer localListener.Close()
	go func() {
		connection, acceptErr := localListener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		_, _ = io.Copy(connection, connection)
	}()
	localPort := localListener.Addr().(*net.TCPAddr).Port

	pending, err := svc.StartSSHTunnel(context.Background(), host.ID, domain.SSHTunnelConfig{
		Direction: domain.SSHTunnelDirectionReverse,
		LocalHost: "127.0.0.1", LocalPort: localPort,
		RemoteHost: "0.0.0.0", RemotePort: 0,
	}, "verify reverse forwarding", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	approved, err := svc.Approve(context.Background(), pending.ApprovalID, "approved reverse tunnel", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if approved.Tunnel == nil || approved.Tunnel.Direction != domain.SSHTunnelDirectionReverse || approved.Tunnel.RemoteHost != "0.0.0.0" || approved.Tunnel.RemotePort == 0 {
		t.Fatalf("unexpected reverse tunnel result: %#v", approved)
	}

	forwarded, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(approved.Tunnel.RemotePort)), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer forwarded.Close()
	if err := forwarded.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	payload := []byte("forwarded-through-reverse-listener")
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

	deadline := time.Now().Add(time.Second)
	for {
		current := svc.ListSSHTunnels()
		if current.Count == 1 && current.Tunnels[0].TotalConnections == 1 &&
			current.Tunnels[0].BytesSent >= int64(len(payload)) &&
			current.Tunnels[0].BytesReceived >= int64(len(payload)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("unexpected reverse tunnel counters: %#v", current)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := svc.StopSSHTunnel(context.Background(), approved.Tunnel.ID, "operator"); err != nil {
		t.Fatal(err)
	}
}

func TestOperatorCanEditTunnel(t *testing.T) {
	svc, _, host := newTestService(t)
	replacementHost, err := svc.SaveHost(context.Background(), hostInputForTunnelTest("replacement", ""), "test")
	if err != nil {
		t.Fatal(err)
	}
	original, err := svc.StartOperatorSSHTunnel(context.Background(), host.ID, domain.SSHTunnelConfig{RemotePort: 8080}, "admin-web")
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := svc.UpdateOperatorSSHTunnel(context.Background(), original.ID, original.HostID, domain.SSHTunnelConfig{
		Direction: original.Direction, LocalHost: original.LocalHost, LocalPort: original.LocalPort,
		RemoteHost: original.RemoteHost, RemotePort: original.RemotePort,
	}, "admin-web")
	if err != nil || unchanged.ID != original.ID {
		t.Fatalf("unchanged edit restarted the tunnel: tunnel=%#v err=%v", unchanged, err)
	}

	updated, err := svc.UpdateOperatorSSHTunnel(context.Background(), original.ID, replacementHost.ID, domain.SSHTunnelConfig{
		Direction: domain.SSHTunnelDirectionLocal,
		LocalHost: original.LocalHost, LocalPort: original.LocalPort,
		RemoteHost: "database.internal", RemotePort: 5432,
	}, "admin-web")
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID == original.ID || updated.HostID != replacementHost.ID || updated.LocalHost != original.LocalHost ||
		updated.LocalPort != original.LocalPort || updated.RemoteHost != "database.internal" || updated.RemotePort != 5432 {
		t.Fatalf("unexpected edited tunnel: original=%#v updated=%#v", original, updated)
	}
	list := svc.ListSSHTunnels()
	if list.Count != 1 || list.Tunnels[0].ID != updated.ID {
		t.Fatalf("edited tunnel did not replace the original: %#v", list)
	}
	if _, err := svc.StopSSHTunnel(context.Background(), updated.ID, "admin-web"); err != nil {
		t.Fatal(err)
	}
}

func TestOperatorCanStartTunnelWithoutAgentApproval(t *testing.T) {
	svc, _, host := newTestService(t)

	tunnel, err := svc.StartOperatorSSHTunnel(context.Background(), host.ID, domain.SSHTunnelConfig{RemotePort: 8080}, "admin-web")
	if err != nil {
		t.Fatal(err)
	}
	if tunnel.HostID != host.ID || tunnel.Direction != domain.SSHTunnelDirectionLocal || tunnel.LocalHost != "127.0.0.1" ||
		tunnel.RemoteHost != "127.0.0.1" || tunnel.RemotePort != 8080 || tunnel.LocalPort == 0 {
		t.Fatalf("unexpected operator tunnel: %#v", tunnel)
	}
	assertNoPendingApprovals(t, svc)
	if _, err := svc.StopSSHTunnel(context.Background(), tunnel.ID, "admin-web"); err != nil {
		t.Fatal(err)
	}
}

func TestOperatorCanRetryFailedTunnel(t *testing.T) {
	svc, transport, host := newTestService(t)

	failed, err := svc.StartOperatorSSHTunnel(context.Background(), host.ID, domain.SSHTunnelConfig{RemotePort: 8080}, "admin-web")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RetryOperatorSSHTunnel(context.Background(), failed.ID, "admin-web"); err == nil {
		t.Fatal("running tunnel was retried")
	}

	transport.mu.Lock()
	client := transport.tunnelClients[0]
	transport.mu.Unlock()
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		list := svc.ListSSHTunnels()
		if len(list.Tunnels) == 1 && list.Tunnels[0].Status == "failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tunnel did not enter failed state: %#v", list)
		}
		time.Sleep(10 * time.Millisecond)
	}

	retried, err := svc.RetryOperatorSSHTunnel(context.Background(), failed.ID, "admin-web")
	if err != nil {
		t.Fatal(err)
	}
	if retried.ID == failed.ID || retried.Status != "running" || retried.HostID != failed.HostID || retried.Direction != failed.Direction ||
		retried.LocalHost != failed.LocalHost ||
		retried.RemoteHost != failed.RemoteHost || retried.RemotePort != failed.RemotePort || retried.LocalPort != failed.LocalPort {
		t.Fatalf("unexpected retried tunnel: failed=%#v retried=%#v", failed, retried)
	}
	list := svc.ListSSHTunnels()
	if list.Count != 1 || len(list.Tunnels) != 1 || list.Tunnels[0].ID != retried.ID {
		t.Fatalf("failed tunnel was not replaced: %#v", list)
	}
	if _, err := svc.StopSSHTunnel(context.Background(), retried.ID, "admin-web"); err != nil {
		t.Fatal(err)
	}
}

func TestOperatorTunnelEditValidatesBeforeStopAndRollsBackRuntimeFailure(t *testing.T) {
	svc, _, host := newTestService(t)
	original, err := svc.StartOperatorSSHTunnel(context.Background(), host.ID, domain.SSHTunnelConfig{RemotePort: 8080}, "admin-web")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateOperatorSSHTunnel(context.Background(), original.ID, original.HostID, domain.SSHTunnelConfig{
		LocalHost: "localhost", LocalPort: original.LocalPort, RemotePort: 9090,
	}, "admin-web"); err == nil {
		t.Fatal("invalid edit was accepted")
	}
	list := svc.ListSSHTunnels()
	if list.Count != 1 || list.Tunnels[0].ID != original.ID || list.Tunnels[0].Status != "running" {
		t.Fatalf("invalid edit disturbed the original tunnel: %#v", list)
	}

	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	occupiedPort := occupied.Addr().(*net.TCPAddr).Port
	if _, err := svc.UpdateOperatorSSHTunnel(context.Background(), original.ID, original.HostID, domain.SSHTunnelConfig{
		LocalHost: "127.0.0.1", LocalPort: occupiedPort, RemotePort: 9090,
	}, "admin-web"); err == nil {
		t.Fatal("edit with an occupied listener unexpectedly succeeded")
	}
	list = svc.ListSSHTunnels()
	if list.Count != 1 || list.Tunnels[0].ID != original.ID || list.Tunnels[0].Status != "running" ||
		list.Tunnels[0].LocalPort != original.LocalPort || list.Tunnels[0].RemotePort != original.RemotePort {
		t.Fatalf("failed edit did not restore the original tunnel: %#v", list)
	}
	if _, err := svc.StopSSHTunnel(context.Background(), original.ID, "admin-web"); err != nil {
		t.Fatal(err)
	}
}

func hostInputForTunnelTest(name, id string) domain.HostInput {
	return domain.HostInput{
		ID: id, Name: name, Address: "127.0.0.1", Port: 22, User: "test",
		AuthType: "agent", SudoMode: "none",
	}
}
