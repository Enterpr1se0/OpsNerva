package agent

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"

	modelopenai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

const modelRequestMaxRetries = 4

func modelRequestRetryConfig() *adk.ModelRetryConfig {
	return &adk.ModelRetryConfig{
		MaxRetries: modelRequestMaxRetries,
		ShouldRetry: func(ctx context.Context, retryCtx *adk.RetryContext) *adk.RetryDecision {
			if retryCtx == nil || ctx.Err() != nil || modelResponseHasContent(retryCtx.OutputMessage) {
				return &adk.RetryDecision{}
			}
			return &adk.RetryDecision{Retry: retryCtx.Err == nil || isRetryableModelRequestError(retryCtx.Err)}
		},
	}
}

func modelResponseHasContent(message *schema.Message) bool {
	return message != nil && (message.Content != "" || message.ReasoningContent != "" || len(message.ToolCalls) > 0)
}

func isRetryableModelRequestError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var apiErr *modelopenai.APIError
	if errors.As(err, &apiErr) {
		status := apiErr.HTTPStatusCode
		return status == 408 || status == 409 || status == 425 || status == 429 || status >= 500
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) && (networkErr.Timeout() || networkErr.Temporary()) {
		return true
	}

	message := strings.ToLower(err.Error())
	for _, status := range []int{408, 409, 425, 429, 500, 502, 503, 504} {
		code := strconv.Itoa(status)
		if strings.Contains(message, "status code: "+code) || strings.Contains(message, "status: "+code) || strings.Contains(message, "http "+code) {
			return true
		}
	}
	for _, fragment := range []string{
		"bad gateway", "service unavailable", "gateway timeout", "temporarily unavailable",
		"connection reset", "connection refused", "connection lost", "tls handshake timeout",
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

func isModelRequestTooLargeError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *modelopenai.APIError
	if errors.As(err, &apiErr) && apiErr.HTTPStatusCode == 413 {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "status code: 413") || strings.Contains(message, "status: 413") ||
		strings.Contains(message, "http 413") || strings.Contains(message, "request entity too large") ||
		strings.Contains(message, "request payload is too large")
}

func normalizeModelRequestError(err error) error {
	if isModelRequestTooLargeError(err) {
		return ErrRequestTooLarge
	}
	return err
}

func generateModelWithRetry(ctx context.Context, chatModel model.ToolCallingChatModel, messages []*schema.Message) (*schema.Message, error) {
	agentInstance, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:             "model-connection-test",
		Description:      "Tests a model connection",
		Model:            chatModel,
		MaxIterations:    1,
		ModelRetryConfig: modelRequestRetryConfig(),
	})
	if err != nil {
		return nil, err
	}
	runner := adk.NewRunner(ctx, adk.RunnerConfig{Agent: agentInstance})
	iterator := runner.Run(ctx, messages)
	var response *schema.Message
	for {
		event, ok := iterator.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			return nil, event.Err
		}
		if event.Output == nil || event.Output.MessageOutput == nil || event.Output.MessageOutput.Role != schema.Assistant {
			continue
		}
		response, err = event.Output.MessageOutput.GetMessage()
		if err != nil {
			return nil, err
		}
	}
	return response, nil
}
