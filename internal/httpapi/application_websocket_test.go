package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

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
	server := &Server{}
	httpServer := httptest.NewServer(requestLogMiddleware(http.HandlerFunc(server.applicationWebSocket), nil))
	defer httpServer.Close()
	config, err := websocket.NewConfig("ws"+strings.TrimPrefix(httpServer.URL, "http"), httpServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := websocket.DialConfig(config)
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
	if err := st.AppendAudit(ctx, domain.AuditEvent{ID: "event-websocket", Type: "test", Actor: "test"}); err != nil {
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
	if update.Topic != "audit" || update.Mode != "snapshot" || audit["id"] != "event-websocket" {
		t.Fatalf("unexpected audit update: %#v payload=%#v", update, audit)
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

func TestApplicationLogUpdateUsesSnapshotThenDelta(t *testing.T) {
	now := time.Now().UTC()
	older := observability.LogEntry{Time: now.Add(-time.Second), Level: "info", Message: "older"}
	newer := observability.LogEntry{Time: now, Level: "warn", Message: "newer"}
	initial := applicationLogResponse{Entries: []observability.LogEntry{older}, Components: []string{"http"}, MinimumLevel: "debug"}
	mode, payload, changed, previous, err := applicationLogUpdate(nil, initial)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || mode != "snapshot" || len(previous) != 1 {
		t.Fatalf("initial update = mode %q, changed %t, previous %d", mode, changed, len(previous))
	}
	var snapshot applicationLogResponse
	if err := json.Unmarshal(payload, &snapshot); err != nil || len(snapshot.Entries) != 1 {
		t.Fatalf("snapshot payload = %#v, error %v", snapshot, err)
	}

	current := applicationLogResponse{Entries: []observability.LogEntry{newer, older}, Components: []string{"http"}, MinimumLevel: "debug"}
	mode, payload, changed, previous, err = applicationLogUpdate(previous, current)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || mode != "delta" || len(previous) != 2 {
		t.Fatalf("incremental update = mode %q, changed %t, previous %d", mode, changed, len(previous))
	}
	var delta applicationLogResponse
	if err := json.Unmarshal(payload, &delta); err != nil || len(delta.Entries) != 1 || delta.Entries[0].Message != "newer" {
		t.Fatalf("delta payload = %#v, error %v", delta, err)
	}

	mode, payload, changed, _, err = applicationLogUpdate(previous, current)
	if err != nil || changed || mode != "" || payload != nil {
		t.Fatalf("unchanged update = mode %q, changed %t, payload %q, error %v", mode, changed, payload, err)
	}
}

func TestApplicationLogUpdateFallsBackToSnapshotAfterCursorLoss(t *testing.T) {
	now := time.Now().UTC()
	previous := []observability.LogEntry{{Time: now.Add(-time.Minute), Level: "info", Message: "expired"}}
	current := applicationLogResponse{Entries: []observability.LogEntry{{Time: now, Level: "info", Message: "current"}}}
	mode, _, changed, next, err := applicationLogUpdate(previous, current)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || mode != "snapshot" || len(next) != 1 || next[0].Message != "current" {
		t.Fatalf("cursor-loss update = mode %q, changed %t, next %#v", mode, changed, next)
	}
}
