package service

import (
	"context"
	"os/exec"
	"testing"
	"time"

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

func TestOperatorCanRetryFailedTunnel(t *testing.T) {
	svc, transport, host := newTestService(t)

	failed, err := svc.StartOperatorSSHTunnel(context.Background(), host.ID, "127.0.0.1", 8080, 0, "admin-web")
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
	if retried.ID == failed.ID || retried.Status != "running" || retried.HostID != failed.HostID ||
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

func TestOperatorCanStartWorkspaceShellWithoutApproval(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	svc, _ := newWorkspaceService(t, "read_write")
	hostMode := domain.WorkspaceShellModeHost
	if _, err := svc.SaveSystemSettings(context.Background(), domain.SystemSettingsInput{
		AgentMaxIterations: domain.DefaultAgentMaxIterations, WorkspaceShellMode: &hostMode,
	}, "test"); err != nil {
		t.Fatal(err)
	}
	shell, err := svc.StartOperatorWorkspaceShell(context.Background(), "project", ".", "admin-web")
	if err != nil {
		t.Fatal(err)
	}
	if shell.Kind != domain.SSHShellKindWorkspace || shell.WorkspaceID != "project" || shell.Surface != domain.WorkspaceShellSurfaceOperator || shell.SessionID != "" {
		t.Fatalf("unexpected operator Workspace shell: %#v", shell)
	}
	assertNoPendingApprovals(t, svc)
	if _, err := svc.UpdateAdminWorkspace(context.Background(), "project", domain.WorkspaceInput{ID: "project", Access: "read_only"}, "admin-web"); err == nil {
		t.Fatal("active Workspace terminal allowed access downgrade")
	}
	sandboxMode := domain.WorkspaceShellModeSandbox
	if _, err := svc.SaveSystemSettings(context.Background(), domain.SystemSettingsInput{
		AgentMaxIterations: domain.DefaultAgentMaxIterations, WorkspaceShellMode: &sandboxMode,
	}, "admin-web"); err == nil {
		t.Fatal("active Workspace terminal allowed backend switch")
	}
	if err := svc.DeleteAdminWorkspace(context.Background(), "project", "admin-web"); err == nil {
		t.Fatal("active Workspace terminal allowed Workspace deletion")
	}
	if _, err := svc.CloseSSHShell(context.Background(), shell.ID, "", "", "admin-web"); err != nil {
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
