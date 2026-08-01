package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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

func (*toolActivityAgentRunner) Run(ctx context.Context, _ []*schema.Message, _ ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	iterator, generator := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	go func() {
		call := schema.ToolCall{
			ID: "call-live", Type: "function",
			Function: schema.FunctionCall{Name: "ssh_exec", Arguments: `{"host_id":"host-live","program":"uptime","reason":"inspect uptime"}`},
		}
		generator.Send(adk.EventFromMessage(schema.AssistantMessage("", []schema.ToolCall{call}), nil, schema.Assistant, ""))
		notifyToolActivity(ctx, &compose.ToolInput{CallID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments})
		generator.Send(adk.EventFromMessage(
			schema.ToolMessage(`{"status":"completed","stdout":"up 1 day"}`, call.ID, schema.WithToolName(call.Function.Name)),
			nil, schema.Tool, call.Function.Name,
		))
		generator.Send(adk.EventFromMessage(schema.AssistantMessage("Host is available.", nil), nil, schema.Assistant, ""))
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
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if len(request.Messages) != 1 || request.Messages[0].Content != "Hello" {
			t.Fatalf("unexpected test prompt %#v", request.Messages)
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
		APIKey: "fixture-key", BaseURL: server.URL + "/v1", Name: "fixture-model",
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
		APIKey: apiKey, Kind: "anthropic", BaseURL: server.URL, Name: "claude-fixture", UserAgent: userAgent,
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
		requests <- request.Messages
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-prompt\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"fixture-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"done\"},\"finish_reason\":null}]}\n\n"))
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
	cfg.Model = config.Model{APIKey: "fixture-key", BaseURL: server.URL + "/v1", Name: "fixture-model"}
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
	if _, err := runtime.Query(ctx, "same_session", "first turn", nil); err != nil {
		t.Fatal(err)
	}
	assertSystemPrompt := func(want string) {
		t.Helper()
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
	if err := runtime.Reload(ctx); err != nil {
		t.Fatal(err)
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
	select {
	case messages := <-requests:
		for _, message := range messages {
			if message.Role == "system" && message.Content != "" {
				t.Fatalf("empty prompt fell back to non-empty system prompt: %q", message.Content)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for model request using empty prompt")
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

func TestQueryInjectsPersistedPlanBeforeCurrentUser(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/runtime.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, err = st.ReplaceAgentPlan(ctx, domain.AgentPlan{
		SessionID: "session_plan_context",
		Goal:      "Repair the API",
		Status:    "active",
		Steps: []domain.AgentPlanStep{
			{Number: 1, Title: "Inspect logs", Status: "completed", Evidence: "timeout observed"},
			{Number: 2, Title: "Fix timeout", Status: "in_progress"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := &scriptedAgentRunner{attempts: [][]*adk.AgentEvent{
		{adk.EventFromMessage(schema.AssistantMessage("continuing the current step", nil), nil, schema.Assistant, "")},
	}}
	runtime := &Runtime{runner: runner, store: st}
	if _, err := runtime.Query(ctx, "session_plan_context", "continue", nil); err != nil {
		t.Fatal(err)
	}
	_, inputs := runner.snapshot()
	if len(inputs) != 1 || len(inputs[0]) != 2 {
		t.Fatalf("model inputs = %#v", inputs)
	}
	planMessage, userMessage := inputs[0][0], inputs[0][1]
	if planMessage.Role != schema.System || !strings.Contains(planMessage.Content, "Repair the API") || !strings.Contains(planMessage.Content, `"status":"in_progress"`) || !strings.Contains(planMessage.Content, "untrusted data") {
		t.Fatalf("plan context = %#v", planMessage)
	}
	if strings.Contains(planMessage.Content, "session_plan_context") {
		t.Fatalf("plan context exposed the internal session id: %s", planMessage.Content)
	}
	if userMessage.Role != schema.User || userMessage.Content != "continue" {
		t.Fatalf("current user message = %#v", userMessage)
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
	if _, err := st.ReplaceAgentPlan(ctx, domain.AgentPlan{
		SessionID: "session_safe_finalizer",
		Goal:      "Deploy the service",
		Status:    "completed",
		Steps: []domain.AgentPlanStep{
			{Number: 1, Title: "Deploy", Status: "completed", Evidence: "service is active"},
		},
	}); err != nil {
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
	if input.Request != "部署服务" || input.Plan == nil || input.Plan.Status != "completed" || len(input.ToolResults) != 1 || input.ToolResults[0].ToolName != "ssh_exec" || !strings.Contains(input.ToolResults[0].Content, "active") {
		t.Fatalf("final answer context = %#v", input)
	}
	messages, err := st.ListChatMessages(ctx, "session_safe_finalizer", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 || messages[0].Status != "completed" || messages[1].Role != "tool" || messages[2].Role != "assistant" || messages[2].Content != answer {
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

func TestQueryBlocksPersistedToolEvidenceLeakAndUsesFinalizer(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/runtime.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	leaked := "temporary preamble\n" + persistedToolEvidenceHeader + "\n\nTool: ssh_shell\nResult:\n{\"status\":\"completed\",\"stdout\":\"private-shell-output\"}\n\n" + persistedToolEvidenceTrailer
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
	runnerCalls, _ := runner.snapshot()
	finalizerCalls, _ := finalizer.snapshot()
	if runnerCalls != 1 || finalizerCalls != 1 {
		t.Fatalf("calls: runner=%d finalizer=%d", runnerCalls, finalizerCalls)
	}
	messages, err := st.ListChatMessages(ctx, "session_context_leak", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 || messages[0].Status != "completed" || messages[1].Role != "tool" || messages[2].Role != "assistant" || messages[2].Content != answer {
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

func TestQueryPersistsOnlyTerminalAssistantOutput(t *testing.T) {
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
	if len(messages) != 3 || messages[0].Status != "completed" || messages[1].Role != "tool" || messages[2].Role != "assistant" || messages[2].Content != answer {
		t.Fatalf("stored messages = %#v", messages)
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
	if len(messages) != 3 || messages[0].Role != "user" || messages[0].Status != "failed" || messages[1].Role != "tool" || messages[2].Role != "reasoning" {
		t.Fatalf("stored messages = %#v", messages)
	}
	for _, event := range emitted {
		if event.Type == "done" {
			t.Fatalf("reasoning-only query emitted done: %#v", emitted)
		}
	}
}

func TestQueryStreamingPersistsOnlyTerminalAssistantOutput(t *testing.T) {
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

	answer, err := runtime.Query(ctx, "session_stream_terminal", "inspect host", nil)
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
	if len(messages) != 4 || messages[0].Status != "completed" || messages[1].Role != "tool" || messages[2].Role != "reasoning" || messages[3].Role != "assistant" || messages[3].Content != answer {
		t.Fatalf("stored messages = %#v", messages)
	}
}

func TestNextQueryReceivesToolEvidenceFromFailedTurn(t *testing.T) {
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
	if inputs[1][1].Role != schema.Assistant || !strings.Contains(inputs[1][1].Content, "Persisted operational tool evidence") || !strings.Contains(inputs[1][1].Content, "disk is healthy") {
		t.Fatalf("failed turn tool context = %#v", inputs[1][1])
	}
	if inputs[1][2].Role != schema.User || inputs[1][2].Content != "continue" {
		t.Fatalf("current query context = %#v", inputs[1][2])
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
		t.Fatalf("tool evidence was not retained: %#v", messages)
	}
	if messages[5].Content != incompleteTurnContext {
		t.Fatalf("incomplete turn marker = %q", messages[5].Content)
	}
	if stats.StoredTurns != 3 || stats.IncludedTurns != 3 || stats.ToolResults != 2 {
		t.Fatalf("context stats = %#v", stats)
	}
}

func TestBuildModelContextPreservesCompleteToolEvidence(t *testing.T) {
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
	evidence := messages[1].Content
	for _, expected := range []string{first, second} {
		if !strings.Contains(evidence, expected) {
			t.Fatalf("complete tool evidence was not preserved: evidence_bytes=%d expected_bytes=%d", len(evidence), len(expected))
		}
	}
	if stats.ToolResults != 2 || stats.IncludedTurns != 1 {
		t.Fatalf("context stats = %#v", stats)
	}
}

func TestBuildModelContextUsesFinalAnswerInsteadOfRepeatingCompletedToolResults(t *testing.T) {
	history := []domain.ChatMessage{
		{Role: "user", Content: "inspect host", Status: "completed"},
		{Role: "tool", ToolName: "ssh_exec", Content: `{"status":"completed","stdout":"complete output"}`, Status: "completed"},
		{Role: "assistant", Content: "The host is healthy.", Status: "completed"},
	}
	messages, stats := buildModelContext(history, "continue")
	if len(messages) != 3 || messages[1].Role != schema.Assistant || messages[1].Content != "The host is healthy." {
		t.Fatalf("model messages = %#v", messages)
	}
	if strings.Contains(messages[1].Content, "complete output") || containsInternalContextMarker(messages[1].Content) {
		t.Fatalf("completed Tool results were repeated in Assistant context: %q", messages[1].Content)
	}
	if stats.ToolResults != 0 {
		t.Fatalf("context stats = %#v", stats)
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
	evidence := messages[1].Content
	for _, leaked := range []string{"ai_review", "PRIVATE_REVIEW_MODEL", "PRIVATE_REVIEW_SUMMARY", "PRIVATE_REVIEW_MECHANISM", "PRIVATE_REVIEW_RISK", "_display"} {
		if strings.Contains(evidence, leaked) {
			t.Fatalf("stored SSH history leaked private metadata %q: %s", leaked, evidence)
		}
	}
	for _, retained := range []string{"run-demo", `\"program\":\"uname\"`, "Linux"} {
		if !strings.Contains(evidence, retained) {
			t.Fatalf("stored SSH history lost operational field %q: %s", retained, evidence)
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
}
