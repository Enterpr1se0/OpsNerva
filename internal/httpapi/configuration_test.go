package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"eino-ops-agent/internal/config"
	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/security"
	"eino-ops-agent/internal/service"
	"eino-ops-agent/internal/store"
)

func TestAuthenticatedConfigurationExportIsEncryptedAndImportable(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dataDir, "configuration.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	encryptor, err := security.NewEncryptor("", dataDir)
	if err != nil {
		t.Fatal(err)
	}
	apiKeyCipher, err := encryptor.Encrypt([]byte("http-export-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelProvider(ctx, domain.ModelProvider{
		ID: "model-http-export", Name: "HTTP export", Kind: "openai", Model: "gpt-http", APIKeyCipher: apiKeyCipher, Active: true,
	}); err != nil {
		t.Fatal(err)
	}
	svc := service.New(st, nil, encryptor, security.NewRedactor(), config.Default().Limits)
	auth := config.Auth{Username: "operator", Password: "migration-password", SessionTTLHours: 1}
	server := httptest.NewServer(New(svc, nil, Options{Version: "test", Auth: auth}).Handler())
	defer server.Close()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := server.Client()
	client.Jar = jar

	response, err := client.Get(server.URL + "/api/v1/configuration/export")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || response.Header.Get("X-OpsNerva-Auth") != "required" {
		t.Fatalf("unauthenticated export = %d, %#v", response.StatusCode, response.Header)
	}
	login, err := client.Post(server.URL+"/api/v1/auth/login", "application/json", strings.NewReader(`{"username":"operator","password":"migration-password"}`))
	if err != nil {
		t.Fatal(err)
	}
	login.Body.Close()
	if login.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d", login.StatusCode)
	}
	response, err = client.Get(server.URL + "/api/v1/configuration/export")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || bytes.Contains(payload, []byte("http-export-secret")) {
		t.Fatalf("encrypted export = status %d, body %s", response.StatusCode, payload)
	}
	disposition, parameters, err := mime.ParseMediaType(response.Header.Get("Content-Disposition"))
	if err != nil || disposition != "attachment" || !strings.HasSuffix(parameters["filename"], ".opsnerva") {
		t.Fatalf("export disposition = %q %#v, %v", disposition, parameters, err)
	}
	plain, encrypted, err := security.OpenPortable("migration-password", payload)
	if err != nil || !encrypted || !bytes.Contains(plain, []byte("http-export-secret")) {
		t.Fatalf("decrypt export = encrypted %v, error %v, body %s", encrypted, err, plain)
	}

	wrongPasswordResponse := postConfigurationPackage(t, client, server.URL, payload, "wrong-password")
	defer wrongPasswordResponse.Body.Close()
	if wrongPasswordResponse.StatusCode != http.StatusUnauthorized || wrongPasswordResponse.Header.Get("X-OpsNerva-Auth") != "" {
		t.Fatalf("wrong package password = %d, %#v", wrongPasswordResponse.StatusCode, wrongPasswordResponse.Header)
	}
	importResponse := postConfigurationPackage(t, client, server.URL, payload, "migration-password")
	defer importResponse.Body.Close()
	if importResponse.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(importResponse.Body)
		t.Fatalf("import status = %d, body %s", importResponse.StatusCode, body)
	}
	var result domain.ConfigurationImportResult
	if err := json.NewDecoder(importResponse.Body).Decode(&result); err != nil || result.ModelProviders != 1 || !result.SecretsImported {
		t.Fatalf("import result = %#v, %v", result, err)
	}
}

func postConfigurationPackage(t *testing.T, client *http.Client, baseURL string, payload []byte, password string) *http.Response {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, err := writer.CreateFormFile("file", "configuration.opsnerva")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("password", password); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, baseURL+"/api/v1/configuration/import", &body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
