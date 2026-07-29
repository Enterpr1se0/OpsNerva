package agent

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

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
	ctx := context.Background()
	if modelRequestRetryDecision(ctx, 1, nil, &modelopenai.APIError{HTTPStatusCode: 400}).Retry {
		t.Fatal("HTTP 400 was marked retryable")
	}
	if !modelRequestRetryDecision(ctx, 1, nil, &modelopenai.APIError{HTTPStatusCode: 429}).Retry {
		t.Fatal("HTTP 429 was not marked retryable")
	}
	if !modelRequestRetryDecision(ctx, 1, nil, &modelopenai.APIError{HTTPStatusCode: 502}).Retry {
		t.Fatal("HTTP 502 was not marked retryable")
	}
	if modelRequestRetryDecision(ctx, 1, schema.AssistantMessage("partial", nil), errors.New("connection reset")).Retry {
		t.Fatal("partial model output was marked retryable")
	}
	if !modelRequestRetryDecision(ctx, 2, nil, context.DeadlineExceeded).Retry {
		t.Fatal("a second model timeout was not marked retryable")
	}
	if modelRequestRetryDecision(ctx, 3, nil, context.DeadlineExceeded).Retry {
		t.Fatal("a third model timeout was marked retryable")
	}
	if !modelRequestRetryDecision(ctx, 4, nil, &modelopenai.APIError{HTTPStatusCode: 503}).Retry {
		t.Fatal("the fourth transient provider failure was not marked retryable")
	}
	if modelRequestRetryDecision(ctx, 5, nil, &modelopenai.APIError{HTTPStatusCode: 503}).Retry {
		t.Fatal("a fifth transient provider failure was marked retryable")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if modelRequestRetryDecision(cancelled, 1, nil, &modelopenai.APIError{HTTPStatusCode: 502}).Retry {
		t.Fatal("a canceled request was marked retryable")
	}
}

func TestModelRequestRetryNoticeMatchesBackoff(t *testing.T) {
	var notices []modelRequestRetryNotice
	ctx := withModelRequestRetryNotifier(context.Background(), func(notice modelRequestRetryNotice) {
		notices = append(notices, notice)
	})
	for attempt, expectedDelay := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second} {
		retryAttempt := attempt + 1
		if !modelRequestRetryDecision(ctx, retryAttempt, nil, &modelopenai.APIError{HTTPStatusCode: 502}).Retry {
			t.Fatalf("attempt %d was not marked retryable", retryAttempt)
		}
		if delay := modelRequestRetryBackoff(ctx, retryAttempt); delay != expectedDelay {
			t.Fatalf("attempt %d delay = %v, want %v", retryAttempt, delay, expectedDelay)
		}
	}
	if len(notices) != modelRequestMaxRetries {
		t.Fatalf("notices = %#v", notices)
	}
	for index, notice := range notices {
		if notice.Attempt != index+1 || notice.Max != modelRequestMaxRetries || notice.Delay != time.Second*time.Duration(1<<index) {
			t.Fatalf("notice %d = %#v", index, notice)
		}
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
	retryConfig := modelRequestRetryConfig()
	retryConfig.BackoffFunc = func(context.Context, int) time.Duration { return 0 }
	agentInstance, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name: "retry-test", Description: "model request retry regression", Model: chatModel, MaxIterations: 3,
		ModelRetryConfig: retryConfig,
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
