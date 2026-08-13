package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"eino-ops-agent/internal/config"
)

func TestOptionalAuthenticationSessionLifecycle(t *testing.T) {
	manager := newAuthManager(config.Auth{Username: "operator", Password: "test-password", SessionTTLHours: 1})
	protected := manager.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/hosts", nil)
	response := httptest.NewRecorder()
	protected.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || response.Header().Get("X-OpsNerva-Auth") != "required" {
		t.Fatalf("unauthenticated response = %d, %#v", response.Code, response.Header())
	}

	server := &Server{auth: manager}
	login := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"operator","password":"test-password"}`))
	login.RemoteAddr = "127.0.0.1:12345"
	loginResponse := httptest.NewRecorder()
	server.authLogin(loginResponse, login)
	if loginResponse.Code != http.StatusOK || len(loginResponse.Result().Cookies()) != 1 {
		t.Fatalf("login response = %d, cookies %#v, body %s", loginResponse.Code, loginResponse.Result().Cookies(), loginResponse.Body.String())
	}
	cookie := loginResponse.Result().Cookies()[0]
	authorized := httptest.NewRequest(http.MethodGet, "/api/v1/hosts", nil)
	authorized.AddCookie(cookie)
	authorizedResponse := httptest.NewRecorder()
	protected.ServeHTTP(authorizedResponse, authorized)
	if authorizedResponse.Code != http.StatusNoContent {
		t.Fatalf("authenticated response = %d", authorizedResponse.Code)
	}

	logout := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	logout.AddCookie(cookie)
	logoutResponse := httptest.NewRecorder()
	server.authLogout(logoutResponse, logout)
	if logoutResponse.Code != http.StatusNoContent {
		t.Fatalf("logout response = %d", logoutResponse.Code)
	}
	afterLogout := httptest.NewRequest(http.MethodGet, "/api/v1/hosts", nil)
	afterLogout.AddCookie(cookie)
	afterLogoutResponse := httptest.NewRecorder()
	protected.ServeHTTP(afterLogoutResponse, afterLogout)
	if afterLogoutResponse.Code != http.StatusUnauthorized {
		t.Fatalf("logged-out session response = %d", afterLogoutResponse.Code)
	}
}

func TestAuthenticationAllowsSPAAssets(t *testing.T) {
	manager := newAuthManager(config.Auth{Username: "operator", Password: "test-password", SessionTTLHours: 1})
	handler := manager.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, path := range []string{"/", "/assets/index.js", "/favicon.svg"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusNoContent {
			t.Fatalf("expected %s to remain publicly loadable, got %d", path, response.Code)
		}
	}
}
