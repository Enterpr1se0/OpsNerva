package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/security"
	"github.com/Enterpr1se0/opsnerva/internal/service"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"
	"github.com/Enterpr1se0/opsnerva/internal/store"
	"golang.org/x/net/websocket"
)

type httpDeletionTransport struct {
	*sshx.NativeSSHTransport
	complete chan struct{}
}

func (transport *httpDeletionTransport) RemoveSFTPEntry(ctx context.Context, _ sshx.ConnectionSpec, target string, _ bool, report func(sshx.SFTPDeleteProgress)) (sshx.SFTPFileEntry, error) {
	report(sshx.SFTPDeleteProgress{Discovered: 4, Removed: 2, CurrentPath: target + "/child"})
	select {
	case <-transport.complete:
		report(sshx.SFTPDeleteProgress{Discovered: 4, Removed: 4, RootRemoved: true, ScanComplete: true})
		return sshx.SFTPFileEntry{Path: target}, nil
	case <-ctx.Done():
		return sshx.SFTPFileEntry{}, ctx.Err()
	}
}

func TestSFTPDeletionHTTPAndWebSocketLifecycle(t *testing.T) {
	for _, cancelTask := range []bool{false, true} {
		name := "complete"
		if cancelTask {
			name = "cancel"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			st, err := store.Open(ctx, filepath.Join(t.TempDir(), "delete.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			host, err := st.UpsertHost(ctx, domain.Host{ID: "sftp-host", Name: "sftp-host", Address: "192.0.2.1", Port: 22, User: "ops", AuthType: "agent", SudoMode: "none", CreatedAt: time.Now()})
			if err != nil {
				t.Fatal(err)
			}
			cfg := config.Default()
			transport := &httpDeletionTransport{NativeSSHTransport: sshx.NewNativeSSHTransport(cfg.SSH, cfg.Limits), complete: make(chan struct{})}
			svc := service.New(st, transport, nil, security.NewRedactor(), cfg.Limits, cfg)
			t.Cleanup(func() {
				shutdownCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
				defer cancel()
				if err := svc.Shutdown(shutdownCtx); err != nil {
					t.Error(err)
				}
			})
			server := &Server{service: svc}
			mux := http.NewServeMux()
			mux.HandleFunc("GET /events", server.applicationWebSocket)
			mux.HandleFunc("DELETE /hosts/{id}/entries", server.deleteSFTPEntry)
			mux.HandleFunc("POST /deletions/{id}/cancel", server.cancelSFTPDeletion)
			httpServer := httptest.NewServer(mux)
			t.Cleanup(httpServer.Close)
			connect := func() *websocket.Conn {
				wsConfig, err := websocket.NewConfig("ws"+strings.TrimPrefix(httpServer.URL, "http")+"/events", httpServer.URL)
				if err != nil {
					t.Fatal(err)
				}
				ws, err := websocket.DialConfig(wsConfig)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = ws.Close() })
				if err := ws.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
					t.Fatal(err)
				}
				if err := websocket.JSON.Send(ws, applicationWebSocketCommand{Type: "subscribe", Topics: []string{"sftp_deletions"}}); err != nil {
					t.Fatal(err)
				}
				return ws
			}
			request := func(method, url string, wantStatus int) service.SFTPDeletion {
				req, err := http.NewRequest(method, httpServer.URL+url, nil)
				if err != nil {
					t.Fatal(err)
				}
				response, err := httpServer.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				if response.StatusCode != wantStatus {
					t.Fatalf("%s %s: status = %d, want %d", method, url, response.StatusCode, wantStatus)
				}
				var job service.SFTPDeletion
				if wantStatus < 300 {
					if err := json.NewDecoder(response.Body).Decode(&job); err != nil {
						t.Fatal(err)
					}
				}
				return job
			}
			ws := connect()
			snapshot := receiveApplicationEvent(t, ws)
			if snapshot.Mode != "snapshot" || string(snapshot.Data) != "[]" {
				t.Fatalf("initial snapshot = %+v", snapshot)
			}
			request(http.MethodDelete, "/hosts/"+host.ID+"/entries?path=/&recursive=true", http.StatusBadRequest)
			request(http.MethodDelete, "/hosts/"+host.ID+"/entries?path=/tree&recursive=invalid", http.StatusBadRequest)
			request(http.MethodDelete, "/hosts/missing/entries?path=/tree", http.StatusNotFound)
			job := request(http.MethodDelete, "/hosts/"+host.ID+"/entries?path=/tree&recursive=true", http.StatusAccepted)
			if job.ID == "" || job.Status != "running" {
				t.Fatalf("accepted job = %+v", job)
			}
			request(http.MethodDelete, "/hosts/"+host.ID+"/entries?path=/another", http.StatusConflict)
			var lastRevision uint64
			for {
				event := receiveApplicationEvent(t, ws)
				var update service.SFTPDeletion
				if err := json.Unmarshal(event.Data, &update); err != nil {
					t.Fatal(err)
				}
				if event.Topic != "sftp_deletions" || event.Mode != "delta" || update.Revision <= lastRevision {
					t.Fatalf("invalid delta: %+v %+v", event, update)
				}
				lastRevision = update.Revision
				if update.Progress.Removed == 2 {
					break
				}
			}
			// Reconnect without cancelling the task: the new socket gets live state.
			_ = ws.Close()
			ws = connect()
			snapshot = receiveApplicationEvent(t, ws)
			var jobs []service.SFTPDeletion
			if err := json.Unmarshal(snapshot.Data, &jobs); err != nil {
				t.Fatal(err)
			}
			if snapshot.Mode != "snapshot" || len(jobs) != 1 || jobs[0].ID != job.ID || jobs[0].Progress.Removed != 2 || jobs[0].Status != "running" {
				t.Fatalf("reconnect snapshot = %+v", jobs)
			}
			terminal := "completed"
			if cancelTask {
				terminal = "cancelled"
				stopping := request(http.MethodPost, "/deletions/"+job.ID+"/cancel", http.StatusOK)
				if stopping.Status != "stopping" {
					t.Fatalf("cancel = %+v", stopping)
				}
			} else {
				close(transport.complete)
			}
			for {
				event := receiveApplicationEvent(t, ws)
				var update service.SFTPDeletion
				if err := json.Unmarshal(event.Data, &update); err != nil {
					t.Fatal(err)
				}
				if update.Status != terminal {
					continue
				}
				if update.Revision <= lastRevision || update.Progress.Removed < 2 {
					t.Fatalf("terminal progress lost: %+v", update)
				}
				break
			}
			// Terminal results are still available after another reconnect.
			_ = ws.Close()
			ws = connect()
			snapshot = receiveApplicationEvent(t, ws)
			if err := json.Unmarshal(snapshot.Data, &jobs); err != nil {
				t.Fatal(err)
			}
			if len(jobs) != 1 || jobs[0].Status != terminal {
				t.Fatalf("terminal snapshot = %+v", jobs)
			}
			request(http.MethodPost, "/deletions/missing/cancel", http.StatusNotFound)
		})
	}
}
