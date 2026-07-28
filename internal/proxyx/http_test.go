package proxyx

import (
	"net/http"
	"testing"
	"time"
)

func TestNormalizeURL(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "http://127.0.0.1:7890", want: "http://127.0.0.1:7890"},
		{input: "HTTPS://proxy.example:8443/", want: "https://proxy.example:8443"},
		{input: "socks5://127.0.0.1:1080", want: "socks5://127.0.0.1:1080"},
		{input: "SOCKS5H://proxy.example:1080", want: "socks5h://proxy.example:1080"},
	}
	for _, test := range tests {
		got, err := NormalizeURL(test.input)
		if err != nil || got != test.want {
			t.Errorf("NormalizeURL(%q) = %q, %v; want %q", test.input, got, err, test.want)
		}
	}
	for _, input := range []string{
		"ftp://proxy.example:21", "http://user:password@proxy.example", "http://proxy.example/path", "proxy.example:8080",
	} {
		if _, err := NormalizeURL(input); err == nil {
			t.Errorf("NormalizeURL(%q) accepted an invalid proxy URL", input)
		}
	}
}

func TestWrapHeadersDoesNotMutateInputClient(t *testing.T) {
	base := http.DefaultTransport.(*http.Transport).Clone()
	client := &http.Client{
		Transport: base,
		Timeout:   3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	headers := map[string]string{"User-Agent": "fixed-agent"}

	wrapped := WrapHeaders(client, headers)
	if wrapped == client {
		t.Fatal("WrapHeaders returned the input client")
	}
	if client.Transport != base {
		t.Fatal("WrapHeaders mutated the input client transport")
	}
	if wrapped.Timeout != client.Timeout || wrapped.CheckRedirect == nil {
		t.Fatal("WrapHeaders did not preserve the client settings")
	}
	transport, ok := wrapped.Transport.(HeaderRewriteTransport)
	if !ok {
		t.Fatalf("wrapped transport type = %T, want HeaderRewriteTransport", wrapped.Transport)
	}
	headers["User-Agent"] = "mutated-agent"
	if transport.Headers["User-Agent"] != "fixed-agent" {
		t.Fatal("WrapHeaders retained the caller's mutable header map")
	}
}
