package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"eino-ops-agent/internal/domain"
)

func TestApplyConfigurationRollsBackEntirePackage(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "configuration.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.UpsertHost(ctx, domain.Host{ID: "host-existing", Name: "duplicate", Address: "127.0.0.1", Port: 22, User: "ops", AuthType: "agent", SudoMode: "none"}); err != nil {
		t.Fatal(err)
	}
	err = st.ApplyConfiguration(ctx, ConfigurationSnapshot{
		Proxies: []domain.Proxy{{ID: "proxy-new", Name: "new", URL: "socks5://127.0.0.1:1080"}},
		Hosts:   []domain.Host{{ID: "host-new", Name: "duplicate", Address: "192.0.2.1", Port: 22, User: "ops", AuthType: "agent", SudoMode: "none"}},
	})
	if err == nil {
		t.Fatal("conflicting package was accepted")
	}
	if _, err := st.GetProxy(ctx, "proxy-new"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("proxy write was not rolled back: %v", err)
	}
}
