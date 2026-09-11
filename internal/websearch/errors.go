package websearch

import (
	"errors"
	"fmt"
	"time"
)

var ErrUpstream = errors.New("Tavily provider request failed")

const (
	ErrorInvalidRequest       = "invalid_request"
	ErrorAuthenticationFailed = "authentication_failed"
	ErrorRateLimited          = "rate_limited"
	ErrorQuotaExhausted       = "quota_exhausted"
	ErrorProviderUnavailable  = "provider_unavailable"
	ErrorTimeout              = "timeout"
)

// ProviderError keeps provider failures machine-readable without
// exposing large response bodies to the model or audit log.
type ProviderError struct {
	Code       string
	StatusCode int
	Retryable  bool
	RetryAfter time.Duration
	Message    string
}

func (e *ProviderError) Error() string {
	if e == nil {
		return ErrUpstream.Error()
	}
	if e.Message == "" {
		return fmt.Sprintf("%s: %s", ErrUpstream, e.Code)
	}
	return fmt.Sprintf("%s: %s: %s", ErrUpstream, e.Code, e.Message)
}

func (e *ProviderError) Unwrap() error { return ErrUpstream }
