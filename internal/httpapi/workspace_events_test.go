package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/security"
	"github.com/Enterpr1se0/opsnerva/internal/service"
	"github.com/Enterpr1se0/opsnerva/internal/store"
)

func TestWorkspaceFileEventsStreamsExternalChanges(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	workspaceRoot := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dataDir, "workspace-events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	encryptor, err := security.NewEncryptor("", dataDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.DataDir = dataDir
	svc := service.New(st, nil, encryptor, security.NewRedactor(), cfg.Limits, cfg)
	if err := svc.InitializeWorkspaces(ctx, workspaceRoot); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(New(svc, nil, Options{}).Handler())
	defer server.Close()
	requestCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, server.URL+"/api/v1/workspaces/default/events?path=.", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("unexpected workspace event response: status=%d content_type=%q", response.StatusCode, response.Header.Get("Content-Type"))
	}

	lines := make(chan string, 16)
	scanErrors := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(response.Body)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		scanErrors <- scanner.Err()
	}()
	if err := os.WriteFile(filepath.Join(workspaceRoot, "default", "outside.txt"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(3 * time.Second)
	seenEvent := false
	for {
		select {
		case line := <-lines:
			if line == "event: workspace-change" {
				seenEvent = true
				continue
			}
			if !seenEvent || !strings.HasPrefix(line, "data: ") {
				continue
			}
			var change service.WorkspaceFileChange
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &change); err != nil {
				t.Fatal(err)
			}
			if change.WorkspaceID != "default" || change.Path != "." {
				t.Fatalf("unexpected streamed workspace change: %#v", change)
			}
			return
		case scanErr := <-scanErrors:
			t.Fatalf("workspace event stream ended early: %v", scanErr)
		case <-deadline:
			t.Fatal("timed out waiting for workspace SSE event")
		}
	}
}
