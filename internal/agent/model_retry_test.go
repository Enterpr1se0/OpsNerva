package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
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

type deadlineThenSuccessModel struct {
	calls atomic.Int32
}

type partialReasoningDeadlineModel struct {
	calls atomic.Int32
}

type reasoningOnlyThenSuccessModel struct {
	calls atomic.Int32
}

func (m *reasoningOnlyThenSuccessModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	if m.calls.Add(1) == 1 {
		message := schema.AssistantMessage("", nil)
		message.ReasoningContent = "completed reasoning without a final answer"
		return message, nil
	}
	return schema.AssistantMessage("recovered after reasoning-only response", nil), nil
}

func (m *reasoningOnlyThenSuccessModel) Stream(ctx context.Context, messages []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func (m *reasoningOnlyThenSuccessModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func (m *partialReasoningDeadlineModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return nil, errors.New("Generate is not used in this streaming regression test")
}

func (m *partialReasoningDeadlineModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	m.calls.Add(1)
	reader, writer := schema.Pipe[*schema.Message](2)
	go func() {
		partial := schema.AssistantMessage("", nil)
		partial.ReasoningContent = "partial reasoning"
		writer.Send(partial, nil)
		writer.Send(nil, fmt.Errorf("failed to receive stream chunk: %w (Client.Timeout or context cancellation while reading body)", context.DeadlineExceeded))
		writer.Close()
	}()
	return reader, nil
}

func (m *partialReasoningDeadlineModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func (m *deadlineThenSuccessModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	if m.calls.Add(1) == 1 {
		return nil, context.DeadlineExceeded
	}
	return schema.AssistantMessage("recovered after model timeout", nil), nil
}

func (m *deadlineThenSuccessModel) Stream(ctx context.Context, messages []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func (m *deadlineThenSuccessModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
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

func collectAgentAnswer(t *testing.T, iterator *adk.AsyncIterator[*adk.AgentEvent]) string {
	t.Helper()
	var answer strings.Builder
	for {
		event, ok := iterator.Next()
		if !ok {
			return answer.String()
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
			answer.WriteString(output.Message.Content)
		}
		if output.MessageStream == nil || output.Role != schema.Assistant {
			continue
		}
		for {
			message, recvErr := output.MessageStream.Recv()
			if errors.Is(recvErr, io.EOF) {
				break
			}
			if recvErr != nil {
				var retryErr *adk.WillRetryError
				if errors.As(recvErr, &retryErr) {
					break
				}
				t.Fatal(recvErr)
			}
			answer.WriteString(message.Content)
		}
		output.MessageStream.Close()
	}
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
		t.Fatal("stream failure with partial content was marked retryable")
	}
	if shouldRetry(ctx, &schema.Message{Role: schema.Assistant, ReasoningContent: "partial reasoning"}, io.ErrUnexpectedEOF) {
		t.Fatal("unexpected EOF with partial reasoning was marked retryable")
	}
	if shouldRetry(ctx, &schema.Message{Role: schema.Assistant, ReasoningContent: "partial reasoning"}, context.DeadlineExceeded) {
		t.Fatal("deadline exceeded with partial reasoning was marked retryable")
	}
	if shouldRetry(ctx, schema.AssistantMessage("", []schema.ToolCall{{ID: "call-1"}}), io.ErrUnexpectedEOF) {
		t.Fatal("unexpected EOF with a partial tool call was marked retryable")
	}
	if shouldRetry(ctx, schema.AssistantMessage("partial", nil), fmt.Errorf("failed to receive stream chunk: %w", io.ErrUnexpectedEOF)) {
		t.Fatal("wrapped stream unexpected EOF with partial content was marked retryable")
	}
	if !shouldRetry(ctx, nil, fmt.Errorf("failed to receive stream chunk: %w", io.ErrUnexpectedEOF)) {
		t.Fatal("wrapped stream unexpected EOF without output was not marked retryable")
	}
	if !shouldRetry(ctx, nil, context.DeadlineExceeded) {
		t.Fatal("model timeout was not marked retryable")
	}
	if !shouldRetry(ctx, nil, nil) {
		t.Fatal("empty model output was not delegated to Eino retry")
	}
	if !shouldRetry(ctx, &schema.Message{Role: schema.Assistant, ReasoningContent: "completed reasoning"}, nil) {
		t.Fatal("reasoning-only terminal output was not delegated to Eino retry")
	}
	if !shouldRetry(ctx, schema.AssistantMessage(" \n", nil), nil) {
		t.Fatal("blank terminal output was not delegated to Eino retry")
	}
	if shouldRetry(ctx, schema.AssistantMessage("completed answer", nil), nil) {
		t.Fatal("completed answer was marked retryable")
	}
	if shouldRetry(ctx, schema.AssistantMessage("", []schema.ToolCall{{ID: "call-1"}}), nil) {
		t.Fatal("completed tool call was marked retryable")
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

func TestModelRequestTimeoutRetriesWhileRunContextIsActive(t *testing.T) {
	observedRetries := atomic.Int32{}
	ctx := withModelRetryObserver(context.Background(), func(err error, attempt int) {
		if !errors.Is(err, context.DeadlineExceeded) || attempt != 1 {
			t.Errorf("retry notification = (%v, %d)", err, attempt)
		}
		observedRetries.Add(1)
	})
	chatModel := &deadlineThenSuccessModel{}
	agentInstance, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name: "timeout-retry-test", Description: "model timeout retry regression", Model: chatModel, MaxIterations: 1,
		ModelRetryConfig: modelRequestRetryConfig(),
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := adk.NewRunner(ctx, adk.RunnerConfig{Agent: agentInstance, EnableStreaming: true})
	iterator := runner.Run(ctx, []*schema.Message{schema.UserMessage("respond")})
	answer := collectAgentAnswer(t, iterator)
	if answer != "recovered after model timeout" {
		t.Fatalf("answer = %q", answer)
	}
	if calls := chatModel.calls.Load(); calls != 2 {
		t.Fatalf("model calls = %d, want 2", calls)
	}
	if retries := observedRetries.Load(); retries != 1 {
		t.Fatalf("retry notifications = %d, want 1", retries)
	}
}

func TestModelRequestRetriesReasoningOnlyTerminalOutput(t *testing.T) {
	var observedRetries atomic.Int32
	ctx := withModelRetryObserver(context.Background(), func(err error, attempt int) {
		if !errors.Is(err, ErrEmptyResponse) || attempt != 1 {
			t.Errorf("retry notification = (%v, %d)", err, attempt)
		}
		observedRetries.Add(1)
	})
	chatModel := &reasoningOnlyThenSuccessModel{}
	agentInstance, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name: "reasoning-only-retry-test", Description: "reasoning-only terminal output retry regression", Model: chatModel, MaxIterations: 1,
		ModelRetryConfig: modelRequestRetryConfig(),
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := adk.NewRunner(ctx, adk.RunnerConfig{Agent: agentInstance, EnableStreaming: true})
	iterator := runner.Run(ctx, []*schema.Message{schema.UserMessage("respond")})
	answer := collectAgentAnswer(t, iterator)
	if answer != "recovered after reasoning-only response" {
		t.Fatalf("answer = %q", answer)
	}
	if calls := chatModel.calls.Load(); calls != 2 {
		t.Fatalf("model calls = %d, want 2", calls)
	}
	if retries := observedRetries.Load(); retries != 1 {
		t.Fatalf("retry notifications = %d, want 1", retries)
	}
}

func TestModelRequestDoesNotRetryDeadlineAfterPartialReasoning(t *testing.T) {
	var observedRetries atomic.Int32
	ctx := withModelRetryObserver(context.Background(), func(err error, attempt int) {
		if !errors.Is(err, context.DeadlineExceeded) || attempt != 1 {
			t.Errorf("retry notification = (%v, %d)", err, attempt)
		}
		observedRetries.Add(1)
	})
	chatModel := &partialReasoningDeadlineModel{}
	agentInstance, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name: "partial-stream-retry-test", Description: "partial stream retry regression", Model: chatModel, MaxIterations: 1,
		ModelRetryConfig: modelRequestRetryConfig(),
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := adk.NewRunner(ctx, adk.RunnerConfig{Agent: agentInstance, EnableStreaming: true})
	iterator := runner.Run(ctx, []*schema.Message{schema.UserMessage("respond")})
	reasoning := ""
	var streamErr error
	for {
		event, ok := iterator.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			var retryErr *adk.WillRetryError
			if errors.As(event.Err, &retryErr) {
				t.Fatalf("partial reasoning triggered retry: %v", retryErr)
			}
			streamErr = event.Err
			break
		}
		if event.Output == nil || event.Output.MessageOutput == nil {
			continue
		}
		output := event.Output.MessageOutput
		if output.MessageStream == nil || output.Role != schema.Assistant {
			continue
		}
		attempt := ""
		for {
			message, recvErr := output.MessageStream.Recv()
			if errors.Is(recvErr, io.EOF) {
				break
			}
			if recvErr != nil {
				var retryErr *adk.WillRetryError
				if errors.As(recvErr, &retryErr) {
					t.Fatalf("partial reasoning triggered retry: %v", retryErr)
				}
				streamErr = recvErr
				break
			}
			attempt += message.ReasoningContent
		}
		reasoning += attempt
		output.MessageStream.Close()
	}
	if reasoning != "partial reasoning" || !errors.Is(streamErr, context.DeadlineExceeded) {
		t.Fatalf("reasoning = %q, stream error = %v", reasoning, streamErr)
	}
	if calls := chatModel.calls.Load(); calls != 1 {
		t.Fatalf("model calls = %d, want 1", calls)
	}
	if retries := observedRetries.Load(); retries != 0 {
		t.Fatalf("retry notifications = %d, want 0", retries)
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
