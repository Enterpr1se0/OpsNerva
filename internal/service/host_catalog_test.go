package service

import (
	"context"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestAgentHostCatalogTracksEffectiveRootAccess(t *testing.T) {
	svc, _, ordinary := newTestService(t)
	ctx := context.Background()
	rootHost, err := svc.SaveHost(ctx, domain.HostInput{
		Name: "root catalog fixture", Address: "127.0.0.2", Port: 22, User: "root", AuthType: "agent", SudoMode: "none",
	}, "operator")
	if err != nil {
		t.Fatal(err)
	}

	capabilities, err := svc.ListHostCapabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(capabilities) != 1 || capabilities[0].ID != ordinary.ID {
		t.Fatalf("root-login host without root access was advertised: %#v", capabilities)
	}

	if _, err := svc.SetHostAgentRootEnabled(ctx, rootHost.ID, true, "operator"); err != nil {
		t.Fatal(err)
	}
	capabilities, err = svc.ListHostCapabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, capability := range capabilities {
		if capability.ID == rootHost.ID {
			if !capability.Root || capability.Shell != "bash" {
				t.Fatalf("enabled root host capability = %#v", capability)
			}
			return
		}
	}
	t.Fatalf("enabled root host is missing: %#v", capabilities)
}

func TestAgentHostCatalogPersistsShellForStableConnection(t *testing.T) {
	svc, transport, host := newTestService(t)
	ctx := context.Background()
	capabilities, err := svc.ListHostCapabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(capabilities) != 1 || capabilities[0].Shell != "bash" {
		t.Fatalf("host capabilities = %#v", capabilities)
	}
	if transport.probeCalls != 1 {
		t.Fatalf("stable host shell probes = %d, want 1", transport.probeCalls)
	}
	stored, err := svc.store.GetHost(ctx, host.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.DetectedShell != "/usr/bin/bash" || stored.DetectedShellBinding == "" {
		t.Fatalf("detected shell was not persisted: %#v", stored)
	}

	restarted := New(svc.store, transport, svc.encryptor, svc.redactor, svc.limits)
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := restarted.Shutdown(shutdownCtx); err != nil {
			t.Errorf("shutdown restarted service: %v", err)
		}
	})
	capabilities, err = restarted.ListHostCapabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(capabilities) != 1 || capabilities[0].Shell != "bash" || transport.probeCalls != 1 {
		t.Fatalf("restarted service did not reuse the persisted shell: capabilities=%#v probes=%d", capabilities, transport.probeCalls)
	}

	if _, err := svc.SaveHost(ctx, domain.HostInput{
		ID: host.ID, Name: host.Name, Address: host.Address, Port: host.Port, User: host.User,
		AuthType: host.AuthType, SudoMode: host.SudoMode,
	}, "operator"); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.ListHostCapabilities(ctx); err != nil {
		t.Fatal(err)
	}
	if transport.probeCalls != 2 {
		t.Fatalf("changed host shell probes = %d, want 2", transport.probeCalls)
	}
}

func TestShellDetectionIsSharedByProbeScriptAndInteractiveShell(t *testing.T) {
	svc, transport, host := newTestService(t)
	ctx := context.Background()
	saveApprovalMode(t, svc, domain.ApprovalModeFullAccess)

	info, err := svc.ProbeHost(ctx, host.ID, "operator")
	if err != nil || info.Shell != "bash" {
		t.Fatalf("probe result: info=%#v err=%v", info, err)
	}
	result, err := svc.Submit(ctx, domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecScript, Script: "printf ok", Reason: "verify persisted shell reuse",
	}, "eino-agent")
	if err != nil || result.Status != "completed" {
		t.Fatalf("script result: result=%#v err=%v", result, err)
	}

	sessionCtx := WithSessionID(ctx, "session-shell-reuse")
	if _, err := svc.PrepareChatSession(sessionCtx, "session-shell-reuse", "", "test"); err != nil {
		t.Fatal(err)
	}
	result, err = svc.StartSSHShell(sessionCtx, host.ID, "", false, 80, 24, "verify persisted shell reuse", "eino-agent")
	if err != nil || result.Status != "completed" || result.Shell == nil {
		t.Fatalf("interactive shell result: result=%#v err=%v", result, err)
	}

	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.probeCalls != 1 {
		t.Fatalf("shared shell detection probes = %d, want 1", transport.probeCalls)
	}
	if len(transport.connectionShells) != 1 || transport.connectionShells[0] != "/usr/bin/bash" {
		t.Fatalf("script connection shells = %#v", transport.connectionShells)
	}
	if len(transport.shellConnectionShells) != 1 || transport.shellConnectionShells[0] != "/usr/bin/bash" {
		t.Fatalf("interactive connection shells = %#v", transport.shellConnectionShells)
	}
}

func TestRunScriptDetectsAndPersistsMissingShellOnce(t *testing.T) {
	svc, transport, host := newTestService(t)
	ctx := context.Background()
	saveApprovalMode(t, svc, domain.ApprovalModeFullAccess)

	for range 2 {
		result, err := svc.Submit(ctx, domain.ExecRequest{
			HostID: host.ID, Mode: domain.ExecScript, Script: "printf ok", Reason: "verify first script shell detection",
		}, "eino-agent")
		if err != nil || result.Status != "completed" {
			t.Fatalf("script result: result=%#v err=%v", result, err)
		}
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.probeCalls != 1 {
		t.Fatalf("script shell probes = %d, want 1", transport.probeCalls)
	}
	if len(transport.connectionShells) != 2 || transport.connectionShells[0] != "/usr/bin/bash" || transport.connectionShells[1] != "/usr/bin/bash" {
		t.Fatalf("script connection shells = %#v", transport.connectionShells)
	}
}
