package service

import (
	"context"
	"strings"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestApprovedRequestRejectsChangedSSHConnection(t *testing.T) {
	svc, transport, host := newTestService(t)
	result, err := svc.Submit(context.Background(), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "example.service"},
		Reason: "restart the example service",
	}, "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "approval_required" {
		t.Fatalf("expected an approval, got %#v", result)
	}
	_, err = svc.SaveHost(context.Background(), domain.HostInput{
		ID: host.ID, Name: host.Name, Address: "127.0.0.2", Port: host.Port, User: host.User,
		AuthType: host.AuthType, SudoMode: host.SudoMode,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	approved, err := svc.Approve(context.Background(), result.ApprovalID, "connection reviewed", "operator")
	if err == nil || !strings.Contains(err.Error(), "changed after submission") {
		t.Fatalf("changed SSH connection was executed: result=%#v error=%v", approved, err)
	}
	if len(transport.calls) != 0 {
		t.Fatal("changed SSH connection reached the transport")
	}
}

func TestProxyJumpChainResolutionAndCycleDetection(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	outer, err := svc.SaveHost(ctx, domain.HostInput{
		Name: "outer-jump", Address: "192.0.2.20", Port: 22, User: "ops",
		AuthType: "agent", SudoMode: "none",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	inner, err := svc.SaveHost(ctx, domain.HostInput{
		Name: "inner-jump", Address: "192.0.2.21", Port: 22, User: "ops",
		AuthType: "agent", ProxyJumpHostID: outer.ID, SudoMode: "none",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	target, err := svc.SaveHost(ctx, domain.HostInput{
		Name: "jump-target", Address: "192.0.2.22", Port: 22, User: "ops",
		AuthType: "agent", ProxyJumpHostID: inner.ID, SudoMode: "none",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	connection, digest, err := svc.resolveSSHConnection(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if digest == "" || len(connection.Jumps) != 2 || connection.Jumps[0].ID != outer.ID || connection.Jumps[1].ID != inner.ID {
		t.Fatalf("unexpected resolved jump chain: %#v digest=%q", connection, digest)
	}
	outer, err = svc.SaveHost(ctx, domain.HostInput{
		ID: outer.ID, Name: outer.Name, Address: outer.Address, Port: outer.Port, User: outer.User,
		AuthType: "agent", ProxyJumpHostID: target.ID, SudoMode: "none",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.resolveSSHConnection(ctx, target); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("ProxyJump cycle was not rejected: %v", err)
	}
}
