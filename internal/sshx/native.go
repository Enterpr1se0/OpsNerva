package sshx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"eino-ops-agent/internal/config"
	"eino-ops-agent/internal/domain"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

const (
	nativeConnectTimeout  = 10 * time.Second
	nativeDialCancelWait  = time.Second
	nativeKeepalivePeriod = 15 * time.Second
	nativeKeepaliveWait   = 10 * time.Second
)

var errHostKeyCaptured = errors.New("SSH host key captured")

type NativeSSHTransport struct {
	config       config.SSH
	limits       config.Limits
	knownHostsMu sync.Mutex
}

type nativeClient struct {
	client  *ssh.Client
	clients []*ssh.Client
	cancel  context.CancelFunc
	once    sync.Once
}

func NewNativeSSHTransport(cfg config.SSH, limits config.Limits) *NativeSSHTransport {
	return &NativeSSHTransport{config: cfg, limits: limits}
}

func (t *NativeSSHTransport) Exec(ctx context.Context, connection ConnectionSpec, req domain.ExecRequest) (RawResult, error) {
	return t.execWithCallback(ctx, connection, req, nil)
}

func (t *NativeSSHTransport) ExecStream(ctx context.Context, connection ConnectionSpec, req domain.ExecRequest, callback func(string, []byte)) (RawResult, error) {
	return t.execWithCallback(ctx, connection, req, callback)
}

func (t *NativeSSHTransport) OpenTunnel(ctx context.Context, connection ConnectionSpec) (TunnelClient, error) {
	if err := validateNativeConnection(connection); err != nil {
		return nil, err
	}
	client, err := t.connect(ctx, connection, nil, false)
	if err != nil {
		return nil, fmt.Errorf("connect native SSH tunnel: %w", err)
	}
	return &nativeTunnelClient{client: client}, nil
}

type nativeTunnelClient struct {
	client *nativeClient
}

func (c *nativeTunnelClient) Dial(network, address string) (net.Conn, error) {
	return c.client.client.Dial(network, address)
}

func (c *nativeTunnelClient) Wait() error {
	return c.client.client.Wait()
}

func (c *nativeTunnelClient) Close() error {
	return c.client.Close()
}

func (t *NativeSSHTransport) execWithCallback(ctx context.Context, connection ConnectionSpec, req domain.ExecRequest, callback func(string, []byte)) (RawResult, error) {
	if err := validateNativeConnection(connection); err != nil {
		return RawResult{}, err
	}
	if req.Mode == domain.ExecWorkspaceUpload {
		return t.transfer(ctx, connection, req)
	}
	command, stdin, err := buildRemoteCommand(req)
	if err != nil {
		return RawResult{}, err
	}
	command, stdin, err = applyElevation(connection.Target, req, command, stdin)
	if err != nil {
		return RawResult{}, err
	}
	timeout := effectiveTimeout(req.TimeoutSeconds, t.limits)
	execCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	stdout := newCaptureBuffer()
	stderr := newCaptureBuffer()
	stdoutWriter, stderrWriter := io.Writer(stdout), io.Writer(stderr)
	if callback != nil {
		stdoutWriter = io.MultiWriter(stdout, callbackWriter{stream: "stdout", callback: callback})
		stderrWriter = io.MultiWriter(stderr, callbackWriter{stream: "stderr", callback: callback})
	}

	started := time.Now()
	client, err := t.connect(execCtx, connection, nil, false)
	if err != nil {
		return RawResult{ExitCode: -1, Duration: time.Since(started)}, fmt.Errorf("connect native SSH: %w", err)
	}
	defer client.Close()
	session, err := client.client.NewSession()
	if err != nil {
		return RawResult{ExitCode: -1, Duration: time.Since(started)}, fmt.Errorf("create SSH session: %w", err)
	}
	defer session.Close()
	session.Stdin = stdin
	session.Stdout = stdoutWriter
	session.Stderr = stderrWriter

	done := make(chan error, 1)
	go func() { done <- session.Run(command) }()
	var runErr error
	select {
	case runErr = <-done:
	case <-execCtx.Done():
		_ = session.Close()
		_ = client.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		result := RawResult{ExitCode: -1, Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Duration: time.Since(started)}
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if errors.Is(execCtx.Err(), context.DeadlineExceeded) {
			return result, fmt.Errorf("remote command timed out after %s", time.Duration(timeout)*time.Second)
		}
		return result, execCtx.Err()
	}
	if ctx.Err() != nil {
		return RawResult{ExitCode: -1, Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Duration: time.Since(started)}, ctx.Err()
	}
	if execCtx.Err() != nil {
		result := RawResult{ExitCode: -1, Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Duration: time.Since(started)}
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, fmt.Errorf("remote command timed out after %s", time.Duration(timeout)*time.Second)
	}

	result := RawResult{
		ExitCode: nativeExitCode(runErr),
		Stdout:   stdout.Bytes(),
		Stderr:   stderr.Bytes(),
		Duration: time.Since(started),
	}
	if runErr != nil {
		var exitErr *ssh.ExitError
		if !errors.As(runErr, &exitErr) {
			return result, fmt.Errorf("run native SSH command: %w", runErr)
		}
	}
	return result, nil
}

