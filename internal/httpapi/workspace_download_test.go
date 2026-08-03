package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
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

func TestWorkspaceUploadStreamsRawRequestBody(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	workspaceRoot := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dataDir, "workspace-upload.db"))
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
	directory := filepath.Join(workspaceRoot, "default", "imports")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(New(svc, nil, Options{}).Handler())
	defer server.Close()
	content := []byte{'W', 0, 'S', 0xff}
	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/workspaces/default/files?path=imports%2Farchive.bin&filename=source.bin", bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("workspace upload status = %d: %s", response.StatusCode, body)
	}
	var result service.WorkspaceUploadResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	wantSHA := fmt.Sprintf("%x", sha256.Sum256(content))
	if result.Path != "imports/archive.bin" || result.Size != int64(len(content)) || result.SHA256 != wantSHA {
		t.Fatalf("unexpected upload result: %#v", result)
	}
	stored, err := os.ReadFile(filepath.Join(directory, "archive.bin"))
	if err != nil || !bytes.Equal(stored, content) {
		t.Fatalf("uploaded content = %v, err=%v", stored, err)
	}
}

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

	server := httptest.NewServer(New(svc, nil, Options{}).Handler())
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
