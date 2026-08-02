package security

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/store"

	"golang.org/x/crypto/argon2"
)

const SessionCookieName = "opsnerva_session"

var ErrAlreadyInitialized = errors.New("administrator password is already initialized")

type WebAuth struct {
	store      *store.Store
	sessionTTL time.Duration
}

func NewWebAuth(st *store.Store, sessionTTL time.Duration) *WebAuth {
	if sessionTTL <= 0 {
		sessionTTL = 12 * time.Hour
	}
	return &WebAuth{store: st, sessionTTL: sessionTTL}
}

func (a *WebAuth) IsInitialized(ctx context.Context) (bool, error) {
	_, err := a.store.AdminPasswordHash(ctx)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return false, err
}

func (a *WebAuth) InitializePassword(ctx context.Context, password string) (string, domain.WebSession, error) {
	if initialized, err := a.IsInitialized(ctx); err != nil {
		return "", domain.WebSession{}, err
	} else if initialized {
		return "", domain.WebSession{}, ErrAlreadyInitialized
	}
	if err := validateAdminPassword(password); err != nil {
		return "", domain.WebSession{}, err
	}
	hash, err := hashAdminPassword(password)
	if err != nil {
		return "", domain.WebSession{}, err
	}
	if err := a.store.CreateAdminPasswordHash(ctx, hash); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			return "", domain.WebSession{}, ErrAlreadyInitialized
		}
		return "", domain.WebSession{}, err
	}
	return a.createSession(ctx)
}

func (a *WebAuth) Login(ctx context.Context, password string) (string, domain.WebSession, error) {
	hash, err := a.store.AdminPasswordHash(ctx)
	if err != nil {
		return "", domain.WebSession{}, err
	}
	valid, err := verifyAdminPassword(password, hash)
	if err != nil || !valid {
		return "", domain.WebSession{}, fmt.Errorf("invalid administrator credentials")
	}
	return a.createSession(ctx)
}

func (a *WebAuth) createSession(ctx context.Context) (string, domain.WebSession, error) {
	token, err := randomToken(32)
	if err != nil {
		return "", domain.WebSession{}, err
	}
	csrf, err := randomToken(24)
	if err != nil {
		return "", domain.WebSession{}, err
	}
	now := time.Now().UTC()
	session := domain.WebSession{TokenHash: tokenHash(token), CSRFToken: csrf, CreatedAt: now, ExpiresAt: now.Add(a.sessionTTL)}
	if err := a.store.CreateWebSession(ctx, session); err != nil {
		return "", domain.WebSession{}, err
	}
	return token, session, nil
}

func (a *WebAuth) Authenticate(ctx context.Context, token string) (domain.WebSession, error) {
	if strings.TrimSpace(token) == "" {
		return domain.WebSession{}, store.ErrNotFound
	}
	return a.store.GetWebSession(ctx, tokenHash(token))
}

func (a *WebAuth) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return a.store.DeleteWebSession(ctx, tokenHash(token))
}

func (a *WebAuth) ChangePassword(ctx context.Context, current, replacement string) error {
	if err := validateAdminPassword(replacement); err != nil {
		return err
	}
	hash, err := a.store.AdminPasswordHash(ctx)
	if err != nil {
		return err
	}
	valid, err := verifyAdminPassword(current, hash)
	if err != nil || !valid {
		return fmt.Errorf("current administrator password is invalid")
	}
	replacementHash, err := hashAdminPassword(replacement)
	if err != nil {
		return err
	}
	if err := a.store.SetAdminPasswordHash(ctx, replacementHash); err != nil {
		return err
	}
	return a.store.DeleteAllWebSessions(ctx)
}

func (a *WebAuth) ResetPassword(ctx context.Context, replacement string) error {
	if err := validateAdminPassword(replacement); err != nil {
		return err
	}
	hash, err := hashAdminPassword(replacement)
	if err != nil {
		return err
	}
	if err := a.store.SetAdminPasswordHash(ctx, hash); err != nil {
		return err
	}
	return a.store.DeleteAllWebSessions(ctx)
}

func validateAdminPassword(value string) error {
	if len(value) < 12 {
		return fmt.Errorf("administrator password must contain at least 12 characters")
	}
	if len(value) > 1024 || strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("administrator password is invalid")
	}
	return nil
}

func hashAdminPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	const memory = 64 * 1024
	const iterations = 3
	const parallelism = 2
	key := argon2.IDKey([]byte(password), salt, iterations, memory, parallelism, 32)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", memory, iterations, parallelism,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

func verifyAdminPassword(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" {
		return false, fmt.Errorf("invalid password hash")
	}
	params := strings.Split(parts[3], ",")
	if len(params) != 3 {
		return false, fmt.Errorf("invalid password hash parameters")
	}
	memoryValue, err := strconv.ParseUint(strings.TrimPrefix(params[0], "m="), 10, 32)
	if err != nil {
		return false, err
	}
	iterationValue, err := strconv.ParseUint(strings.TrimPrefix(params[1], "t="), 10, 32)
	if err != nil {
		return false, err
	}
	parallelValue, err := strconv.ParseUint(strings.TrimPrefix(params[2], "p="), 10, 8)
	if err != nil {
		return false, err
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, err
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, uint32(iterationValue), uint32(memoryValue), uint8(parallelValue), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

func randomToken(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func tokenHash(token string) string {
	digest := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%x", digest[:])
}
