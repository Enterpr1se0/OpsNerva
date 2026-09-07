package httpapi

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/service"
	"github.com/Enterpr1se0/opsnerva/internal/store"
	"golang.org/x/net/websocket"
)

func TestShellReconnectReceivesAuthoritativeState(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "shell-reconnect.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.Default()
	svc := service.New(st, nil, nil, nil, cfg.Limits, cfg)
	t.Cleanup(func() { _ = svc.Shutdown(context.Background()) })
	for _, status := range []string{"running", "completed"} {
		if err := st.CreateSSHShell(ctx, domain.SSHShell{ID: status, RunID: "run-" + status, Kind: "ssh", Surface: "agent", Status: status, StartedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(New(svc, nil, Options{}).Handler())
	defer server.Close()
	for _, test := range []struct{ id, eventType string }{{"running", "ready"}, {"completed", "ended"}, {"missing", "unavailable"}} {
		t.Run(test.id, func(t *testing.T) {
			connection, err := websocket.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/v1/ssh-shells/"+test.id+"/ws", "", server.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
			var event sshShellWebSocketEvent
			if err := websocket.JSON.Receive(connection, &event); err != nil {
				t.Fatal(err)
			}
			if event.Type != test.eventType {
				t.Fatalf("reconnect event = %#v, want %s", event, test.eventType)
			}
			if test.id != "missing" && (event.Shell == nil || event.Shell.Status != test.id) {
				t.Fatalf("reconnect state = %#v", event.Shell)
			}
		})
	}
}
