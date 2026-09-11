package websearch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/proxyx"
)

const (
	maxWebSearchResponseBytes = 2 << 20
	maxErrorBytes             = 4 << 10
	maxWebRetryAfter          = 2 * time.Second
	webRetryDelay             = 100 * time.Millisecond
)

type tavilyRawResponse struct {
	Body []byte
	Meta CallInfo
}

func (c *Client) requestTavily(ctx context.Context, settings Config, path string, payload, output any) (CallInfo, error) {
	if err := ctx.Err(); err != nil {
		return CallInfo{}, err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return CallInfo{}, err
	}
	keyDigest := sha256.Sum256(bytes.Join([][]byte{
		[]byte(settings.BaseURL), []byte(settings.ProxyURL), []byte(settings.ProxyUsername), []byte(settings.ProxyPassword),
		[]byte(settings.APIKey), []byte(path), encoded,
	}, []byte{'\n'}))
	requestKey := hex.EncodeToString(keyDigest[:])
	resultChannel := c.requests.DoChan(requestKey, func() (any, error) {
		workerCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Duration(settings.TimeoutSeconds)*time.Second)
		defer cancel()
		return c.requestTavilyRaw(workerCtx, settings, path, encoded)
	})
	select {
	case <-ctx.Done():
		return CallInfo{}, ctx.Err()
	case shared := <-resultChannel:
		if shared.Err != nil {
			var requestError *tavilyRequestFailure
			if errors.As(shared.Err, &requestError) {
				return requestError.Meta, requestError.Err
			}
			return CallInfo{}, shared.Err
		}
		raw, ok := shared.Val.(tavilyRawResponse)
		if !ok {
			return CallInfo{}, fmt.Errorf("%w: invalid shared response", ErrUpstream)
		}
		if err := json.Unmarshal(raw.Body, output); err != nil {
			return raw.Meta, &ProviderError{Code: ErrorProviderUnavailable, StatusCode: raw.Meta.StatusCode, Retryable: true, Message: "decode response: " + err.Error()}
		}
		return raw.Meta, nil
	}
}

type tavilyRequestFailure struct {
	Meta CallInfo
	Err  error
}

func (e *tavilyRequestFailure) Error() string { return e.Err.Error() }
func (e *tavilyRequestFailure) Unwrap() error { return e.Err }

func (c *Client) requestTavilyRaw(ctx context.Context, settings Config, path string, encoded []byte) (tavilyRawResponse, error) {
	endpoint := strings.TrimRight(settings.BaseURL, "/") + path
	client, err := webSearchHTTPClient(settings)
	if err != nil {
		return tavilyRawResponse{}, err
	}
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return tavilyRawResponse{}, ctx.Err()
	}
	meta := CallInfo{}
	for attempt := 0; attempt < 2; attempt++ {
		meta.RetryCount = attempt
		httpRequest, requestErr := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
		if requestErr != nil {
			return tavilyRawResponse{}, requestErr
		}
		httpRequest.Header.Set("Authorization", "Bearer "+settings.APIKey)
		httpRequest.Header.Set("Content-Type", "application/json")
		httpRequest.Header.Set("Accept", "application/json")
		httpRequest.Header.Set("User-Agent", "OpsNerva-Tavily/1.1")
		response, requestErr := client.Do(httpRequest)
		if requestErr != nil {
			if errors.Is(requestErr, context.Canceled) {
				return tavilyRawResponse{}, requestErr
			}
			providerError := &ProviderError{
				Code: ErrorProviderUnavailable, Retryable: true,
				Message: truncateUTF8Bytes(c.scrubWebSearchText(requestErr.Error(), settings), maxErrorBytes),
			}
			if errors.Is(requestErr, context.DeadlineExceeded) {
				providerError.Code = ErrorTimeout
				providerError.Retryable = false
			}
			if attempt == 0 && providerError.Retryable {
				if err := waitWebRetry(ctx, webRetryDelay); err != nil {
					return tavilyRawResponse{}, err
				}
				continue
			}
			return tavilyRawResponse{}, &tavilyRequestFailure{Meta: meta, Err: providerError}
		}
		meta.StatusCode = response.StatusCode
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, maxErrorBytes+1))
			_ = response.Body.Close()
			if readErr != nil {
				body = []byte("unable to read provider error")
			}
			message := truncateUTF8Bytes(c.scrubWebSearchText(string(body), settings), maxErrorBytes)
			providerError := classifyTavilyStatus(response.StatusCode, response.Header.Get("Retry-After"), message)
			if attempt == 0 && providerError.Retryable {
				delay := providerError.RetryAfter
				if delay == 0 {
					delay = webRetryDelay
				}
				if delay <= maxWebRetryAfter {
					if err := waitWebRetry(ctx, delay); err != nil {
						return tavilyRawResponse{}, err
					}
					continue
				}
			}
			return tavilyRawResponse{}, &tavilyRequestFailure{Meta: meta, Err: providerError}
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, maxWebSearchResponseBytes+1))
		_ = response.Body.Close()
		if readErr != nil {
			return tavilyRawResponse{}, &tavilyRequestFailure{Meta: meta, Err: &ProviderError{
				Code: ErrorProviderUnavailable, StatusCode: response.StatusCode, Retryable: true, Message: "read response: " + readErr.Error(),
			}}
		}
		meta.ResponseBytes = len(body)
		if len(body) > maxWebSearchResponseBytes {
			return tavilyRawResponse{}, &tavilyRequestFailure{Meta: meta, Err: &ProviderError{
				Code: ErrorProviderUnavailable, StatusCode: response.StatusCode, Retryable: false, Message: "response exceeded 2 MiB",
			}}
		}
		return tavilyRawResponse{Body: body, Meta: meta}, nil
	}
	return tavilyRawResponse{}, &tavilyRequestFailure{Meta: meta, Err: &ProviderError{Code: ErrorProviderUnavailable, Retryable: true}}
}

func classifyTavilyStatus(statusCode int, retryAfterValue, message string) *ProviderError {
	retryAfter := parseWebRetryAfter(retryAfterValue, time.Now())
	result := &ProviderError{StatusCode: statusCode, Message: message, RetryAfter: retryAfter}
	switch {
	case statusCode == http.StatusBadRequest || statusCode == http.StatusUnprocessableEntity:
		result.Code = ErrorInvalidRequest
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		result.Code = ErrorAuthenticationFailed
	case statusCode == 432 || statusCode == 433:
		result.Code = ErrorQuotaExhausted
	case statusCode == http.StatusTooManyRequests:
		result.Code = ErrorRateLimited
		result.Retryable = retryAfter == 0 || retryAfter <= maxWebRetryAfter
	case statusCode == http.StatusRequestTimeout:
		result.Code = ErrorTimeout
		result.Retryable = true
	case statusCode >= 500:
		result.Code = ErrorProviderUnavailable
		result.Retryable = true
	default:
		result.Code = ErrorInvalidRequest
	}
	return result
}

func parseWebRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0
	}
	return when.Sub(now)
}

func waitWebRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func webSearchHTTPClient(settings Config) (*http.Client, error) {
	timeout := time.Duration(settings.TimeoutSeconds) * time.Second
	if settings.ProxyURL != "" {
		return proxyx.NewHTTPClient(settings.ProxyURL, settings.ProxyUsername, settings.ProxyPassword, timeout)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.ResponseHeaderTimeout = timeout
	return &http.Client{Transport: transport, Timeout: timeout}, nil
}
