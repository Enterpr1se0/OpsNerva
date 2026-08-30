package agent

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/agenttool"
	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/security"
	"github.com/Enterpr1se0/opsnerva/internal/service"
	opsstore "github.com/Enterpr1se0/opsnerva/internal/store"

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

type approvalResumeFixtureModel struct {
	mu    sync.Mutex
	calls int
}

type parallelApprovalFixtureModel struct {
	mu    sync.Mutex
	calls int
}

func (m *parallelApprovalFixtureModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.calls == 1 {
		return schema.AssistantMessage("", []schema.ToolCall{
			{ID: "approval-call-one", Function: schema.FunctionCall{Name: "parallel_approval_fixture", Arguments: `{"unit":"one"}`}},
			{ID: "approval-call-two", Function: schema.FunctionCall{Name: "parallel_approval_fixture", Arguments: `{"unit":"two"}`}},
		}), nil
	}
	return schema.AssistantMessage("both approvals resumed", nil), nil
}

func (m *parallelApprovalFixtureModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.Generate(ctx, input, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func (m *parallelApprovalFixtureModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func (m *approvalResumeFixtureModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.calls == 1 {
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "approval-call", Function: schema.FunctionCall{Name: "approval_fixture", Arguments: `{}`},
		}}), nil
	}
	return schema.AssistantMessage("approval resumed", nil), nil
}

