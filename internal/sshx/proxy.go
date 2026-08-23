package sshx

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Enterpr1se0/opsnerva/internal/domain"

	netproxy "golang.org/x/net/proxy"
)

const maxHTTPProxyResponseHeaderBytes = 64 << 10

var errHTTPProxyResponseHeaderTooLarge = errors.New("HTTP proxy response headers exceed 64 KiB")

// NormalizeProxyURL validates the supported proxy schemes and returns a
// canonical URL that is safe to expose in the host API.
func NormalizeProxyURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("invalid SSH proxy URL")
	}
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "socks5", "socks5h", "http":
	default:
		return "", fmt.Errorf("unsupported SSH proxy scheme %q; use socks5, socks5h, or http", parsed.Scheme)
	}
	if parsed.User != nil {
		return "", fmt.Errorf("SSH proxy credentials must use the separate username and password fields")
	}
	if parsed.Path != "" && parsed.Path != "/" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", fmt.Errorf("SSH proxy URL must contain only a scheme, host, and port")
	}
	host := parsed.Hostname()
	port := parsed.Port()
	if host == "" || port == "" {
		return "", fmt.Errorf("SSH proxy URL requires a host and port")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return "", fmt.Errorf("invalid SSH proxy port")
	}
	return scheme + "://" + net.JoinHostPort(host, port), nil
}

func dialFirstHop(ctx context.Context, targetAddress string, proxyHost domain.Host) (net.Conn, error) {
	normalized, err := NormalizeProxyURL(proxyHost.ProxyURL)
	if err != nil {
		return nil, err
	}
	if normalized == "" {
		return (&net.Dialer{}).DialContext(ctx, "tcp", targetAddress)
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		return nil, err
	}
	switch parsed.Scheme {
	case "socks5", "socks5h":
		return dialSOCKS5Proxy(ctx, parsed.Host, targetAddress, proxyHost.ProxyUsername, proxyHost.ProxyPassword)
	case "http":
		return dialHTTPConnectProxy(ctx, parsed.Host, targetAddress, proxyHost.ProxyUsername, proxyHost.ProxyPassword)
	default:
		return nil, fmt.Errorf("unsupported SSH proxy scheme %q", parsed.Scheme)
	}
}

func dialSOCKS5Proxy(ctx context.Context, proxyAddress, targetAddress, username, password string) (net.Conn, error) {
	var auth *netproxy.Auth
	if username != "" {
		auth = &netproxy.Auth{User: username, Password: password}
	}
	dialer, err := netproxy.SOCKS5("tcp", proxyAddress, auth, &net.Dialer{})
	if err != nil {
		return nil, fmt.Errorf("configure SOCKS5 proxy: %w", err)
	}
	contextDialer, ok := dialer.(netproxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("SOCKS5 proxy dialer does not support cancellation")
	}
	connection, err := contextDialer.DialContext(ctx, "tcp", targetAddress)
	if err != nil {
		return nil, fmt.Errorf("SOCKS5 proxy connection failed: %w", err)
	}
	return connection, nil
}

func dialHTTPConnectProxy(ctx context.Context, proxyAddress, targetAddress, username, password string) (net.Conn, error) {
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", proxyAddress)
	if err != nil {
		return nil, fmt.Errorf("dial HTTP proxy: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = connection.Close()
		}
	}()
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	stopCancellation := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stopCancellation()

	request := "CONNECT " + targetAddress + " HTTP/1.1\r\nHost: " + targetAddress + "\r\nProxy-Connection: Keep-Alive\r\n"
	if username != "" {
		token := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
		request += "Proxy-Authorization: Basic " + token + "\r\n"
	}
	if _, err := io.WriteString(connection, request+"\r\n"); err != nil {
		return nil, fmt.Errorf("write HTTP CONNECT request: %w", err)
	}
	headerSource := &limitedProxyHeaderReader{reader: connection, remaining: maxHTTPProxyResponseHeaderBytes}
	reader := bufio.NewReader(headerSource)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return nil, fmt.Errorf("read HTTP CONNECT response: %w", err)
	}
	headerSource.unlimited = true
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_ = response.Body.Close()
		return nil, fmt.Errorf("HTTP proxy rejected CONNECT: %s", response.Status)
	}
	keep = true
	if reader.Buffered() > 0 {
		return &bufferedProxyConn{Conn: connection, reader: reader}, nil
	}
	return connection, nil
}

type bufferedProxyConn struct {
	net.Conn
	reader *bufio.Reader
}

type limitedProxyHeaderReader struct {
	reader    io.Reader
	remaining int
	unlimited bool
}

func (r *limitedProxyHeaderReader) Read(data []byte) (int, error) {
	if r.unlimited {
		return r.reader.Read(data)
	}
	if r.remaining <= 0 {
		return 0, errHTTPProxyResponseHeaderTooLarge
	}
	if len(data) > r.remaining {
		data = data[:r.remaining]
	}
	n, err := r.reader.Read(data)
	r.remaining -= n
	return n, err
}

func (c *bufferedProxyConn) Read(data []byte) (int, error) {
	return c.reader.Read(data)
}
