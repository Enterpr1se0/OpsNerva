package sshx

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

type testNetworkProxy struct {
	kind     string
	username string
	password string
	listener net.Listener
	wg       sync.WaitGroup
}

func startTestNetworkProxy(t *testing.T, kind, username, password string) *testNetworkProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &testNetworkProxy{kind: kind, username: username, password: password, listener: listener}
	proxy.wg.Add(1)
	go proxy.serve()
	t.Cleanup(func() {
		_ = listener.Close()
		proxy.wg.Wait()
	})
	return proxy
}

func (p *testNetworkProxy) serve() {
	defer p.wg.Done()
	for {
		connection, err := p.listener.Accept()
		if err != nil {
			return
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			defer connection.Close()
			switch p.kind {
			case "http":
				p.handleHTTP(connection)
			case "socks5":
				p.handleSOCKS5(connection)
			}
		}()
	}
}

func (p *testNetworkProxy) handleHTTP(connection net.Conn) {
	reader := bufio.NewReader(connection)
	request, err := http.ReadRequest(reader)
	if err != nil || request.Method != http.MethodConnect {
		return
	}
	expected := "Basic " + base64.StdEncoding.EncodeToString([]byte(p.username+":"+p.password))
	if request.Header.Get("Proxy-Authorization") != expected {
		_, _ = io.WriteString(connection, "HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 0\r\n\r\n")
		return
	}
	upstream, err := net.Dial("tcp", request.Host)
	if err != nil {
		_, _ = io.WriteString(connection, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	defer upstream.Close()
	if _, err := io.WriteString(connection, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	relayProxy(connection, reader, upstream)
}

func (p *testNetworkProxy) handleSOCKS5(connection net.Conn) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(connection, header); err != nil || header[0] != 5 {
		return
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(connection, methods); err != nil || !strings.ContainsRune(string(methods), rune(2)) {
		_, _ = connection.Write([]byte{5, 0xff})
		return
	}
	if _, err := connection.Write([]byte{5, 2}); err != nil {
		return
	}
	if _, err := io.ReadFull(connection, header); err != nil || header[0] != 1 {
		return
	}
	username := make([]byte, int(header[1]))
	if _, err := io.ReadFull(connection, username); err != nil {
		return
	}
	if _, err := io.ReadFull(connection, header[:1]); err != nil {
		return
	}
	password := make([]byte, int(header[0]))
	if _, err := io.ReadFull(connection, password); err != nil || string(username) != p.username || string(password) != p.password {
		_, _ = connection.Write([]byte{1, 1})
		return
	}
	if _, err := connection.Write([]byte{1, 0}); err != nil {
		return
	}

	request := make([]byte, 4)
	if _, err := io.ReadFull(connection, request); err != nil || request[0] != 5 || request[1] != 1 {
		return
	}
	var host string
	switch request[3] {
	case 1:
		address := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(connection, address); err != nil {
			return
		}
		host = net.IP(address).String()
	case 3:
		if _, err := io.ReadFull(connection, header[:1]); err != nil {
			return
		}
		address := make([]byte, int(header[0]))
		if _, err := io.ReadFull(connection, address); err != nil {
			return
		}
		host = string(address)
	case 4:
		address := make([]byte, net.IPv6len)
		if _, err := io.ReadFull(connection, address); err != nil {
			return
		}
		host = net.IP(address).String()
	default:
		return
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(connection, portBytes); err != nil {
		return
	}
	port := int(portBytes[0])<<8 | int(portBytes[1])
	upstream, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		_, _ = connection.Write([]byte{5, 4, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstream.Close()
	if _, err := connection.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	relayProxy(connection, connection, upstream)
}

func relayProxy(client net.Conn, clientReader io.Reader, upstream net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, clientReader); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
	_ = client.Close()
	_ = upstream.Close()
	<-done
}

func TestNativeSSHNetworkProxy(t *testing.T) {
	for _, kind := range []string{"socks5", "http"} {
		t.Run(kind, func(t *testing.T) {
			server := startTestSSHServer(t, "target-password")
			proxy := startTestNetworkProxy(t, kind, "proxy-user", "proxy-password")
			host := server.host()
			host.ProxyURL = fmt.Sprintf("%s://%s", kind, proxy.listener.Addr())
			host.ProxyUsername = proxy.username
			host.ProxyPassword = proxy.password
			transport := NewNativeSSHTransport(config.SSH{DefaultKnownHosts: t.TempDir() + "/known_hosts"}, config.Default().Limits)
			connection := ConnectionSpec{Target: host}

			key, err := transport.ScanHostKey(t.Context(), connection)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := transport.TrustHostKey(t.Context(), connection, key.Fingerprint); err != nil {
				t.Fatal(err)
			}
			result, err := transport.Exec(t.Context(), connection, domain.ExecRequest{Mode: domain.ExecProgram, Program: "printf", TimeoutSeconds: 5})
			if err != nil {
				t.Fatal(err)
			}
			if result.ExitCode != 0 || string(result.Stdout) != "native-ok\n" {
				t.Fatalf("unexpected proxied SSH result: %#v", result)
			}
		})
	}
}

func TestNativeSSHNetworkProxyWithJumpHost(t *testing.T) {
	jumpServer := startTestSSHServer(t, "jump-password")
	targetServer := startTestSSHServer(t, "target-password")
	proxy := startTestNetworkProxy(t, "socks5", "proxy-user", "proxy-password")
	knownHosts := t.TempDir() + "/known_hosts"
	transport := NewNativeSSHTransport(config.SSH{DefaultKnownHosts: knownHosts}, config.Default().Limits)

	jump := jumpServer.host()
	jump.Name = "jump"
	jumpConnection := ConnectionSpec{Target: jump}
	jumpKey, err := transport.ScanHostKey(t.Context(), jumpConnection)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.TrustHostKey(t.Context(), jumpConnection, jumpKey.Fingerprint); err != nil {
		t.Fatal(err)
	}

	target := targetServer.host()
	target.Name = "target"
	target.ProxyURL = "socks5://" + proxy.listener.Addr().String()
	target.ProxyUsername = proxy.username
	target.ProxyPassword = proxy.password
	connection := ConnectionSpec{Target: target, Jumps: []domain.Host{jump}}
	targetKey, err := transport.ScanHostKey(t.Context(), connection)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.TrustHostKey(t.Context(), connection, targetKey.Fingerprint); err != nil {
		t.Fatal(err)
	}
	result, err := transport.Exec(t.Context(), connection, domain.ExecRequest{Mode: domain.ExecProgram, Program: "printf", TimeoutSeconds: 5})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || string(result.Stdout) != "native-ok\n" {
		t.Fatalf("unexpected proxy plus ProxyJump result: %#v", result)
	}

	echoListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoListener.Close()
	echoDone := make(chan struct{})
	go func() {
		defer close(echoDone)
		connection, acceptErr := echoListener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		_, _ = io.Copy(connection, connection)
	}()
	tunnel, err := transport.OpenTunnel(t.Context(), connection)
	if err != nil {
		t.Fatal(err)
	}
	defer tunnel.Close()
	forwarded, err := tunnel.Dial("tcp", echoListener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(forwarded, "through-proxy-and-jump"); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, len("through-proxy-and-jump"))
	readDone := make(chan error, 1)
	go func() {
		_, readErr := io.ReadFull(forwarded, reply)
		readDone <- readErr
	}()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out reading tunnel reply")
	}
	_ = forwarded.Close()
	if string(reply) != "through-proxy-and-jump" {
		t.Fatalf("unexpected tunnel reply %q", reply)
	}
	select {
	case <-echoDone:
	case <-time.After(time.Second):
		t.Fatal("tunnel forwarding connection did not close")
	}
}

func TestNormalizeProxyURL(t *testing.T) {
	normalized, err := NormalizeProxyURL(" SOCKS5H://proxy.example:1080/ ")
	if err != nil || normalized != "socks5h://proxy.example:1080" {
		t.Fatalf("unexpected normalized proxy URL %q: %v", normalized, err)
	}
	for _, invalid := range []string{
		"https://proxy.example:443", "socks5://user:pass@proxy.example:1080", "http://proxy.example", "http://proxy.example:8080/path",
	} {
		if _, err := NormalizeProxyURL(invalid); err == nil {
			t.Fatalf("invalid proxy URL %q was accepted", invalid)
		}
	}
}