func (t *NativeSSHTransport) transfer(ctx context.Context, connection ConnectionSpec, req domain.ExecRequest) (RawResult, error) {
	if !path.IsAbs(req.RemotePath) || strings.ContainsAny(req.RemotePath, "\r\n\x00") {
		return RawResult{}, fmt.Errorf("remote transfer path must be absolute and contain no control characters")
	}
	if !filepath.IsAbs(req.LocalPath) || strings.ContainsAny(req.LocalPath, "\r\n\x00") {
		return RawResult{}, fmt.Errorf("workspace upload source was not prepared by the control plane")
	}
	local, err := os.Open(req.LocalPath)
	if err != nil {
		return RawResult{}, fmt.Errorf("open transfer source: %w", err)
	}
	defer local.Close()
	info, err := local.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return RawResult{}, fmt.Errorf("transfer source is missing or not a regular file")
	}

	timeout := effectiveTimeout(req.TimeoutSeconds, t.limits)
	transferCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	started := time.Now()
	client, err := t.connect(transferCtx, connection, nil, false)
	if err != nil {
		return RawResult{ExitCode: -1, Duration: time.Since(started)}, fmt.Errorf("connect native SSH for SFTP: %w", err)
	}
	defer client.Close()
	sftpClient, err := sftp.NewClient(client.client)
	if err != nil {
		return RawResult{ExitCode: -1, Duration: time.Since(started)}, fmt.Errorf("start native SFTP: %w", err)
	}
	defer sftpClient.Close()
	remote, err := sftpClient.OpenFile(req.RemotePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return RawResult{ExitCode: -1, Duration: time.Since(started)}, fmt.Errorf("open remote SFTP path: %w", err)
	}

	type copyResult struct {
		written int64
		err     error
	}
	done := make(chan copyResult, 1)
	go func() {
		written, copyErr := io.Copy(remote, local)
		if closeErr := remote.Close(); copyErr == nil {
			copyErr = closeErr
		}
		done <- copyResult{written: written, err: copyErr}
	}()
	select {
	case result := <-done:
		if result.err != nil {
			return RawResult{ExitCode: -1, Duration: time.Since(started)}, fmt.Errorf("upload with native SFTP: %w", result.err)
		}
		return RawResult{ExitCode: 0, Duration: time.Since(started)}, nil
	case <-transferCtx.Done():
		_ = remote.Close()
		_ = sftpClient.Close()
		_ = client.Close()
		if ctx.Err() != nil {
			return RawResult{ExitCode: -1, Duration: time.Since(started)}, ctx.Err()
		}
		if errors.Is(transferCtx.Err(), context.DeadlineExceeded) {
			return RawResult{ExitCode: -1, Duration: time.Since(started)}, fmt.Errorf("SFTP transfer timed out after %s", time.Duration(timeout)*time.Second)
		}
		return RawResult{ExitCode: -1, Duration: time.Since(started)}, transferCtx.Err()
	}
}