func (m *approvalResumeFixtureModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.Generate(ctx, input, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func (m *approvalResumeFixtureModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
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

func TestApprovalToolStatefulInterruptResumesExactRunOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := opsstore.Open(ctx, filepath.Join(t.TempDir(), "approval-resume.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	encryptor, err := security.NewEncryptor("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	transport := &fileReadToolTransport{}
	svc := service.New(st, transport, encryptor, security.NewRedactor(), config.Default().Limits)
	host, err := svc.SaveHost(ctx, domain.HostInput{
		Name: "approval-host", Address: "192.0.2.42", Port: 22, User: "ops", AuthType: "agent", SudoMode: "none",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "session-approval-resume"
	if _, err := st.CreateChatSession(ctx, sessionID, ""); err != nil {
		t.Fatal(err)
	}
	userMessageID, err := st.AppendPendingChatMessage(ctx, sessionID, "user", "restart demo")
	if err != nil {
		t.Fatal(err)
	}
	fixtureTool, err := toolutils.InferTool("approval_fixture", "approval fixture", func(toolCtx context.Context, _ struct{}) (agenttool.ExecResult, error) {
		return RunExecutionTool(toolCtx, svc, domain.ExecRequest{
			HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", "demo"}, Reason: "test approval resume",
		}, "eino-agent")
	})
	if err != nil {
		t.Fatal(err)
	}
	modelFixture := &approvalResumeFixtureModel{}
	agentInstance, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name: "approval-resume-test", Description: "approval resume regression", Model: modelFixture, MaxIterations: 3,
		ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{
			Tools: []tool.BaseTool{fixtureTool}, ToolCallMiddlewares: []compose.ToolMiddleware{{Invokable: func(next compose.InvokableToolEndpoint) compose.InvokableToolEndpoint {
				return normalizeToolCallErrors(svc, next)
			}}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := adk.NewRunner(ctx, adk.RunnerConfig{Agent: agentInstance, EnableStreaming: true, CheckPointStore: st})
	const checkpointID = "checkpoint-approval-resume"
	runCtx := service.WithAgentApprovalContinuation(service.WithSessionID(ctx, sessionID), checkpointID)
	iterator := runner.Run(runCtx, []*schema.Message{schema.UserMessage("restart demo")}, adk.WithCheckPointID(checkpointID))
	var interruptID string
	var state approvalInterrupt
	for {
		event, ok := iterator.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			t.Fatal(event.Err)
		}
		if event.Action != nil && event.Action.Interrupted != nil {
			targets := approvalInterruptTargets(event.Action.Interrupted)
			if len(targets) != 1 {
				t.Fatalf("approval interrupt target missing: %#v", event.Action.Interrupted)
			}
			state, interruptID = targets[0].State, targets[0].InterruptID
		}
	}
	if state.ApprovalID == "" || state.RunID == "" || interruptID == "" || transport.callCount != 0 {
		t.Fatalf("invalid initial interrupt: state=%#v interrupt=%q calls=%d", state, interruptID, transport.callCount)
	}
	if _, err := svc.ActivateAgentApprovals(ctx, checkpointID, map[string]string{state.ApprovalID: interruptID}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartChatToolCall(ctx, domain.ChatToolCall{
		SessionID: sessionID, UserMessageID: userMessageID, ToolCallID: "approval-call", ToolName: "approval_fixture",
		ArgumentsJSON: `{}`, ResultJSON: `{"status":"approval_required"}`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.BindChatToolCallRun(ctx, sessionID, "approval-call", state.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetChatToolCallActiveStatus(ctx, sessionID, "approval-call", domain.ChatToolCallApprovalRequired, `{"status":"approval_required"}`); err != nil {
		t.Fatal(err)
	}
	if err := st.SetChatMessageStatus(ctx, userMessageID, "waiting_for_approval"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DecideAgentApproval(ctx, state.ApprovalID, domain.ApprovalStatusApproved, "reviewed", "operator"); err != nil {
		t.Fatal(err)
	}
	approval, err := svc.GetApproval(ctx, state.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{runner: runner, store: st, service: svc}
	if recoverable, err := runtime.approvalCheckpointRecoverable(ctx, checkpointID); err != nil || !recoverable {
		t.Fatalf("decided checkpoint was not recoverable: recoverable=%v err=%v", recoverable, err)
	}
	answer, err := runtime.ResumeAgentApprovals(ctx, []domain.Approval{approval}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if answer != "approval resumed" {
		t.Fatalf("resumed answer = %q", answer)
	}
	modelFixture.mu.Lock()
	modelCalls := modelFixture.calls
	modelFixture.mu.Unlock()
	if modelCalls != 2 || transport.callCount != 1 {
		t.Fatalf("model calls=%d execution calls=%d", modelCalls, transport.callCount)
	}
	if _, present, err := st.Get(ctx, checkpointID); err != nil || present {
		t.Fatalf("completed continuation retained its checkpoint: present=%v err=%v", present, err)
	}
	if replayed, err := svc.ResumeAgentApproval(ctx, state.ApprovalID); err != nil || replayed.Status != "completed" || transport.callCount != 1 {
		t.Fatalf("completed approval was not replay-safe: result=%#v calls=%d err=%v", replayed, transport.callCount, err)
	}
}

func TestParallelApprovalInterruptsResumeAfterEveryDecision(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := opsstore.Open(ctx, filepath.Join(t.TempDir(), "parallel-approval.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	encryptor, err := security.NewEncryptor("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	transport := &fileReadToolTransport{}
	svc := service.New(st, transport, encryptor, security.NewRedactor(), config.Default().Limits)
	host, err := svc.SaveHost(ctx, domain.HostInput{
		Name: "parallel-approval-host", Address: "192.0.2.43", Port: 22, User: "ops", AuthType: "agent", SudoMode: "none",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	type approvalInput struct {
		Unit string `json:"unit"`
	}
	fixtureTool, err := toolutils.InferTool("parallel_approval_fixture", "parallel approval fixture", func(toolCtx context.Context, input approvalInput) (agenttool.ExecResult, error) {
		return RunExecutionTool(toolCtx, svc, domain.ExecRequest{
			HostID: host.ID, Mode: domain.ExecProgram, Program: "systemctl", Args: []string{"restart", input.Unit}, Reason: "test parallel approval resume",
		}, "eino-agent")
	})
	if err != nil {
		t.Fatal(err)
	}
	modelFixture := &parallelApprovalFixtureModel{}
	agentInstance, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name: "parallel-approval-test", Description: "parallel approval regression", Model: modelFixture, MaxIterations: 3,
		ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{
			Tools: []tool.BaseTool{fixtureTool}, ToolCallMiddlewares: []compose.ToolMiddleware{{Invokable: func(next compose.InvokableToolEndpoint) compose.InvokableToolEndpoint {
				return normalizeToolCallErrors(svc, next)
			}}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := adk.NewRunner(ctx, adk.RunnerConfig{Agent: agentInstance, EnableStreaming: true, CheckPointStore: st})
	type pauseRegistration struct {
		pauses []ApprovalPause
		resume chan struct{}
	}
	registrations := make(chan pauseRegistration, 1)
	queryCtx := WithApprovalPauseRegistrar(ctx, func(pauses []ApprovalPause) (ApprovalWait, error) {
		registration := pauseRegistration{pauses: append([]ApprovalPause(nil), pauses...), resume: make(chan struct{})}
		registrations <- registration
		return func(waitCtx context.Context) error {
			select {
			case <-waitCtx.Done():
				return waitCtx.Err()
			case <-registration.resume:
				return nil
			}
		}, nil
	})
	runtime := &Runtime{runner: runner, store: st, service: svc}
	type queryResult struct {
		answer string
		err    error
	}
	result := make(chan queryResult, 1)
	go func() {
		answer, queryErr := runtime.Query(queryCtx, "session-parallel-approval", "approve both", nil)
		result <- queryResult{answer: answer, err: queryErr}
	}()
	waitRegistration := func() pauseRegistration {
		t.Helper()
		select {
		case registration := <-registrations:
			return registration
		case <-ctx.Done():
			t.Fatal("timed out waiting for approval pause")
			return pauseRegistration{}
		}
	}
	waitPending := func(approvalID string) domain.Approval {
		t.Helper()
		for {
			approval, approvalErr := svc.GetApproval(ctx, approvalID)
			if approvalErr == nil && approval.Status == domain.ApprovalStatusPending {
				return approval
			}
			select {
			case <-ctx.Done():
				t.Fatalf("approval %s did not become pending: approval=%#v err=%v", approvalID, approval, approvalErr)
			case <-time.After(5 * time.Millisecond):
			}
		}
	}

	firstPause := waitRegistration()
	if len(firstPause.pauses) != 2 || transport.callCount != 0 {
		t.Fatalf("initial pause=%#v execution calls=%d", firstPause.pauses, transport.callCount)
	}
	waitPending(firstPause.pauses[0].ApprovalID)
	waitPending(firstPause.pauses[1].ApprovalID)
	if _, err := svc.DecideAgentApproval(ctx, firstPause.pauses[0].ApprovalID, domain.ApprovalStatusApproved, "reviewed first", "operator"); err != nil {
		t.Fatal(err)
	}
	if transport.callCount != 0 {
		t.Fatalf("partial approval group executed %d operations", transport.callCount)
	}
	if _, err := svc.DecideAgentApproval(ctx, firstPause.pauses[1].ApprovalID, domain.ApprovalStatusApproved, "reviewed second", "operator"); err != nil {
		t.Fatal(err)
	}
	close(firstPause.resume)
	select {
	case completed := <-result:
		if completed.err != nil || completed.answer != "both approvals resumed" {
			t.Fatalf("query result: answer=%q err=%v", completed.answer, completed.err)
		}
	case <-ctx.Done():
		t.Fatal("parallel approval query did not complete")
	}
	modelFixture.mu.Lock()
	modelCalls := modelFixture.calls
	modelFixture.mu.Unlock()
	if modelCalls != 2 || transport.callCount != 2 {
		t.Fatalf("model calls=%d execution calls=%d", modelCalls, transport.callCount)
	}
	messagePage, err := st.ListChatMessagesPage(ctx, "session-parallel-approval", 10, "", "")
	if err != nil || len(messagePage.Messages) == 0 || messagePage.Messages[0].Status != "completed" {
		t.Fatalf("persisted messages=%#v err=%v", messagePage.Messages, err)
	}
	toolCalls, err := st.ListChatToolCalls(ctx, "session-parallel-approval")
	if err != nil || len(toolCalls) != 2 {
		t.Fatalf("persisted tool calls=%#v err=%v", toolCalls, err)
	}
	for _, call := range toolCalls {
		if call.Status != domain.ChatToolCallCompleted {
			t.Fatalf("tool call did not complete: %#v", call)
		}
	}
	if _, present, err := st.Get(ctx, firstPause.pauses[0].CheckpointID); err != nil || present {
		t.Fatalf("completed approval group retained its checkpoint: present=%v err=%v", present, err)
	}
}
