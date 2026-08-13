package httpapi

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"eino-ops-agent/internal/config"
)

const authSessionCookie = "opsnerva_session"
const maxAuthSessions = 64

type authManager struct {
	config   config.Auth
	mu       sync.Mutex
	sessions map[[sha256.Size]byte]time.Time
	failures map[string]authFailures
}

type authFailures struct {
	count        int
	windowStart  time.Time
	blockedUntil time.Time
}

func newAuthManager(auth config.Auth) *authManager {
	return &authManager{config: auth, sessions: make(map[[sha256.Size]byte]time.Time), failures: make(map[string]authFailures)}
}

func (a *authManager) enabled() bool { return a != nil && a.config.Enabled() }

func (a *authManager) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.enabled() || authPublicPath(r.URL.Path) || r.URL.Path == "/mcp" {
			next.ServeHTTP(w, r)
			return
		}
		if !a.authenticated(r) {
			w.Header().Set("X-OpsNerva-Auth", "required")
			w.Header().Set("Cache-Control", "no-store")
			writeErrorStatus(w, fmt.Errorf("authentication required"), http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func authPublicPath(path string) bool {
	if path == "/api/v1/auth/status" || path == "/api/v1/auth/login" || path == "/api/v1/auth/logout" {
		return true
	}
	return !strings.HasPrefix(path, "/api/")
}

func (a *authManager) authenticated(r *http.Request) bool {
	if !a.enabled() {
		return true
	}
	cookie, err := r.Cookie(authSessionCookie)
	if err != nil || cookie.Value == "" {
		return false
	}
	digest := sha256.Sum256([]byte(cookie.Value))
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	expires, ok := a.sessions[digest]
	if !ok || !expires.After(now) {
		delete(a.sessions, digest)
		return false
	}
	return true
}

func (a *authManager) validCredentials(username, password string) bool {
	if !a.enabled() {
		return true
	}
	expectedUser := sha256.Sum256([]byte(a.config.Username))
	actualUser := sha256.Sum256([]byte(strings.TrimSpace(username)))
	expectedPassword := sha256.Sum256([]byte(a.config.Password))
	actualPassword := sha256.Sum256([]byte(password))
	userMatch := subtle.ConstantTimeCompare(expectedUser[:], actualUser[:])
	passwordMatch := subtle.ConstantTimeCompare(expectedPassword[:], actualPassword[:])
	return userMatch&passwordMatch == 1
}

func (a *authManager) loginAllowed(client string) (bool, time.Duration) {
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	state := a.failures[client]
	if state.blockedUntil.After(now) {
		return false, state.blockedUntil.Sub(now)
	}
	if !state.windowStart.IsZero() && now.Sub(state.windowStart) > 10*time.Minute {
		delete(a.failures, client)
	}
	return true, 0
}

func (a *authManager) recordLoginFailure(client string) {
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	state := a.failures[client]
	if state.windowStart.IsZero() || now.Sub(state.windowStart) > 10*time.Minute {
		state = authFailures{windowStart: now}
	}
	state.count++
	if state.count >= 5 {
		state.blockedUntil = now.Add(30 * time.Second)
		state.count = 0
		state.windowStart = now
	}
	a.failures[client] = state
}

func (a *authManager) clearLoginFailures(client string) {
	a.mu.Lock()
	delete(a.failures, client)
	a.mu.Unlock()
}

func (a *authManager) createSession(w http.ResponseWriter, r *http.Request) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("generate authentication session: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	digest := sha256.Sum256([]byte(token))
	ttl := time.Duration(a.config.SessionTTLHours) * time.Hour
	expires := time.Now().Add(ttl)
	a.mu.Lock()
	var earliestKey [sha256.Size]byte
	hasEarliest := false
	earliestExpiry := expires
	now := time.Now()
	for key, expiry := range a.sessions {
		if !expiry.After(now) {
			delete(a.sessions, key)
			continue
		}
		if !hasEarliest || expiry.Before(earliestExpiry) {
			earliestKey = key
			earliestExpiry = expiry
			hasEarliest = true
		}
	}
	if len(a.sessions) >= maxAuthSessions && hasEarliest {
		delete(a.sessions, earliestKey)
	}
	a.sessions[digest] = expires
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: authSessionCookie, Value: token, Path: "/", HttpOnly: true,
		Secure: requestIsHTTPS(r), SameSite: http.SameSiteLaxMode, Expires: expires, MaxAge: int(ttl.Seconds()),
	})
	return nil
}

func (a *authManager) clearSession(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(authSessionCookie); err == nil && cookie.Value != "" {
		digest := sha256.Sum256([]byte(cookie.Value))
		a.mu.Lock()
		delete(a.sessions, digest)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name: authSessionCookie, Value: "", Path: "/", HttpOnly: true,
		Secure: requestIsHTTPS(r), SameSite: http.SameSiteLaxMode, MaxAge: -1, Expires: time.Unix(1, 0),
	})
}

func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	authenticated := s.auth.authenticated(r)
	result := map[string]any{"enabled": s.auth.enabled(), "authenticated": authenticated}
	if authenticated && s.auth.enabled() {
		result["username"] = s.auth.config.Username
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.auth.enabled() {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "authenticated": true})
		return
	}
	client := requestClientIP(r)
	if allowed, retryAfter := s.auth.loginAllowed(client); !allowed {
		seconds := int(retryAfter.Round(time.Second).Seconds())
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", fmt.Sprintf("%d", seconds))
		writeErrorStatus(w, fmt.Errorf("too many login attempts; retry later"), http.StatusTooManyRequests)
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeLimit(w, r, &input, 8<<10) {
		return
	}
	if !s.auth.validCredentials(input.Username, input.Password) {
		s.auth.recordLoginFailure(client)
		writeErrorStatus(w, fmt.Errorf("invalid username or password"), http.StatusUnauthorized)
		return
	}
	s.auth.clearLoginFailures(client)
	if err := s.auth.createSession(w, r); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "authenticated": true, "username": s.auth.config.Username})
}

func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	s.auth.clearSession(w, r)
	w.WriteHeader(http.StatusNoContent)
}

func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])
	return strings.EqualFold(forwarded, "https")
}

func requestClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}
