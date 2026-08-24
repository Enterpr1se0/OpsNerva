package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"
)

func TestRequireAgentRootAccessUsesHostSetting(t *testing.T) {
	disabled := domain.Host{ID: "host-root", AgentEnabled: true, User: "root"}
	if err := requireAgentRootAccess("eino-agent", disabled, false); !errors.Is(err, ErrAgentRootAccessDenied) {
		t.Fatalf("disabled root-login error = %v", err)
	}
	enabled := disabled
	enabled.AgentRootEnabled = true
	if err := requireAgentRootAccess("eino-agent", enabled, false); err != nil {
		t.Fatalf("enabled root login was rejected: %v", err)
	}
	for _, actor := range []string{"operator", "mcp-client"} {
		if err := requireAgentRootAccess(actor, disabled, true); err != nil {
			t.Fatalf("explicit %s operation was restricted by the Agent setting: %v", actor, err)
		}
	}
}

func TestHostSupportsRootUsesActualConnectionCapabilities(t *testing.T) {
	tests := []struct {
		name string
		host domain.Host
		want bool
	}{
		{name: "root login", host: domain.Host{AgentEnabled: true, User: "root"}, want: true},
		{name: "passwordless sudo", host: domain.Host{AgentEnabled: true, User: "ops", SudoMode: "nopasswd"}, want: true},
		{name: "managed sudo password", host: domain.Host{AgentEnabled: true, User: "ops", SudoMode: "password", HasSudoPassword: true}, want: true},
		{name: "missing sudo password", host: domain.Host{AgentEnabled: true, User: "ops", SudoMode: "password"}},
		{name: "disabled Agent host", host: domain.Host{User: "root"}, want: true},
		{name: "workspace", host: domain.Host{AgentEnabled: true, User: "root", AuthType: "workspace"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := hostSupportsRoot(test.host); got != test.want {
				t.Fatalf("hostSupportsRoot() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestAgentAvailabilityDoesNotChangeRootSetting(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestService(t)
	host, err := svc.SaveHost(ctx, domain.HostInput{
		Name: "independent root fixture", Address: "127.0.0.6", Port: 22, User: "root", AuthType: "agent", SudoMode: "none",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	host, err = svc.SetHostAgentRootEnabled(ctx, host.ID, true, "test")
	if err != nil {
		t.Fatal(err)
	}
	agentDisabled := false
	host, err = svc.SaveHost(ctx, domain.HostInput{
		ID: host.ID, Name: host.Name, Address: host.Address, Port: host.Port, User: host.User,
		AgentEnabled: &agentDisabled, AuthType: host.AuthType, SudoMode: host.SudoMode,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if host.AgentEnabled || !host.AgentRootEnabled {
		t.Fatalf("disabling Agent changed root setting: %#v", host)
	}
	if err := svc.requireCurrentAgentSSHAccess(ctx, "eino-agent", host.ID, false); err == nil {
		t.Fatal("Agent-disabled host remained available for execution")
	}
	if _, err := svc.SetHostAgentRootEnabled(ctx, host.ID, false, "test"); err != nil {
		t.Fatalf("disable root while Agent is unavailable: %v", err)
	}
	host, err = svc.SetHostAgentRootEnabled(ctx, host.ID, true, "test")
	if err != nil {
		t.Fatalf("enable root while Agent is unavailable: %v", err)
	}
	if host.AgentEnabled || !host.AgentRootEnabled {
		t.Fatalf("independent root update was not persisted: %#v", host)
	}
}

func TestDisablingAgentRootKeepsHostSudoConfiguration(t *testing.T) {
	svc, _, _ := newTestService(t)
	host, err := svc.SaveHost(context.Background(), domain.HostInput{
		Name: "sudo fixture", Address: "127.0.0.4", Port: 22, User: "ops", AuthType: "password",
		Password: "ssh-secret", SudoMode: "password", SudoPassword: "sudo-secret",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetHostAgentRootEnabled(context.Background(), host.ID, true, "test"); err != nil {
		t.Fatal(err)
	}
	host, err = svc.SetHostAgentRootEnabled(context.Background(), host.ID, false, "test")
	if err != nil {
		t.Fatal(err)
	}
	if host.AgentRootEnabled || host.SudoMode != "password" || !host.HasSudoPassword {
		t.Fatalf("disabling Agent root changed sudo configuration: %#v", host)
	}
}

func TestSavingHostWithoutRootCapabilityClearsAgentRoot(t *testing.T) {
	svc, _, _ := newTestService(t)
	host, err := svc.SaveHost(context.Background(), domain.HostInput{
		Name: "sudo fixture", Address: "127.0.0.5", Port: 22, User: "ops", AuthType: "agent", SudoMode: "nopasswd",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetHostAgentRootEnabled(context.Background(), host.ID, true, "test"); err != nil {
		t.Fatal(err)
	}
	host, err = svc.SaveHost(context.Background(), domain.HostInput{
		ID: host.ID, Name: host.Name, Address: host.Address, Port: host.Port, User: host.User,
		AuthType: host.AuthType, SudoMode: "none",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if host.AgentRootEnabled {
		t.Fatal("Agent root remained enabled after sudo capability was removed")
	}
	if _, err := svc.SetHostAgentRootEnabled(context.Background(), host.ID, true, "test"); !errors.Is(err, ErrHostAgentRootUnavailable) {
		t.Fatalf("host without root capability was enabled: %v", err)
	}
}

func TestAgentRootAccessCoversEveryTransferEndpoint(t *testing.T) {
	destination := sshx.ConnectionSpec{Target: domain.Host{ID: "destination", AgentEnabled: true, User: "ops"}}
	source := sshx.ConnectionSpec{Target: domain.Host{ID: "source", AgentEnabled: true, User: "root"}}
	if err := authorizeAgentRootConnections("eino-agent", false, destination, source); !errors.Is(err, ErrAgentRootAccessDenied) {
		t.Fatalf("root transfer source bypassed the setting: %v", err)
	}
	source.Target.AgentRootEnabled = true
	if err := authorizeAgentRootConnections("eino-agent", false, destination, source); err != nil {
		t.Fatalf("enabled root transfer source was rejected: %v", err)
	}
}

func TestAgentRootAccessIsRecheckedWhenApprovalResumes(t *testing.T) {
	svc, transport, _ := newTestService(t)
	host, err := svc.AddHost(context.Background(), domain.Host{
		Name: "root fixture", Address: "127.0.0.2", Port: 22, User: "root", AgentEnabled: true,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	host, err = svc.SetHostAgentRootEnabled(context.Background(), host.ID, true, "test")
	if err != nil {
		t.Fatal(err)
	}
	const checkpointID = "checkpoint-root-revoked"
	ctx := WithAgentApprovalContinuation(WithSessionID(context.Background(), "session-root-revoked"), checkpointID)
	pending, err := svc.Submit(ctx, domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "id", Reason: "test revoked root access",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.store.Set(ctx, checkpointID, []byte("checkpoint")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ActivateAgentApprovals(ctx, checkpointID, map[string]string{pending.ApprovalID: "interrupt-root-revoked"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DecideAgentApproval(ctx, pending.ApprovalID, domain.ApprovalStatusApproved, "reviewed", "operator"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetHostAgentRootEnabled(ctx, host.ID, false, "operator"); err != nil {
		t.Fatal(err)
	}
	result, err := svc.ResumeAgentApproval(ctx, pending.ApprovalID)
	if !errors.Is(err, ErrAgentRootAccessDenied) {
		t.Fatalf("revoked approval resume error = %v", err)
	}
	if result.Status != "failed" || len(transport.calls) != 0 {
		t.Fatalf("revoked approval executed: result=%#v calls=%d", result, len(transport.calls))
	}
}

func TestAgentRootAccessCoversProxyJumpChain(t *testing.T) {
	svc, transport, _ := newTestService(t)
	jump, err := svc.AddHost(context.Background(), domain.Host{
		Name: "root jump", Address: "127.0.0.2", Port: 22, User: "root", AgentEnabled: true,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	target, err := svc.AddHost(context.Background(), domain.Host{
		Name: "target", Address: "127.0.0.3", Port: 22, User: "ops", AgentEnabled: true, ProxyJumpHostID: jump.ID,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	request := domain.ExecRequest{HostID: target.ID, Mode: domain.ExecProgram, Program: "id", Reason: "test root jump authorization"}
	if _, err := svc.Submit(context.Background(), request, "eino-agent"); !errors.Is(err, ErrAgentRootAccessDenied) {
		t.Fatalf("root ProxyJump bypassed the setting: %v", err)
	}
	if len(transport.calls) != 0 {
		t.Fatalf("denied ProxyJump request reached transport %d times", len(transport.calls))
	}
	if _, err := svc.SetHostAgentRootEnabled(context.Background(), jump.ID, true, "operator"); err != nil {
		t.Fatal(err)
	}
	pending, err := svc.Submit(context.Background(), request, "eino-agent")
	if err != nil || pending.Status != "approval_required" {
		t.Fatalf("enabled ProxyJump request = %#v, %v", pending, err)
	}
}