func (t *NativeSSHTransport) Probe(ctx context.Context, connection ConnectionSpec) (HostInfo, error) {
	result, err := t.Exec(ctx, connection, domain.ExecRequest{Mode: domain.ExecScript, Script: probeScript, TimeoutSeconds: 15})
	if err != nil {
		return HostInfo{}, err
	}
	if result.ExitCode != 0 {
		return HostInfo{}, fmt.Errorf("probe failed: %s", strings.TrimSpace(string(result.Stderr)))
	}
	return parseProbeOutput(result.Stdout)
}

func (t *NativeSSHTransport) ScanHostKey(ctx context.Context, connection ConnectionSpec) (HostKey, error) {
	if err := validateNativeConnection(connection); err != nil {
		return HostKey{}, err
	}
	var captured ssh.PublicKey
	capture := func(_ string, _ net.Addr, key ssh.PublicKey) error {
		captured = key
		return errHostKeyCaptured
	}
	client, err := t.connect(ctx, connection, capture, true)
	if client != nil {
		_ = client.Close()
	}
	if captured == nil {
		if err == nil {
			err = fmt.Errorf("server completed SSH handshake without presenting a host key")
		}
		return HostKey{}, fmt.Errorf("scan native SSH host key: %w", err)
	}
	address := normalizedSSHAddress(connection.Target)
	line := knownhosts.Line([]string{knownhosts.Normalize(address)}, captured)
	return HostKey{
		Lines: line + "\n", Fingerprint: ssh.FingerprintSHA256(captured), Algorithm: captured.Type(),
		Trusted: t.isHostKeyTrusted(connection.Target, address, captured),
	}, nil
}

type knownHostAddress string

func (address knownHostAddress) Network() string { return "tcp" }
func (address knownHostAddress) String() string  { return string(address) }

func (t *NativeSSHTransport) isHostKeyTrusted(host domain.Host, address string, key ssh.PublicKey) bool {
	t.knownHostsMu.Lock()
	defer t.knownHostsMu.Unlock()
	knownHostsPath := t.knownHostsPath(host)
	if knownHostsPath == "" {
		return false
	}
	callback, err := knownhosts.New(knownHostsPath)
	if err != nil {
		return false
	}
	return callback(address, knownHostAddress(address), key) == nil
}

func (t *NativeSSHTransport) TrustHostKey(ctx context.Context, connection ConnectionSpec, expectedFingerprint string) (HostKey, error) {
	expectedFingerprint = strings.TrimSpace(expectedFingerprint)
	if expectedFingerprint == "" {
		return HostKey{}, fmt.Errorf("expected host key fingerprint is required")
	}
	knownHostsPath := t.knownHostsPath(connection.Target)
	if knownHostsPath == "" {
		return HostKey{}, fmt.Errorf("known_hosts path is not configured")
	}
	key, err := t.ScanHostKey(ctx, connection)
	if err != nil {
		return HostKey{}, err
	}
	if key.Fingerprint != expectedFingerprint {
		return HostKey{}, fmt.Errorf("host key fingerprint mismatch; scanned: %s", key.Fingerprint)
	}
	t.knownHostsMu.Lock()
	defer t.knownHostsMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(knownHostsPath), 0o700); err != nil {
		return HostKey{}, fmt.Errorf("create known_hosts directory: %w", err)
	}
	existing, err := os.ReadFile(knownHostsPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return HostKey{}, fmt.Errorf("read known_hosts: %w", err)
	}
	line := strings.TrimSpace(key.Lines)
	for _, existingLine := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(existingLine) == line {
			key.Trusted = true
			return key, nil
		}
	}
	file, err := os.OpenFile(knownHostsPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return HostKey{}, fmt.Errorf("open known_hosts: %w", err)
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return HostKey{}, fmt.Errorf("secure known_hosts permissions: %w", err)
	}
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		if _, err := file.WriteString("\n"); err != nil {
			return HostKey{}, err
		}
	}
	if _, err := file.WriteString(line + "\n"); err != nil {
		return HostKey{}, fmt.Errorf("append known_hosts: %w", err)
	}
	if err := file.Sync(); err != nil {
		return HostKey{}, fmt.Errorf("sync known_hosts: %w", err)
	}
	key.Trusted = true
	return key, nil
}

