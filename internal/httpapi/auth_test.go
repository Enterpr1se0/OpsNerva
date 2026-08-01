package httpapi

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"eino-ops-agent/internal/security"
	"eino-ops-agent/internal/store"
)

func TestWebAuthenticationCookieAndCSRF(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/http-auth.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	auth := security.NewWebAuth(st, time.Hour)
	handler := New(nil, nil, auth, Options{}).Handler()

	statusRequest := httptest.NewRequest(http.MethodGet, "/api/v1/auth/status", nil)
	statusResponse := httptest.NewRecorder()
	handler.ServeHTTP(statusResponse, statusRequest)
	if statusResponse.Code != http.StatusOK || !strings.Contains(statusResponse.Body.String(), `"initialized":false`) {
		t.Fatalf("initial auth status=%d body=%s", statusResponse.Code, statusResponse.Body.String())
	}

	initialize := httptest.NewRequest(http.MethodPost, "/api/v1/auth/initialize", bytes.NewBufferString(`{"password":"correct horse battery staple"}`))
	initialize.Header.Set("Content-Type", "application/json")
	initialize.RemoteAddr = "127.0.0.1:12345"
	initializeResponse := httptest.NewRecorder()
	handler.ServeHTTP(initializeResponse, initialize)
	if initializeResponse.Code != http.StatusCreated {
		t.Fatalf("initialize status=%d body=%s", initializeResponse.Code, initializeResponse.Body.String())
	}
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(initializeResponse.Body.Bytes(), &session); err != nil || session.CSRF == "" {
		t.Fatalf("invalid initialization response: %v %s", err, initializeResponse.Body.String())
	}
	var cookie *http.Cookie
	for _, candidate := range initializeResponse.Result().Cookies() {
		if candidate.Name == security.SessionCookieName {
			cookie = candidate
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("secure session cookie missing: %#v", cookie)
	}
	initializedStatus := httptest.NewRequest(http.MethodGet, "/api/v1/auth/status", nil)
	initializedStatusResponse := httptest.NewRecorder()
	handler.ServeHTTP(initializedStatusResponse, initializedStatus)
	if initializedStatusResponse.Code != http.StatusOK || !strings.Contains(initializedStatusResponse.Body.String(), `"initialized":true`) {
		t.Fatalf("initialized auth status=%d body=%s", initializedStatusResponse.Code, initializedStatusResponse.Body.String())
	}
	secondInitialize := httptest.NewRequest(http.MethodPost, "/api/v1/auth/initialize", bytes.NewBufferString(`{"password":"different secure password"}`))
	secondInitializeResponse := httptest.NewRecorder()
	handler.ServeHTTP(secondInitializeResponse, secondInitialize)
	if secondInitializeResponse.Code != http.StatusConflict {
		t.Fatalf("second initialization status=%d body=%s", secondInitializeResponse.Code, secondInitializeResponse.Body.String())
	}

	unauthenticatedExport := httptest.NewRequest(http.MethodGet, "/api/v1/logs/export", nil)
	unauthenticatedExportResponse := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticatedExportResponse, unauthenticatedExport)
	if unauthenticatedExportResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated log export returned %d", unauthenticatedExportResponse.Code)
	}

	withoutCSRF := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", bytes.NewBufferString(`{}`))
	withoutCSRF.AddCookie(cookie)
	withoutCSRFResponse := httptest.NewRecorder()
	handler.ServeHTTP(withoutCSRFResponse, withoutCSRF)
	if withoutCSRFResponse.Code != http.StatusForbidden {
		t.Fatalf("unsafe request without CSRF returned %d", withoutCSRFResponse.Code)
	}

	logout := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", bytes.NewBufferString(`{}`))
	logout.AddCookie(cookie)
	logout.Header.Set("X-CSRF-Token", session.CSRF)
	logoutResponse := httptest.NewRecorder()
	handler.ServeHTTP(logoutResponse, logout)
	if logoutResponse.Code != http.StatusNoContent {
		t.Fatalf("logout status=%d body=%s", logoutResponse.Code, logoutResponse.Body.String())
	}

	stale := httptest.NewRequest(http.MethodGet, "/api/v1/auth/session", nil)
	stale.AddCookie(cookie)
	staleResponse := httptest.NewRecorder()
	handler.ServeHTTP(staleResponse, stale)
	if staleResponse.Code != http.StatusUnauthorized {
		t.Fatalf("logged out cookie returned %d", staleResponse.Code)
	}

	login := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewBufferString(`{"password":"correct horse battery staple"}`))
	login.Header.Set("Content-Type", "application/json")
	login.RemoteAddr = "127.0.0.1:12345"
	loginResponse := httptest.NewRecorder()
	handler.ServeHTTP(loginResponse, login)
	if loginResponse.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", loginResponse.Code, loginResponse.Body.String())
	}
}

