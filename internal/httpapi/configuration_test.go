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

func TestAuthenticatedConfigurationExportIsJSONAndImportableWithoutPackagePassword(t *testing.T) {
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
	if response.StatusCode != http.StatusOK || !bytes.Contains(payload, []byte("http-export-secret")) {
		t.Fatalf("configuration export = status %d, body %s", response.StatusCode, payload)
	}
	disposition, parameters, err := mime.ParseMediaType(response.Header.Get("Content-Disposition"))
	if err != nil || disposition != "attachment" || !strings.HasSuffix(parameters["filename"], ".json") {
		t.Fatalf("export disposition = %q %#v, %v", disposition, parameters, err)
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("export content type = %q", contentType)
	}
	var exported domain.ConfigurationPackage
	if err := json.Unmarshal(payload, &exported); err != nil || !exported.SecretsIncluded {
		t.Fatalf("decode export = %#v, %v", exported, err)
	}

	importResponse := postConfigurationPackage(t, client, server.URL, payload)
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

func TestConfigurationCredentialsExportAndImportWithoutAuthentication(t *testing.T) {
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
	payload, err := json.Marshal(domain.ConfigurationPackage{
		Schema: domain.ConfigurationSchema, SchemaVersion: domain.ConfigurationSchemaVersion, SecretsIncluded: true,
		Proxies: []domain.ConfigurationProxy{}, Hosts: []domain.ConfigurationHost{},
		ModelProviders: []domain.ConfigurationModelProvider{{
			ID: "model-import", Name: "Imported model", Kind: "openai", Model: "gpt-import",
			APIKeyConfigured: true, APIKey: "import-secret", Active: true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := postConfigurationPackage(t, server.Client(), server.URL, payload)
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("credential import without authentication = %d, %s", response.StatusCode, body)
	}

	response, err = server.Client().Get(server.URL + "/api/v1/configuration/export")
	if err != nil {
		t.Fatal(err)
	}
	exported, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !bytes.Contains(exported, []byte("import-secret")) {
		t.Fatalf("credential export without authentication = %d, %s", response.StatusCode, exported)
	}
	var configuration domain.ConfigurationPackage
	if err := json.Unmarshal(exported, &configuration); err != nil || !configuration.SecretsIncluded {
		t.Fatalf("decode credential export = %#v, %v", configuration, err)
	}
}

func postConfigurationPackage(t *testing.T, client *http.Client, baseURL string, payload []byte) *http.Response {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, err := writer.CreateFormFile("file", "configuration.json")
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
