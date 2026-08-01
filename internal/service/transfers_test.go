package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"eino-ops-agent/internal/domain"
)

func TestSSHFileTransferBindsBothHostsAndFileVersionsToApproval(t *testing.T) {
	svc, transport, _ := newTestService(t)
	ctx := context.Background()
	source, err := svc.SaveHost(ctx, domain.HostInput{
		Name: "source", Address: "192.0.2.31", Port: 22, User: "ops", AuthType: "agent", SudoMode: "none",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	destination, err := svc.SaveHost(ctx, domain.HostInput{
		Name: "destination", Address: "192.0.2.32", Port: 22, User: "ops", AuthType: "agent", SudoMode: "none",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	sourceSHA := strings.Repeat("a", 64)
	destinationSHA := strings.Repeat("b", 64)
	pending, err := svc.TransferFileBetweenHosts(ctx, source.ID, "/srv/releases/app.tar", sourceSHA, destination.ID, "/srv/releases/app.tar", destinationSHA, 300, "migrate the reviewed release artifact", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != "approval_required" {
		t.Fatalf("transfer did not require change approval: %#v", pending)
	}
	approval, err := svc.store.GetApproval(ctx, pending.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{source.ID, destination.ID, sourceSHA, destinationSHA, `"mode":"ssh_file_transfer"`, `"source_connection_digest"`, `"ssh_connection_digest"`} {
		if !strings.Contains(approval.RequestJSON, expected) {
			t.Fatalf("approval request does not bind %q: %s", expected, approval.RequestJSON)
		}
	}
	approved, err := svc.Approve(ctx, pending.ApprovalID, "reviewed both hosts and file versions", "operator")
	if err != nil || approved.Status != "completed" {
		t.Fatalf("approved transfer failed: result=%#v err=%v", approved, err)
	}
	if len(transport.calls) != 1 || transport.calls[0].Mode != domain.ExecSSHFileTransfer || transport.calls[0].SourceHostID != source.ID || transport.calls[0].HostID != destination.ID {
		t.Fatalf("transfer request did not reach the dual-host transport: %#v", transport.calls)
	}
	if len(transport.hosts) != 2 || transport.hosts[0].ID != source.ID || transport.hosts[1].ID != destination.ID {
		t.Fatalf("transfer connections were not resolved in source/destination order: %#v", transport.hosts)
	}
}

func TestSSHFileTransferPublishesByteProgress(t *testing.T) {
	svc, _, _ := newTestService(t)
	source, err := svc.SaveHost(context.Background(), domain.HostInput{
		Name: "progress-source", Address: "192.0.2.61", Port: 22, User: "ops", AuthType: "agent", SudoMode: "none",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	destination, err := svc.SaveHost(context.Background(), domain.HostInput{
		Name: "progress-destination", Address: "192.0.2.62", Port: 22, User: "ops", AuthType: "agent", SudoMode: "none",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "transfer_progress"
	events, unsubscribe := svc.SubscribeExecutionEvents(sessionID)
	defer unsubscribe()
	ctx := WithExecutionOwner(WithSessionID(context.Background(), sessionID), "call_transfer", "ssh_file_transfer", `{}`)
	pending, err := svc.TransferFileBetweenHosts(ctx, source.ID, "/tmp/source.bin", strings.Repeat("a", 64), destination.ID, "/tmp/destination.bin", "", 60, "transfer the artifact", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApproveAsync(context.Background(), pending.ApprovalID, "reviewed", "operator"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case event := <-events:
			if event.Stream == "progress" {
				if event.ToolCallID != "call_transfer" || event.TransferredBytes != 12 || event.TotalBytes != 12 {
					t.Fatalf("unexpected transfer progress event: %#v", event)
				}
				return
			}
		case <-deadline:
			t.Fatal("transfer progress event was not published")
		}
	}
}

func TestSSHFileTransferRejectsChangedSourceConnectionAfterApproval(t *testing.T) {
	svc, transport, _ := newTestService(t)
	ctx := context.Background()
	source, err := svc.SaveHost(ctx, domain.HostInput{
		Name: "source", Address: "192.0.2.41", Port: 22, User: "ops", AuthType: "agent", SudoMode: "none",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	destination, err := svc.SaveHost(ctx, domain.HostInput{
		Name: "destination", Address: "192.0.2.42", Port: 22, User: "ops", AuthType: "agent", SudoMode: "none",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	pending, err := svc.TransferFileBetweenHosts(ctx, source.ID, "/tmp/source.bin", strings.Repeat("c", 64), destination.ID, "/tmp/destination.bin", "", 60, "move a versioned artifact", "eino-agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SaveHost(ctx, domain.HostInput{
		ID: source.ID, Name: source.Name, Address: "192.0.2.99", Port: 22, User: source.User, AuthType: source.AuthType, SudoMode: source.SudoMode,
	}, "test"); err != nil {
		t.Fatal(err)
	}
	result, err := svc.Approve(ctx, pending.ApprovalID, "reviewed before host edit", "operator")
	if err == nil || !strings.Contains(err.Error(), "source SSH connection changed") || result.Status != "failed" {
		t.Fatalf("changed source connection was not rejected: result=%#v err=%v", result, err)
	}
	if len(transport.calls) != 0 {
		t.Fatalf("changed source connection reached transport: %#v", transport.calls)
	}
}

func TestSSHFileTransferRejectsMalformedDestinationVersion(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	source, _ := svc.SaveHost(ctx, domain.HostInput{Name: "source", Address: "192.0.2.51", Port: 22, User: "ops", AuthType: "agent", SudoMode: "none"}, "test")
	destination, _ := svc.SaveHost(ctx, domain.HostInput{Name: "destination", Address: "192.0.2.52", Port: 22, User: "ops", AuthType: "agent", SudoMode: "none"}, "test")
	_, err := svc.TransferFileBetweenHosts(ctx, source.ID, "/tmp/source", strings.Repeat("d", 64), destination.ID, "/tmp/destination", "not-a-sha256", 60, "replace destination", "test")
	if err == nil || !strings.Contains(err.Error(), "expected_destination_sha256") {
		t.Fatalf("malformed destination version was accepted: %v", err)
	}
}
