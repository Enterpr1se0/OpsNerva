package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestDirectSudoIsRejectedInProgramAndScriptModes(t *testing.T) {
	svc, _, host := newTestService(t)
	requests := []domain.ExecRequest{
		{HostID: host.ID, Mode: domain.ExecProgram, Program: "sudo", Args: []string{"id"}, Reason: "bad direct sudo"},
		{HostID: host.ID, Mode: domain.ExecScript, Script: "echo preparing\nsudo systemctl restart api", Reason: "bad script sudo"},
	}
	for _, req := range requests {
		if _, err := svc.Submit(context.Background(), req, "test"); err == nil || !strings.Contains(err.Error(), "elevated=true") {
			t.Fatalf("direct sudo was not rejected: %v", err)
		}
	}
}

func TestInteractiveCommandsAndPackagePromptsAreRejected(t *testing.T) {
	svc, transport, host := newTestService(t)
	routingRequests := []struct {
		request       domain.ExecRequest
		suggestedTool string
	}{
		{domain.ExecRequest{HostID: host.ID, Mode: domain.ExecProgram, Program: "bash", Reason: "open shell"}, "ssh_shell"},
		{domain.ExecRequest{HostID: host.ID, Mode: domain.ExecProgram, Program: "bash", Args: []string{"-lc", "uname -a | head -1"}, Reason: "inspect kernel"}, "ssh_run_script"},
		{domain.ExecRequest{HostID: host.ID, Mode: domain.ExecProgram, Program: "top", Reason: "inspect processes"}, "ssh_exec"},
	}
	for _, testCase := range routingRequests {
		_, err := svc.Submit(context.Background(), testCase.request, "test")
		var selectionErr *ExecutionToolSelectionError
		if !errors.As(err, &selectionErr) || selectionErr.SuggestedTool != testCase.suggestedTool || selectionErr.NextAction == "" || len(selectionErr.Example) == 0 {
			t.Fatalf("interactive request did not return actionable routing details: request=%#v err=%#v", testCase.request, err)
		}
	}
	requests := []domain.ExecRequest{
		{HostID: host.ID, Mode: domain.ExecProgram, Program: "pacman", Args: []string{"-S", "nginx"}, Reason: "install nginx"},
		{HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"edit", "nginx"}, Reason: "edit unit"},
	}
	for _, request := range requests {
		if _, err := svc.Submit(context.Background(), request, "test"); err == nil {
			t.Fatalf("interactive request was accepted: %#v", request)
		}
	}
	if len(transport.calls) != 0 {
		t.Fatal("rejected interactive commands reached transport")
	}
}

func TestNormalizeRequestUsesSeparateSyncAndBackgroundTimeoutDefaults(t *testing.T) {
	limits := config.Limits{SyncTimeoutSeconds: 60, MaxTimeoutSeconds: 600}
	synchronous := domain.ExecRequest{Mode: domain.ExecProgram}
	normalizeRequest(&synchronous, limits)
	if synchronous.TimeoutSeconds != 60 {
		t.Fatalf("synchronous default timeout = %d, want 60", synchronous.TimeoutSeconds)
	}
	background := domain.ExecRequest{Mode: domain.ExecProgram, Background: true}
	normalizeRequest(&background, limits)
	if background.TimeoutSeconds != 600 {
		t.Fatalf("background default timeout = %d, want 600", background.TimeoutSeconds)
	}
	explicit := domain.ExecRequest{Mode: domain.ExecProgram, Background: true, TimeoutSeconds: 90}
	normalizeRequest(&explicit, limits)
	if explicit.TimeoutSeconds != 90 {
		t.Fatalf("explicit background timeout = %d, want 90", explicit.TimeoutSeconds)
	}
}

func TestToolInputValidationIsTyped(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "execution contract",
			err: validateExecutionRequest(domain.Host{}, domain.ExecRequest{
				Mode: domain.ExecScript, Script: "sudo id", Reason: "test direct sudo rejection",
			}),
		},
		{name: "remote path", err: validateRemoteFilePath("relative.txt")},
		{name: "file search", err: validateFileSearchInput("needle", "contains", 0)},
		{
			name: "host transfer",
			err: validateSSHFileTransferRequest(domain.ExecRequest{
				HostID: "destination", SourceHostID: "source", SourcePath: "relative.txt",
			}),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var validation *InputValidationError
			if !errors.As(test.err, &validation) {
				t.Fatalf("input error is not typed validation: %T %v", test.err, test.err)
			}
		})
	}
}