func (t *NativeSSHTransport) connect(ctx context.Context, connection ConnectionSpec, targetCallback ssh.HostKeyCallback, skipTargetAuth bool) (*nativeClient, error) {
	hops := append(append([]domain.Host(nil), connection.Jumps...), connection.Target)
	clients := make([]*ssh.Client, 0, len(hops))
	closeClients := func() {
		for index := len(clients) - 1; index >= 0; index-- {
			_ = clients[index].Close()
		}
	}
	for index, host := range hops {
		address := normalizedSSHAddress(host)
		hopCtx, cancel := context.WithTimeout(ctx, nativeConnectTimeout)
		var raw net.Conn
		var err error
		if len(clients) == 0 {
			raw, err = dialFirstHop(hopCtx, address, connection.Target)
		} else {
			raw, err = dialSSHClientContext(hopCtx, clients[len(clients)-1], address)
		}
		if err != nil {
			cancel()
			closeClients()
			return nil, fmt.Errorf("dial %s: %w", address, err)
		}

		isTarget := index == len(hops)-1
		callback := targetCallback
		if !isTarget || callback == nil {
			callback, err = t.strictHostKeyCallback(host)
			if err != nil {
				cancel()
				_ = raw.Close()
				closeClients()
				return nil, err
			}
		}
		auth := nativeAuth{}
		if !(isTarget && skipTargetAuth) {
			auth, err = prepareNativeAuthentication(hopCtx, host)
			if err != nil {
				cancel()
				_ = raw.Close()
				closeClients()
				return nil, fmt.Errorf("prepare authentication for %q: %w", host.Name, err)
			}
		}
		clientConfig := &ssh.ClientConfig{
			User: host.User, Auth: auth.methods, HostKeyCallback: callback,
			ClientVersion: "SSH-2.0-OpsPilot", Timeout: nativeConnectTimeout,
		}
		_ = raw.SetDeadline(time.Now().Add(nativeConnectTimeout))
		stopCancellation := context.AfterFunc(hopCtx, func() { _ = raw.Close() })
		sshConn, channels, requests, handshakeErr := ssh.NewClientConn(raw, address, clientConfig)
		stopCancellation()
		cancel()
		if auth.closer != nil {
			_ = auth.closer.Close()
		}
		if handshakeErr != nil {
			_ = raw.Close()
			closeClients()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("SSH handshake with %s: %w", address, handshakeErr)
		}
		_ = raw.SetDeadline(time.Time{})
		clients = append(clients, ssh.NewClient(sshConn, channels, requests))
	}
	keepaliveCtx, keepaliveCancel := context.WithCancel(context.Background())
	for _, client := range clients {
		go nativeKeepalive(keepaliveCtx, client)
	}
	return &nativeClient{client: clients[len(clients)-1], clients: clients, cancel: keepaliveCancel}, nil
}

func (c *nativeClient) Close() error {
	var closeErr error
	c.once.Do(func() {
		c.cancel()
		for index := len(c.clients) - 1; index >= 0; index-- {
			if err := c.clients[index].Close(); closeErr == nil {
				closeErr = err
			}
		}
	})
	return closeErr
}

