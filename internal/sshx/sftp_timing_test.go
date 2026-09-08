package sshx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/observability"
)

func TestSFTPTimingLogLevels(t *testing.T) {
	for _, test := range []struct {
		name, operation, level, status string
		slow, canceled                 bool
		err                            error
	}{
		{name: "fast list", operation: "list", level: "DEBUG", status: "completed"},
		{name: "delete", operation: "delete", level: "INFO", status: "completed"},
		{name: "slow list", operation: "list", slow: true, level: "WARN", status: "completed"},
		{name: "failure", operation: "delete", err: errors.New("permission denied"), level: "WARN", status: "failed"},
		{name: "superseded list", operation: "list", slow: true, canceled: true, err: context.Canceled, level: "DEBUG", status: "canceled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})).With("request_id", "sftp-request")
			ctx, cancel := context.WithCancel(observability.WithLogger(context.Background(), logger))
			defer cancel()
			if test.canceled {
				cancel()
			}
			timing := newSFTPTiming(ctx, test.operation, "host-1")
			if test.slow {
				timing.started = timing.started.Add(-slowSFTPOperationThreshold)
			}
			timing.last = timing.last.Add(-10 * time.Millisecond)
			timing.mark("connection_acquire")
			timing.last = timing.last.Add(-10 * time.Millisecond)
			timing.mark("connection_acquire")
			timing.finish(test.err)
			var record map[string]any
			if err := json.Unmarshal(output.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			if record["level"] != test.level || record["status"] != test.status || record["request_id"] != "sftp-request" || record["host_id"] != "host-1" {
				t.Fatalf("unexpected diagnostic: %v", record)
			}
			if record["connection_acquire_ms"].(float64) < 20 {
				t.Fatalf("retry stage timings were not accumulated: %v", record)
			}
		})
	}
}

func TestNativeSFTPDeleteAndListDiagnostics(t *testing.T) {
	server := startTestSSHServer(t, "sftp-diagnostics-password")
	transport := NewNativeSSHTransport(config.SSH{DefaultKnownHosts: filepath.Join(t.TempDir(), "known_hosts")}, config.Default().Limits)
	t.Cleanup(func() { _ = transport.Close() })
	connection := ConnectionSpec{Target: server.host()}
	connection.Target.ID = "sftp-diagnostics-host"
	key, err := transport.ScanHostKey(context.Background(), connection)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.TrustHostKey(context.Background(), connection, key.Fingerprint); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(server.root, "delete-tree")
	if err := os.MkdirAll(filepath.Join(directory, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one.txt", "nested/two.txt"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("keep until deletion"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	remotePath := testSFTPPath(directory)
	var output bytes.Buffer
	ctx := observability.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})))
	check := func(operation, status string, stages ...string) {
		t.Helper()
		found := 0
		for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
			var record map[string]any
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				t.Fatal(err)
			}
			if record["operation"] != operation {
				continue
			}
			found++
			if record["status"] != status {
				t.Fatalf("unexpected status: %v", record)
			}
			for _, stage := range stages {
				if _, ok := record[stage+"_ms"].(float64); !ok {
					t.Fatalf("missing %s timing: %v", stage, record)
				}
			}
		}
		if found != 1 {
			t.Fatalf("expected one %s summary, got %d: %s", operation, found, output.String())
		}
	}

	if _, err := transport.RemoveSFTPEntry(ctx, connection, remotePath, false, nil); err == nil {
		t.Fatal("non-recursive deletion of a non-empty directory succeeded")
	}
	check("delete", "failed", "open", "inspect", "delete", "release")
	check("open", "completed", "connection_acquire", "subsystem_start")
	output.Reset()
	listing, err := transport.ListSFTPFiles(ctx, connection, remotePath)
	if err != nil || len(listing.Entries) != 2 {
		t.Fatalf("directory reconciliation = %+v, error = %v", listing, err)
	}
	check("list", "completed", "open", "read_directory", "format_sort", "release")
	output.Reset()
	deleted, err := transport.RemoveSFTPEntry(ctx, connection, remotePath, true, nil)
	if err != nil || deleted.Path != remotePath || deleted.Type != "directory" {
		t.Fatalf("deletion result = %+v, error = %v", deleted, err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("directory still exists: %v", err)
	}
	check("delete", "completed", "open", "inspect", "delete", "release")

	// Cancellation of an obsolete directory read must not prevent the
	// replacement directory request from succeeding.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := transport.ListSFTPFiles(canceled, connection, testSFTPPath(server.root)); err == nil {
		t.Fatal("canceled directory request succeeded")
	}
	if _, err := transport.ListSFTPFiles(ctx, connection, testSFTPPath(server.root)); err != nil {
		t.Fatalf("listing after cancellation failed: %v", err)
	}
}
