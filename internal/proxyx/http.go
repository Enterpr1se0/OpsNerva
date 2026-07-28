package proxyx

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HeaderRewriteTransport sets fixed header values on every outgoing request,
// replacing any value the underlying client or SDK would otherwise send.
type HeaderRewriteTransport struct {
	Base    http.RoundTripper
	Headers map[string]string
}

func (t HeaderRewriteTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	for name, value := range t.Headers {
		clone.Header.Set(name, value)
	}
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

// WrapHeaders returns a shallow copy of client with a header-rewriting
// transport. SDK header options typically append instead of replacing defaults,
// so callers that must override a default header (e.g. User-Agent) need the
// transport-level rewrite. The input client remains safe for independent use.
func WrapHeaders(client *http.Client, headers map[string]string) *http.Client {
	if client == nil {
		client = &http.Client{}
	}
	result := *client
	fixedHeaders := make(map[string]string, len(headers))
	for name, value := range headers {
		fixedHeaders[name] = value
	}
	result.Transport = HeaderRewriteTransport{Base: client.Transport, Headers: fixedHeaders}
	return &result
}

func NormalizeURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", fmt.Errorf("invalid proxy URL")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	switch parsed.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return "", fmt.Errorf("unsupported proxy scheme %q", parsed.Scheme)
	}
	parsed.Path = ""
	return parsed.String(), nil
}

func NewHTTPClient(proxyURL, username, password string, timeout time.Duration) (*http.Client, error) {
	normalized, err := NormalizeURL(proxyURL)
	if err != nil {
		return nil, err
	}
	if normalized == "" {
		return nil, fmt.Errorf("invalid proxy URL")
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URL")
	}
	if username != "" {
		parsed.User = url.UserPassword(username, password)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(parsed)
	transport.ResponseHeaderTimeout = timeout
	return &http.Client{Transport: transport, Timeout: timeout}, nil
}
