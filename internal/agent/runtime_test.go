package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"eino-ops-agent/internal/config"
	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/security"
	"eino-ops-agent/internal/service"
	"eino-ops-agent/internal/store"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

type scriptedAgentRunner struct {
	mu       sync.Mutex
	attempts [][]*adk.AgentEvent
	calls    int
	inputs   [][]*schema.Message
}

type blockingAgentRunner struct {
	started chan struct{}
}

type toolActivityAgentRunner struct{}

type modelFailureWithRunningToolRunner struct {
	started  chan struct{}
	release  chan struct{}
	finished chan error
}

type assistantLifecycleReplay struct {
	content      map[string]string
	committedIDs []string
	resetIDs     []string
	active       map[string]bool
}

func replayAssistantLifecycle(t *testing.T, events []Event) assistantLifecycleReplay {
	t.Helper()
	result := assistantLifecycleReplay{content: make(map[string]string), active: make(map[string]bool)}
	for _, event := range events {
		if event.Role != string(schema.Assistant) {
			continue
		}
		switch event.Type {
		case "message_start":
			if event.MessageID == "" || result.active[event.MessageID] {
				t.Fatalf("invalid assistant message start: %#v", event)
			}
			result.active[event.MessageID] = true
		case "message":
			if event.MessageID == "" || !result.active[event.MessageID] {
				t.Fatalf("assistant delta without active lifecycle: %#v", event)
			}
			result.content[event.MessageID] += event.Content
		case "message_commit":
			if event.MessageID == "" || !result.active[event.MessageID] {
				t.Fatalf("assistant commit without active lifecycle: %#v", event)
			}
			delete(result.active, event.MessageID)
			result.committedIDs = append(result.committedIDs, event.MessageID)
		case "message_reset":
			if event.MessageID == "" || !result.active[event.MessageID] {
				t.Fatalf("assistant reset without active lifecycle: %#v", event)
			}
			delete(result.active, event.MessageID)
			result.resetIDs = append(result.resetIDs, event.MessageID)
		}
	}
	return result
}

func (*toolActivityAgentRunner) Run(ctx context.Context, _ []*schema.Message, _ ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	iterator, generator := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	go func() {
		call := schema.ToolCall{
			ID: "call-live", Type: "function",
			Function: schema.FunctionCall{Name: "ssh_exec", Arguments: `{"host_id":"host-live","program":"uptime","reason":"inspect uptime"}`},
		}
		generator.Send(adk.EventFromMessage(schema.AssistantMessage("", []schema.ToolCall{call}), nil, schema.Assistant, ""))
		notifyToolStarted(ctx, &compose.ToolInput{CallID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments})
		generator.Send(adk.EventFromMessage(
			schema.ToolMessage(`{"status":"completed","stdout":"up 1 day"}`, call.ID, schema.WithToolName(call.Function.Name)),
			nil, schema.Tool, call.Function.Name,
		))
		generator.Send(adk.EventFromMessage(schema.AssistantMessage("Host is available.", nil), nil, schema.Assistant, ""))
		generator.Close()
	}()
	return iterator
}

func (r *modelFailureWithRunningToolRunner) Run(ctx context.Context, _ []*schema.Message, _ ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	iterator, generator := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	endpoint := normalizeToolCallErrors(func(toolCtx context.Context, _ *compose.ToolInput) (*compose.ToolOutput, error) {
		close(r.started)
		select {
		case <-r.release:
			return &compose.ToolOutput{Result: `{"status":"completed","stdout":"finished"}`}, nil
		case <-toolCtx.Done():
			return nil, toolCtx.Err()
		}
	})
	go func() {
		_, err := endpoint(ctx, &compose.ToolInput{CallID: "call-detached", Name: "ssh_exec", Arguments: `{"host_id":"host-one","program":"sleep"}`})
		r.finished <- err
	}()
	go func() {
		<-r.started
		generator.Send(&adk.AgentEvent{Err: errors.New("model request failed")})
		generator.Close()
	}()
	return iterator
}

func (r *blockingAgentRunner) Run(ctx context.Context, _ []*schema.Message, _ ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	iterator, generator := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	go func() {
		close(r.started)
		<-ctx.Done()
		generator.Close()
	}()
	return iterator
}

func (r *scriptedAgentRunner) Run(_ context.Context, messages []*schema.Message, _ ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	r.mu.Lock()
	index := r.calls
	r.calls++
	r.inputs = append(r.inputs, append([]*schema.Message(nil), messages...))
	var events []*adk.AgentEvent
	if index < len(r.attempts) {
		events = r.attempts[index]
	}
	r.mu.Unlock()
	iterator, generator := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	go func() {
		for _, event := range events {
			generator.Send(event)
		}
		generator.Close()
	}()
	return iterator
}

func (r *scriptedAgentRunner) snapshot() (int, [][]*schema.Message) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, append([][]*schema.Message(nil), r.inputs...)
}

func TestProviderSendsHelloAndAcceptsNonEmptyResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var request struct {
			ReasoningEffort string `json:"reasoning_effort"`
			Messages        []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if len(request.Messages) != 1 || request.Messages[0].Content != "Hello" {
			t.Fatalf("unexpected test prompt %#v", request.Messages)
		}
		if request.ReasoningEffort != "high" {
			t.Fatalf("reasoning_effort = %q, want high", request.ReasoningEffort)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "id":"chatcmpl-test","object":"chat.completion","created":1,"model":"fixture-model",
  "choices":[{"index":0,"message":{"role":"assistant","content":"Hello from fixture"},"finish_reason":"stop"}],
  "usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
}`))
	}))
	defer server.Close()

	result, err := (&Runtime{}).TestProvider(context.Background(), config.Model{
		APIKey: "fixture-key", BaseURL: server.URL + "/v1", Name: "fixture-model", ReasoningEffort: "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Response != "Hello from fixture" || result.Model != "fixture-model" {
		t.Fatalf("unexpected test result %#v", result)
	}
}

func TestProviderRetriesTransientUpstreamFailure(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":{"message":"temporary upstream failure"}}`, http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "id":"chatcmpl-retry","object":"chat.completion","created":1,"model":"fixture-model",
  "choices":[{"index":0,"message":{"role":"assistant","content":"recovered"},"finish_reason":"stop"}]
}`))
	}))
	defer server.Close()

	result, err := (&Runtime{}).TestProvider(context.Background(), config.Model{
		APIKey: "fixture-key", BaseURL: server.URL + "/v1", Name: "fixture-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Response != "recovered" || requests.Load() != 2 {
		t.Fatalf("result=%#v requests=%d", result, requests.Load())
	}
}

func TestProviderRejectsEmptyResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "id":"chatcmpl-test","object":"chat.completion","created":1,"model":"fixture-model",
  "choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}]
}`))
	}))
	defer server.Close()

	_, err := (&Runtime{}).TestProvider(context.Background(), config.Model{
		BaseURL: server.URL + "/v1", Name: "fixture-model",
	})
	if err == nil {
		t.Fatal("empty model response was accepted")
	}
}

