package service

import (
	"context"
	"testing"

	"eino-ops-agent/internal/domain"
)

func TestOperatorCanStartTunnelWithoutAgentApproval(t *testing.T) {
	svc, _, host := newTestService(t)

	tunnel, err := svc.StartOperatorSSHTunnel(context.Background(), host.ID, "", 8080, 0, "admin-web")
	if err != nil {
		t.Fatal(err)
	}
	if tunnel.HostID != host.ID || tunnel.RemoteHost != "127.0.0.1" || tunnel.RemotePort != 8080 || tunnel.LocalPort == 0 {
		t.Fatalf("unexpected operator tunnel: %#v", tunnel)
	}
	assertNoPendingApprovals(t, svc)
	if _, err := svc.StopSSHTunnel(context.Background(), tunnel.ID, "admin-web"); err != nil {
		t.Fatal(err)
	}
}

func TestOperatorCanStartShellWithoutAgentConversation(t *testing.T) {
	svc, _, host := newTestService(t)

	shell, err := svc.StartOperatorSSHShell(context.Background(), host.ID, domain.SSHShellSurfaceWorkspace, "admin-web")
	if err != nil {
		t.Fatal(err)
	}
	if shell.HostID != host.ID || shell.SessionID != "" || shell.Surface != domain.SSHShellSurfaceWorkspace || shell.Status != "running" || shell.Elevated {
		t.Fatalf("unexpected operator shell: %#v", shell)
	}
	assertNoPendingApprovals(t, svc)
	if _, err := svc.CloseSSHShell(context.Background(), shell.ID, "", "", "admin-web"); err != nil {
		t.Fatal(err)
	}

	quickShell, err := svc.StartOperatorSSHShell(context.Background(), host.ID, "", "admin-web")
	if err != nil {
		t.Fatal(err)
	}
	if quickShell.Surface != domain.SSHShellSurfaceQuick {
		t.Fatalf("quick shell has unexpected surface: %#v", quickShell)
	}
	if _, err := svc.CloseSSHShell(context.Background(), quickShell.ID, "", "", "admin-web"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartOperatorSSHShell(context.Background(), host.ID, "invalid", "admin-web"); err == nil {
		t.Fatal("invalid shell surface was accepted")
	}
}

func assertNoPendingApprovals(t *testing.T, svc *Service) {
	t.Helper()
	approvals, err := svc.ListApprovals(context.Background(), "pending", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(approvals) != 0 {
		t.Fatalf("operator connection unexpectedly created approvals: %#v", approvals)
	}
}
