package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/proxyx"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"
	"github.com/Enterpr1se0/opsnerva/internal/store"
)

var ErrProxyInUse = errors.New("proxy is in use")

const proxyTestTarget = "https://example.com/"

type resolvedProxy struct {
	domain.Proxy
	Password string
}

func (s *Service) SaveProxy(ctx context.Context, input domain.ProxyInput, actor string) (domain.Proxy, error) {
	input.ID = strings.TrimSpace(input.ID)
	input.Name = strings.TrimSpace(input.Name)
	input.Username = strings.TrimSpace(input.Username)
	if input.Name == "" {
		return domain.Proxy{}, fmt.Errorf("proxy name is required")
	}
	if len(input.Name) > 128 {
		return domain.Proxy{}, fmt.Errorf("proxy name is too long")
	}
	proxyURL, err := proxyx.NormalizeURL(input.URL)
	if err != nil {
		return domain.Proxy{}, err
	}
	if proxyURL == "" {
		return domain.Proxy{}, fmt.Errorf("proxy URL is required")
	}
	if len(proxyURL) > 2048 {
		return domain.Proxy{}, fmt.Errorf("proxy URL is too long")
	}
	if containsCredentialControl(input.Username) || containsCredentialControl(input.Password) {
		return domain.Proxy{}, fmt.Errorf("proxy credentials cannot contain NUL, carriage return, or newline characters")
	}
	if len(input.Username) > 255 || len(input.Password) > 255 {
		return domain.Proxy{}, fmt.Errorf("proxy credentials are too long")
	}

	proxy := domain.Proxy{ID: input.ID, Name: input.Name, URL: proxyURL, Username: input.Username}
	if input.ID != "" {
		existing, err := s.store.GetProxy(ctx, input.ID)
		if err != nil {
			return domain.Proxy{}, err
		}
		proxy.CreatedAt = existing.CreatedAt
		if existing.Username == proxy.Username {
			proxy.PasswordCipher = existing.PasswordCipher
		}
	}
	if input.ClearPassword || proxy.Username == "" {
		proxy.PasswordCipher = ""
	} else if input.Password != "" {
		proxy.PasswordCipher, err = s.encryptor.Encrypt([]byte(input.Password))
		if err != nil {
			return domain.Proxy{}, fmt.Errorf("encrypt proxy password: %w", err)
		}
	}
	saved, err := s.store.UpsertProxy(ctx, proxy)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique constraint") {
			return domain.Proxy{}, fmt.Errorf("proxy name already exists")
		}
		return domain.Proxy{}, err
	}
	s.audit(ctx, "", "proxy_saved", actor, map[string]any{
		"proxy_id": saved.ID, "name": saved.Name, "url": saved.URL, "authenticated": saved.HasPassword,
	})
	return decorateProxy(saved), nil
}

func (s *Service) ListProxies(ctx context.Context) ([]domain.Proxy, error) {
	proxies, err := s.store.ListProxies(ctx)
	if err != nil {
		return nil, err
	}
	for index := range proxies {
		proxies[index] = decorateProxy(proxies[index])
	}
	return proxies, nil
}

func (s *Service) GetProxy(ctx context.Context, id string) (domain.Proxy, error) {
	proxy, err := s.store.GetProxy(ctx, strings.TrimSpace(id))
	if err != nil {
		return domain.Proxy{}, err
	}
	return decorateProxy(proxy), nil
}

func decorateProxy(proxy domain.Proxy) domain.Proxy {
	_, err := sshx.NormalizeProxyURL(proxy.URL)
	proxy.SSHCompatible = err == nil
	return proxy
}

func (s *Service) DeleteProxy(ctx context.Context, id, actor string) error {
	proxy, err := s.store.GetProxy(ctx, strings.TrimSpace(id))
	if err != nil {
		return err
	}
	references, err := s.store.ProxyReferences(ctx, proxy.ID)
	if err != nil {
		return err
	}
	if len(references) != 0 {
		return fmt.Errorf("%w: %s", ErrProxyInUse, strings.Join(references, ", "))
	}
	if err := s.store.DeleteProxy(ctx, proxy.ID); err != nil {
		if errors.Is(err, store.ErrInUse) {
			references, referenceErr := s.store.ProxyReferences(ctx, proxy.ID)
			if referenceErr != nil {
				return referenceErr
			}
			return fmt.Errorf("%w: %s", ErrProxyInUse, strings.Join(references, ", "))
		}
		return err
	}
	s.audit(ctx, "", "proxy_deleted", actor, map[string]any{"proxy_id": proxy.ID, "name": proxy.Name})
	return nil
}

func (s *Service) resolveProxy(ctx context.Context, id string) (resolvedProxy, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return resolvedProxy{}, nil
	}
	proxy, err := s.store.GetProxy(ctx, id)
	if err != nil {
		return resolvedProxy{}, fmt.Errorf("load proxy %q: %w", id, err)
	}
	password, err := s.encryptor.Decrypt(proxy.PasswordCipher)
	if err != nil {
		return resolvedProxy{}, fmt.Errorf("decrypt proxy password: %w", err)
	}
	return resolvedProxy{Proxy: proxy, Password: string(password)}, nil
}

func (s *Service) TestProxy(ctx context.Context, id, actor string) (domain.ProxyTestResult, error) {
	proxy, err := s.resolveProxy(ctx, id)
	if err != nil {
		return domain.ProxyTestResult{}, err
	}
	if proxy.ID == "" {
		return domain.ProxyTestResult{}, fmt.Errorf("proxy is required")
	}
	client, err := proxyx.NewHTTPClient(proxy.URL, proxy.Username, proxy.Password, 10*time.Second)
	if err != nil {
		return domain.ProxyTestResult{}, err
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, proxyTestTarget, nil)
	if err != nil {
		return domain.ProxyTestResult{}, err
	}
	started := time.Now()
	response, err := client.Do(request)
	latency := time.Since(started).Milliseconds()
	if err != nil {
		s.audit(ctx, "", "proxy_test_failed", actor, map[string]any{"proxy_id": proxy.ID, "latency_ms": latency})
		return domain.ProxyTestResult{}, fmt.Errorf("proxy test failed: %w", err)
	}
	response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusBadRequest {
		s.audit(ctx, "", "proxy_test_failed", actor, map[string]any{
			"proxy_id": proxy.ID, "status_code": response.StatusCode, "latency_ms": latency,
		})
		return domain.ProxyTestResult{}, fmt.Errorf("proxy test failed: target returned HTTP %d", response.StatusCode)
	}
	result := domain.ProxyTestResult{OK: true, StatusCode: response.StatusCode, LatencyMS: latency, Target: proxyTestTarget}
	s.audit(ctx, "", "proxy_test_completed", actor, map[string]any{
		"proxy_id": proxy.ID, "status_code": response.StatusCode, "latency_ms": latency,
	})
	return result, nil
}
