package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/security"
	"github.com/Enterpr1se0/opsnerva/internal/service"
	"github.com/Enterpr1se0/opsnerva/internal/store"
	"golang.org/x/net/websocket"
)

func newAuditHistoryServer(t *testing.T) (*Server, *store.Store, domain.Host) {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dataDir, "audit-history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	encryptor, err := security.NewEncryptor("", dataDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.DataDir = dataDir
	svc := service.New(st, nil, encryptor, security.NewRedactor(), cfg.Limits, cfg)
	t.Cleanup(func() {
		if err := svc.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	host, err := st.UpsertHost(ctx, domain.Host{ID: "host-audit", Name: "audit", Address: "127.0.0.1", Port: 22,
		User: "ops", AuthType: "agent", SudoMode: "none", CreatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	return New(svc, nil, Options{}), st, host
}

func auditHistoryGET(t *testing.T, handler http.Handler, path string, expectedStatus int, result any) {
	t.Helper()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != expectedStatus {
		t.Fatalf("GET %s = %d, want %d: %s", path, w.Code, expectedStatus, w.Body.String())
	}
	if result != nil {
		if err := json.Unmarshal(w.Body.Bytes(), result); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAuditHistoryMetadataPushRevalidatesCommittedGroups(t *testing.T) {
	server, st, host := newAuditHistoryServer(t)
	ctx := t.Context()
	if _, err := st.CreateChatSession(ctx, "metadata", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetChatSessionTitle(ctx, "metadata", "Before"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRun(ctx, domain.Run{ID: "metadata-run", SessionID: "metadata", HostID: host.ID,
		RequestJSON: `{}`, RequestDigest: "digest", Status: "completed", StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	connection, err := websocket.Dial("ws"+httpServer.URL[len("http"):]+"/api/v1/events/ws", "", httpServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := websocket.JSON.Send(connection, applicationWebSocketCommand{Type: "subscribe", Topics: []string{"audit"}}); err != nil {
		t.Fatal(err)
	}
	readInvalidation := func() {
		t.Helper()
		var event applicationWebSocketEvent
		if err := websocket.JSON.Receive(connection, &event); err != nil {
			t.Fatal(err)
		}
		if event.Topic != "audit" || event.Mode != "snapshot" {
			t.Fatalf("metadata invalidation: %#v", event)
		}
	}
	readInvalidation()
	if _, err := st.SetChatSessionTitle(ctx, "metadata", "After"); err != nil {
		t.Fatal(err)
	}
	readInvalidation()
	var groups domain.AuditHistoryGroupPage
	auditHistoryGET(t, server.Handler(), "/api/v1/audit/groups", http.StatusOK, &groups)
	if len(groups.Groups) != 1 || groups.Groups[0].Title != "After" {
		t.Fatalf("renamed group: %#v", groups)
	}
	if err := st.DeleteChatSession(ctx, "metadata"); err != nil {
		t.Fatal(err)
	}
	readInvalidation()
	auditHistoryGET(t, server.Handler(), "/api/v1/audit/groups", http.StatusOK, &groups)
	if len(groups.Groups) != 1 || groups.Groups[0].SessionExists || groups.Groups[0].RunCount != 1 {
		t.Fatalf("deleted session evidence: %#v", groups)
	}
}

func TestAuditHistoryEndpointsPaginateAndSearch(t *testing.T) {
	server, st, host := newAuditHistoryServer(t)
	handler := server.Handler()
	ctx := context.Background()
	now := time.Now().UTC().Add(-time.Minute)
	for _, session := range []string{"chat", "mcp_sess_a", ""} {
		for i := range 55 {
			run := domain.Run{ID: fmt.Sprintf("%s-%03d", session, i), SessionID: session, HostID: host.ID, RequestJSON: `{"program":"echo"}`,
				RequestDigest: "digest", Status: "completed", StartedAt: now}
			if i == 0 {
				run.SearchText = "old-needle"
			}
			if err := st.CreateRun(ctx, run); err != nil {
				t.Fatal(err)
			}
		}
	}
	var groups domain.AuditHistoryGroupPage
	auditHistoryGET(t, handler, "/api/v1/audit/groups?limit=2", http.StatusOK, &groups)
	if len(groups.Groups) != 2 || !groups.HasMore || groups.NextCursor == nil || groups.SnapshotAt.IsZero() {
		t.Fatalf("first groups = %#v", groups)
	}
	params := url.Values{"limit": {"2"}, "snapshot_at": {groups.SnapshotAt.Format(time.RFC3339Nano)},
		"cursor_started_at": {groups.NextCursor.StartedAt.Format(time.RFC3339Nano)}, "cursor_id": {groups.NextCursor.ID}}
	var older domain.AuditHistoryGroupPage
	auditHistoryGET(t, handler, "/api/v1/audit/groups?"+params.Encode(), http.StatusOK, &older)
	if len(older.Groups) != 1 || older.Groups[0].SessionID != "" || older.Groups[0].RunCount != 55 || older.HasMore || !older.SnapshotAt.Equal(groups.SnapshotAt) {
		t.Fatalf("second groups = %#v", older)
	}
	var first domain.AuditHistoryRunPage
	auditHistoryGET(t, handler, "/api/v1/audit/runs?session_id=&snapshot_at="+url.QueryEscape(groups.SnapshotAt.Format(time.RFC3339Nano)), http.StatusOK, &first)
	if len(first.Runs) != 50 || !first.HasMore || first.NextCursor == nil {
		t.Fatalf("first runs = %#v", first)
	}
	params = url.Values{"session_id": {""}, "snapshot_at": {first.SnapshotAt.Format(time.RFC3339Nano)},
		"cursor_started_at": {first.NextCursor.StartedAt.Format(time.RFC3339Nano)}, "cursor_id": {first.NextCursor.ID}}
	var last domain.AuditHistoryRunPage
	auditHistoryGET(t, handler, "/api/v1/audit/runs?"+params.Encode(), http.StatusOK, &last)
	if len(last.Runs) != 5 || last.HasMore || last.NextCursor != nil {
		t.Fatalf("last runs = %#v", last)
	}
	for _, run := range append(first.Runs, last.Runs...) {
		if run.SessionID != "" {
			t.Fatal("empty session query fetched another session")
		}
	}
	var matchedGroups domain.AuditHistoryGroupPage
	auditHistoryGET(t, handler, "/api/v1/audit/groups?q=old-needle", http.StatusOK, &matchedGroups)
	if len(matchedGroups.Groups) != 3 {
		t.Fatalf("history search groups = %#v", matchedGroups)
	}
	for _, group := range matchedGroups.Groups {
		if group.RunCount != 1 {
			t.Fatalf("search count = %#v", group)
		}
		var matchedRuns domain.AuditHistoryRunPage
		auditHistoryGET(t, handler, "/api/v1/audit/runs?session_id="+url.QueryEscape(group.SessionID)+"&q=old-needle", http.StatusOK, &matchedRuns)
		if len(matchedRuns.Runs) != 1 || matchedRuns.HasMore {
			t.Fatalf("search missed older run: %#v", matchedRuns)
		}
	}
	var legacy domain.RunSearchPage
	auditHistoryGET(t, handler, "/api/v1/run-summaries?limit=100", http.StatusOK, &legacy)
	if len(legacy.Runs) != 100 || !legacy.HasMore {
		t.Fatal("existing run-summaries endpoint changed")
	}
}

func TestAuditHistoryEndpointsValidateBoundariesAndAuthentication(t *testing.T) {
	server, _, _ := newAuditHistoryServer(t)
	handler := server.Handler()
	past := url.QueryEscape(time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano))
	future := url.QueryEscape(time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano))
	for _, suffix := range []string{"limit=0", "limit=-1", "limit=201", "limit=wat", "snapshot_at=", "snapshot_at=invalid", "snapshot_at=" + future,
		"cursor_id=one", "cursor_started_at=" + past, "cursor_started_at=" + past + "&cursor_id=one",
		"snapshot_at=" + past + "&cursor_id=one", "snapshot_at=" + past + "&cursor_started_at=invalid&cursor_id=one",
		"snapshot_at=" + past + "&cursor_started_at=" + future + "&cursor_id=one"} {
		t.Run(suffix, func(t *testing.T) {
			auditHistoryGET(t, handler, "/api/v1/audit/groups?"+suffix, http.StatusBadRequest, nil)
			auditHistoryGET(t, handler, "/api/v1/audit/runs?session_id=chat&"+suffix, http.StatusBadRequest, nil)
		})
	}
	auditHistoryGET(t, handler, "/api/v1/audit/runs", http.StatusBadRequest, nil)
	// Presence, not truthiness, distinguishes the direct group's cursor ID.
	boundary := "snapshot_at=" + past + "&cursor_started_at=" + past + "&cursor_id="
	auditHistoryGET(t, handler, "/api/v1/audit/groups?"+boundary, http.StatusOK, nil)
	auditHistoryGET(t, handler, "/api/v1/audit/runs?session_id=&"+boundary, http.StatusBadRequest, nil)
	server.auth = newAuthManager(config.Auth{Username: "operator", Password: "test-password"})
	for _, path := range []string{"/api/v1/audit/groups", "/api/v1/audit/runs?session_id="} {
		auditHistoryGET(t, server.Handler(), path, http.StatusUnauthorized, nil)
	}
}
