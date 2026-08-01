package httpapi

import (
	"context"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"eino-ops-agent/internal/config"
	"eino-ops-agent/internal/security"
	"eino-ops-agent/internal/service"
	"eino-ops-agent/internal/store"
)

func TestWorkspaceDownloadStreamsRegularFileAsAttachment(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	workspaceRoot := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dataDir, "workspace-download.db"))
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
	directory := filepath.Join(workspaceRoot, "default", "exports")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	content := []byte{'W', 0, 'S', 0xff}
	if err := os.WriteFile(filepath.Join(directory, "report.bin"), content, 0o600); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(New(svc, nil, nil, Options{}).Handler())
	defer server.Close()
	response, err := server.Client().Get(server.URL + "/api/v1/workspaces/default/download?path=exports%2Freport.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("workspace download status = %d", response.StatusCode)
	}
	disposition, parameters, err := mime.ParseMediaType(response.Header.Get("Content-Disposition"))
	if err != nil || disposition != "attachment" || parameters["filename"] != "report.bin" {
		t.Fatalf("workspace download disposition = %q %#v, err=%v", disposition, parameters, err)
	}
	if response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("workspace download safety headers = %#v", response.Header)
	}
	downloaded, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(downloaded) != string(content) {
		t.Fatalf("workspace download body = %v, want %v", downloaded, content)
	}
}
