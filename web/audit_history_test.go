package webui

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestAuditHistoryStateLifecycle(t *testing.T) {
	runAuditFrontendTest(t, "audit-history.test.mjs")
}

func TestAuditHistoryDetailLifecycle(t *testing.T) {
	runAuditFrontendTest(t, "audit-detail.test.mjs")
}

func TestAuditHistoryRendering(t *testing.T) {
	if _, err := os.Stat("node_modules/react/package.json"); os.IsNotExist(err) {
		t.Skip("Install Web dependencies to run component rendering tests")
	}
	runAuditFrontendTest(t, "audit-rendering.test.mjs")
}

func runAuditFrontendTest(t *testing.T, script string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for frontend state regression tests")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, node, "--experimental-strip-types", "scripts/tests/"+script)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("audit history state: %v\n%s", err, output)
	}
	t.Log(string(output))
}