func TestUnknownAPIRouteReturnsJSONNotSPA(t *testing.T) {
	handler := New(nil, nil, nil, Options{}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/no-such-endpoint", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown API status=%d body=%s", response.Code, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("unknown API returned content type %q", contentType)
	}
}

func TestSPAHandlerServesAssetsAndIndexFallback(t *testing.T) {
	handler := spaHandler(fstest.MapFS{
		"index.html":    {Data: []byte("<main>embedded app</main>")},
		"assets/app.js": {Data: []byte("console.log('embedded')")},
	})

	for _, test := range []struct {
		path string
		body string
	}{
		{path: "/", body: "<main>embedded app</main>"},
		{path: "/conversations/session", body: "<main>embedded app</main>"},
		{path: "/assets/app.js", body: "console.log('embedded')"},
	} {
		request := httptest.NewRequest(http.MethodGet, test.path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || response.Body.String() != test.body {
			t.Fatalf("GET %s: status=%d body=%q", test.path, response.Code, response.Body.String())
		}
	}
}

func TestAgentToolsEndpointReportsAnUnloadedRuntimeWithoutPanicking(t *testing.T) {
	handler := New(nil, nil, nil, Options{}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/agent/tools", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("tool catalog status=%d body=%s", response.Code, response.Body.String())
	}
	var catalog struct {
		Loaded bool  `json:"loaded"`
		Count  int   `json:"count"`
		Tools  []any `json:"tools"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if catalog.Loaded || catalog.Count != 0 || catalog.Tools == nil {
		t.Fatalf("unexpected unloaded catalog: %#v", catalog)
	}
}

func TestLogExportReturnsDownloadableZip(t *testing.T) {
	handler := New(nil, nil, nil, Options{Version: "test-version"}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/logs/export", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("log export status=%d body=%s", response.Code, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "application/zip" {
		t.Fatalf("log export content type = %q", contentType)
	}
	if disposition := response.Header().Get("Content-Disposition"); !strings.HasPrefix(disposition, "attachment;") || !strings.Contains(disposition, "opsnerva-diagnostics-") {
		t.Fatalf("log export content disposition = %q", disposition)
	}
	archive, err := zip.NewReader(bytes.NewReader(response.Body.Bytes()), int64(response.Body.Len()))
	if err != nil {
		t.Fatalf("parse log export: %v", err)
	}
	if len(archive.File) != 2 || archive.File[0].Name != "diagnostics.json" || archive.File[1].Name != "ops-agent-memory.jsonl" {
		t.Fatalf("unexpected log export entries: %#v", archive.File)
	}
	manifest, err := archive.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	defer manifest.Close()
	var diagnostics struct {
		SchemaVersion int `json:"schema_version"`
		Application   struct {
			Version string `json:"version"`
		} `json:"application"`
	}
	if err := json.NewDecoder(manifest).Decode(&diagnostics); err != nil {
		t.Fatal(err)
	}
	if diagnostics.SchemaVersion != 1 || diagnostics.Application.Version != "test-version" {
		t.Fatalf("unexpected diagnostics: %#v", diagnostics)
	}
}

func TestCancelChatSessionReportsUnavailableRuntime(t *testing.T) {
	handler := New(nil, nil, nil, Options{}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/chat/session_test/cancel", bytes.NewBufferString(`{}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("cancel status=%d body=%s", response.Code, response.Body.String())
	}
}
