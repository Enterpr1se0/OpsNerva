package httpapi

import (
	"encoding/binary"
	"net/http/httptest"
	"strings"
	"testing"

	"eino-ops-agent/internal/domain"

	"golang.org/x/net/websocket"
)

func TestSSHShellEventSequencePrefersReconnectHeader(t *testing.T) {
	request := httptest.NewRequest("GET", "/api/v1/ssh-shells/shell-1/events?after=0", nil)
	request.Header.Set("Last-Event-ID", "47")
	sequence, err := sshShellEventSequence(request)
	if err != nil {
		t.Fatal(err)
	}
	if sequence != 47 {
		t.Fatalf("reconnect sequence = %d, want 47", sequence)
	}

	initial := httptest.NewRequest("GET", "/api/v1/ssh-shells/shell-1/events?after=9", nil)
	sequence, err = sshShellEventSequence(initial)
	if err != nil {
		t.Fatal(err)
	}
	if sequence != 9 {
		t.Fatalf("initial sequence = %d, want 9", sequence)
	}
}

func TestSSHShellWebSocketOutputFramePreservesBytesAndSequence(t *testing.T) {
	event := domain.SSHShellEvent{Sequence: 42, Stream: "stderr", Content: string([]byte{0x1b, '[', 'H', 0xff})}
	frame := encodeSSHShellOutputFrame(event)
	if len(frame) != sshShellWebSocketHeaderBytes+4 || frame[0] != sshShellWebSocketOutputFrame || frame[1] != 1 {
		t.Fatalf("unexpected frame header: %v", frame)
	}
	if sequence := binary.BigEndian.Uint64(frame[2:sshShellWebSocketHeaderBytes]); sequence != 42 {
		t.Fatalf("sequence = %d, want 42", sequence)
	}
	if got := frame[sshShellWebSocketHeaderBytes:]; string(got) != event.Content {
		t.Fatalf("payload = %v, want %v", got, []byte(event.Content))
	}
}

func TestSSHShellWebSocketOriginMustMatchRequestHost(t *testing.T) {
	request := httptest.NewRequest("GET", "http://127.0.0.1:8080/api/v1/ssh-shells/shell-1/ws", nil)
	request.Header.Set("Origin", "http://127.0.0.1:8080")
	if err := validateWebSocketOrigin(&websocket.Config{}, request); err != nil {
		t.Fatalf("same-origin request rejected: %v", err)
	}
	request.Header.Set("Origin", "https://example.com")
	if err := validateWebSocketOrigin(&websocket.Config{}, request); err == nil {
		t.Fatal("cross-origin request was accepted")
	}
}

func TestSSHShellWebSocketPassesThroughRequestLogger(t *testing.T) {
	server := httptest.NewServer(requestLogMiddleware(websocket.Handler(func(connection *websocket.Conn) {
		var input string
		if err := websocket.Message.Receive(connection, &input); err == nil {
			_ = websocket.Message.Send(connection, input)
		}
	}), nil))
	defer server.Close()

	config, err := websocket.NewConfig("ws"+strings.TrimPrefix(server.URL, "http"), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := websocket.DialConfig(config)
	if err != nil {
		t.Fatalf("dial WebSocket through request logger: %v", err)
	}
	defer connection.Close()
	if err := websocket.Message.Send(connection, "terminal"); err != nil {
		t.Fatal(err)
	}
	var output string
	if err := websocket.Message.Receive(connection, &output); err != nil {
		t.Fatal(err)
	}
	if output != "terminal" {
		t.Fatalf("output = %q, want terminal", output)
	}
}
