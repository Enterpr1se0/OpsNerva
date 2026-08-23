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

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/security"
	"github.com/Enterpr1se0/opsnerva/internal/service"
	"github.com/Enterpr1se0/opsnerva/internal/store"
)

func TestAuthenticatedConfigurationExportIsEncryptedAndPasswordProtected(t *testing.T) {
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
	auth := config.Auth{Username: "operator", Password: "control-password", SessionTTLHours: 1}
	server := httptest.NewServer(New(svc, nil, Options{Version: "test", Auth: auth}).Handler())
	defer server.Close()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := server.Client()
	client.Jar = jar

	response := exportConfigurationPackage(t, client, server.URL, "migration-password")
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || response.Header.Get("X-OpsNerva-Auth") != "required" {
		t.Fatalf("unauthenticated export = %d, %#v", response.StatusCode, response.Header)
	}
	login, err := client.Post(server.URL+"/api/v1/auth/login", "application/json", strings.NewReader(`{"username":"operator","password":"control-password"}`))
	if err != nil {
		t.Fatal(err)
	}
	login.Body.Close()
	if login.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d", login.StatusCode)
	}
	shortPassword := exportConfigurationPackage(t, client, server.URL, "short")
	shortPassword.Body.Close()
	if shortPassword.StatusCode != http.StatusBadRequest {
		t.Fatalf("short export password = %d", shortPassword.StatusCode)
	}

	response = exportConfigurationPackage(t, client, server.URL, "migration-password")
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || bytes.Contains(payload, []byte("http-export-secret")) || bytes.Contains(payload, []byte(domain.ConfigurationSchema)) {
		t.Fatalf("encrypted configuration export = status %d, body %q", response.StatusCode, payload)
	}
	disposition, parameters, err := mime.ParseMediaType(response.Header.Get("Content-Disposition"))
	if err != nil || disposition != "attachment" || !strings.HasSuffix(parameters["filename"], ".opsnerva-config") {
		t.Fatalf("export disposition = %q %#v, %v", disposition, parameters, err)
	}
	if contentType := response.Header.Get("Content-Type"); contentType != configurationPackageMediaType {
		t.Fatalf("export content type = %q", contentType)
	}
	plain, err := security.DecryptConfigurationPackage(payload, "migration-password")
	if err != nil {
		t.Fatal(err)
	}
	var exported domain.ConfigurationPackage
	if err := json.Unmarshal(plain, &exported); err != nil || exported.SchemaVersion != domain.ConfigurationSchemaVersion || exported.ModelProviders[0].APIKey != "http-export-secret" {
		t.Fatalf("decode export = %#v, %v", exported, err)
	}

	wrongPassword := postConfigurationPackage(t, client, server.URL, payload, "different-password")
	wrongPassword.Body.Close()
	if wrongPassword.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong password import = %d", wrongPassword.StatusCode)
	}
	importResponse := postConfigurationPackage(t, client, server.URL, payload, "migration-password")
	defer importResponse.Body.Close()
	if importResponse.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(importResponse.Body)
		t.Fatalf("import status = %d, body %s", importResponse.StatusCode, body)
	}
	var result domain.ConfigurationImportResult
	if err := json.NewDecoder(importResponse.Body).Decode(&result); err != nil || result.ModelProviders != 1 {
		t.Fatalf("import result = %#v, %v", result, err)
	}
}

func TestConfigurationTransferWithoutControlAuthenticationStillRequiresPackagePassword(t *testing.T) {
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
	svc := service.New(st, nil, encryptor, security.NewRedactor(), config.Default().Limits)
	server := httptest.NewServer(New(svc, nil, Options{Version: "test"}).Handler())
	defer server.Close()

	plain, err := json.Marshal(domain.ConfigurationPackage{
		Schema: domain.ConfigurationSchema, SchemaVersion: domain.ConfigurationSchemaVersion,
		Proxies: []domain.ConfigurationProxy{}, Hosts: []domain.ConfigurationHost{},
		ModelProviders: []domain.ConfigurationModelProvider{{
			ID: "model-import", Name: "Imported model", Kind: "openai", Model: "gpt-import", APIKey: "import-secret", Active: true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := security.EncryptConfigurationPackage(plain, "migration-password")
	if err != nil {
		t.Fatal(err)
	}
	response := postConfigurationPackage(t, server.Client(), server.URL, payload, "migration-password")
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("encrypted import without authentication = %d, %s", response.StatusCode, body)
	}

	response = exportConfigurationPackage(t, server.Client(), server.URL, "new-package-password")
	exported, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || bytes.Contains(exported, []byte("import-secret")) {
		t.Fatalf("encrypted export without authentication = %d, %q", response.StatusCode, exported)
	}
	decoded, err := security.DecryptConfigurationPackage(exported, "new-package-password")
	if err != nil || !bytes.Contains(decoded, []byte("import-secret")) {
		t.Fatalf("decrypt exported configuration = %q, %v", decoded, err)
	}
}

func exportConfigurationPackage(t *testing.T, client *http.Client, baseURL, password string) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]string{"password": password})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Post(baseURL+"/api/v1/configuration/export", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func postConfigurationPackage(t *testing.T, client *http.Client, baseURL string, payload []byte, password string) *http.Response {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("password", password); err != nil {
		t.Fatal(err)
	}
	file, err := writer.CreateFormFile("file", "configuration.opsnerva-config")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(payload); err != nil {
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
