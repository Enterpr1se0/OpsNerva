package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	modelopenai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

const (
	modelRequestMaxRetries        = 4
	modelRequestTimeoutMaxRetries = 2
)

type modelRequestRetryNotice struct {
	Attempt int
	Max     int
	Delay   time.Duration
	Err     error
}

type modelRequestRetryNotifier func(modelRequestRetryNotice)

type modelRequestRetryNotifierKey struct{}

func withModelRequestRetryNotifier(ctx context.Context, notifier modelRequestRetryNotifier) context.Context {
	return context.WithValue(ctx, modelRequestRetryNotifierKey{}, notifier)
}

func notifyModelRequestRetry(ctx context.Context, notice modelRequestRetryNotice) {
	notifier, _ := ctx.Value(modelRequestRetryNotifierKey{}).(modelRequestRetryNotifier)
	if notifier != nil {
		notifier(notice)
	}
}

func modelRequestRetryConfig() *adk.ModelRetryConfig {
	return &adk.ModelRetryConfig{
		MaxRetries: modelRequestMaxRetries,
		ShouldRetry: func(ctx context.Context, retryCtx *adk.RetryContext) *adk.RetryDecision {
			return modelRequestRetryDecision(ctx, retryCtx.RetryAttempt, retryCtx.OutputMessage, retryCtx.Err)
		},
		BackoffFunc: modelRequestRetryBackoff,
	}
}

func modelRequestRetryDecision(ctx context.Context, attempt int, output *schema.Message, err error) *adk.RetryDecision {
	retryLimit := modelRequestRetryLimit(err)
	if err == nil || attempt > retryLimit || ctx.Err() != nil || modelResponseHasContent(output) || !isRetryableModelRequestError(err) {
		return &adk.RetryDecision{}
	}
	notifyModelRequestRetry(ctx, modelRequestRetryNotice{
		Attempt: attempt,
		Max:     retryLimit,
		Delay:   modelRequestRetryBackoff(ctx, attempt),
		Err:     err,
	})
	return &adk.RetryDecision{Retry: true}
}

func modelRequestRetryLimit(err error) int {
	if isModelRequestTimeout(err) {
		return modelRequestTimeoutMaxRetries
	}
	return modelRequestMaxRetries
}

func modelResponseHasContent(message *schema.Message) bool {
	return message != nil && (message.Content != "" || message.ReasoningContent != "" || len(message.ToolCalls) > 0)
}

func isRetryableModelRequestError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var apiErr *modelopenai.APIError
	if errors.As(err, &apiErr) {
		return retryableModelHTTPStatus(apiErr.HTTPStatusCode)
	}
	if isModelRequestTimeout(err) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, status := range []int{408, 409, 425, 429, 500, 502, 503, 504} {
		code := fmt.Sprintf("%d", status)
		if strings.Contains(message, "status code: "+code) ||
			strings.Contains(message, "status: "+code) ||
			strings.Contains(message, "http "+code) {
			return true
		}
	}
	for _, fragment := range []string{
		"bad gateway",
		"service unavailable",
		"gateway timeout",
		"connection reset",
		"connection refused",
		"server closed idle connection",
		"client connection lost",
		"tls handshake timeout",
		"temporarily unavailable",
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

func retryableModelHTTPStatus(status int) bool {
	return status == 408 || status == 409 || status == 425 || status == 429 || status >= 500
}

func isModelRequestTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var networkErr net.Error
	return errors.As(err, &networkErr) && networkErr.Timeout()
}

func modelRequestRetryBackoff(_ context.Context, attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > modelRequestMaxRetries {
		attempt = modelRequestMaxRetries
	}
	return time.Second * time.Duration(1<<(attempt-1))
}

func generateModelWithRetry(ctx context.Context, chatModel model.ToolCallingChatModel, messages []*schema.Message) (*schema.Message, int, error) {
	for retry := 0; ; retry++ {
		message, err := chatModel.Generate(ctx, messages)
		decision := modelRequestRetryDecision(ctx, retry+1, message, err)
		if !decision.Retry || retry >= modelRequestMaxRetries {
			return message, retry, err
		}
		delay := modelRequestRetryBackoff(ctx, retry+1)
		select {
		case <-ctx.Done():
			return nil, retry, ctx.Err()
		case <-time.After(delay):
		}
	}
}
