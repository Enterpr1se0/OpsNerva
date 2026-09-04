package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Enterpr1se0/opsnerva/internal/agent"
	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/observability"
	"github.com/Enterpr1se0/opsnerva/internal/security"
	"github.com/Enterpr1se0/opsnerva/internal/service"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"
	"github.com/Enterpr1se0/opsnerva/internal/store"

	"golang.org/x/net/websocket"
)

func TestApplicationWebSocketSubscribesThroughRequestLogger(t *testing.T) {
	if err := observability.Configure(config.Logging{Level: "debug", Format: "text", File: "-", RecentLimit: 100}); err != nil {
		t.Fatal(err)
	}
	server := &Server{}
	httpServer := httptest.NewServer(requestLogMiddleware(http.HandlerFunc(server.applicationWebSocket), nil))
	defer httpServer.Close()
	webSocketConfig, err := websocket.NewConfig("ws"+strings.TrimPrefix(httpServer.URL, "http"), httpServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := websocket.DialConfig(webSocketConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := websocket.JSON.Send(connection, applicationWebSocketCommand{Type: "subscribe", Topics: []string{"logs"}, Logs: applicationWebSocketLogFilter{Limit: 20}}); err != nil {
		t.Fatal(err)
	}
	var event applicationWebSocketEvent
	if err := websocket.JSON.Receive(connection, &event); err != nil {
		t.Fatal(err)
	}
	if event.Type != "event" || event.Topic != "logs" || event.Mode != "snapshot" || event.Sequence != 1 {
		t.Fatalf("unexpected subscription event: %#v", event)
	}
	slog.Info("live websocket log", "component", "test")
	if err := websocket.JSON.Receive(connection, &event); err != nil {
		t.Fatal(err)
	}
	var update applicationLogResponse
	if err := json.Unmarshal(event.Data, &update); err != nil {
		t.Fatal(err)
	}
	if event.Topic != "logs" || event.Mode != "delta" || len(update.Entries) != 1 || update.Entries[0].Message != "live websocket log" {
		t.Fatalf("unexpected live log event: event=%#v payload=%#v", event, update)
	}
}

func TestApplicationControlTopicsAreEventDriven(t *testing.T) {
	for _, topic := range []string{"approvals", "sessions", "chat_state", "audit"} {
		if _, sampled := applicationSampleIntervals[topic]; sampled {
			t.Errorf("control topic %q still has a polling interval", topic)
		}
		if _, eventDriven := applicationStateTopics[topic]; !eventDriven {
			t.Errorf("control topic %q is not event driven", topic)
		}
	}
}

func TestApplicationLongRunningTopicsAreEventDriven(t *testing.T) {
	for _, topic := range []string{"logs", "model_tests"} {
		if _, sampled := applicationSampleIntervals[topic]; sampled {
			t.Errorf("push topic %q still has a polling interval", topic)
		}
		if _, eventDriven := applicationPushTopics[topic]; !eventDriven {
			t.Errorf("push topic %q is not event driven", topic)
		}
	}
}

func TestApplicationWebSocketPushesModelTestCompletion(t *testing.T) {
	jobs := newModelTestJobs()
	release := make(chan struct{})
	job := jobs.start(context.Background(), config.Model{}, modelTestIdentity{}, func(context.Context, config.Model) (agent.TestResult, error) {
		<-release
		return agent.TestResult{Model: "test-model", Response: "Hello", LatencyMS: 12}, nil
	})
	server := &Server{modelTests: jobs}
	httpServer := httptest.NewServer(http.HandlerFunc(server.applicationWebSocket))
	defer httpServer.Close()
	webSocketConfig, err := websocket.NewConfig("ws"+strings.TrimPrefix(httpServer.URL, "http"), httpServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := websocket.DialConfig(webSocketConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := websocket.JSON.Send(connection, applicationWebSocketCommand{Type: "subscribe", Topics: []string{"model_tests"}, ModelTestIDs: []string{job.ID}}); err != nil {
		t.Fatal(err)
	}
	snapshot := receiveApplicationEvent(t, connection)
	var jobsByID map[string]modelTestJob
	if err := json.Unmarshal(snapshot.Data, &jobsByID); err != nil {
		t.Fatal(err)
	}
	if snapshot.Topic != "model_tests" || snapshot.Mode != "snapshot" || jobsByID[job.ID].Status != "running" {
		t.Fatalf("unexpected model test snapshot: event=%#v payload=%#v", snapshot, jobsByID)
	}
	close(release)
	delta := receiveApplicationEvent(t, connection)
	var completed modelTestJob
	if err := json.Unmarshal(delta.Data, &completed); err != nil {
		t.Fatal(err)
	}
	if delta.Topic != "model_tests" || delta.Mode != "delta" || completed.ID != job.ID || completed.Status != "completed" || completed.Result == nil {
		t.Fatalf("unexpected model test delta: event=%#v payload=%#v", delta, completed)
	}
}

func openApplicationStateSocket(t *testing.T) (context.Context, *store.Store, *websocket.Conn) {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dataDir, "application-state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.Default()
	svc := service.New(st, nil, nil, nil, cfg.Limits, cfg)
	t.Cleanup(func() { _ = svc.Shutdown(context.Background()) })
	server := &Server{service: svc, chatQueue: newChatMessageQueue(svc.PublishChatState)}
	httpServer := httptest.NewServer(requestLogMiddleware(http.HandlerFunc(server.applicationWebSocket), nil))
	t.Cleanup(httpServer.Close)
	webSocketConfig, err := websocket.NewConfig("ws"+strings.TrimPrefix(httpServer.URL, "http"), httpServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := websocket.DialConfig(webSocketConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	if err := connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return ctx, st, connection
}

func receiveApplicationEvent(t *testing.T, connection *websocket.Conn) applicationWebSocketEvent {
	t.Helper()
	var event applicationWebSocketEvent
	if err := websocket.JSON.Receive(connection, &event); err != nil {
		t.Fatal(err)
	}
	return event
}

func TestApplicationSessionsDeltaUsesExposedFields(t *testing.T) {
	now := time.Now().UTC()
	previous := []domain.ChatSession{
		{ID: "session-one", Title: "One", UpdatedAt: now},
		{ID: "session-two", Title: "Two", UpdatedAt: now.Add(-time.Minute)},
	}
	current := []domain.ChatSession{
		{ID: "session-one", Title: "Renamed", TitleSet: true, UpdatedAt: now},
		{ID: "session-three", Title: "Three", UpdatedAt: now.Add(-30 * time.Second)},
	}
	delta, changed := applicationSessionsDelta(previous, current)
	if !changed || len(delta.Sessions) != 2 || delta.Sessions[0].ID != "session-one" || delta.Sessions[1].ID != "session-three" {
		t.Fatalf("unexpected session upserts: %#v", delta)
	}
	if len(delta.RemovedIDs) != 1 || delta.RemovedIDs[0] != "session-two" {
		t.Fatalf("unexpected removed sessions: %#v", delta.RemovedIDs)
	}
	current[0].Title = previous[0].Title
	current[1] = previous[1]
	if delta, changed := applicationSessionsDelta(previous, current); changed {
		t.Fatalf("non-serialized TitleSet produced a delta: %#v", delta)
	}
}

func TestApplicationObjectDeltaOnlyIncludesChangedFields(t *testing.T) {
	previous := []byte(`{"active":true,"context_tokens":10,"context_summary":{"revision":1}}`)
	current := []byte(`{"active":false,"context_tokens":10}`)
	payload, changed, err := applicationObjectDelta(previous, current)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected object delta")
	}
	var delta map[string]json.RawMessage
	if err := json.Unmarshal(payload, &delta); err != nil {
		t.Fatal(err)
	}
	if string(delta["active"]) != "false" || string(delta["context_summary"]) != "null" {
		t.Fatalf("unexpected object delta: %s", payload)
	}
	if _, exists := delta["context_tokens"]; exists {
		t.Fatalf("unchanged field included in object delta: %s", payload)
	}
}

func TestApplicationWebSocketPushesSessionDelta(t *testing.T) {
	ctx, st, connection := openApplicationStateSocket(t)
	if err := websocket.JSON.Send(connection, applicationWebSocketCommand{Type: "subscribe", Topics: []string{"sessions"}}); err != nil {
		t.Fatal(err)
	}
	snapshot := receiveApplicationEvent(t, connection)
	if snapshot.Topic != "sessions" || snapshot.Mode != "snapshot" {
		t.Fatalf("unexpected session snapshot: %#v", snapshot)
	}
	if _, err := st.CreateChatSession(ctx, "session-delta", ""); err != nil {
		t.Fatal(err)
	}
	update := receiveApplicationEvent(t, connection)
	var added applicationSessionDelta
	if err := json.Unmarshal(update.Data, &added); err != nil {
		t.Fatal(err)
	}
	if update.Topic != "sessions" || update.Mode != "delta" || len(added.Sessions) != 1 || added.Sessions[0].ID != "session-delta" || len(added.RemovedIDs) != 0 {
		t.Fatalf("unexpected session create delta: event=%#v payload=%#v", update, added)
	}
	if err := st.DeleteChatSession(ctx, "session-delta"); err != nil {
		t.Fatal(err)
	}
	update = receiveApplicationEvent(t, connection)
	var removed applicationSessionDelta
	if err := json.Unmarshal(update.Data, &removed); err != nil {
		t.Fatal(err)
	}
	if update.Topic != "sessions" || update.Mode != "delta" || len(removed.Sessions) != 0 || len(removed.RemovedIDs) != 1 || removed.RemovedIDs[0] != "session-delta" {
		t.Fatalf("unexpected session delete delta: event=%#v payload=%#v", update, removed)
	}
}

func TestApplicationWebSocketPushesChatStateFieldDelta(t *testing.T) {
	ctx, st, connection := openApplicationStateSocket(t)
	const sessionID = "session-chat-state-delta"
	if _, err := st.CreateChatSession(ctx, sessionID, ""); err != nil {
		t.Fatal(err)
	}
	if err := websocket.JSON.Send(connection, applicationWebSocketCommand{Type: "subscribe", Topics: []string{"chat_state"}, SessionID: sessionID}); err != nil {
		t.Fatal(err)
	}
	snapshot := receiveApplicationEvent(t, connection)
	if snapshot.Topic != "chat_state" || snapshot.Mode != "snapshot" {
		t.Fatalf("unexpected chat state snapshot: %#v", snapshot)
	}
	if err := st.SetChatSessionContextUsage(ctx, sessionID, 2048, 8192); err != nil {
		t.Fatal(err)
	}
	update := receiveApplicationEvent(t, connection)
	var delta map[string]json.RawMessage
	if err := json.Unmarshal(update.Data, &delta); err != nil {
		t.Fatal(err)
	}
	if update.Topic != "chat_state" || update.Mode != "delta" || string(delta["context_tokens"]) != "2048" || string(delta["context_window"]) != "8192" {
		t.Fatalf("unexpected chat state delta: event=%#v payload=%s", update, update.Data)
	}
	for _, field := range []string{"active", "tasks", "queued_messages", "running_tool_calls", "workspace_id"} {
		if _, exists := delta[field]; exists {
			t.Errorf("unchanged field %q included in chat state delta: %s", field, update.Data)
		}
	}
}

func TestApplicationWebSocketPreservesUnchangedTopicState(t *testing.T) {
	_, _, connection := openApplicationStateSocket(t)
	if err := websocket.JSON.Send(connection, applicationWebSocketCommand{Type: "subscribe", Topics: []string{"sessions"}}); err != nil {
		t.Fatal(err)
	}
	if snapshot := receiveApplicationEvent(t, connection); snapshot.Topic != "sessions" || snapshot.Mode != "snapshot" {
		t.Fatalf("unexpected initial snapshot: %#v", snapshot)
	}
	if err := websocket.JSON.Send(connection, applicationWebSocketCommand{Type: "subscribe", Topics: []string{"sessions", "health"}}); err != nil {
		t.Fatal(err)
	}
	if snapshot := receiveApplicationEvent(t, connection); snapshot.Topic != "health" || snapshot.Mode != "snapshot" {
		t.Fatalf("unexpected added-topic snapshot: %#v", snapshot)
	}
	if err := connection.SetReadDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var repeated applicationWebSocketEvent
	err := websocket.JSON.Receive(connection, &repeated)
	if err == nil {
		t.Fatalf("unchanged sessions topic was snapshotted again: %#v", repeated)
	}
	if networkError, ok := err.(net.Error); !ok || !networkError.Timeout() {
		t.Fatalf("expected read timeout after changed-topic snapshot, got %v", err)
	}
}

func TestApplicationWebSocketStreamsAuditAfterCommittedWrite(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dataDir, "audit-events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.Default()
	svc := service.New(st, nil, nil, nil, cfg.Limits, cfg)
	t.Cleanup(func() { _ = svc.Shutdown(context.Background()) })
	server := &Server{service: svc}
	httpServer := httptest.NewServer(requestLogMiddleware(http.HandlerFunc(server.applicationWebSocket), nil))
	defer httpServer.Close()
	webSocketConfig, err := websocket.NewConfig("ws"+strings.TrimPrefix(httpServer.URL, "http"), httpServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := websocket.DialConfig(webSocketConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := websocket.JSON.Send(connection, applicationWebSocketCommand{Type: "subscribe", Topics: []string{"audit"}}); err != nil {
		t.Fatal(err)
	}
	var snapshot applicationWebSocketEvent
	if err := websocket.JSON.Receive(connection, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Topic != "audit" || snapshot.Mode != "snapshot" {
		t.Fatalf("unexpected audit snapshot: %#v", snapshot)
	}
	if err := st.AppendAudit(ctx, domain.AuditEvent{ID: "event-websocket", Type: "test", Actor: "test", Data: map[string]any{"scope": "session"}}); err != nil {
		t.Fatal(err)
	}
	var update applicationWebSocketEvent
	if err := websocket.JSON.Receive(connection, &update); err != nil {
		t.Fatal(err)
	}
	var audit map[string]any
	if err := json.Unmarshal(update.Data, &audit); err != nil {
		t.Fatal(err)
	}
	auditData, _ := audit["data"].(map[string]any)
	if update.Topic != "audit" || update.Mode != "delta" || audit["id"] != "event-websocket" || auditData["scope"] != "session" {
		t.Fatalf("unexpected audit update: %#v payload=%#v", update, audit)
	}

	now := time.Now().UTC()
	host, err := st.UpsertHost(ctx, domain.Host{ID: "host-audit-websocket", Name: "audit-websocket", Address: "192.0.2.30", Port: 22, User: "ops", AuthType: "agent", SudoMode: "none", CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "session-audit-websocket"
	if err := st.CreateRun(ctx, domain.Run{ID: "run-audit-websocket", SessionID: sessionID, HostID: host.ID, RequestJSON: `{}`, RequestDigest: "audit-websocket", Status: "completed", StartedAt: now, CompletedAt: now}); err != nil {
		t.Fatal(err)
	}
	deleteSessionID := sessionID
	if _, err := st.DeleteAuditRuns(ctx, &deleteSessionID, "test"); err != nil {
		t.Fatal(err)
	}
	deletion := receiveApplicationEvent(t, connection)
	var deletionEvent applicationAuditEvent
	if err := json.Unmarshal(deletion.Data, &deletionEvent); err != nil {
		t.Fatal(err)
	}
	if deletion.Topic != "audit" || deletion.Mode != "delta" || deletionEvent.Type != "audit_records_deleted" || deletionEvent.Data["scope"] != "session" || deletionEvent.Data["session_id"] != sessionID || deletionEvent.Data["deleted"] != float64(1) {
		t.Fatalf("unexpected audit deletion delta: event=%#v payload=%#v", deletion, deletionEvent)
	}
}

func TestApplicationWebSocketStreamsMCPActivityAfterSnapshot(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dataDir, "mcp-events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	encryptor, err := security.NewEncryptor("", dataDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	svc := service.New(st, nil, encryptor, security.NewRedactor(), cfg.Limits, cfg)
	t.Cleanup(func() { _ = svc.Shutdown(context.Background()) })
	server := &Server{service: svc}
	httpServer := httptest.NewServer(requestLogMiddleware(http.HandlerFunc(server.applicationWebSocket), nil))
	defer httpServer.Close()
	webSocketConfig, err := websocket.NewConfig("ws"+strings.TrimPrefix(httpServer.URL, "http"), httpServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := websocket.DialConfig(webSocketConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := websocket.JSON.Send(connection, applicationWebSocketCommand{Type: "subscribe", Topics: []string{"mcp_activity"}}); err != nil {
		t.Fatal(err)
	}
	var snapshot applicationWebSocketEvent
	if err := websocket.JSON.Receive(connection, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Topic != "mcp_activity" || snapshot.Mode != "snapshot" {
		t.Fatalf("unexpected MCP activity snapshot: %#v", snapshot)
	}
	_, call, err := svc.BeginMCPToolCall(ctx, domain.MCPClientSession{ID: "mcp_sess_ws", Transport: "streamable_http"}, "ssh_history", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	var delta applicationWebSocketEvent
	if err := websocket.JSON.Receive(connection, &delta); err != nil {
		t.Fatal(err)
	}
	var activity domain.MCPActivityEvent
	if err := json.Unmarshal(delta.Data, &activity); err != nil {
		t.Fatal(err)
	}
	if delta.Topic != "mcp_activity" || delta.Mode != "delta" || activity.Type != "call_started" || activity.CallID != call.ID {
		t.Fatalf("unexpected MCP activity delta: %#v, payload %#v", delta, activity)
	}
}

func TestApplicationTaskSnapshotBoundsLiveOutput(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dataDir, "task-events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	host, err := st.UpsertHost(ctx, domain.Host{
		ID: "host-task-events", Name: "task-events", Address: "192.0.2.20", Port: 22,
		User: "ops", AuthType: "agent", SudoMode: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now().UTC()
	task := domain.Task{ID: "task-events", RunID: "run-task-events", SessionID: "session-task-events", HostID: host.ID, Status: "running", Revision: 7, StartedAt: startedAt}
	suffix := "实时输出"
	stdout := "界" + strings.Repeat("x", applicationTaskOutputLimit-2-len(suffix)) + suffix
	if err := st.UpsertTask(ctx, task, domain.ExecResult{RunID: task.RunID, Status: task.Status, Stdout: stdout}, ""); err != nil {
		t.Fatal(err)
	}
	encryptor, err := security.NewEncryptor("", dataDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	svc := service.New(st, nil, encryptor, security.NewRedactor(), cfg.Limits, cfg)
	t.Cleanup(func() { _ = svc.Shutdown(context.Background()) })
	snapshots, _, unsubscribe := svc.SubscribeTaskEvents(ctx, task.SessionID, []string{task.ID})
	defer unsubscribe()
	snapshots = applicationTaskSnapshots(snapshots)
	snapshot := snapshots[task.ID]
	if snapshot.Task.ID != task.ID || snapshot.Task.Status != "running" {
		t.Fatalf("unexpected task snapshot: %#v", snapshot.Task)
	}
	if snapshot.Result.StdoutTotalBytes != len(stdout) || snapshot.Result.StdoutOmittedBytes == 0 || len(snapshot.Result.Stdout) > applicationTaskOutputLimit || !utf8.ValidString(snapshot.Result.Stdout) || !strings.HasSuffix(snapshot.Result.Stdout, suffix) {
		t.Fatalf("unexpected bounded task output: total=%d omitted=%d bytes=%d suffix=%q", snapshot.Result.StdoutTotalBytes, snapshot.Result.StdoutOmittedBytes, len(snapshot.Result.Stdout), snapshot.Result.Stdout)
	}
}

type applicationTaskEventTransport struct {
	started chan struct{}
	emit    chan struct{}
	finish  chan struct{}
	once    sync.Once
}

func (*applicationTaskEventTransport) Probe(context.Context, sshx.ConnectionSpec) (sshx.HostInfo, error) {
	return sshx.HostInfo{Shell: "sh", ShellPath: "/bin/sh"}, nil
}

func (*applicationTaskEventTransport) ScanHostKey(context.Context, sshx.ConnectionSpec) (sshx.HostKey, error) {
	return sshx.HostKey{}, nil
}

func (*applicationTaskEventTransport) TrustHostKey(context.Context, sshx.ConnectionSpec, string) (sshx.HostKey, error) {
	return sshx.HostKey{}, nil
}

func (*applicationTaskEventTransport) StoredHostKey(domain.Host) (sshx.HostKey, bool) {
	return sshx.HostKey{}, false
}

func (transport *applicationTaskEventTransport) Exec(ctx context.Context, _ sshx.ConnectionSpec, _ domain.ExecRequest) (sshx.RawResult, error) {
	select {
	case <-transport.finish:
		return sshx.RawResult{ExitCode: 0, Stdout: []byte("live output"), Duration: time.Millisecond}, nil
	case <-ctx.Done():
		return sshx.RawResult{ExitCode: -1, Duration: time.Millisecond}, ctx.Err()
	}
}

func (transport *applicationTaskEventTransport) ExecStream(ctx context.Context, connection sshx.ConnectionSpec, request domain.ExecRequest, emit func(string, []byte)) (sshx.RawResult, error) {
	transport.once.Do(func() { close(transport.started) })
	select {
	case <-transport.emit:
		emit("stdout", []byte("live output"))
	case <-ctx.Done():
		return sshx.RawResult{ExitCode: -1, Duration: time.Millisecond}, ctx.Err()
	}
	return transport.Exec(ctx, connection, request)
}

func TestApplicationWebSocketPushesTaskOutputDelta(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dataDir, "task-delta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	host, err := st.UpsertHost(ctx, domain.Host{
		ID: "host-task-delta", Name: "task-delta", Address: "192.0.2.21", Port: 22,
		User: "ops", AuthType: "agent", AgentEnabled: true, SudoMode: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	transport := &applicationTaskEventTransport{started: make(chan struct{}), emit: make(chan struct{}), finish: make(chan struct{})}
	encryptor, err := security.NewEncryptor("", dataDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	svc := service.New(st, transport, encryptor, security.NewRedactor(), cfg.Limits, cfg)
	t.Cleanup(func() { _ = svc.Shutdown(context.Background()) })
	const sessionID = "session-task-delta"
	task, err := svc.StartTask(service.WithSessionID(ctx, sessionID), domain.ExecRequest{
		HostID: host.ID, Mode: domain.ExecProgram, Program: "printf", Args: []string{"live output"}, Reason: "test task event delivery",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-transport.started:
	case <-time.After(time.Second):
		t.Fatal("background task did not start")
	}

	server := &Server{service: svc}
	httpServer := httptest.NewServer(requestLogMiddleware(http.HandlerFunc(server.applicationWebSocket), nil))
	defer httpServer.Close()
	webSocketConfig, err := websocket.NewConfig("ws"+strings.TrimPrefix(httpServer.URL, "http"), httpServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := websocket.DialConfig(webSocketConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := websocket.JSON.Send(connection, applicationWebSocketCommand{Type: "subscribe", Topics: []string{"tasks"}, SessionID: sessionID, TaskIDs: []string{task.ID}}); err != nil {
		t.Fatal(err)
	}
	var snapshot applicationWebSocketEvent
	if err := websocket.JSON.Receive(connection, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Topic != "tasks" || snapshot.Mode != "snapshot" {
		t.Fatalf("unexpected task snapshot: %#v", snapshot)
	}
	close(transport.emit)
	close(transport.finish)
	var delta applicationWebSocketEvent
	if err := websocket.JSON.Receive(connection, &delta); err != nil {
		t.Fatal(err)
	}
	var taskEvent domain.TaskEvent
	if err := json.Unmarshal(delta.Data, &taskEvent); err != nil {
		t.Fatal(err)
	}
	if delta.Topic != "tasks" || delta.Mode != "delta" || taskEvent.Type != "output" || taskEvent.TaskID != task.ID || taskEvent.Stream != "stdout" || taskEvent.OffsetBytes != 0 || taskEvent.Content != "live output" {
		t.Fatalf("unexpected task delta: event=%#v payload=%#v", delta, taskEvent)
	}
}
