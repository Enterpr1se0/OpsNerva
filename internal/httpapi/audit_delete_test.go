package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/security"
	"github.com/Enterpr1se0/opsnerva/internal/service"
	"github.com/Enterpr1se0/opsnerva/internal/store"
)

func TestDeleteAuditRunsEndpointRetainsActiveRun(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dataDir, "audit-delete.db"))
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
	now := time.Now().UTC()
	host, err := st.UpsertHost(ctx, domain.Host{ID: "host-http-audit", Name: "audit", Address: "192.0.2.60", Port: 22, User: "ops", AuthType: "agent", SudoMode: "none", CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range []domain.Run{
		{ID: "run-http-finished", HostID: host.ID, RequestJSON: `{}`, RequestDigest: "finished", Status: "completed", StartedAt: now, CompletedAt: now},
		{ID: "run-http-active", HostID: host.ID, RequestJSON: `{}`, RequestDigest: "active", Status: "running", StartedAt: now},
	} {
		if err := st.CreateRun(ctx, run); err != nil {
			t.Fatal(err)
		}
	}
	httpServer := httptest.NewServer(New(svc, nil, Options{}).Handler())
	defer httpServer.Close()

	request, err := http.NewRequest(http.MethodDelete, httpServer.URL+"/api/v1/audit/runs?session_id=", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := httpServer.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var result domain.AuditRunDeleteResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || result.Deleted != 1 || result.Retained != 1 || result.Scope != "direct" || len(result.RetainedRunIDs) != 1 || result.RetainedRunIDs[0] != "run-http-active" {
		t.Fatalf("delete status=%d result=%#v", response.StatusCode, result)
	}
}
