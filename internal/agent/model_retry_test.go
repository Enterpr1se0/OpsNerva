package agent

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"

	modelopenai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

type transientAfterToolModel struct {
	calls atomic.Int32
}

func (m *transientAfterToolModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	switch m.calls.Add(1) {
	case 1:
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "call-once", Function: schema.FunctionCall{Name: "counted_tool", Arguments: `{}`},
		}}), nil
	case 2:
		return nil, &modelopenai.APIError{HTTPStatusCode: 502, HTTPStatus: "502 Bad Gateway", Message: "temporary upstream failure"}
	default:
		return schema.AssistantMessage("recovered without repeating the tool", nil), nil
	}
}

func (m *transientAfterToolModel) Stream(ctx context.Context, messages []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func (m *transientAfterToolModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func TestModelRequestRetryPolicy(t *testing.T) {
	shouldRetry := func(ctx context.Context, output *schema.Message, err error) bool {
		decision := modelRequestRetryConfig().ShouldRetry(ctx, &adk.RetryContext{OutputMessage: output, Err: err})
		return decision != nil && decision.Retry
	}
	ctx := context.Background()
	if shouldRetry(ctx, nil, &modelopenai.APIError{HTTPStatusCode: 400}) {
		t.Fatal("HTTP 400 was marked retryable")
	}
	if !shouldRetry(ctx, nil, &modelopenai.APIError{HTTPStatusCode: 429}) {
		t.Fatal("HTTP 429 was not marked retryable")
	}
	if !shouldRetry(ctx, nil, &modelopenai.APIError{HTTPStatusCode: 502}) {
		t.Fatal("HTTP 502 was not marked retryable")
	}
	if !shouldRetry(ctx, nil, errors.New("[NodeRunError] error, status code: 503, status: 503 Service Unavailable")) {
		t.Fatal("wrapped HTTP 503 was not marked retryable")
	}
	if shouldRetry(ctx, schema.AssistantMessage("partial", nil), errors.New("connection reset")) {
		t.Fatal("partial model output was marked retryable")
	}
	if shouldRetry(ctx, &schema.Message{Role: schema.Assistant, ReasoningContent: "partial reasoning"}, io.ErrUnexpectedEOF) {
		t.Fatal("partial reasoning output was marked retryable")
	}
	if shouldRetry(ctx, schema.AssistantMessage("", []schema.ToolCall{{ID: "call-1"}}), io.ErrUnexpectedEOF) {
		t.Fatal("partial tool-call output was marked retryable")
	}
	if !shouldRetry(ctx, nil, context.DeadlineExceeded) {
		t.Fatal("model timeout was not marked retryable")
	}
	if !shouldRetry(ctx, nil, nil) {
		t.Fatal("empty model output was not delegated to Eino retry")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if shouldRetry(cancelled, nil, &modelopenai.APIError{HTTPStatusCode: 502}) {
		t.Fatal("a canceled request was marked retryable")
	}
}

func TestModelRequestTooLargeIsActionableAndNotRetryable(t *testing.T) {
	err := errors.New("[NodeRunError] error, status code: 413, status: 413 Request Entity Too Large, message: Request payload is too large")
	if isRetryableModelRequestError(err) {
		t.Fatal("HTTP 413 was marked retryable")
	}
	if normalized := normalizeModelRequestError(err); !errors.Is(normalized, ErrRequestTooLarge) {
		t.Fatalf("normalized error = %v", normalized)
	}
	if !isModelRequestTooLargeError(&modelopenai.APIError{HTTPStatusCode: 413}) {
		t.Fatal("structured HTTP 413 was not detected")
	}
}

func TestModelRequestRetryConfigUsesEinoBackoff(t *testing.T) {
	config := modelRequestRetryConfig()
	if config.MaxRetries != modelRequestMaxRetries || config.ShouldRetry == nil {
		t.Fatalf("retry config = %#v", config)
	}
	if config.BackoffFunc != nil {
		t.Fatal("custom retry backoff is still configured")
	}
}

func TestModelRequestRetryDoesNotRepeatCompletedTool(t *testing.T) {
	ctx := context.Background()
	var toolCalls atomic.Int32
	countedTool, err := toolutils.InferTool("counted_tool", "counts executions", func(context.Context, struct{}) (string, error) {
		toolCalls.Add(1)
		return `{"status":"completed"}`, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	chatModel := &transientAfterToolModel{}
	agentInstance, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name: "retry-test", Description: "model request retry regression", Model: chatModel, MaxIterations: 3,
		ModelRetryConfig: modelRequestRetryConfig(),
		ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{
			Tools: []tool.BaseTool{countedTool}, ExecuteSequentially: true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := adk.NewRunner(ctx, adk.RunnerConfig{Agent: agentInstance, EnableStreaming: true})
	iterator := runner.Run(ctx, []*schema.Message{schema.UserMessage("run once")})
	answer := ""
	for {
		event, ok := iterator.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			var retryErr *adk.WillRetryError
			if errors.As(event.Err, &retryErr) {
				continue
			}
			t.Fatal(event.Err)
		}
		if event.Output == nil || event.Output.MessageOutput == nil {
			continue
		}
		output := event.Output.MessageOutput
		if output.Message != nil && output.Role == schema.Assistant {
			answer = output.Message.Content
		}
		if output.MessageStream != nil && output.Role == schema.Assistant {
			for {
				message, recvErr := output.MessageStream.Recv()
				if errors.Is(recvErr, io.EOF) {
					break
				}
				if recvErr != nil {
					t.Fatal(recvErr)
				}
				answer += message.Content
			}
			output.MessageStream.Close()
		}
	}
	if answer != "recovered without repeating the tool" {
		t.Fatalf("answer = %q", answer)
	}
	if calls := chatModel.calls.Load(); calls != 3 {
		t.Fatalf("model calls = %d, want 3", calls)
	}
	if calls := toolCalls.Load(); calls != 1 {
		t.Fatalf("tool calls = %d, want 1", calls)
	}
}