func TestProviderAnthropicUsesNativeMessagesRequest(t *testing.T) {
	const (
		apiKey    = "anthropic-fixture-key"
		userAgent = "OpsNerva-Test/1.0"
	)
	requestPaths := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPaths <- r.URL.Path
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("x-api-key") != apiKey {
			t.Errorf("x-api-key = %q, want configured key", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Error("anthropic-version header is missing")
		}
		if r.Header.Get("User-Agent") != userAgent {
			t.Errorf("User-Agent = %q, want %q", r.Header.Get("User-Agent"), userAgent)
		}
		if r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected Authorization header %q", r.Header.Get("Authorization"))
		}
		var request struct {
			MaxTokens int             `json:"max_tokens"`
			Model     string          `json:"model"`
			Messages  json.RawMessage `json:"messages"`
			Thinking  struct {
				Type string `json:"type"`
			} `json:"thinking"`
			OutputConfig struct {
				Effort string `json:"effort"`
			} `json:"output_config"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode Anthropic request: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if request.MaxTokens != modelConnectionTestMaxTokens {
			t.Errorf("max_tokens = %d, want %d", request.MaxTokens, modelConnectionTestMaxTokens)
		}
		if request.Model != "claude-fixture" || !strings.Contains(string(request.Messages), "Hello") {
			t.Errorf("unexpected Anthropic request: model=%q messages=%s", request.Model, request.Messages)
		}
		if request.Thinking.Type != "adaptive" || request.OutputConfig.Effort != "high" {
			t.Errorf("unexpected Anthropic reasoning config: thinking=%q effort=%q", request.Thinking.Type, request.OutputConfig.Effort)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "id":"msg_fixture","type":"message","role":"assistant","model":"claude-fixture",
  "content":[{"type":"text","text":"Hello from Claude"}],
  "stop_reason":"end_turn","stop_sequence":null,
  "usage":{"input_tokens":1,"output_tokens":3}
}`))
	}))
	defer server.Close()

	result, err := (&Runtime{}).TestProvider(context.Background(), config.Model{
		APIKey: apiKey, Kind: "anthropic", BaseURL: server.URL, Name: "claude-fixture", ReasoningEffort: "high", UserAgent: userAgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Response != "Hello from Claude" {
		t.Fatalf("unexpected Anthropic test response %#v", result)
	}
	select {
	case path := <-requestPaths:
		if path != "/v1/messages" {
			t.Fatalf("request path = %q, want /v1/messages", path)
		}
	default:
		t.Fatal("Anthropic test did not make a request")
	}
	select {
	case path := <-requestPaths:
		t.Fatalf("Anthropic connection test made an extra request to %q", path)
	default:
	}
}

func TestAnthropicRequestsDoNotFollowRedirects(t *testing.T) {
	const apiKey = "must-not-leak"
	redirected := make(chan http.Header, 2)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected <- r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_sink","type":"message","role":"assistant","model":"claude-fixture","content":[{"type":"text","text":"unexpected"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer sink.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL+"/credential-sink", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	cfg := config.Model{APIKey: apiKey, Kind: "anthropic", BaseURL: redirector.URL, Name: "claude-fixture"}
	if _, err := (&Runtime{}).TestProvider(context.Background(), cfg); err == nil {
		t.Fatal("Anthropic connection test accepted a redirect response")
	}
	client, err := modelHTTPClient(cfg, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lookupAnthropicOutputLimit(context.Background(), cfg, client); err == nil {
		t.Fatal("Anthropic model lookup accepted a redirect response")
	}
	select {
	case headers := <-redirected:
		t.Fatalf("Anthropic redirect reached another origin with x-api-key %q", headers.Get("x-api-key"))
	default:
	}
}

func TestRuntimeReloadAppliesCompleteSystemPromptToExistingConversation(t *testing.T) {
	type wireMessage struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	requests := make(chan []wireMessage, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []wireMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode model request: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		responseContent := "done"
		for _, message := range request.Messages {
			if message.Role == "system" && message.Content == sessionTitleInstruction {
				responseContent = `{"title":"Prompt test"}`
			} else if message.Role == "system" {
				requests <- request.Messages
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		responseData, _ := json.Marshal(responseContent)
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-prompt\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"fixture-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%s},\"finish_reason\":null}]}\n\n", responseData)
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-prompt\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"fixture-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/runtime-prompt.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	encryptor, err := security.NewEncryptor("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Model = config.Model{APIKey: "fixture-key", BaseURL: server.URL + "/v1", Name: "fixture-model", ContextWindow: 200000}
	svc := service.New(st, nil, encryptor, security.NewRedactor(), cfg.Limits, cfg)
	firstPrompt := "first complete system prompt"
	if _, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{
		AgentMaxIterations: domain.DefaultAgentMaxIterations, SystemPrompt: &firstPrompt,
	}, "test"); err != nil {
		t.Fatal(err)
	}
	runtime, err := New(ctx, cfg.Model, svc, st)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Status().ContextWindow != 200000 {
		t.Fatalf("configured context window = %d", runtime.Status().ContextWindow)
	}
	if _, err := runtime.Query(ctx, "same_session", "first turn", nil); err != nil {
		t.Fatal(err)
	}
	assertSystemPrompt := func(configured string) {
		t.Helper()
		want := hostPlatformSystemPrompt(configured, goruntime.GOOS, goruntime.GOARCH)
		select {
		case messages := <-requests:
			for _, message := range messages {
				if message.Role == "system" {
					if message.Content != want {
						t.Fatalf("system prompt = %q, want %q", message.Content, want)
					}
					return
				}
			}
			t.Fatalf("model request did not contain a system prompt: %#v", messages)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for model request")
		}
	}
	assertSystemPrompt(firstPrompt)

	secondPrompt := "replacement prompt without the built-in instructions"
	if _, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{
		AgentMaxIterations: domain.DefaultAgentMaxIterations, SystemPrompt: &secondPrompt,
	}, "test"); err != nil {
		t.Fatal(err)
	}
	runtime.fallback.ContextWindow = 160000
	if err := runtime.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if runtime.Status().ContextWindow != 160000 {
		t.Fatalf("manual context window = %d", runtime.Status().ContextWindow)
	}
	if _, err := runtime.Query(ctx, "same_session", "second turn", nil); err != nil {
		t.Fatal(err)
	}
	assertSystemPrompt(secondPrompt)

	emptyPrompt := ""
	if _, err := svc.SaveSystemSettings(ctx, domain.SystemSettingsInput{
		AgentMaxIterations: domain.DefaultAgentMaxIterations, SystemPrompt: &emptyPrompt,
	}, "test"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Query(ctx, "same_session", "third turn", nil); err != nil {
		t.Fatal(err)
	}
	assertSystemPrompt(emptyPrompt)
}

func TestHostPlatformSystemPromptDistinguishesLocalAndRemoteHosts(t *testing.T) {
	got := hostPlatformSystemPrompt("custom instructions", "windows", "amd64")
	for _, want := range []string{"custom instructions", "service host platform: windows/amd64", "local Workspace tools", "not registered SSH hosts"} {
		if !strings.Contains(got, want) {
			t.Fatalf("host platform prompt is missing %q: %s", want, got)
		}
	}

	empty := hostPlatformSystemPrompt("", "linux", "arm64")
	if strings.Contains(empty, domain.DefaultSystemPrompt) || !strings.Contains(empty, "service host platform: linux/arm64") {
		t.Fatalf("empty configured prompt did not produce only runtime host context: %q", empty)
	}
}

func TestProviderUsesConfiguredHTTPProxy(t *testing.T) {
	const proxyPassword = "runtime-proxy-secret"
	wantProxyAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("proxy-user:"+proxyPassword))
	proxyHits := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits++
		if r.Method != http.MethodPost || r.URL.Host != "model.invalid" || r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected proxied model request: %s %s", r.Method, r.URL.String())
		}
		if got := r.Header.Get("Proxy-Authorization"); got != wantProxyAuth {
			t.Errorf("unexpected proxy authorization: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "id":"chatcmpl-proxy","object":"chat.completion","created":1,"model":"fixture-model",
  "choices":[{"index":0,"message":{"role":"assistant","content":"Hello through proxy"},"finish_reason":"stop"}]
}`))
	}))
	defer proxy.Close()

	result, err := (&Runtime{}).TestProvider(context.Background(), config.Model{
		BaseURL: "http://model.invalid/v1", Name: "fixture-model", ProxyURL: proxy.URL,
		ProxyUsername: "proxy-user", ProxyPassword: proxyPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	if proxyHits != 1 || result.Response != "Hello through proxy" {
		t.Fatalf("model test did not use the configured proxy: hits=%d result=%#v", proxyHits, result)
	}
}

func TestProviderRedactsCredentialEchoFromUpstreamError(t *testing.T) {
	const apiKey = "api-secret-value"
	const proxyPassword = "proxy-secret-value"
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, apiKey+" "+proxyPassword, http.StatusUnauthorized)
	}))
	defer proxy.Close()

	_, err := (&Runtime{}).TestProvider(context.Background(), config.Model{
		APIKey: apiKey, BaseURL: "http://model.invalid/v1", Name: "fixture-model", ProxyURL: proxy.URL,
		ProxyUsername: "proxy-user", ProxyPassword: proxyPassword,
	})
	if err == nil {
		t.Fatal("upstream error was accepted")
	}
	if strings.Contains(err.Error(), apiKey) || strings.Contains(err.Error(), proxyPassword) {
		t.Fatalf("model error exposed credentials: %v", err)
	}
}

func TestRuntimeTracksOneActiveRunPerSession(t *testing.T) {
	runtime := &Runtime{}
	runCtx, started := runtime.beginSession(context.Background(), "session_test")
	if !started {
		t.Fatal("first run was not registered")
	}
	if !runtime.IsSessionActive("session_test") {
		t.Fatal("registered run is not active")
	}
	if _, duplicateStarted := runtime.beginSession(context.Background(), "session_test"); duplicateStarted {
		t.Fatal("second concurrent run for the same session was accepted")
	}
	if !runtime.CancelSession("session_test") {
		t.Fatal("active run was not cancelled")
	}
	select {
	case <-runCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("registered run context was not cancelled")
	}
	runtime.endSession("session_test")
	if runtime.IsSessionActive("session_test") {
		t.Fatal("completed run remained active")
	}
	if runtime.CancelSession("session_test") {
		t.Fatal("inactive session reported a cancellation")
	}
}

func TestCancelSessionStopsQueryAndPersistsInterruption(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/runtime.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	runner := &blockingAgentRunner{started: make(chan struct{})}
	runtime := &Runtime{runner: runner, store: st}
	events := make(chan Event, 8)
	queryDone := make(chan error, 1)
	go func() {
		_, queryErr := runtime.Query(ctx, "session_cancel", "keep investigating", func(event Event) { events <- event })
		queryDone <- queryErr
	}()
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("Agent query did not start")
	}
	if !runtime.CancelSession("session_cancel") {
		t.Fatal("active Agent query was not cancelled")
	}
	select {
	case queryErr := <-queryDone:
		if !errors.Is(queryErr, context.Canceled) {
			t.Fatalf("query error = %v", queryErr)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled Agent query did not stop")
	}
	close(events)
	interrupted := 0
	for event := range events {
		if event.Type == "interrupted" && event.Content == interruptedRunMessage {
			interrupted++
		}
	}
	if interrupted != 1 {
		t.Fatalf("interrupted events = %d", interrupted)
	}
	messages, err := st.ListChatMessages(ctx, "session_cancel", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 0 {
		t.Fatalf("interrupted prompt excluded from future context was retained: %#v", messages)
	}
}

func TestQueryRetriesEmptyResponseWithoutDuplicatingUserMessage(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/runtime.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	runner := &scriptedAgentRunner{attempts: [][]*adk.AgentEvent{
		nil,
		{adk.EventFromMessage(schema.AssistantMessage("recovered", nil), nil, schema.Assistant, "")},
	}}
	runtime := &Runtime{runner: runner, store: st, retryWait: func(context.Context, time.Duration) error { return nil }}
	var emitted []Event
	answer, err := runtime.Query(ctx, "session_retry", "continue", func(event Event) {
		emitted = append(emitted, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if answer != "recovered" {
		t.Fatalf("answer = %q", answer)
	}
	calls, inputs := runner.snapshot()
	if calls != 2 || len(inputs) != 2 || len(inputs[0]) != 1 || len(inputs[1]) != 1 {
		t.Fatalf("retry calls/inputs = %d %#v", calls, inputs)
	}
	messages, err := st.ListChatMessages(ctx, "session_retry", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Role != "user" || messages[0].Status != "completed" || messages[1].Role != "assistant" {
		t.Fatalf("stored messages = %#v", messages)
	}
	done := 0
	for _, event := range emitted {
		if event.Type == "done" {
			done++
		}
	}
	if done != 1 {
		t.Fatalf("done events = %d, events = %#v", done, emitted)
	}
	retries := make([]Event, 0, 1)
	for _, event := range emitted {
		if event.Type == "retry" {
			retries = append(retries, event)
		}
	}
	if len(retries) != 1 || retries[0].RetryAttempt != 1 || retries[0].RetryMax != modelRequestMaxRetries || retries[0].RetryDelayMS != 1000 {
		t.Fatalf("retry events = %#v", retries)
	}
}

func TestQueryInjectsPersistedTasksBeforeCurrentUser(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/runtime.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.WriteAgentTaskFile(ctx, "session_task_context", "agent-tasks/1.json", `{"id":"1","subject":"Inspect logs","description":"Inspect logs","status":"completed","blocks":[],"blockedBy":[]}`); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteAgentTaskFile(ctx, "session_task_context", "agent-tasks/2.json", `{"id":"2","subject":"Fix timeout","description":"Fix timeout","status":"in_progress","blocks":[],"blockedBy":[]}`); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedAgentRunner{attempts: [][]*adk.AgentEvent{
		{adk.EventFromMessage(schema.AssistantMessage("continuing the current step", nil), nil, schema.Assistant, "")},
	}}
	runtime := &Runtime{runner: runner, store: st}
	if _, err := runtime.Query(ctx, "session_task_context", "continue", nil); err != nil {
		t.Fatal(err)
	}
	_, inputs := runner.snapshot()
	if len(inputs) != 1 || len(inputs[0]) != 2 {
		t.Fatalf("model inputs = %#v", inputs)
	}
	taskMessage, userMessage := inputs[0][0], inputs[0][1]
	if taskMessage.Role != schema.System || !strings.Contains(taskMessage.Content, "Fix timeout") || !strings.Contains(taskMessage.Content, `"status":"in_progress"`) || !strings.Contains(taskMessage.Content, "text untrusted") {
		t.Fatalf("task context = %#v", taskMessage)
	}
	if strings.Contains(taskMessage.Content, "session_task_context") {
		t.Fatalf("task context exposed the internal session id: %s", taskMessage.Content)
	}
	if userMessage.Role != schema.User || userMessage.Content != "continue" {
		t.Fatalf("current user message = %#v", userMessage)
	}
}

func TestQueryEmitsCurrentTasksWithTaskToolResult(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/runtime.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const sessionID = "session_task_event"
	if err := st.WriteAgentTaskFile(ctx, sessionID, "agent-tasks/1.json", `{"id":"1","subject":"Inspect logs","description":"Inspect logs","status":"in_progress","blocks":[],"blockedBy":[]}`); err != nil {
		t.Fatal(err)
	}
	toolCall := schema.ToolCall{ID: "call-task-list", Type: "function", Function: schema.FunctionCall{Name: "TaskList", Arguments: `{}`}}
	runner := &scriptedAgentRunner{attempts: [][]*adk.AgentEvent{{
		adk.EventFromMessage(schema.AssistantMessage("", []schema.ToolCall{toolCall}), nil, schema.Assistant, ""),
		adk.EventFromMessage(schema.ToolMessage(`{"result":"#1 [in_progress] Inspect logs"}`, toolCall.ID, schema.WithToolName(toolCall.Function.Name)), nil, schema.Tool, toolCall.Function.Name),
		adk.EventFromMessage(schema.AssistantMessage("Continuing.", nil), nil, schema.Assistant, ""),
	}}}
	runtime := &Runtime{runner: runner, store: st}
	var taskEvent Event
	if _, err := runtime.Query(ctx, sessionID, "continue", func(event Event) {
		if event.Type == "tool" && event.ToolName == "TaskList" && event.Status != "in_progress" {
			taskEvent = event
		}
	}); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Tasks domain.AgentTaskList `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(taskEvent.Content), &payload); err != nil {
		t.Fatalf("decode task event: %v, content=%s", err, taskEvent.Content)
	}
	if payload.Tasks.SessionID != sessionID || len(payload.Tasks.Items) != 1 || payload.Tasks.Items[0].Subject != "Inspect logs" {
		t.Fatalf("task event did not include current session state: %#v", payload.Tasks)
	}
}

func TestQueryRejectsRepeatedEmptyResponseAndExcludesFailedTurn(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/runtime.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	runner := &scriptedAgentRunner{attempts: [][]*adk.AgentEvent{nil, nil}}
	runtime := &Runtime{runner: runner, store: st, retryWait: func(context.Context, time.Duration) error { return nil }}
	var emitted []Event
	_, err = runtime.Query(ctx, "session_empty", "continue", func(event Event) {
		emitted = append(emitted, event)
	})
	if !errors.Is(err, ErrEmptyResponse) {
		t.Fatalf("error = %v", err)
	}
	calls, _ := runner.snapshot()
	if calls != emptyResponseMaxAttempts {
		t.Fatalf("calls = %d", calls)
	}
	messages, err := st.ListChatMessages(ctx, "session_empty", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 0 {
		t.Fatalf("failed prompt excluded from future context was retained: %#v", messages)
	}
	modelMessages, err := st.ListChatModelMessages(ctx, "session_empty", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(modelMessages) != 0 {
		t.Fatalf("failed turn leaked into model history: %#v", modelMessages)
	}
	for _, event := range emitted {
		if event.Type == "done" {
			t.Fatalf("empty query emitted done: %#v", emitted)
		}
	}
}

func TestQueryDoesNotRetryAfterToolActivityWithoutFinalAnswer(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/runtime.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	runner := &scriptedAgentRunner{attempts: [][]*adk.AgentEvent{{
		adk.EventFromMessage(schema.ToolMessage("tool completed", "call-1"), nil, schema.Tool, "ssh_exec"),
	}}}
	runtime := &Runtime{runner: runner, store: st}
	var emitted []Event
	_, err = runtime.Query(ctx, "session_tool_empty", "inspect host", func(event Event) {
		emitted = append(emitted, event)
	})
	if !errors.Is(err, ErrEmptyResponse) {
		t.Fatalf("error = %v", err)
	}
	calls, _ := runner.snapshot()
	if calls != 1 {
		t.Fatalf("unsafe retry after tool activity: calls = %d", calls)
	}
	messages, err := st.ListChatMessages(ctx, "session_tool_empty", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Status != "failed" || messages[1].Role != "tool" {
		t.Fatalf("stored messages = %#v", messages)
	}
	for _, event := range emitted {
		if event.Type == "done" {
			t.Fatalf("incomplete query emitted done: %#v", emitted)
		}
	}
}

func TestQueryUsesNoToolFinalizerAfterToolActivityWithoutFinalAnswer(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/runtime.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.WriteAgentTaskFile(ctx, "session_safe_finalizer", "agent-tasks/1.json", `{"id":"1","subject":"Deploy","description":"Deploy the service","status":"completed","blocks":[],"blockedBy":[]}`); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedAgentRunner{attempts: [][]*adk.AgentEvent{{
		adk.EventFromMessage(schema.ToolMessage(`{"status":"completed","stdout":"active"}`, "call-1", schema.WithToolName("ssh_exec")), nil, schema.Tool, "ssh_exec"),
	}}}
	finalizer := &scriptedAgentRunner{attempts: [][]*adk.AgentEvent{{
		adk.EventFromMessage(schema.AssistantMessage("服务已部署并验证为 active。", nil), nil, schema.Assistant, ""),
	}}}
	runtime := &Runtime{runner: runner, finalizer: finalizer, store: st}
	var emitted []Event

	answer, err := runtime.Query(ctx, "session_safe_finalizer", "部署服务", func(event Event) {
		emitted = append(emitted, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if answer != "服务已部署并验证为 active。" {
		t.Fatalf("answer = %q", answer)
	}
	runnerCalls, _ := runner.snapshot()
	finalizerCalls, finalizerInputs := finalizer.snapshot()
	if runnerCalls != 1 || finalizerCalls != 1 {
		t.Fatalf("calls: runner=%d finalizer=%d", runnerCalls, finalizerCalls)
	}
	if len(finalizerInputs) != 1 || len(finalizerInputs[0]) != 1 || finalizerInputs[0][0].Role != schema.User {
		t.Fatalf("finalizer inputs = %#v", finalizerInputs)
	}
	var input finalAnswerInput
	if err := json.Unmarshal([]byte(finalizerInputs[0][0].Content), &input); err != nil {
		t.Fatal(err)
	}
	if input.Request != "部署服务" || len(input.Tasks) != 1 || input.Tasks[0].Status != "completed" || len(input.ToolResults) != 1 || input.ToolResults[0].ToolName != "ssh_exec" || !strings.Contains(input.ToolResults[0].Content, "active") {
		t.Fatalf("final answer context = %#v", input)
	}
	messages, err := st.ListChatMessages(ctx, "session_safe_finalizer", 10)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := replayAssistantLifecycle(t, emitted)
	if len(lifecycle.committedIDs) != 1 || len(lifecycle.resetIDs) != 0 || len(lifecycle.active) != 0 || lifecycle.content[lifecycle.committedIDs[0]] != answer {
		t.Fatalf("assistant lifecycle = %#v, events = %#v", lifecycle, emitted)
	}
	if len(messages) != 3 || messages[0].Status != "completed" || messages[1].Role != "tool" || messages[2].Role != "assistant" || messages[2].Content != answer || messages[2].ID != lifecycle.committedIDs[0] {
		t.Fatalf("stored messages = %#v", messages)
	}
	messageEvents, doneEvents := 0, 0
	for _, event := range emitted {
		if event.Type == "message" && event.Role == string(schema.Assistant) && event.Content == answer {
			messageEvents++
		}
		if event.Type == "done" && event.Content == answer {
			doneEvents++
		}
	}
	if messageEvents != 1 || doneEvents != 1 {
		t.Fatalf("final answer events = %#v", emitted)
	}
}

func TestQueryBlocksPersistedToolResultLeakAndUsesFinalizer(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/runtime.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	leaked := "temporary preamble\n" + persistedToolResultsHeader + "\n\nTool: ssh_shell\nResult:\n{\"status\":\"completed\",\"stdout\":\"private-shell-output\"}\n\n" + persistedToolResultsTrailer
	split := len("temporary preamble\n") + 30
	leakedStream := schema.StreamReaderFromArray([]*schema.Message{
		{Role: schema.Assistant, Content: leaked[:split]},
		{Role: schema.Assistant, Content: leaked[split:], ResponseMeta: &schema.ResponseMeta{FinishReason: "stop"}},
	})
	runner := &scriptedAgentRunner{attempts: [][]*adk.AgentEvent{{
		adk.EventFromMessage(schema.ToolMessage(`{"status":"completed"}`, "call-1", schema.WithToolName("ssh_shell")), nil, schema.Tool, "ssh_shell"),
		adk.EventFromMessage(nil, leakedStream, schema.Assistant, ""),
	}}}
	finalizer := &scriptedAgentRunner{attempts: [][]*adk.AgentEvent{{
		adk.EventFromMessage(schema.AssistantMessage("终端已创建。", nil), nil, schema.Assistant, ""),
	}}}
	runtime := &Runtime{runner: runner, finalizer: finalizer, store: st}
	var emitted []Event

	answer, err := runtime.Query(ctx, "session_context_leak", "创建终端", func(event Event) {
		emitted = append(emitted, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if answer != "终端已创建。" {
		t.Fatalf("answer = %q", answer)
	}
	for _, event := range emitted {
		if containsInternalContextMarker(event.Content) || strings.Contains(event.Content, "private-shell-output") {
			t.Fatalf("internal context reached the event stream: %#v", emitted)
		}
	}
	lifecycle := replayAssistantLifecycle(t, emitted)
	if len(lifecycle.resetIDs) != 1 || len(lifecycle.committedIDs) != 1 || len(lifecycle.active) != 0 || lifecycle.content[lifecycle.committedIDs[0]] != answer {
		t.Fatalf("assistant lifecycle = %#v, events = %#v", lifecycle, emitted)
	}
	runnerCalls, _ := runner.snapshot()
	finalizerCalls, _ := finalizer.snapshot()
	if runnerCalls != 1 || finalizerCalls != 1 {
		t.Fatalf("calls: runner=%d finalizer=%d", runnerCalls, finalizerCalls)
	}
	messages, err := st.ListChatMessages(ctx, "session_context_leak", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 || messages[0].Status != "completed" || messages[1].Role != "tool" || messages[2].Role != "assistant" || messages[2].Content != answer || messages[2].ID != lifecycle.committedIDs[0] {
		t.Fatalf("stored messages = %#v", messages)
	}
}

func TestQueryReportsFinalizerFailureWithoutRerunningTools(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/runtime.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	runner := &scriptedAgentRunner{attempts: [][]*adk.AgentEvent{{
		adk.EventFromMessage(schema.ToolMessage("tool completed", "call-1", schema.WithToolName("ssh_exec")), nil, schema.Tool, "ssh_exec"),
	}}}
	finalizer := &scriptedAgentRunner{attempts: [][]*adk.AgentEvent{nil}}
	runtime := &Runtime{runner: runner, finalizer: finalizer, store: st}
	var emitted []Event

	_, err = runtime.Query(ctx, "session_failed_finalizer", "inspect host", func(event Event) {
		emitted = append(emitted, event)
	})
	if !errors.Is(err, ErrEmptyResponse) || !strings.Contains(err.Error(), "safe final answer generation failed") {
		t.Fatalf("error = %v", err)
	}
	runnerCalls, _ := runner.snapshot()
	finalizerCalls, _ := finalizer.snapshot()
	if runnerCalls != 1 || finalizerCalls != 1 {
		t.Fatalf("unsafe retry: runner=%d finalizer=%d", runnerCalls, finalizerCalls)
	}
	for _, event := range emitted {
		if event.Type == "done" {
			t.Fatalf("failed finalizer emitted done: %#v", emitted)
		}
	}
}

func TestQueryPersistsToolCallPreambleForDisplay(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/runtime.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	toolCall := schema.ToolCall{
		ID: "call-1", Type: "function",
		Function: schema.FunctionCall{Name: "ssh_exec", Arguments: `{}`},
	}
	preamble := schema.AssistantMessage("I will inspect the host.", []schema.ToolCall{toolCall})
	preamble.ResponseMeta = &schema.ResponseMeta{FinishReason: "tool_calls"}
	terminal := schema.AssistantMessage("Host memory is stable.", nil)
	terminal.ResponseMeta = &schema.ResponseMeta{FinishReason: "stop"}
	runner := &scriptedAgentRunner{attempts: [][]*adk.AgentEvent{{
		adk.EventFromMessage(preamble, nil, schema.Assistant, ""),
		adk.EventFromMessage(schema.ToolMessage("tool completed", "call-1", schema.WithToolName("ssh_exec")), nil, schema.Tool, "ssh_exec"),
		adk.EventFromMessage(terminal, nil, schema.Assistant, ""),
	}}}
	runtime := &Runtime{runner: runner, store: st}

	answer, err := runtime.Query(ctx, "session_terminal", "inspect host", nil)
	if err != nil {
		t.Fatal(err)
	}
	if answer != "Host memory is stable." {
		t.Fatalf("answer = %q", answer)
	}
	messages, err := st.ListChatMessages(ctx, "session_terminal", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 4 || messages[0].Status != "completed" ||
		messages[1].Role != domain.ChatMessageRoleAssistantProgress || messages[1].Content != "I will inspect the host." ||
		messages[2].Role != "tool" || messages[3].Role != "assistant" || messages[3].Content != answer {
		t.Fatalf("stored messages = %#v", messages)
	}
	contextMessages, err := st.ListChatContextMessages(ctx, "session_terminal")
	if err != nil {
		t.Fatal(err)
	}
	if len(contextMessages) != 4 || contextMessages[0].Role != "user" ||
		contextMessages[1].Role != domain.ChatMessageRoleAssistantProgress ||
		contextMessages[2].Role != "tool" || contextMessages[3].Role != "assistant" {
		t.Fatalf("model context omitted tool preamble: %#v", contextMessages)
	}
}

func TestQueryEmitsAndPersistsContextUsage(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/runtime.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	message := schema.AssistantMessage("Context recorded.", nil)
	message.ResponseMeta = &schema.ResponseMeta{
		FinishReason: "stop",
		Usage:        &schema.TokenUsage{PromptTokens: 1200, CompletionTokens: 80, TotalTokens: 1280},
	}
	stream := schema.StreamReaderFromArray([]*schema.Message{message})
	runner := &scriptedAgentRunner{attempts: [][]*adk.AgentEvent{{
		adk.EventFromMessage(nil, stream, schema.Assistant, ""),
	}}}
	runtime := &Runtime{runner: runner, store: st, contextWindow: 200000}
	var usage Event
	if _, err := runtime.Query(ctx, "session_context_usage", "inspect context", func(event Event) {
		if event.Type == "context_usage" {
			usage = event
		}
	}); err != nil {
		t.Fatal(err)
	}
	if usage.ContextTokens != 1280 || usage.ContextWindow != 200000 {
		t.Fatalf("context usage event = %#v", usage)
	}
	session, err := st.GetChatSession(ctx, "session_context_usage")
	if err != nil {
		t.Fatal(err)
	}
	if session.ContextTokens != 1280 || session.ContextWindow != 200000 {
		t.Fatalf("stored context usage = %d/%d", session.ContextTokens, session.ContextWindow)
	}
}

func TestQueryKeepsLatestModelContextUsageInsteadOfAccumulatingToolLoop(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/runtime.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	toolCall := schema.ToolCall{ID: "call-context", Type: "function", Function: schema.FunctionCall{Name: "ssh_exec", Arguments: `{}`}}
	preamble := schema.AssistantMessage("Checking.", []schema.ToolCall{toolCall})
	preamble.ResponseMeta = &schema.ResponseMeta{FinishReason: "tool_calls", Usage: &schema.TokenUsage{TotalTokens: 400}}
	terminal := schema.AssistantMessage("Completed.", nil)
	terminal.ResponseMeta = &schema.ResponseMeta{FinishReason: "stop", Usage: &schema.TokenUsage{TotalTokens: 900}}
	runner := &scriptedAgentRunner{attempts: [][]*adk.AgentEvent{{
		adk.EventFromMessage(preamble, nil, schema.Assistant, ""),
		adk.EventFromMessage(schema.ToolMessage("done", "call-context", schema.WithToolName("ssh_exec")), nil, schema.Tool, "ssh_exec"),
		adk.EventFromMessage(terminal, nil, schema.Assistant, ""),
	}}}
	runtime := &Runtime{runner: runner, store: st, contextWindow: 128000}
	var usages []int
	if _, err := runtime.Query(ctx, "session_context_loop", "inspect", func(event Event) {
		if event.Type == "context_usage" {
			usages = append(usages, event.ContextTokens)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if len(usages) != 2 || usages[0] != 400 || usages[1] != 900 {
		t.Fatalf("context usage events = %#v", usages)
	}
	session, err := st.GetChatSession(ctx, "session_context_loop")
	if err != nil {
		t.Fatal(err)
	}
	if session.ContextTokens != 900 || session.ContextWindow != 128000 {
		t.Fatalf("stored context usage = %d/%d", session.ContextTokens, session.ContextWindow)
	}
}

func TestQueryDoesNotClaimRunReferencedByTaskResult(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/runtime.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const sessionID = "session_referenced_run"
	if _, err := st.CreateChatSession(ctx, sessionID, ""); err != nil {
		t.Fatal(err)
	}
	ownerMessageID, err := st.AppendPendingChatMessage(ctx, sessionID, "user", "start background task")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartChatToolCall(ctx, domain.ChatToolCall{
		SessionID: sessionID, UserMessageID: ownerMessageID, ToolCallID: "call-owner",
		ToolName: "ssh_exec", ArgumentsJSON: `{"background":true}`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.BindChatToolCallRun(ctx, sessionID, "call-owner", "run-shared"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FinishChatToolCall(ctx, sessionID, "call-owner", "run-shared", domain.ChatToolCallCompleted,
		`{"status":"running","run_id":"run-shared","task_id":"task-one"}`, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.SetChatMessageStatus(ctx, ownerMessageID, "completed"); err != nil {
		t.Fatal(err)
	}

	toolCall := schema.ToolCall{
		ID: "call-task-status", Type: "function",
		Function: schema.FunctionCall{Name: "ssh_task", Arguments: `{"action":"status","task_id":"task-one"}`},
	}
	terminal := schema.AssistantMessage("Task completed.", nil)
	terminal.ResponseMeta = &schema.ResponseMeta{FinishReason: "stop"}
	runner := &scriptedAgentRunner{attempts: [][]*adk.AgentEvent{{
		adk.EventFromMessage(schema.AssistantMessage("", []schema.ToolCall{toolCall}), nil, schema.Assistant, ""),
		adk.EventFromMessage(schema.ToolMessage(`{"status":"completed","run_id":"run-shared","task_id":"task-one","stdout":"ok"}`, "call-task-status", schema.WithToolName("ssh_task")), nil, schema.Tool, "ssh_task"),
		adk.EventFromMessage(terminal, nil, schema.Assistant, ""),
	}}}
	runtime := &Runtime{runner: runner, store: st}

	answer, err := runtime.Query(ctx, sessionID, "check task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if answer != "Task completed." {
		t.Fatalf("answer = %q", answer)
	}
	owner, err := st.GetChatToolCallByRun(ctx, "run-shared")
	if err != nil {
		t.Fatal(err)
	}
	if owner.ToolCallID != "call-owner" {
		t.Fatalf("run owner changed: %#v", owner)
	}
	statusCall, err := st.GetChatToolCall(ctx, sessionID, "call-task-status")
	if err != nil {
		t.Fatal(err)
	}
	if statusCall.Status != domain.ChatToolCallCompleted || statusCall.RunID != "" || !strings.Contains(statusCall.ResultJSON, `"run_id":"run-shared"`) {
		t.Fatalf("task status call = %#v", statusCall)
	}
}

func TestQueryStreamsToolLifecycleWithStableCallID(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/runtime.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	runtime := &Runtime{runner: &toolActivityAgentRunner{}, store: st}
	var emitted []Event

	answer, err := runtime.Query(ctx, "session_tool_lifecycle", "inspect uptime", func(event Event) {
		emitted = append(emitted, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if answer != "Host is available." {
		t.Fatalf("answer = %q", answer)
	}
	var startedIndex, completedIndex = -1, -1
	for index, event := range emitted {
		if event.Type != "tool" || event.ToolCallID != "call-live" {
			continue
		}
		if event.Status == "in_progress" {
			startedIndex = index
			var payload struct {
				Status  string `json:"status"`
				Display struct {
					Arguments map[string]any `json:"arguments"`
				} `json:"_display"`
			}
			if err := json.Unmarshal([]byte(event.Content), &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Status != "in_progress" || payload.Display.Arguments["host_id"] != "host-live" {
				t.Fatalf("started tool payload = %#v", payload)
			}
		} else {
			completedIndex = index
			if !strings.Contains(event.Content, `"status":"completed"`) || !strings.Contains(event.Content, `"program":"uptime"`) {
				t.Fatalf("completed tool payload = %s", event.Content)
			}
		}
	}
	if startedIndex < 0 || completedIndex <= startedIndex {
		t.Fatalf("tool lifecycle order = %#v", emitted)
	}
}

func TestModelFailureDoesNotCancelRunningToolAndPersistsItsTerminalResult(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/runtime.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	runner := &modelFailureWithRunningToolRunner{
		started: make(chan struct{}), release: make(chan struct{}), finished: make(chan error, 1),
	}
	runtime := &Runtime{runner: runner, store: st}
	if _, err := runtime.Query(ctx, "session_model_failure_tool", "run it", nil); err == nil || !strings.Contains(err.Error(), "model request failed") {
		t.Fatalf("query error = %v", err)
	}
	call, err := st.GetChatToolCall(ctx, "session_model_failure_tool", "call-detached")
	if err != nil {
		t.Fatal(err)
	}
	if call.Status != domain.ChatToolCallRunning {
		t.Fatalf("tool call after model failure = %#v", call)
	}
	close(runner.release)
	select {
	case err := <-runner.finished:
		if err != nil {
			t.Fatalf("tool inherited model cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("tool did not finish")
	}
	deadline := time.Now().Add(time.Second)
	for {
		call, err = st.GetChatToolCall(ctx, "session_model_failure_tool", "call-detached")
		if err != nil {
			t.Fatal(err)
		}
		if call.Status == domain.ChatToolCallCompleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("terminal tool result was not persisted: %#v", call)
		}
		time.Sleep(10 * time.Millisecond)
	}
	history, err := st.ListChatContextMessages(ctx, "session_model_failure_tool")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].Role != "user" || history[0].Status != "failed" || history[1].Role != "tool" || !strings.Contains(history[1].Content, "finished") {
		t.Fatalf("persisted failed-turn context = %#v", history)
	}
}

func TestUserCanStopToolAfterModelPhaseHasFailed(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/runtime.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	runner := &modelFailureWithRunningToolRunner{
		started: make(chan struct{}), release: make(chan struct{}), finished: make(chan error, 1),
	}
	runtime := &Runtime{runner: runner, store: st}
	if _, err := runtime.Query(ctx, "session_stop_orphan_tool", "run it", nil); err == nil {
		t.Fatal("model failure was not returned")
	}
	if runtime.IsSessionActive("session_stop_orphan_tool") {
		t.Fatal("model phase remained active")
	}
	if !runtime.CancelSession("session_stop_orphan_tool") {
		t.Fatal("running tool was not available to explicit stop")
	}
	select {
	case err := <-runner.finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stopped tool error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("explicit stop did not cancel the tool")
	}
	deadline := time.Now().Add(time.Second)
	for {
		call, err := st.GetChatToolCall(ctx, "session_stop_orphan_tool", "call-detached")
		if err != nil {
			t.Fatal(err)
		}
		if call.Status == domain.ChatToolCallInterrupted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stopped tool terminal status = %#v", call)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestQueryRejectsReasoningOnlyTerminalOutputAfterToolActivity(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/runtime.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	toolCall := schema.ToolCall{
		ID: "call-1", Type: "function",
		Function: schema.FunctionCall{Name: "ssh_exec", Arguments: `{}`},
	}
	preamble := schema.AssistantMessage("I will inspect the host.", []schema.ToolCall{toolCall})
	preamble.ResponseMeta = &schema.ResponseMeta{FinishReason: "tool_calls"}
	reasoningOnly := &schema.Message{
		Role: schema.Assistant, ReasoningContent: "I have the latest data and will summarize it.",
		ResponseMeta: &schema.ResponseMeta{FinishReason: "stop"},
	}
	runner := &scriptedAgentRunner{attempts: [][]*adk.AgentEvent{{
		adk.EventFromMessage(preamble, nil, schema.Assistant, ""),
		adk.EventFromMessage(schema.ToolMessage("tool completed", "call-1", schema.WithToolName("ssh_exec")), nil, schema.Tool, "ssh_exec"),
		adk.EventFromMessage(reasoningOnly, nil, schema.Assistant, ""),
	}}}
	runtime := &Runtime{runner: runner, store: st}
	var emitted []Event

	_, err = runtime.Query(ctx, "session_reasoning_only", "inspect host", func(event Event) {
		emitted = append(emitted, event)
	})
	if !errors.Is(err, ErrEmptyResponse) {
		t.Fatalf("error = %v", err)
	}
	calls, _ := runner.snapshot()
	if calls != 1 {
		t.Fatalf("unsafe retry after tool activity: calls = %d", calls)
	}
	messages, err := st.ListChatMessages(ctx, "session_reasoning_only", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 4 || messages[0].Role != "user" || messages[0].Status != "failed" ||
		messages[1].Role != domain.ChatMessageRoleAssistantProgress || messages[1].Content != "I will inspect the host." ||
		messages[2].Role != "tool" || messages[3].Role != "reasoning" {
		t.Fatalf("stored messages = %#v", messages)
	}
	contextMessages, err := st.ListChatContextMessages(ctx, "session_reasoning_only")
	if err != nil {
		t.Fatal(err)
	}
	if len(contextMessages) != 4 || contextMessages[0].Role != "user" ||
		contextMessages[1].Role != domain.ChatMessageRoleAssistantProgress ||
		contextMessages[2].Role != "tool" || contextMessages[3].Role != "reasoning" {
		t.Fatalf("model context omitted persisted model output: %#v", contextMessages)
	}
	for _, event := range emitted {
		if event.Type == "done" {
			t.Fatalf("reasoning-only query emitted done: %#v", emitted)
		}
	}
}

func TestQueryStreamingPersistsToolCallPreambleForDisplay(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/runtime.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	toolCall := schema.ToolCall{
		ID: "call-1", Type: "function",
		Function: schema.FunctionCall{Name: "ssh_exec", Arguments: `{}`},
	}
	preambleStream := schema.StreamReaderFromArray([]*schema.Message{
		{Role: schema.Assistant, Content: "I will "},
		{Role: schema.Assistant, Content: "inspect.", ToolCalls: []schema.ToolCall{toolCall}, ResponseMeta: &schema.ResponseMeta{FinishReason: "tool_calls"}},
	})
	terminalStream := schema.StreamReaderFromArray([]*schema.Message{
		{Role: schema.Assistant, ReasoningContent: "The command completed successfully."},
		{Role: schema.Assistant, Content: "Host memory "},
		{Role: schema.Assistant, Content: "is stable.", ResponseMeta: &schema.ResponseMeta{FinishReason: "stop"}},
	})
	runner := &scriptedAgentRunner{attempts: [][]*adk.AgentEvent{{
		adk.EventFromMessage(nil, preambleStream, schema.Assistant, ""),
		adk.EventFromMessage(schema.ToolMessage("tool completed", "call-1", schema.WithToolName("ssh_exec")), nil, schema.Tool, "ssh_exec"),
		adk.EventFromMessage(nil, terminalStream, schema.Assistant, ""),
	}}}
	runtime := &Runtime{runner: runner, store: st}
	var emitted []Event

	answer, err := runtime.Query(ctx, "session_stream_terminal", "inspect host", func(event Event) {
		emitted = append(emitted, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if answer != "Host memory is stable." {
		t.Fatalf("answer = %q", answer)
	}
	messages, err := st.ListChatMessages(ctx, "session_stream_terminal", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 5 || messages[0].Status != "completed" ||
		messages[1].Role != domain.ChatMessageRoleAssistantProgress || messages[1].Content != "I will inspect." ||
		messages[2].Role != "tool" || messages[3].Role != "reasoning" || messages[4].Role != "assistant" || messages[4].Content != answer {
		t.Fatalf("stored messages = %#v", messages)
	}
	var assistantEvents []Event
	for _, event := range emitted {
		if event.Type == "message" && event.Role == string(schema.Assistant) {
			assistantEvents = append(assistantEvents, event)
		}
	}
	lifecycle := replayAssistantLifecycle(t, emitted)
	if len(assistantEvents) != 4 || len(lifecycle.resetIDs) != 0 || len(lifecycle.committedIDs) != 2 || len(lifecycle.active) != 0 ||
		lifecycle.content[lifecycle.committedIDs[0]] != "I will inspect." || lifecycle.content[lifecycle.committedIDs[1]] != answer ||
		messages[1].ID != lifecycle.committedIDs[0] || messages[4].ID != lifecycle.committedIDs[1] {
		t.Fatalf("assistant message events = %#v, lifecycle = %#v, stored messages = %#v", assistantEvents, lifecycle, messages)
	}
}

func TestQueryCommitsNonStreamingToolCallPreamble(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/runtime.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	toolCall := schema.ToolCall{
		ID: "call-1", Type: "function",
		Function: schema.FunctionCall{Name: "ssh_exec", Arguments: `{}`},
	}
	runner := &scriptedAgentRunner{attempts: [][]*adk.AgentEvent{{
		adk.EventFromMessage(schema.AssistantMessage("I will inspect.", []schema.ToolCall{toolCall}), nil, schema.Assistant, ""),
		adk.EventFromMessage(schema.ToolMessage("tool completed", "call-1", schema.WithToolName("ssh_exec")), nil, schema.Tool, "ssh_exec"),
		adk.EventFromMessage(schema.AssistantMessage("Host memory is stable.", nil), nil, schema.Assistant, ""),
	}}}
	runtime := &Runtime{runner: runner, store: st}
	var emitted []Event

	answer, err := runtime.Query(ctx, "session_nonstream_terminal", "inspect host", func(event Event) {
		emitted = append(emitted, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	var assistantEvents []Event
	for _, event := range emitted {
		if event.Type == "message" && event.Role == string(schema.Assistant) {
			assistantEvents = append(assistantEvents, event)
		}
	}
	lifecycle := replayAssistantLifecycle(t, emitted)
	messages, err := st.ListChatMessages(ctx, "session_nonstream_terminal", 10)
	if err != nil {
		t.Fatal(err)
	}
	if answer != "Host memory is stable." || len(assistantEvents) != 2 || assistantEvents[0].Content != "I will inspect." || assistantEvents[1].Content != answer ||
		len(lifecycle.committedIDs) != 2 || len(lifecycle.resetIDs) != 0 || len(lifecycle.active) != 0 ||
		lifecycle.content[lifecycle.committedIDs[0]] != "I will inspect." || lifecycle.content[lifecycle.committedIDs[1]] != answer ||
		len(messages) != 4 || messages[1].ID != lifecycle.committedIDs[0] || messages[3].ID != lifecycle.committedIDs[1] {
		t.Fatalf("answer = %q, assistant message events = %#v, lifecycle = %#v, stored messages = %#v", answer, assistantEvents, lifecycle, messages)
	}
}

func TestQueryResetsSupersededAssistantMessageLifecycle(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/runtime.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	runner := &scriptedAgentRunner{attempts: [][]*adk.AgentEvent{{
		adk.EventFromMessage(schema.AssistantMessage("superseded draft", nil), nil, schema.Assistant, ""),
		adk.EventFromMessage(schema.AssistantMessage("final answer", nil), nil, schema.Assistant, ""),
	}}}
	runtime := &Runtime{runner: runner, store: st}
	var emitted []Event

	answer, err := runtime.Query(ctx, "session_superseded", "continue", func(event Event) {
		emitted = append(emitted, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := replayAssistantLifecycle(t, emitted)
	if answer != "final answer" || len(lifecycle.resetIDs) != 1 || len(lifecycle.committedIDs) != 1 || len(lifecycle.active) != 0 ||
		lifecycle.content[lifecycle.resetIDs[0]] != "superseded draft" || lifecycle.content[lifecycle.committedIDs[0]] != answer {
		t.Fatalf("answer = %q, lifecycle = %#v, events = %#v", answer, lifecycle, emitted)
	}
	messages, err := st.ListChatMessages(ctx, "session_superseded", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[1].ID != lifecycle.committedIDs[0] || messages[1].Content != answer {
		t.Fatalf("stored messages = %#v", messages)
	}
}

func TestNextQueryReceivesToolResultsFromFailedTurn(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/runtime.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	runner := &scriptedAgentRunner{attempts: [][]*adk.AgentEvent{
		{adk.EventFromMessage(schema.ToolMessage(`{"status":"completed","stdout":"disk is healthy"}`, "call-1", schema.WithToolName("ssh_exec")), nil, schema.Tool, "ssh_exec")},
		{adk.EventFromMessage(schema.AssistantMessage("continued without repeating the check", nil), nil, schema.Assistant, "")},
	}}
	runtime := &Runtime{runner: runner, store: st}
	if _, err := runtime.Query(ctx, "session_tool_context", "inspect disk", nil); !errors.Is(err, ErrEmptyResponse) {
		t.Fatalf("first query error = %v", err)
	}
	answer, err := runtime.Query(ctx, "session_tool_context", "continue", nil)
	if err != nil {
		t.Fatal(err)
	}
	if answer != "continued without repeating the check" {
		t.Fatalf("answer = %q", answer)
	}
	_, inputs := runner.snapshot()
	if len(inputs) != 2 || len(inputs[1]) != 3 {
		t.Fatalf("model inputs = %#v", inputs)
	}
	if inputs[1][0].Role != schema.User || inputs[1][0].Content != "inspect disk" {
		t.Fatalf("failed turn user context = %#v", inputs[1][0])
	}
	if inputs[1][1].Role != schema.Assistant || !strings.Contains(inputs[1][1].Content, persistedToolResultsHeader) || !strings.Contains(inputs[1][1].Content, "disk is healthy") {
		t.Fatalf("failed turn tool context = %#v", inputs[1][1])
	}
	if inputs[1][2].Role != schema.User || inputs[1][2].Content != "continue" {
		t.Fatalf("current query context = %#v", inputs[1][2])
	}
}

func TestNextQueryReceivesPureAssistantReplyAndReasoning(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/runtime.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	first := schema.AssistantMessage("The deployment uses port 8080.", nil)
	first.ReasoningContent = "The user asked me to remember the configured port."
	runner := &scriptedAgentRunner{attempts: [][]*adk.AgentEvent{
		{adk.EventFromMessage(first, nil, schema.Assistant, "")},
		{adk.EventFromMessage(schema.AssistantMessage("It is still 8080.", nil), nil, schema.Assistant, "")},
	}}
	runtime := &Runtime{runner: runner, store: st}
	if _, err := runtime.Query(ctx, "session_plain_context", "Use port 8080", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Query(ctx, "session_plain_context", "Which port?", nil); err != nil {
		t.Fatal(err)
	}
	_, inputs := runner.snapshot()
	if len(inputs) != 2 || len(inputs[1]) != 3 {
		t.Fatalf("second model input = %#v", inputs)
	}
	assistant := inputs[1][1]
	if assistant.Role != schema.Assistant || assistant.Content != first.Content || assistant.ReasoningContent != first.ReasoningContent {
		t.Fatalf("persisted pure assistant context = %#v", assistant)
	}
}

func TestNextQueryRestoresAnthropicThinkingMetadata(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/runtime.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	first := schema.AssistantMessage("The service is healthy.", nil)
	first.ReasoningContent = "I checked the service state."
	first.Extra = map[string]any{
		claudeThinkingExtraKey:  first.ReasoningContent,
		claudeSignatureExtraKey: "signed-thinking",
	}
	runner := &scriptedAgentRunner{attempts: [][]*adk.AgentEvent{
		{adk.EventFromMessage(first, nil, schema.Assistant, "")},
		{adk.EventFromMessage(schema.AssistantMessage("It remains healthy.", nil), nil, schema.Assistant, "")},
	}}
	runtime := &Runtime{runner: runner, store: st, modelKind: "anthropic"}
	if _, err := runtime.Query(ctx, "session_anthropic_context", "Check the service", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Query(ctx, "session_anthropic_context", "What was the result?", nil); err != nil {
		t.Fatal(err)
	}
	_, inputs := runner.snapshot()
	if len(inputs) != 2 || len(inputs[1]) != 4 {
		t.Fatalf("second model input = %#v", inputs)
	}
	thinking := inputs[1][1]
	if thinking.Role != schema.Assistant || thinking.Content != "" || thinking.ReasoningContent != first.ReasoningContent {
		t.Fatalf("restored Anthropic thinking = %#v", thinking)
	}
	if thinking.Extra[claudeThinkingExtraKey] != first.ReasoningContent || thinking.Extra[claudeSignatureExtraKey] != "signed-thinking" {
		t.Fatalf("restored Anthropic metadata = %#v", thinking.Extra)
	}
	if inputs[1][2].Role != schema.Assistant || inputs[1][2].Content != first.Content {
		t.Fatalf("restored assistant reply = %#v", inputs[1][2])
	}
}

func TestBuildModelContextPreservesTurnBoundaries(t *testing.T) {
	history := []domain.ChatMessage{
		{Role: "user", Content: "install docker", Status: "completed"},
		{Role: "tool", ToolName: "ssh_exec", Content: `{"status":"completed","stdout":"docker installed"}`, Status: "completed"},
		{Role: "user", Content: "update mihomo", Status: "completed"},
		{Role: "tool", ToolName: "ssh_exec", Content: `{"status":"completed","stdout":"mihomo updated"}`, Status: "completed"},
		{Role: "user", Content: "hello", Status: "completed"},
	}
	messages, stats := buildModelContext(history, "what is the current state?")
	if len(messages) != 7 {
		t.Fatalf("model messages = %#v", messages)
	}
	wantRoles := []schema.RoleType{schema.User, schema.Assistant, schema.User, schema.Assistant, schema.User, schema.Assistant, schema.User}
	for index, role := range wantRoles {
		if messages[index].Role != role {
			t.Fatalf("message %d role = %s, want %s", index, messages[index].Role, role)
		}
	}
	if !strings.Contains(messages[1].Content, "docker installed") || !strings.Contains(messages[3].Content, "mihomo updated") {
		t.Fatalf("tool results were not retained: %#v", messages)
	}
	if messages[5].Content != incompleteTurnContext {
		t.Fatalf("incomplete turn marker = %q", messages[5].Content)
	}
	if stats.StoredTurns != 3 || stats.IncludedTurns != 3 || stats.ToolResults != 2 {
		t.Fatalf("context stats = %#v", stats)
	}
}

func TestBuildModelContextPreservesCompleteToolResults(t *testing.T) {
	first := "first-start\n" + strings.Repeat("甲", 50_000) + "\nfirst-end"
	second := "second-start\n" + strings.Repeat("乙", 50_000) + "\nsecond-end"
	history := []domain.ChatMessage{
		{Role: "user", Content: "inspect complete output", Status: "completed"},
		{Role: "tool", ToolName: "ssh_exec", Content: first, Status: "completed"},
		{Role: "tool", ToolName: "workspace_shell", Content: second, Status: "completed"},
	}
	messages, stats := buildModelContext(history, "continue")
	if len(messages) != 3 {
		t.Fatalf("model messages = %#v", messages)
	}
	results := messages[1].Content
	for _, expected := range []string{first, second} {
		if !strings.Contains(results, expected) {
			t.Fatalf("complete tool results were not preserved: result_bytes=%d expected_bytes=%d", len(results), len(expected))
		}
	}
	if stats.ToolResults != 2 || stats.IncludedTurns != 1 {
		t.Fatalf("context stats = %#v", stats)
	}
}

func TestBuildModelContextPreservesFinalAnswerAndCompletedToolResults(t *testing.T) {
	history := []domain.ChatMessage{
		{Role: "user", Content: "inspect host", Status: "completed"},
		{Role: "tool", ToolName: "ssh_exec", Content: `{"status":"completed","stdout":"complete output"}`, Status: "completed"},
		{Role: "assistant", Content: "The host is healthy.", Status: "completed"},
	}
	messages, stats := buildModelContext(history, "continue")
	if len(messages) != 3 || messages[1].Role != schema.Assistant || !strings.Contains(messages[1].Content, "The host is healthy.") {
		t.Fatalf("model messages = %#v", messages)
	}
	if !strings.Contains(messages[1].Content, "complete output") || !containsInternalContextMarker(messages[1].Content) {
		t.Fatalf("completed Tool results were omitted from Assistant context: %q", messages[1].Content)
	}
	if stats.ToolResults != 1 {
		t.Fatalf("context stats = %#v", stats)
	}
}

func TestBuildModelContextPreservesToolReasoningAndVisibleReplies(t *testing.T) {
	history := []domain.ChatMessage{
		{Role: "user", Content: "inspect host", Status: "completed"},
		{Role: "reasoning", Content: "I should inspect memory before answering.", Status: "completed"},
		{Role: domain.ChatMessageRoleAssistantProgress, Content: "I will inspect memory.", Status: "completed"},
		{Role: "tool", ToolName: "ssh_exec", Content: `{"status":"completed","stdout":"memory is stable"}`, Status: "completed"},
		{Role: "reasoning", Content: "The result confirms memory is healthy.", Status: "completed"},
		{Role: "assistant", Content: "Memory is healthy.", Status: "completed"},
	}
	messages, stats := buildModelContext(history, "continue")
	if len(messages) != 3 {
		t.Fatalf("model messages = %#v", messages)
	}
	assistant := messages[1]
	for _, expected := range []string{"I will inspect memory.", "memory is stable", "Memory is healthy."} {
		if !strings.Contains(assistant.Content, expected) {
			t.Fatalf("assistant context omitted %q: %#v", expected, assistant)
		}
	}
	for _, expected := range []string{"I should inspect memory before answering.", "The result confirms memory is healthy."} {
		if !strings.Contains(assistant.ReasoningContent, expected) {
			t.Fatalf("reasoning context omitted %q: %#v", expected, assistant)
		}
	}
	if stats.ToolResults != 1 || stats.Bytes < len(assistant.Content)+len(assistant.ReasoningContent) {
		t.Fatalf("context stats = %#v", stats)
	}
}

func TestBuildAnthropicContextPreservesEverySignedToolReasoningSegment(t *testing.T) {
	history := []domain.ChatMessage{
		{Role: "user", Content: "inspect host", Status: "completed"},
		{Role: "reasoning", Content: "First inspection.", ModelExtra: map[string]any{
			claudeThinkingExtraKey: "First inspection.", claudeSignatureExtraKey: "signature-one",
		}, Status: "completed"},
		{Role: domain.ChatMessageRoleAssistantProgress, Content: "Checking memory.", Status: "completed"},
		{Role: "tool", ToolName: "ssh_exec", Content: `{"status":"completed","stdout":"stable"}`, Status: "completed"},
		{Role: "reasoning", Content: "Interpret result.", ModelExtra: map[string]any{
			claudeThinkingExtraKey: "Interpret result.", claudeSignatureExtraKey: "signature-two",
		}, Status: "completed"},
		{Role: "assistant", Content: "Memory is healthy.", Status: "completed"},
	}
	messages, _ := buildMultimodalModelContextForProvider(history, domain.ChatMessage{Role: "user", Content: "continue"}, "anthropic")
	if len(messages) != 5 {
		t.Fatalf("Anthropic model messages = %#v", messages)
	}
	for index, expected := range []struct {
		content   string
		signature string
	}{{"First inspection.", "signature-one"}, {"Interpret result.", "signature-two"}} {
		message := messages[index+1]
		if message.ReasoningContent != expected.content || message.Extra[claudeSignatureExtraKey] != expected.signature {
			t.Fatalf("Anthropic reasoning message %d = %#v", index, message)
		}
	}
	if !strings.Contains(messages[3].Content, "Checking memory.") || !strings.Contains(messages[3].Content, "stable") || !strings.Contains(messages[3].Content, "Memory is healthy.") {
		t.Fatalf("Anthropic assistant context = %#v", messages[3])
	}
}

func TestBuildModelContextExcludesUIToolDisplayMetadata(t *testing.T) {
	history := []domain.ChatMessage{
		{Role: "user", Content: "inspect host", Status: "completed"},
		{Role: "tool", ToolName: "ssh_host_inspect", Content: `{"status":"completed","hostname":"demo","_display":{"arguments":{"host_id":"host-demo"},"request":{"host_id":"host-demo","program":"uname"}}}`, Status: "completed"},
	}
	messages, _ := buildModelContext(history, "continue")
	if len(messages) != 3 || strings.Contains(messages[1].Content, "_display") || strings.Contains(messages[1].Content, "arguments") || !strings.Contains(messages[1].Content, `"hostname":"demo"`) {
		t.Fatalf("UI-only Tool display metadata leaked into model context: %#v", messages)
	}
}

func TestBuildModelContextExcludesCommandExplainerDataFromStoredSSHHistory(t *testing.T) {
	history := []domain.ChatMessage{
		{Role: "user", Content: "inspect prior commands", Status: "completed"},
		{Role: "tool", ToolName: "ssh_history", Content: `{"runs":[{"id":"run-demo","request_json":"{\"program\":\"uname\"}","stdout_redacted":"Linux","ai_review":{"status":"completed","model":"PRIVATE_REVIEW_MODEL","explanation":{"summary":"PRIVATE_REVIEW_SUMMARY","mechanism":"PRIVATE_REVIEW_MECHANISM","risks":["PRIVATE_REVIEW_RISK"]}}}],"_display":{"arguments":{"query":"uname"}}}`, Status: "completed"},
	}
	messages, _ := buildModelContext(history, "continue")
	if len(messages) != 3 {
		t.Fatalf("model messages = %#v", messages)
	}
	results := messages[1].Content
	for _, leaked := range []string{"ai_review", "PRIVATE_REVIEW_MODEL", "PRIVATE_REVIEW_SUMMARY", "PRIVATE_REVIEW_MECHANISM", "PRIVATE_REVIEW_RISK", "_display"} {
		if strings.Contains(results, leaked) {
			t.Fatalf("stored SSH history leaked private metadata %q: %s", leaked, results)
		}
	}
	for _, retained := range []string{"run-demo", `\"program\":\"uname\"`, "Linux"} {
		if !strings.Contains(results, retained) {
			t.Fatalf("stored SSH history lost operational field %q: %s", retained, results)
		}
	}
}

func TestBuildModelContextExcludesFailedTurnWithoutActivity(t *testing.T) {
	history := []domain.ChatMessage{
		{Role: "user", Content: "request that never reached the model", Status: "failed"},
		{Role: "user", Content: "successful request", Status: "completed"},
		{Role: "assistant", Content: "successful response", Status: "completed"},
	}
	messages, _ := buildModelContext(history, "next")
	if len(messages) != 3 || messages[0].Content != "successful request" || messages[1].Content != "successful response" || messages[2].Content != "next" {
		t.Fatalf("model messages = %#v", messages)
	}
}

func TestBuildMultimodalModelContextIncludesAllImages(t *testing.T) {
	historyImage := []byte("history-image")
	currentImageOne := []byte("current-image-one")
	currentImageTwo := []byte("current-image-two")
	history := []domain.ChatMessage{
		{Role: "user", Content: "previous screenshot", Status: "completed", Attachments: []domain.ChatAttachment{{Name: "previous.png", MIMEType: "image/png", Data: historyImage}}},
		{Role: "assistant", Content: "I can see it", Status: "completed"},
	}
	current := domain.ChatMessage{Role: "user", Content: "compare these", Attachments: []domain.ChatAttachment{
		{Name: "one.jpg", MIMEType: "image/jpeg", Data: currentImageOne},
		{Name: "two.webp", MIMEType: "image/webp", Data: currentImageTwo},
	}}
	messages, stats := buildMultimodalModelContext(history, current)
	if len(messages) != 3 || len(messages[0].UserInputMultiContent) != 2 || len(messages[2].UserInputMultiContent) != 3 {
		t.Fatalf("multimodal messages = %#v", messages)
	}
	if messages[0].UserInputMultiContent[0].Text != "previous screenshot" || messages[2].UserInputMultiContent[0].Text != "compare these" {
		t.Fatalf("multimodal text parts = %#v", messages)
	}
	wantImages := [][]byte{historyImage, currentImageOne, currentImageTwo}
	imageParts := []schema.MessageInputPart{
		messages[0].UserInputMultiContent[1], messages[2].UserInputMultiContent[1], messages[2].UserInputMultiContent[2],
	}
	for index, part := range imageParts {
		if part.Type != schema.ChatMessagePartTypeImageURL || part.Image == nil || part.Image.Base64Data == nil {
			t.Fatalf("image part %d = %#v", index, part)
		}
		decoded, err := base64.StdEncoding.DecodeString(*part.Image.Base64Data)
		if err != nil || string(decoded) != string(wantImages[index]) {
			t.Fatalf("image part %d data = %q, err = %v", index, decoded, err)
		}
	}
	if stats.Images != 3 || stats.ImageBytes != int64(len(historyImage)+len(currentImageOne)+len(currentImageTwo)) {
		t.Fatalf("multimodal context stats = %#v", stats)
	}
}

func TestToolHistoryIsEnrichedWithCompleteAuditedCommand(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/runtime.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.UpsertHost(ctx, domain.Host{ID: "host_display", Name: "display", Address: "127.0.0.1", Port: 22, User: "ops", AuthType: "agent", SudoMode: "none"}); err != nil {
		t.Fatal(err)
	}
	run := domain.Run{
		ID: "run_display", HostID: "host_display", Status: "completed",
		RequestJSON:   `{"host_id":"host_display","mode":"program","program":"journalctl","args":["-u","demo service","-n","100"],"cwd":"/srv/demo","reason":"inspect logs"}`,
		RequestDigest: "digest", StartedAt: time.Now().UTC(), CompletedAt: time.Now().UTC(),
	}
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{store: st}
	enriched := runtime.enrichToolContent(ctx, `{"run_id":"run_display","status":"completed"}`)
	var payload struct {
		Display struct {
			HostID  string         `json:"host_id"`
			Request map[string]any `json:"request"`
		} `json:"_display"`
	}
	if err := json.Unmarshal([]byte(enriched), &payload); err != nil {
		t.Fatal(err)
	}
	args, _ := payload.Display.Request["args"].([]any)
	if payload.Display.HostID != "host_display" || payload.Display.Request["program"] != "journalctl" || len(args) != 4 || args[1] != "demo service" {
		t.Fatalf("complete command was not preserved in Tool display payload: %s", enriched)
	}
	nested := runtime.enrichToolContent(ctx, `{"task":{"id":"task_display","run_id":"run_display","status":"failed"},"result":{"run_id":"run_display","status":"failed","stderr":"command failed"}}`)
	if !strings.Contains(nested, `"_display"`) || !strings.Contains(nested, `"stderr":"command failed"`) {
		t.Fatalf("nested task result was not enriched without losing stderr: %s", nested)
	}
	failedBeforeRun := runtime.enrichToolContent(ctx, `{"ok":false,"status":"failed","message":"host is unavailable"}`, &capturedToolCall{
		Name:      "ssh_host_inspect",
		Arguments: `{"host_id":"host_display"}`,
	})
	if !strings.Contains(failedBeforeRun, `"arguments":{"host_id":"host_display"}`) || !strings.Contains(failedBeforeRun, `"tool_name":"ssh_host_inspect"`) {
		t.Fatalf("tool call arguments were not preserved before an audit run existed: %s", failedBeforeRun)
	}
	workspaceCall := runtime.enrichToolContent(ctx, `{"ok":false,"status":"failed"}`, &capturedToolCall{
		Name:      "workspace_file_read",
		Arguments: `{"path":"README.md"}`,
		Workspace: "workspace-demo",
	})
	if !strings.Contains(workspaceCall, `"workspace_id":"workspace-demo"`) {
		t.Fatalf("conversation workspace target was not preserved: %s", workspaceCall)
	}
	if err := st.WriteAgentTaskFile(ctx, "session-tasks", "agent-tasks/1.json", `{"id":"1","subject":"Inspect","description":"Inspect the service","status":"in_progress","blocks":[],"blockedBy":[]}`); err != nil {
		t.Fatal(err)
	}
	taskCall := runtime.enrichToolContent(service.WithSessionID(ctx, "session-tasks"), `{"result":"Task list"}`, &capturedToolCall{
		Name: "TaskList", Arguments: `{}`,
	})
	var taskPayload struct {
		Tasks domain.AgentTaskList `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(taskCall), &taskPayload); err != nil {
		t.Fatal(err)
	}
	if len(taskPayload.Tasks.Items) != 1 || taskPayload.Tasks.Items[0].Subject != "Inspect" {
		t.Fatalf("task state was not attached to the Tool display payload: %s", taskCall)
	}
}
