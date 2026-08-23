package httpapi

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/store"

	"golang.org/x/net/websocket"
)

const (
	sshShellWebSocketOutputFrame = byte(1)
	sshShellWebSocketHeaderBytes = 10
	sshShellWebSocketMaxMessage  = (64 << 10) + (1 << 10)
)

type sshShellWebSocketCommand struct {
	Type      string `json:"type"`
	Content   string `json:"content,omitempty"`
	Sensitive bool   `json:"sensitive,omitempty"`
	Cols      int    `json:"cols,omitempty"`
	Rows      int    `json:"rows,omitempty"`
}

type sshShellWebSocketEvent struct {
	Type  string               `json:"type"`
	Event domain.SSHShellEvent `json:"event,omitempty"`
	Error string               `json:"error,omitempty"`
}

func (s *Server) sshShellWebSocket(w http.ResponseWriter, r *http.Request) {
	server := websocket.Server{
		Handshake: validateWebSocketOrigin,
		Handler: func(connection *websocket.Conn) {
			connection.MaxPayloadBytes = sshShellWebSocketMaxMessage
			s.serveSSHShellWebSocket(connection, r)
		},
	}
	server.ServeHTTP(w, r)
}

func validateWebSocketOrigin(config *websocket.Config, request *http.Request) error {
	originValue := strings.TrimSpace(request.Header.Get("Origin"))
	if originValue == "" {
		return nil
	}
	origin, err := url.Parse(originValue)
	if err != nil || !strings.EqualFold(origin.Host, request.Host) {
		return fmt.Errorf("WebSocket origin is not allowed")
	}
	config.Origin = origin
	return nil
}

func (s *Server) serveSSHShellWebSocket(connection *websocket.Conn, request *http.Request) {
	defer connection.Close()
	after, err := sshShellEventSequence(request)
	if err != nil {
		_ = websocket.JSON.Send(connection, sshShellWebSocketEvent{Type: "error", Error: err.Error()})
		return
	}
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	liveEvents, overflow, unsubscribe := s.service.SubscribeSSHShellEvents(request.PathValue("id"))
	defer unsubscribe()
	readDone := make(chan error, 1)
	go func() {
		readDone <- s.readSSHShellWebSocket(ctx, connection, request)
		cancel()
	}()

	sendEvent := func(event domain.SSHShellEvent) error {
		if event.Sequence <= after {
			return nil
		}
		if event.Stream == "stdout" || event.Stream == "stderr" {
			if err := websocket.Message.Send(connection, encodeSSHShellOutputFrame(event)); err != nil {
				return err
			}
		} else if err := websocket.JSON.Send(connection, sshShellWebSocketEvent{Type: "event", Event: event}); err != nil {
			return err
		}
		after = event.Sequence
		return nil
	}
	sendSnapshot := func(snapshot domain.SSHShellSnapshot) error {
		for _, event := range snapshot.Events {
			if err := sendEvent(event); err != nil {
				return err
			}
		}
		return nil
	}
	snapshot, snapshotErr := s.service.GetSSHShellSnapshot(ctx, request.PathValue("id"), "", after, 0, false, "", "")
	if snapshotErr != nil {
		if !errors.Is(snapshotErr, context.Canceled) && !errors.Is(snapshotErr, store.ErrNotFound) {
			_ = websocket.JSON.Send(connection, sshShellWebSocketEvent{Type: "error", Error: snapshotErr.Error()})
		}
		return
	}
	if err := sendSnapshot(snapshot); err != nil {
		return
	}
	if !shellHTTPStatusActive(snapshot.Shell.Status) {
		return
	}
	if liveEvents == nil {
		for {
			snapshot, snapshotErr = s.service.GetSSHShellSnapshot(ctx, request.PathValue("id"), "", after, 10*time.Second, false, "", "")
			if snapshotErr != nil {
				return
			}
			if err := sendSnapshot(snapshot); err != nil {
				return
			}
			if !shellHTTPStatusActive(snapshot.Shell.Status) {
				return
			}
			if len(snapshot.Events) == 0 {
				if err := websocket.JSON.Send(connection, sshShellWebSocketEvent{Type: "heartbeat"}); err != nil {
					return
				}
			}
		}
	}
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case event := <-liveEvents:
			if err := sendEvent(event); err != nil {
				return
			}
			if event.Stream == "status" && !shellHTTPStatusActive(event.Status) {
				return
			}
		case <-overflow:
			return
		case <-heartbeat.C:
			if err := websocket.JSON.Send(connection, sshShellWebSocketEvent{Type: "heartbeat"}); err != nil {
				return
			}
		case <-ctx.Done():
			return
		case <-readDone:
			return
		}
	}
}

func (s *Server) readSSHShellWebSocket(ctx context.Context, connection *websocket.Conn, request *http.Request) error {
	for {
		var command sshShellWebSocketCommand
		if err := websocket.JSON.Receive(connection, &command); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		switch command.Type {
		case "input":
			if command.Sensitive {
				if err := s.service.WriteSensitiveSSHShellInput(ctx, request.PathValue("id"), command.Content, actor(request)); err != nil {
					return err
				}
				continue
			}
			if err := s.service.SendSSHShellInput(ctx, request.PathValue("id"), "", command.Content, "", actor(request)); err != nil {
				return err
			}
		case "resize":
			if _, err := s.service.ResizeSSHShell(ctx, request.PathValue("id"), command.Cols, command.Rows, actor(request)); err != nil {
				return err
			}
		case "interrupt":
			if _, err := s.service.InterruptSSHShell(ctx, request.PathValue("id"), "", "", actor(request)); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported SSH shell WebSocket command %q", command.Type)
		}
	}
}

func encodeSSHShellOutputFrame(event domain.SSHShellEvent) []byte {
	payload := make([]byte, sshShellWebSocketHeaderBytes+len(event.Content))
	payload[0] = sshShellWebSocketOutputFrame
	if event.Stream == "stderr" {
		payload[1] = 1
	}
	binary.BigEndian.PutUint64(payload[2:sshShellWebSocketHeaderBytes], event.Sequence)
	copy(payload[sshShellWebSocketHeaderBytes:], event.Content)
	return payload
}
