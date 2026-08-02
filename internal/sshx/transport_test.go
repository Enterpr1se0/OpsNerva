package sshx

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"eino-ops-agent/internal/domain"
)

func TestCaptureBufferPreservesCompleteOutput(t *testing.T) {
	payload := bytes.Repeat([]byte("complete-output-"), 100_000)
	buffer := newCaptureBuffer()
	for offset := 0; offset < len(payload); offset += 8191 {
		end := min(offset+8191, len(payload))
		if _, err := buffer.Write(payload[offset:end]); err != nil {
			t.Fatal(err)
		}
	}
	if got := buffer.Bytes(); !bytes.Equal(got, payload) {
		t.Fatalf("captured output differs: got=%d want=%d", len(got), len(payload))
	}
}

func TestBuildRemoteProgramQuotesArguments(t *testing.T) {
	command, stdin, err := buildRemoteCommand(domain.ExecRequest{
		Mode: domain.ExecProgram, Program: "printf", Args: []string{"%s", "hello; rm -rf /"}, Cwd: "/srv/app",
		Env: map[string]string{"MODE": "safe value"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stdin != nil {
		t.Fatal("program mode unexpectedly has stdin")
	}
	wantParts := []string{"cd -- '/srv/app' &&", "MODE='safe value'", "'hello; rm -rf /'"}
	for _, part := range wantParts {
		if !strings.Contains(command, part) {
			t.Fatalf("command %q does not contain %q", command, part)
		}
	}
}

func TestManagedSudoPasswordUsesStdin(t *testing.T) {
	command, stdin, err := applyElevation(domain.Host{SudoMode: "password", SudoPassword: "sudo-secret"}, domain.ExecRequest{Elevated: true}, "bash -s", strings.NewReader("echo ok\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"sudo -S -p '' -- bash -c", "IFS= read -r OPSNERVA_SUDO_INPUT", "exec bash -c"} {
		if !strings.Contains(command, part) {
			t.Fatalf("managed sudo command %q does not contain %q", command, part)
		}
	}
	if strings.Contains(command, "sudo-secret") {
		t.Fatalf("unexpected elevated command %q", command)
	}
	data, _ := io.ReadAll(stdin)
	lines := strings.SplitN(string(data), "\n", 3)
	if len(lines) != 3 || lines[0] != "sudo-secret" || !strings.HasPrefix(lines[1], "__OPSNERVA_SUDO_INPUT_") || lines[2] != "echo ok\n" {
		t.Fatalf("unexpected managed sudo stdin %q", data)
	}
	if !strings.Contains(command, lines[1]) {
		t.Fatalf("managed sudo command does not contain stdin marker %q", lines[1])
	}
}

func TestManagedSudoDoesNotFeedPasswordToCommand(t *testing.T) {
	dir := t.TempDir()
	fakeSudo := filepath.Join(dir, "sudo")
	if err := os.WriteFile(fakeSudo, []byte(`#!/bin/sh
if [ "${FAKE_SUDO_READ_PASSWORD:-}" = "1" ]; then
	IFS= read -r ignored_password
fi
while [ "$#" -gt 0 ]; do
	case "$1" in
		--) shift; break ;;
		*) shift ;;
	esac
done
exec "$@"
`), 0o700); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name         string
		readPassword bool
	}{
		{name: "cached credential"},
		{name: "password prompt", readPassword: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			password := "printf 'password-leaked\\n'"
			command, stdin, err := applyElevation(
				domain.Host{SudoMode: "password", SudoPassword: password},
				domain.ExecRequest{Elevated: true},
				"bash -s",
				strings.NewReader("printf 'command-input-ok\\n'\n"),
			)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(command, password) {
				t.Fatalf("sudo password was embedded in remote command %q", command)
			}

			process := exec.Command("/bin/sh", "-c", command)
			process.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
			if test.readPassword {
				process.Env = append(process.Env, "FAKE_SUDO_READ_PASSWORD=1")
			}
			process.Stdin = stdin
			output, err := process.CombinedOutput()
			if err != nil {
				t.Fatalf("run managed sudo wrapper: %v: %s", err, output)
			}
			if got := string(output); got != "command-input-ok\n" {
				t.Fatalf("password reached elevated command: output=%q", got)
			}
		})
	}
}

func TestBuildRemoteScriptUsesStdin(t *testing.T) {
	command, stdin, err := buildRemoteCommand(domain.ExecRequest{Mode: domain.ExecScript, Script: "uname -a"})
	if err != nil {
		t.Fatal(err)
	}
	if command != "bash -s" {
		t.Fatalf("unexpected command %q", command)
	}
	data, _ := io.ReadAll(stdin)
	if string(data) != "uname -a" {
		t.Fatalf("unexpected stdin %q", data)
	}
}

func TestProbeScriptFallsBackWhenCommonUtilitiesAreMissing(t *testing.T) {
	cmd := exec.Command("/bin/bash", "-se")
	cmd.Env = []string{"PATH=/opsnerva-no-such-path"}
	cmd.Stdin = strings.NewReader(probeScript)
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	info, err := parseProbeOutput(output)
	if err != nil {
		t.Fatal(err)
	}
	if info.Hostname == "" || info.Kernel == "" || info.Architecture == "" || info.User == "" || info.Uptime == "" {
		t.Fatalf("probe fallback returned incomplete info: %#v", info)
	}
}

func TestValidateHostRejectsOptionInjection(t *testing.T) {
	err := validateHost(domain.Host{Address: "-oProxyCommand=evil", User: "root", Port: 22})
	if err == nil {
		t.Fatal("expected host validation error")
	}
}