func (t *NativeSSHTransport) strictHostKeyCallback(host domain.Host) (ssh.HostKeyCallback, error) {
	knownHostsPath := t.knownHostsPath(host)
	if knownHostsPath == "" {
		return nil, fmt.Errorf("strict host key verification failed for %s: known_hosts path is not configured", normalizedSSHAddress(host))
	}
	if _, err := os.Stat(knownHostsPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("strict host key verification failed for %s: known_hosts file %q does not exist; scan and trust the host key first", normalizedSSHAddress(host), knownHostsPath)
		}
		return nil, fmt.Errorf("inspect known_hosts file: %w", err)
	}
	callback, err := knownhosts.New(knownHostsPath)
	if err != nil {
		return nil, fmt.Errorf("load known_hosts file %q: %w", knownHostsPath, err)
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if err := callback(hostname, remote, key); err != nil {
			var keyErr *knownhosts.KeyError
			if errors.As(err, &keyErr) && len(keyErr.Want) == 0 {
				return fmt.Errorf("unknown SSH host key for %s (%s); scan and trust it first: %w", hostname, ssh.FingerprintSHA256(key), err)
			}
			return fmt.Errorf("SSH host key mismatch for %s (%s): %w", hostname, ssh.FingerprintSHA256(key), err)
		}
		return nil
	}, nil
}

func (t *NativeSSHTransport) knownHostsPath(host domain.Host) string {
	if strings.TrimSpace(host.KnownHostsFile) != "" {
		return strings.TrimSpace(host.KnownHostsFile)
	}
	return strings.TrimSpace(t.config.DefaultKnownHosts)
}

func validateNativeConnection(connection ConnectionSpec) error {
	for _, host := range append(append([]domain.Host(nil), connection.Jumps...), connection.Target) {
		if err := validateHost(host); err != nil {
			return err
		}
	}
	if _, err := NormalizeProxyURL(connection.Target.ProxyURL); err != nil {
		return err
	}
	return nil
}

func normalizedSSHAddress(host domain.Host) string {
	address := strings.TrimSpace(host.Address)
	if strings.HasPrefix(address, "[") && strings.HasSuffix(address, "]") {
		address = strings.TrimSuffix(strings.TrimPrefix(address, "["), "]")
	}
	return net.JoinHostPort(address, strconv.Itoa(host.Port))
}

func dialSSHClientContext(ctx context.Context, client *ssh.Client, address string) (net.Conn, error) {
	type result struct {
		connection net.Conn
		err        error
	}
	done := make(chan result)
	abandoned := make(chan struct{})
	go func() {
		connection, err := client.Dial("tcp", address)
		select {
		case done <- result{connection: connection, err: err}:
		case <-abandoned:
			if connection != nil {
				_ = connection.Close()
			}
		}
	}()
	select {
	case result := <-done:
		return result.connection, result.err
	case <-ctx.Done():
		_ = client.Close()
		timer := time.NewTimer(nativeDialCancelWait)
		defer timer.Stop()
		select {
		case result := <-done:
			if result.connection != nil {
				_ = result.connection.Close()
			}
		case <-timer.C:
			close(abandoned)
		}
		return nil, ctx.Err()
	}
}

func nativeKeepalive(ctx context.Context, client *ssh.Client) {
	ticker := time.NewTicker(nativeKeepalivePeriod)
	defer ticker.Stop()
	misses := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			done := make(chan error, 1)
			go func() {
				_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil {
					misses = 0
					continue
				}
				misses++
			case <-time.After(nativeKeepaliveWait):
				misses++
			case <-ctx.Done():
				return
			}
			if misses >= 2 {
				_ = client.Close()
				return
			}
		}
	}
}

func effectiveTimeout(requested int, limits config.Limits) int {
	timeout := requested
	if timeout <= 0 {
		timeout = limits.SyncTimeoutSeconds
	}
	if limits.MaxTimeoutSeconds > 0 && timeout > limits.MaxTimeoutSeconds {
		timeout = limits.MaxTimeoutSeconds
	}
	if timeout <= 0 {
		timeout = 60
	}
	return timeout
}

func nativeExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *ssh.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitStatus()
	}
	return -1
}
