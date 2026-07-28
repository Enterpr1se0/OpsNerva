package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestModelProviderProxyReferenceMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy-models.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE hosts (
id TEXT PRIMARY KEY,
name TEXT NOT NULL UNIQUE,
address TEXT NOT NULL,
port INTEGER NOT NULL,
username TEXT NOT NULL,
auth_type TEXT NOT NULL DEFAULT 'agent',
private_key_cipher TEXT NOT NULL DEFAULT '',
known_hosts_file TEXT NOT NULL DEFAULT '',
proxy_jump_host_id TEXT NOT NULL DEFAULT '',
proxy_url TEXT NOT NULL DEFAULT '',
proxy_username TEXT NOT NULL DEFAULT '',
proxy_password_cipher TEXT NOT NULL DEFAULT '',
password_cipher TEXT NOT NULL DEFAULT '',
sudo_mode TEXT NOT NULL DEFAULT 'none',
sudo_password_cipher TEXT NOT NULL DEFAULT '',
created_at TEXT NOT NULL,
updated_at TEXT NOT NULL
);
INSERT INTO hosts(id,name,address,port,username,auth_type,proxy_url,proxy_username,proxy_password_cipher,created_at,updated_at)
VALUES('host-legacy','Legacy SSH','192.0.2.10',22,'ops','agent','http://127.0.0.1:7890','host-user','host-cipher','now','now');
CREATE TABLE model_providers (
id TEXT PRIMARY KEY,
name TEXT NOT NULL UNIQUE,
kind TEXT NOT NULL,
base_url TEXT NOT NULL DEFAULT '',
model TEXT NOT NULL,
api_key_cipher TEXT NOT NULL DEFAULT '',
proxy_url TEXT NOT NULL DEFAULT '',
proxy_username TEXT NOT NULL DEFAULT '',
proxy_password_cipher TEXT NOT NULL DEFAULT '',
active INTEGER NOT NULL DEFAULT 0,
created_at TEXT NOT NULL,
updated_at TEXT NOT NULL
);
INSERT INTO model_providers(id,name,kind,base_url,model,api_key_cipher,proxy_url,proxy_username,proxy_password_cipher,active,created_at,updated_at)
VALUES('legacy','Legacy','openai_compatible','http://127.0.0.1:8080/v1','legacy-model','','socks5://127.0.0.1:1080','legacy-user','legacy-cipher',0,'now','now');
CREATE TABLE web_search_settings (
id INTEGER PRIMARY KEY CHECK(id=1),
enabled INTEGER NOT NULL DEFAULT 0,
provider TEXT NOT NULL DEFAULT 'tavily',
base_url TEXT NOT NULL DEFAULT 'https://api.tavily.com',
api_key_cipher TEXT NOT NULL DEFAULT '',
proxy_url TEXT NOT NULL DEFAULT '',
proxy_username TEXT NOT NULL DEFAULT '',
proxy_password_cipher TEXT NOT NULL DEFAULT '',
timeout_seconds INTEGER NOT NULL DEFAULT 20,
max_results INTEGER NOT NULL DEFAULT 10,
updated_at TEXT NOT NULL
);
INSERT INTO web_search_settings(id,proxy_url,proxy_username,proxy_password_cipher,updated_at)
VALUES(1,'https://127.0.0.1:8443','web-user','web-cipher','now');`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	provider, err := st.GetModelProvider(ctx, "legacy")
	if err != nil || provider.ProxyID == "" {
		t.Fatalf("legacy provider proxy was not migrated: provider=%#v err=%v", provider, err)
	}
	proxy, err := st.GetProxy(ctx, provider.ProxyID)
	if err != nil || proxy.URL != "socks5://127.0.0.1:1080" || proxy.Username != "legacy-user" || proxy.PasswordCipher != "legacy-cipher" {
		t.Fatalf("legacy proxy configuration was not preserved: proxy=%#v err=%v", proxy, err)
	}
	host, err := st.GetHost(ctx, "host-legacy")
	if err != nil || host.ProxyID == "" {
		t.Fatalf("legacy SSH proxy was not migrated: host=%#v err=%v", host, err)
	}
	webSettings, err := st.GetWebSearchSettings(ctx)
	if err != nil || webSettings.ProxyID == "" {
		t.Fatalf("legacy Tavily proxy was not migrated: settings=%#v err=%v", webSettings, err)
	}
	proxies, err := st.ListProxies(ctx)
	if err != nil || len(proxies) != 3 {
		t.Fatalf("expected three migrated proxy records: proxies=%#v err=%v", proxies, err)
	}
	rows, err := st.db.QueryContext(ctx, `PRAGMA table_info(model_providers)`)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		found[name] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if !found["proxy_id"] {
		t.Error("migration did not add proxy_id")
	}
	for _, column := range []string{"proxy_url", "proxy_username", "proxy_password_cipher"} {
		if found[column] {
			t.Errorf("migration retained obsolete %s", column)
		}
	}
	for _, table := range []string{"hosts", "web_search_settings"} {
		rows, err := st.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
		if err != nil {
			t.Fatal(err)
		}
		columns := map[string]bool{}
		for rows.Next() {
			var cid, notNull, primaryKey int
			var name, columnType string
			var defaultValue any
			if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			columns[name] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if !columns["proxy_id"] {
			t.Errorf("migration did not add %s.proxy_id", table)
		}
		for _, column := range []string{"proxy_url", "proxy_username", "proxy_password_cipher"} {
			if columns[column] {
				t.Errorf("migration retained obsolete %s.%s", table, column)
			}
		}
	}
}
