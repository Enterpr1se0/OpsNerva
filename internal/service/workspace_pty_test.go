//go:build !windows

package service

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestWorkspacePTYDrainsTrailingOutputOnProcessExit(t *testing.T) {
	var output strings.Builder
	session, err := startWorkspacePTY(
		context.Background(), "/bin/bash", []string{"--noprofile", "--norc", "-c", "printf 'trailing-output\\n'"},
		"", os.Environ(), 80, 24,
		func(_ string, data []byte) { output.Write(data) },
	)
	if err != nil {
		t.Fatal(err)
	}
	result := session.Wait()
	if result.Err != nil || result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("Workspace PTY process failed: %#v", result)
	}
	if !strings.Contains(output.String(), "trailing-output") {
		t.Fatalf("trailing PTY output was lost: %q", output.String())
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceSandboxPTYKeepsJobControl(t *testing.T) {
	bubblewrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("Bubblewrap is unavailable")
	}
	svc, _ := newWorkspaceService(t, "read_write")
	svc.workspaceSandboxPath = bubblewrap
	workspace, ok := svc.workspaceByID("project")
	if !ok {
		t.Fatal("test Workspace is unavailable")
	}
	program, args, directory, environment, err := svc.workspacePTYCommand(workspace, domain.ExecRequest{
		WorkspaceShellBackend: domain.WorkspaceShellModeSandbox,
		Cwd:                   ".",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, argument := range args {
		if argument == "--new-session" {
			t.Fatal("interactive Bubblewrap command starts a second terminal session")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var output strings.Builder
	session, err := startWorkspacePTY(ctx, program, args, directory, environment, 80, 24, func(_ string, data []byte) {
		output.Write(data)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Write([]byte("set -m; sleep 0.05 & wait; echo job-control-ok; exit\r")); err != nil {
		t.Fatal(err)
	}
	result := session.Wait()
	text := output.String()
	if strings.Contains(text, "Operation not permitted") || strings.Contains(text, "No permissions to create new namespace") {
		t.Skipf("Bubblewrap namespaces are unavailable: %s", text)
	}
	if result.Err != nil || result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("interactive sandbox failed: result=%#v output=%q", result, text)
	}
	if strings.Contains(text, "no job control") || strings.Contains(text, "cannot set terminal process group") || !strings.Contains(text, "job-control-ok") {
		t.Fatalf("interactive sandbox has no job control: %q", text)
	}
}
