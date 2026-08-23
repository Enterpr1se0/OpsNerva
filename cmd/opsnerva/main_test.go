package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/config"
)

func TestPrepareQuickStartUsesApplicationDirectoryAndKeepsConfig(t *testing.T) {
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	appDir := t.TempDir()

	path, created, err := prepareQuickStartIn(appDir)
	if err != nil {
		t.Fatal(err)
	}
	if !created || path != filepath.Join(appDir, config.DefaultFileName) {
		t.Fatalf("path=%q created=%v", path, created)
	}
	current, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if current != appDir {
		t.Fatalf("working directory = %q, want %q", current, appDir)
	}

	path, created, err = prepareQuickStartIn(appDir)
	if err != nil {
		t.Fatal(err)
	}
	if created || path != filepath.Join(appDir, config.DefaultFileName) {
		t.Fatalf("second start path=%q created=%v", path, created)
	}
}

func TestPrepareQuickStartUsesConfiguredHome(t *testing.T) {
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	appDir := t.TempDir()
	t.Setenv("OPSNERVA_HOME", appDir)

	path, created, err := prepareQuickStart()
	if err != nil {
		t.Fatal(err)
	}
	if !created || path != filepath.Join(appDir, config.DefaultFileName) {
		t.Fatalf("path=%q created=%v", path, created)
	}
}

func TestLocalWebURLUsesLoopbackForWildcardListener(t *testing.T) {
	address := &net.TCPAddr{IP: net.IPv6zero, Port: 49152}
	if got := localWebURL(address); got != "http://127.0.0.1:49152" {
		t.Fatalf("local URL = %q", got)
	}
}

func TestDesktopReadyLineIsMachineReadable(t *testing.T) {
	line := desktopReadyLine(serveOptions{
		Desktop: true, ConfigPath: `/tmp/opsnerva/config.yaml`, ConfigCreated: true, MCPHTTPEnabled: true,
		WorkspaceRoot: `/tmp/opsnerva/workspace`,
	}, "http://127.0.0.1:49152")
	const prefix = "OPSNERVA_DESKTOP_READY="
	if !strings.HasPrefix(line, prefix) {
		t.Fatalf("desktop line = %q", line)
	}
	var payload struct {
		URL               string `json:"url"`
		ConfigPath        string `json:"config_path"`
		ConfigurationMade bool   `json:"configuration_created"`
		MCPHTTPEnabled    bool   `json:"mcp_http_enabled"`
		WorkspaceRoot     string `json:"workspace_root"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, prefix)), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.URL != "http://127.0.0.1:49152" || !payload.ConfigurationMade || !payload.MCPHTTPEnabled || payload.WorkspaceRoot != "/tmp/opsnerva/workspace" {
		t.Fatalf("desktop payload = %#v", payload)
	}
}

func TestEnvBool(t *testing.T) {
	t.Setenv("OPSNERVA_DESKTOP", "true")
	if !envBool("OPSNERVA_DESKTOP") {
		t.Fatal("expected true")
	}
	t.Setenv("OPSNERVA_DESKTOP", "invalid")
	if envBool("OPSNERVA_DESKTOP") {
		t.Fatal("invalid boolean must be false")
	}
}
