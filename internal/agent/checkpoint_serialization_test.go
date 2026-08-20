package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

type checkpointFixtureModel struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (m *checkpointFixtureModel) Generate(ctx context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.once.Do(func() { close(m.started) })
	select {
	case <-m.release:
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "fixture-call", Function: schema.FunctionCall{Name: "fixture_tool", Arguments: `{}`},
		}}), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *checkpointFixtureModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.Generate(ctx, input, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func (m *checkpointFixtureModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

type checkpointFixtureStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

func (s *checkpointFixtureStore) Get(_ context.Context, key string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.data[key]
	return value, ok, nil
}

func (s *checkpointFixtureStore) Set(_ context.Context, key string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = append([]byte(nil), value...)
	return nil
}

func TestSteeringCheckpointSerializesNestedMessageMetadata(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	fixtureModel := &checkpointFixtureModel{started: make(chan struct{}), release: make(chan struct{})}
	fixtureTool, err := toolutils.InferTool("fixture_tool", "checkpoint fixture", func(context.Context, struct{}) (string, error) {
		return "unused", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	agentInstance, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name: "checkpoint-serialization-test", Description: "checkpoint serialization regression", Model: fixtureModel, MaxIterations: 3,
		ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: []tool.BaseTool{fixtureTool}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	checkpointStore := &checkpointFixtureStore{data: make(map[string][]byte)}
	runner := adk.NewRunner(ctx, adk.RunnerConfig{Agent: agentInstance, EnableStreaming: true, CheckPointStore: checkpointStore})
	cancelOption, cancelRun := adk.WithCancel()
	message := schema.UserMessage("inspect")
	message.Extra = map[string]any{
		"provider_metadata": map[string]any{
			"blocks": []any{map[string]any{"signature": "fixture"}},
		},
	}
	iterator := runner.Run(ctx, []*schema.Message{message}, adk.WithCheckPointID("checkpoint-test"), cancelOption)
	eventsDone := make(chan []*adk.AgentEvent, 1)
	go func() {
		var events []*adk.AgentEvent
		for {
			event, ok := iterator.Next()
			if !ok {
				break
			}
			events = append(events, event)
		}
		eventsDone <- events
	}()

	select {
	case <-fixtureModel.started:
	case <-ctx.Done():
		t.Fatal("model did not start")
	}
	time.Sleep(20 * time.Millisecond)
	handle, accepted := cancelRun(adk.WithAgentCancelMode(adk.CancelAfterChatModel|adk.CancelAfterToolCalls), adk.WithRecursive())
	if !accepted {
		t.Fatal("safe-point cancellation was not accepted")
	}
	time.Sleep(100 * time.Millisecond)
	close(fixtureModel.release)
	handleErr := handle.Wait()

	var events []*adk.AgentEvent
	select {
	case events = <-eventsDone:
	case <-ctx.Done():
		t.Fatal("agent events did not finish")
	}
	if handleErr != nil {
		for _, event := range events {
			if event.Err != nil {
				t.Logf("agent event error: %v", event.Err)
			}
		}
		t.Fatalf("wait for safe-point cancellation: %v", handleErr)
	}
	foundCancel := false
	for _, event := range events {
		if event.Err == nil {
			continue
		}
		var cancelErr *adk.CancelError
		if errors.As(event.Err, &cancelErr) {
			foundCancel = true
			continue
		}
		t.Fatalf("steering checkpoint failed: %v", event.Err)
	}
	if !foundCancel {
		t.Fatal("steering did not emit a cancellation event")
	}
	if checkpoint, ok, err := checkpointStore.Get(ctx, "checkpoint-test"); err != nil || !ok || len(checkpoint) == 0 {
		t.Fatalf("checkpoint was not persisted: present=%v bytes=%d err=%v", ok, len(checkpoint), err)
	}
}
